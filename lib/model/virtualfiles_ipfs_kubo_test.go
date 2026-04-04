// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build integration
// +build integration

package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/scanner"
)

const (
	realKuboAPIEnv    = "STTESTIPFSAPI"
	realKuboBinaryEnv = "STTESTKUBO"
)

type realKuboHarness struct {
	t       *testing.T
	apiURL  string
	repo    string
	bin     string
	logPath string
	managed bool
	cmd     *exec.Cmd
	client  *http.Client
}

func newRealKuboHarness(t *testing.T) *realKuboHarness {
	t.Helper()

	if apiURL := strings.TrimSpace(os.Getenv(realKuboAPIEnv)); apiURL != "" {
		h := &realKuboHarness{
			t:      t,
			apiURL: apiURL,
			client: &http.Client{Timeout: 5 * time.Second},
		}
		if err := h.awaitHealthy(20 * time.Second); err != nil {
			t.Fatalf("%s is set but Kubo is not healthy: %v", realKuboAPIEnv, err)
		}
		return h
	}

	bin := strings.TrimSpace(os.Getenv(realKuboBinaryEnv))
	if bin == "" {
		if path, err := exec.LookPath("kubo"); err == nil {
			bin = path
		} else if path, err := exec.LookPath("ipfs"); err == nil {
			bin = path
		}
	}
	if bin == "" {
		t.Skipf("real Kubo tests require %s or a kubo/ipfs binary on PATH", realKuboAPIEnv)
	}

	h := &realKuboHarness{
		t:       t,
		bin:     bin,
		repo:    filepath.Join(t.TempDir(), "kubo-repo"),
		logPath: filepath.Join(t.TempDir(), "kubo.log"),
		managed: true,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	if err := h.initRepo(); err != nil {
		t.Fatalf("initializing kubo repo: %v", err)
	}
	if err := h.start(); err != nil {
		t.Fatalf("starting kubo daemon: %v", err)
	}
	t.Cleanup(h.stop)
	return h
}

func (h *realKuboHarness) requireManaged(t *testing.T) {
	t.Helper()
	if !h.managed {
		t.Skip("test requires a managed local Kubo daemon; STTESTIPFSAPI uses an external daemon")
	}
}

func (h *realKuboHarness) initRepo() error {
	if err := os.MkdirAll(h.repo, 0o755); err != nil {
		return err
	}

	cmd := exec.Command(h.bin, "init", "--profile=test")
	cmd.Env = append(os.Environ(), "IPFS_PATH="+h.repo)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubo init failed: %w\n%s", err, output)
	}

	apiPort := realKuboFreeTCPPort(h.t)
	gatewayPort := realKuboFreeTCPPort(h.t)
	swarmPort := realKuboFreeTCPPort(h.t)

	cfgPath := filepath.Join(h.repo, "config")
	bs, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}

	var raw map[string]any
	if err := json.Unmarshal(bs, &raw); err != nil {
		return err
	}

	addresses, _ := raw["Addresses"].(map[string]any)
	if addresses == nil {
		addresses = make(map[string]any)
		raw["Addresses"] = addresses
	}
	addresses["API"] = fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", apiPort)
	addresses["Gateway"] = fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", gatewayPort)
	addresses["Swarm"] = []any{fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", swarmPort)}
	raw["Bootstrap"] = []any{}

	updated, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, updated, 0o600); err != nil {
		return err
	}

	h.apiURL = fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	return nil
}

func (h *realKuboHarness) start() error {
	logfd, err := os.Create(h.logPath)
	if err != nil {
		return err
	}

	cmd := exec.Command(h.bin, "daemon", "--offline")
	cmd.Env = append(os.Environ(), "IPFS_PATH="+h.repo)
	cmd.Stdout = logfd
	cmd.Stderr = logfd
	if err := cmd.Start(); err != nil {
		_ = logfd.Close()
		return err
	}
	h.cmd = cmd

	if err := h.awaitHealthy(30 * time.Second); err != nil {
		h.stop()
		return err
	}
	return nil
}

