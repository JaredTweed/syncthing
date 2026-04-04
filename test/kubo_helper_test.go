// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build integration
// +build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/internal/db/sqlite"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/scanner"
)

const (
	virtualFilesPresenceKVPrefix    = "virtualfiles/presence"
	virtualFilesIPFSObjectMetaKV    = "virtualfiles/ipfs/object-meta"
	virtualFilesIPFSFileRefKVPrefix = "virtualfiles/ipfs/file-refs"

	realKuboAPIEnv    = "STTESTIPFSAPI"
	realKuboBinaryEnv = "STTESTKUBO"
)

type integrationVirtualFileRecord struct {
	State      model.PresenceState                `json:"state"`
	ContentRef *model.ContentAddressableReference `json:"contentRef,omitempty"`
	Updated    time.Time                          `json:"updated"`
}

type integrationContentAddressableObjectMeta struct {
	Size         int64     `json:"size"`
	RefCountHint int       `json:"refCountHint"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

type integrationContentAddressableFileRefRecord struct {
	Ref        model.ContentAddressableReference `json:"ref"`
	Size       int64                             `json:"size"`
	BlocksHash []byte                            `json:"blocksHash,omitempty"`
	Updated    time.Time                         `json:"updated"`
}

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
		t.Skipf("real Kubo binary suite requires %s or a kubo/ipfs binary on PATH", realKuboAPIEnv)
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

	apiPort := freeTCPPort(h.t)
	gatewayPort := freeTCPPort(h.t)
	swarmPort := freeTCPPort(h.t)

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
			_, _, _ = readHTTPResponse(resp)
			return nil
		}
		if err == nil {
			body, _, readErr := readHTTPResponse(resp)
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
	responseBody, status, err := readHTTPResponse(resp)
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
	body, status, err := readHTTPResponse(resp)
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

func injectIPFSPlaceholderState(t *testing.T, n *virtualFileNode, file string, data []byte, ref model.ContentAddressableReference) {
	t.Helper()

	mdb, err := sqlite.Open(n.home)
	if err != nil {
		t.Fatal(err)
	}
	defer mdb.Close()

	now := time.Now().UTC()
	fileRefStore := db.NewTyped(mdb, virtualFilesIPFSFileRefKVPrefix+"/"+virtualFilesFolderID)
	presenceStore := db.NewTyped(mdb, virtualFilesPresenceKVPrefix+"/"+virtualFilesFolderID)
	objectMetaStore := db.NewTyped(mdb, virtualFilesIPFSObjectMetaKV)

	record := integrationContentAddressableFileRefRecord{
		Ref:        ref,
		Size:       int64(len(data)),
		BlocksHash: fileBlocksHashForData(t, data),
		Updated:    now,
	}
	recordBytes, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := fileRefStore.PutBytes(file, recordBytes); err != nil {
		t.Fatal(err)
	}

	vfRecord := integrationVirtualFileRecord{
		State:      model.MetadataOnly,
		ContentRef: &ref,
		Updated:    now,
	}
	vfBytes, err := json.Marshal(vfRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := presenceStore.PutBytes(file, vfBytes); err != nil {
		t.Fatal(err)
	}

	meta := integrationContentAddressableObjectMeta{
		Size:         int64(len(data)),
		RefCountHint: 1,
		Created:      now,
		Updated:      now,
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := objectMetaStore.PutBytes(ref.Locator, metaBytes); err != nil {
		t.Fatal(err)
	}
}

func ipfsReferenceForBytes(data []byte, locator string) model.ContentAddressableReference {
	sum := sha256.Sum256(data)
	return model.ContentAddressableReference{
		Scheme:  "sha256",
		Key:     hex.EncodeToString(sum[:]),
		Size:    int64(len(data)),
		Backend: "ipfsCAS",
		Locator: locator,
	}
}

func fileBlocksHashForData(t *testing.T, data []byte) []byte {
	t.Helper()
	blockSize := protocol.BlockSize(int64(len(data)))
	blocks, err := scanner.Blocks(context.Background(), bytes.NewReader(data), blockSize, int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.BlocksHash(blocks)
}

func parseIPFSAddResponse(body []byte) (string, error) {
	lines := bytes.Split(bytes.TrimSpace(body), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var resp struct {
			Hash string `json:"Hash"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			return "", err
		}
		if resp.Hash == "" {
			return "", errors.New("missing Hash in add response")
		}
		return resp.Hash, nil
	}
	return "", errors.New("missing add response")
}

func readHTTPResponse(resp *http.Response) ([]byte, int, error) {
	bs, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	return bs, resp.StatusCode, err
}