func (h *realKuboHarness) stop() {
	if !h.managed || h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func(cmd *exec.Cmd) {
		done <- cmd.Wait()
	}(h.cmd)
	select {
	case <-time.After(10 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	case <-done:
	}
	h.cmd = nil
}

func (h *realKuboHarness) restart() error {
	h.requireManaged(h.t)
	h.stop()
	return h.start()
}

func (h *realKuboHarness) awaitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := h.do(context.Background(), http.MethodPost, "/api/v0/version", nil, nil, "")
		if err == nil && resp.StatusCode == http.StatusOK {
			_, _, _ = (*ProcesslessResponseReader)(nil).read(resp)
			return nil
		}
		if err == nil {
			body, _, readErr := (*ProcesslessResponseReader)(nil).read(resp)
			if readErr != nil {
				lastErr = readErr
			} else {
				lastErr = fmt.Errorf("status %d body=%q", resp.StatusCode, string(body))
			}
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timed out waiting for kubo health")
	}
	return fmt.Errorf("%w\n%s", lastErr, h.logText())
}

func (h *realKuboHarness) do(ctx context.Context, method, endpoint string, params url.Values, body io.Reader, contentType string) (*http.Response, error) {
	apiURL, err := url.Parse(h.apiURL)
	if err != nil {
		return nil, err
	}
	apiURL.Path = path.Join(strings.TrimSuffix(apiURL.Path, "/"), endpoint)
	if len(params) > 0 {
		apiURL.RawQuery = params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, apiURL.String(), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return h.client.Do(req)
}

func (h *realKuboHarness) addObject(data []byte) (string, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "payload")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	params := url.Values{
		"pin":         []string{"false"},
		"cid-version": []string{"1"},
		"raw-leaves":  []string{"true"},
		"hash":        []string{"sha2-256"},
	}
	resp, err := h.do(context.Background(), http.MethodPost, "/api/v0/add", params, body, writer.FormDataContentType())
	if err != nil {
		return "", err
	}
	responseBody, status, err := (*ProcesslessResponseReader)(nil).read(resp)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("add status %d body=%q", status, string(responseBody))
	}
	return parseIPFSAddResponse(responseBody)
}

func (h *realKuboHarness) removeBlock(locator string) error {
	h.requireManaged(h.t)
	resp, err := h.do(context.Background(), http.MethodPost, "/api/v0/block/rm", url.Values{
		"arg":   []string{locator},
		"force": []string{"true"},
	}, nil, "")
	if err != nil {
		return err
	}
	body, status, err := (*ProcesslessResponseReader)(nil).read(resp)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("block/rm status %d body=%q", status, string(body))
	}
	return nil
}

func (h *realKuboHarness) logText() string {
	if h.logPath == "" {
		return ""
	}
	bs, err := os.ReadFile(h.logPath)
	if err != nil {
		return fmt.Sprintf("failed reading kubo log: %v", err)
	}
	return string(bs)
}

func realKuboFreeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestRealKuboIPFSContentAddressableBackendPutGet(t *testing.T) {
	kubo := newRealKuboHarness(t)
	wrapper, _ := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, kubo.apiURL, true)

	m := setupModel(t, wrapper)
	defer cleanupModel(m)

	ipfs := currentIPFSCASBackend(t, m)
	data := []byte("real kubo cas object payload")
	ref := contentAddressableReferenceForBytes(data)

	storedRef, err := ipfs.Put(context.Background(), ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if storedRef.Backend != "ipfsCAS" || storedRef.Locator == "" {
		t.Fatalf("unexpected stored ref: %+v", storedRef)
	}

	rc, err := ipfs.Get(context.Background(), storedRef)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("unexpected IPFS payload %q", got)
	}
	if health := ipfs.Health(context.Background()); !health.Healthy {
		t.Fatalf("expected healthy IPFS backend, got %+v", health)
	}
}

func TestRealKuboFetchVirtualFileUsesExistingIPFSContentWithoutRedownload(t *testing.T) {
	kubo := newRealKuboHarness(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, kubo.apiURL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("fetch directly from real kubo")
	fc.addFile("realkubo-cached.txt", 0o644, protocol.FileInfoTypeFile, data)
	file := fc.files[0]
	if err := m.sdb.Update(fcfg.ID, device1, []protocol.FileInfo{prepareFileInfoForIndex(file)}); err != nil {
		t.Fatal(err)
	}

	ipfs := currentIPFSCASBackend(t, m)
	ref := contentAddressableReferenceForBytes(data)
	storedRef, err := ipfs.Put(context.Background(), ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := ipfs.AssociateFile(fcfg, file, storedRef); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	fc.RequestCalls(func(context.Context, *protocol.Request) ([]byte, error) {
		t.Fatal("real-Kubo-backed fetch should not request remote blocks")
		return nil, nil
	})

	presence, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name)
	if err != nil {
		t.Fatal(err)
	}
	if presence.State != FullLocal {
		t.Fatalf("expected FullLocal after fetch, got %+v", presence)
	}
	if fc.RequestCallCount() != 0 {
		t.Fatalf("expected no remote requests, got %d", fc.RequestCallCount())
	}
}

func TestRealKuboFetchVirtualFileFallsBackWhenObjectMissing(t *testing.T) {
	kubo := newRealKuboHarness(t)
	kubo.requireManaged(t)

	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, kubo.apiURL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("remote after missing real kubo object")
	fc.addFile("realkubo-missing.txt", 0o644, protocol.FileInfoTypeFile, data)
	file := fc.files[0]
	if err := m.sdb.Update(fcfg.ID, device1, []protocol.FileInfo{prepareFileInfoForIndex(file)}); err != nil {
		t.Fatal(err)
	}

	ipfs := currentIPFSCASBackend(t, m)
	ref := contentAddressableReferenceForBytes(data)
	storedRef, err := ipfs.Put(context.Background(), ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := ipfs.AssociateFile(fcfg, file, storedRef); err != nil {
		t.Fatal(err)
	}
	if err := kubo.removeBlock(storedRef.Locator); err != nil {
		t.Skipf("managed kubo block removal unavailable: %v", err)
	}
	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	beforeRequests := fc.RequestCallCount()
	if _, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}
	if fc.RequestCallCount() <= beforeRequests {
		t.Fatal("expected remote fallback after missing real-Kubo object")
	}
}

func TestRealKuboFetchVirtualFileFallsBackWhenContentMismatches(t *testing.T) {
	kubo := newRealKuboHarness(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, kubo.apiURL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	expected := []byte("expected bytes from remote")
	wrong := []byte("wrong bytes from kubo")
	fc.addFile("realkubo-mismatch.txt", 0o644, protocol.FileInfoTypeFile, expected)
	file := fc.files[0]
	if err := m.sdb.Update(fcfg.ID, device1, []protocol.FileInfo{prepareFileInfoForIndex(file)}); err != nil {
		t.Fatal(err)
	}

	locator, err := kubo.addObject(wrong)
	if err != nil {
		t.Fatal(err)
	}
	ref := contentAddressableReferenceForBytes(expected)
	ref.Backend = "ipfsCAS"
	ref.Locator = locator

	ipfs := currentIPFSCASBackend(t, m)
	if err := ipfs.AssociateFile(fcfg, file, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	beforeRequests := fc.RequestCallCount()
	if _, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}
	if fc.RequestCallCount() <= beforeRequests {
		t.Fatal("expected remote fallback after mismatched real-Kubo content")
	}
}

func TestRealKuboFetchVirtualFileFallsBackWhenDaemonUnavailable(t *testing.T) {
	kubo := newRealKuboHarness(t)
	kubo.requireManaged(t)

	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, kubo.apiURL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("remote after kubo unavailable")
	fc.addFile("realkubo-down.txt", 0o644, protocol.FileInfoTypeFile, data)
	file := fc.files[0]
	if err := m.sdb.Update(fcfg.ID, device1, []protocol.FileInfo{prepareFileInfoForIndex(file)}); err != nil {
		t.Fatal(err)
	}

	ipfs := currentIPFSCASBackend(t, m)
	ref := contentAddressableReferenceForBytes(data)
	storedRef, err := ipfs.Put(context.Background(), ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := ipfs.AssociateFile(fcfg, file, storedRef); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	kubo.stop()

	beforeRequests := fc.RequestCallCount()
	if _, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}
	if fc.RequestCallCount() <= beforeRequests {
		t.Fatal("expected remote fallback after kubo became unavailable")
	}
	if health := ipfs.Health(context.Background()); health.Healthy {
		t.Fatalf("expected unhealthy IPFS health after kubo stop, got %+v", health)
	}
}

func TestRealKuboIPFSStatePersistsAcrossRestart(t *testing.T) {
	kubo := newRealKuboHarness(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, kubo.apiURL, true)

	dbDir := t.TempDir()
	m := newModelWithDBDir(t, wrapper, myID, nil, dbDir)
	m.ServeBackground()
	if err := m.ScanFolder(fcfg.ID); err != nil {
		t.Fatal(err)
	}

	data := []byte("restart from real kubo ipfs")
	blockSize := protocol.BlockSize(int64(len(data)))
	blocks, err := scanner.Blocks(context.Background(), bytes.NewReader(data), blockSize, int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	file := protocol.FileInfo{
		Name:         "realkubo-restart.txt",
		Type:         protocol.FileInfoTypeFile,
		Size:         int64(len(data)),
		ModifiedS:    time.Now().Unix(),
		Permissions:  0o644,
		RawBlockSize: int32(blockSize),
		Blocks:       blocks,
		Version:      protocol.Vector{}.Update(device1.Short()),
		Sequence:     1,
	}
	if err := m.sdb.Update(fcfg.ID, device1, []protocol.FileInfo{prepareFileInfoForIndex(file)}); err != nil {
		t.Fatal(err)
	}

	ipfs := currentIPFSCASBackend(t, m)
	ref := contentAddressableReferenceForBytes(data)
	storedRef, err := ipfs.Put(context.Background(), ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := ipfs.AssociateFile(fcfg, file, storedRef); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	cleanupModel(m)

	if kubo.managed {
		if err := kubo.restart(); err != nil {
			t.Fatalf("restarting kubo: %v", err)
		}
	}

	m = newModelWithDBDir(t, wrapper, myID, nil, dbDir)
	m.ServeBackground()
	defer cleanupModel(m)

	presence, err := m.VirtualFilePresence(fcfg.ID, file.Name)
	if err != nil {
		t.Fatal(err)
	}
	if presence.State != MetadataOnly {
		t.Fatalf("expected metadata-only after restart, got %+v", presence)
	}
	if ref, ok, err := currentIPFSCASBackend(t, m).ReferenceForFile(fcfg, file); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("expected IPFS ref after restart")
	} else if ref.Backend != "ipfsCAS" {
		t.Fatalf("unexpected IPFS ref after restart: %+v", ref)
	}
}

func TestRealKuboFetchVirtualFileNoOverwriteLocalWriter(t *testing.T) {
	kubo := newRealKuboHarness(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, kubo.apiURL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("real kubo must not overwrite local writer")
	fc.addFile("realkubo-local-writer.txt", 0o644, protocol.FileInfoTypeFile, data)
	file := fc.files[0]
	if err := m.sdb.Update(fcfg.ID, device1, []protocol.FileInfo{prepareFileInfoForIndex(file)}); err != nil {
		t.Fatal(err)
	}

	ipfs := currentIPFSCASBackend(t, m)
	ref := contentAddressableReferenceForBytes(data)
	storedRef, err := ipfs.Put(context.Background(), ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := ipfs.AssociateFile(fcfg, file, storedRef); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	localPath := filepath.Join(fcfg.Path, filepath.FromSlash(file.Name))
	if err := os.WriteFile(localPath, []byte("local-writer-wins"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name); !errors.Is(err, ErrVirtualFileAlreadyLocal) {
		t.Fatalf("expected ErrVirtualFileAlreadyLocal, got %v", err)
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("local-writer-wins")) {
		t.Fatalf("unexpected local file contents %q", got)
	}
}
