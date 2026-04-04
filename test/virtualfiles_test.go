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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/rc"
)

const (
	virtualFilesFolderID  = "default"
	placeholderPullDelayS = 3600
	virtualFilesTimeout   = 45 * time.Second
)

type virtualFileNode struct {
	t            *testing.T
	name         string
	home         string
	folder       string
	certFixture  string
	myID         protocol.DeviceID
	peerID       protocol.DeviceID
	guiPort      int
	listenPort   int
	peerPort     int
	experimental bool
	pullerDelay  float64
	ipfsEnabled  bool
	ipfsAPIURL   string
	ipfsPrefer   bool
	ipfsTimeoutS int
	ipfsHealthS  int

	proc *rc.Process
}

func TestExperimentalVirtualFilesBinaryMixedPeerSafety(t *testing.T) {
	expID, _ := protocol.DeviceIDFromString(id1)
	normID, _ := protocol.DeviceIDFromString(id2)
	baseDir := t.TempDir()

	exp := newVirtualFileNode(t, baseDir, "exp-mixed", "h1", expID, normID, true, placeholderPullDelayS)
	norm := newVirtualFileNode(t, baseDir, "norm-mixed", "h2", normID, expID, false, 0)
	linkNodes(t, exp, norm)

	exp.start()
	norm.start()
	exp.resume()
	norm.resume()
	awaitPeerConnected(t, exp, norm.myID)
	awaitPeerConnected(t, norm, exp.myID)

	writeNodeFile(t, norm, "placeholder-update.txt", []byte("v1-from-normal"))
	mustRescan(t, norm, "placeholder-update.txt")
	awaitGlobalKnown(t, exp, virtualFilesFolderID, "placeholder-update.txt")

	if _, status, err := norm.proc.SetMetadataOnly(virtualFilesFolderID, "placeholder-update.txt"); err != nil || status != http.StatusConflict {
		t.Fatalf("expected normal peer metadata-only API to be disabled with 409, got status=%d err=%v", status, err)
	}

	presence, status, err := exp.proc.SetMetadataOnly(virtualFilesFolderID, "placeholder-update.txt")
	if err != nil || status != http.StatusOK {
		t.Fatalf("SetMetadataOnly failed: status=%d err=%v", status, err)
	}
	if presence.State != model.MetadataOnly {
		t.Fatalf("expected MetadataOnly, got %+v", presence)
	}
	awaitVirtualFileState(t, exp, "placeholder-update.txt", model.MetadataOnly)
	awaitNeedFiles(t, exp, 0)
	assertPathAbsent(t, nodePath(exp, "placeholder-update.txt"))

	time.Sleep(1100 * time.Millisecond)
	writeNodeFile(t, norm, "placeholder-update.txt", []byte("v2-updated-remotely"))
	mustRescan(t, norm, "placeholder-update.txt")
	awaitVirtualFileState(t, exp, "placeholder-update.txt", model.MetadataOnly)
	assertPathAbsent(t, nodePath(exp, "placeholder-update.txt"))

	presence = fetchVirtualFileUntilReady(t, exp, "placeholder-update.txt")
	if presence.State != model.FullLocal {
		t.Fatalf("expected FullLocal after fetch, got %+v", presence)
	}
	assertFileContents(t, nodePath(exp, "placeholder-update.txt"), []byte("v2-updated-remotely"))

	writeNodeFile(t, norm, "placeholder-delete.txt", []byte("delete-me"))
	mustRescan(t, norm, "placeholder-delete.txt")
	awaitGlobalKnown(t, exp, virtualFilesFolderID, "placeholder-delete.txt")
	if _, status, err := exp.proc.SetMetadataOnly(virtualFilesFolderID, "placeholder-delete.txt"); err != nil || status != http.StatusOK {
		t.Fatalf("SetMetadataOnly(delete) failed: status=%d err=%v", status, err)
	}
	removePathOrFatal(t, nodePath(norm, "placeholder-delete.txt"))
	mustRescan(t, norm, "placeholder-delete.txt")
	awaitVirtualFileMissing(t, exp, "placeholder-delete.txt")
	assertPathAbsent(t, nodePath(exp, "placeholder-delete.txt"))

	writeNodeFile(t, norm, "placeholder-type.txt", []byte("type-file"))
	mustRescan(t, norm, "placeholder-type.txt")
	awaitGlobalKnown(t, exp, virtualFilesFolderID, "placeholder-type.txt")
	if _, status, err := exp.proc.SetMetadataOnly(virtualFilesFolderID, "placeholder-type.txt"); err != nil || status != http.StatusOK {
		t.Fatalf("SetMetadataOnly(type) failed: status=%d err=%v", status, err)
	}
	time.Sleep(1100 * time.Millisecond)
	removePathOrFatal(t, nodePath(norm, "placeholder-type.txt"))
	if err := os.Mkdir(nodePath(norm, "placeholder-type.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodePath(norm, "placeholder-type.txt"), "child.txt"), []byte("dir child"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRescan(t, norm, "placeholder-type.txt")
	awaitVirtualFileMissing(t, exp, "placeholder-type.txt")
	setPullerDelay(t, exp, 0)
	awaitDirExists(t, nodePath(exp, "placeholder-type.txt"))
	setPullerDelay(t, exp, placeholderPullDelayS)

	writeNodeFile(t, norm, "placeholder-ignore.txt", []byte("ignore-me"))
	mustRescan(t, norm, "placeholder-ignore.txt")
	awaitGlobalKnown(t, exp, virtualFilesFolderID, "placeholder-ignore.txt")
	if _, status, err := exp.proc.SetMetadataOnly(virtualFilesFolderID, "placeholder-ignore.txt"); err != nil || status != http.StatusOK {
		t.Fatalf("SetMetadataOnly(ignore) failed: status=%d err=%v", status, err)
	}
	if err := os.WriteFile(filepath.Join(exp.folder, ".stignore"), []byte("placeholder-ignore.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRescan(t, exp, "")
	awaitVirtualFileMissing(t, exp, "placeholder-ignore.txt")

	writeNodeFile(t, norm, "placeholder-local.txt", []byte("remote-content"))
	mustRescan(t, norm, "placeholder-local.txt")
	awaitGlobalKnown(t, exp, virtualFilesFolderID, "placeholder-local.txt")
	if _, status, err := exp.proc.SetMetadataOnly(virtualFilesFolderID, "placeholder-local.txt"); err != nil || status != http.StatusOK {
		t.Fatalf("SetMetadataOnly(local) failed: status=%d err=%v", status, err)
	}
	if err := os.WriteFile(nodePath(exp, "placeholder-local.txt"), []byte("local-writer-wins"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, status, err := exp.proc.FetchVirtualFile(virtualFilesFolderID, "placeholder-local.txt"); err != nil || status != http.StatusConflict {
		t.Fatalf("expected fetch conflict for preexisting local file, got status=%d err=%v", status, err)
	}
	assertFileContents(t, nodePath(exp, "placeholder-local.txt"), []byte("local-writer-wins"))

	setPullerDelay(t, exp, 0)
	writeNodeFile(t, norm, "ordinary-sync.txt", []byte("ordinary-sync-still-works"))
	mustRescan(t, norm, "ordinary-sync.txt")
	awaitSyncWithTimeout(t, virtualFilesFolderID, virtualFilesTimeout, exp.proc, norm.proc)
	assertFileContents(t, nodePath(exp, "ordinary-sync.txt"), []byte("ordinary-sync-still-works"))
}

func TestExperimentalVirtualFilesBinaryCASRestart(t *testing.T) {
	leftID, _ := protocol.DeviceIDFromString(id1)
	rightID, _ := protocol.DeviceIDFromString(id2)
	baseDir := t.TempDir()

	left := newVirtualFileNode(t, baseDir, "exp-left", "h1", leftID, rightID, true, 0)
	right := newVirtualFileNode(t, baseDir, "exp-right", "h2", rightID, leftID, true, placeholderPullDelayS)
	linkNodes(t, left, right)

	left.start()
	right.start()
	left.resume()
	right.resume()
	awaitPeerConnected(t, left, right.myID)
	awaitPeerConnected(t, right, left.myID)

	writeNodeFile(t, left, "cas-restart.txt", []byte("cas-persistent-content"))
	mustRescan(t, left, "cas-restart.txt")
	awaitGlobalKnown(t, right, virtualFilesFolderID, "cas-restart.txt")

	if _, status, err := right.proc.SetMetadataOnly(virtualFilesFolderID, "cas-restart.txt"); err != nil || status != http.StatusOK {
		t.Fatalf("SetMetadataOnly failed: status=%d err=%v", status, err)
	}
	right.restart()
	right.resume()
	awaitPeerConnected(t, right, left.myID)
	awaitVirtualFileState(t, right, "cas-restart.txt", model.MetadataOnly)
	assertPathAbsent(t, nodePath(right, "cas-restart.txt"))

	presence := fetchVirtualFileUntilReady(t, right, "cas-restart.txt")
	if presence.State != model.FullLocal {
		t.Fatalf("expected FullLocal after initial fetch, got %+v", presence)
	}
	content := []byte("cas-persistent-content")
	assertFileContents(t, nodePath(right, "cas-restart.txt"), content)
	assertCASObjectExists(t, right, content)

	right.restart()
	right.resume()
	awaitPeerConnected(t, right, left.myID)
	awaitVirtualFileState(t, right, "cas-restart.txt", model.FullLocal)
	assertFileContents(t, nodePath(right, "cas-restart.txt"), content)
	assertCASObjectExists(t, right, content)
}

func newVirtualFileNode(t *testing.T, baseDir, name, certFixture string, myID, peerID protocol.DeviceID, experimental bool, pullerDelay float64) *virtualFileNode {
	t.Helper()
	return &virtualFileNode{
		t:            t,
		name:         name,
		home:         filepath.Join(baseDir, name, "home"),
		folder:       filepath.Join(baseDir, name, "folder"),
		certFixture:  certFixture,
		myID:         myID,
		peerID:       peerID,
		experimental: experimental,
		pullerDelay:  pullerDelay,
		ipfsTimeoutS: 1,
		ipfsHealthS:  1,
	}
}

func enableNodeIPFS(n *virtualFileNode, apiURL string, prefer bool) {
	n.ipfsEnabled = true
	n.ipfsAPIURL = apiURL
	n.ipfsPrefer = prefer
	if n.ipfsTimeoutS < 1 {
		n.ipfsTimeoutS = 1
	}
	if n.ipfsHealthS < 1 {
		n.ipfsHealthS = 1
	}
}

func linkNodes(t *testing.T, a, b *virtualFileNode) {
	t.Helper()
	a.guiPort = freeTCPPort(t)
	a.listenPort = freeTCPPort(t)
	b.guiPort = freeTCPPort(t)
	b.listenPort = freeTCPPort(t)
	a.peerPort = b.listenPort
	b.peerPort = a.listenPort
}

func (n *virtualFileNode) start() {
	n.t.Helper()
	writeNodeConfig(n.t, n)

	p := rc.NewProcess(fmt.Sprintf("127.0.0.1:%d", n.guiPort))
	if err := p.LogTo(filepath.Join(n.home, "syncthing.log")); err != nil {
		n.t.Fatal(err)
	}
	if err := p.Start(integrationSyncthingBinary(n.t), "--home", n.home, "--no-browser"); err != nil {
		n.t.Fatal(err)
	}
	p.AwaitStartup()
	awaitNodeAPIReady(n.t, n.home, p)
	if err := p.PauseAll(); err != nil {
		n.t.Fatalf("pause failed: %v\n%s", err, readNodeLog(n.home))
	}
	n.proc = p
}

func (n *virtualFileNode) restart() {
	n.t.Helper()
	n.stop()
	n.start()
}

func (n *virtualFileNode) stop() {
	n.t.Helper()
	if n.proc == nil {
		return
	}
	checkedStop(n.t, n.proc)
	n.proc = nil
}

func (n *virtualFileNode) resume() {
	n.t.Helper()
	if err := n.proc.ResumeAll(); err != nil {
		n.t.Fatal(err)
	}
}

func integrationSyncthingBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("STTESTBINARY"); path != "" {
		return path
	}
	candidates := []string{"../bin/syncthing", "../syncthing"}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Fatalf("could not find syncthing binary in %v and STTESTBINARY is unset", candidates)
	return ""
}

func writeNodeConfig(t *testing.T, n *virtualFileNode) {
	t.Helper()
	if err := os.MkdirAll(n.home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(n.folder, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"cert.pem", "key.pem"} {
		src := filepath.Join(n.certFixture, name)
		dst := filepath.Join(n.home, name)
		bs, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, bs, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.New(n.myID)
	cfg.GUI.Enabled = true
	cfg.GUI.RawUseTLS = false
	cfg.GUI.RawAddress = fmt.Sprintf("127.0.0.1:%d", n.guiPort)
	cfg.GUI.APIKey = apiKey
	cfg.GUI.User = ""
	cfg.GUI.Password = ""

	cfg.Options.RawListenAddresses = []string{fmt.Sprintf("tcp://127.0.0.1:%d", n.listenPort)}
	cfg.Options.RawGlobalAnnServers = []string{}
	cfg.Options.GlobalAnnEnabled = false
	cfg.Options.LocalAnnEnabled = false
	cfg.Options.RelaysEnabled = false
	cfg.Options.NATEnabled = false
	cfg.Options.StartBrowser = false
	cfg.Options.KeepTemporariesH = 1
	cfg.Options.ProgressUpdateIntervalS = 1
	cfg.Options.ExperimentalVirtualFiles = n.experimental
	cfg.Options.ExperimentalVirtualFilesIPFSEnabled = n.experimental && n.ipfsEnabled
	cfg.Options.ExperimentalVirtualFilesIPFSAPIURL = n.ipfsAPIURL
	cfg.Options.ExperimentalVirtualFilesIPFSTimeoutS = n.ipfsTimeoutS
	cfg.Options.ExperimentalVirtualFilesIPFSHealthIntervalS = n.ipfsHealthS
	cfg.Options.ExperimentalVirtualFilesIPFSPrefer = n.ipfsPrefer
	cfg.Options.CREnabled = false
	cfg.Options.AuditEnabled = false

	peer := cfg.Defaults.Device.Copy()
	peer.DeviceID = n.peerID
	peer.Name = n.peerID.String()
	peer.Addresses = []string{fmt.Sprintf("tcp://127.0.0.1:%d", n.peerPort)}
	cfg.SetDevice(peer)

	folder := cfg.Defaults.Folder.Copy()
	folder.ID = virtualFilesFolderID
	folder.Label = virtualFilesFolderID
	folder.FilesystemType = config.FilesystemTypeBasic
	folder.Path = n.folder
	folder.Type = config.FolderTypeSendReceive
	folder.RescanIntervalS = 3600
	folder.FSWatcherEnabled = false
	folder.PullerDelayS = n.pullerDelay
	folder.Devices = []config.FolderDeviceConfiguration{
		{DeviceID: n.myID},
		{DeviceID: n.peerID},
	}
	if err := folder.CreateRoot(); err != nil {
		t.Fatal(err)
	}
	cfg.SetFolder(folder)

	fd, err := os.Create(filepath.Join(n.home, "config.xml"))
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	if err := cfg.WriteXML(fd); err != nil {
		t.Fatal(err)
	}
}

func setPullerDelay(t *testing.T, n *virtualFileNode, delay float64) {
	t.Helper()
	cfg, err := n.proc.GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	folder, idx, ok := cfg.Folder(virtualFilesFolderID)
	if !ok {
		t.Fatalf("folder %q missing from config", virtualFilesFolderID)
	}
	folder.PullerDelayS = delay
	cfg.Folders[idx] = folder
	if err := n.proc.PostConfig(cfg); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, virtualFilesTimeout, "config in sync", func() error {
		insync, err := n.proc.ConfigInSync()
		if err != nil {
			return err
		}
		if !insync {
			return errors.New("config not yet in sync")
		}
		return nil
	})
	n.pullerDelay = delay
}

func awaitSyncWithTimeout(t *testing.T, folder string, timeout time.Duration, nodes ...*rc.Process) {
	t.Helper()
	awaitCondition(t, timeout, "folder sync", func() error {
		if !rc.InSync(folder, nodes...) {
			return errors.New("folder not yet in sync")
		}
		return nil
	})
}

func awaitRemoteInSync(t *testing.T, folder string, a, b *virtualFileNode) {
	t.Helper()
	awaitCondition(t, virtualFilesTimeout, "remote completion", func() error {
		return checkRemoteInSync(folder, a.proc, b.proc)
	})
}

func awaitPeerConnected(t *testing.T, n *virtualFileNode, peerID protocol.DeviceID) {
	t.Helper()
	peerKey := peerID.String()
	deadline := time.Now().Add(virtualFilesTimeout)
	var last map[string]rc.ConnectionStats
	var lastErr error
	for time.Now().Before(deadline) {
		conns, err := n.proc.Connections()
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		last = conns
		stats, ok := conns[peerKey]
		if !ok {
			lastErr = fmt.Errorf("peer %s not connected yet", peerKey)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if !stats.Connected {
			lastErr = fmt.Errorf("peer %s present but not connected", peerKey)
			time.Sleep(250 * time.Millisecond)
			continue
		}
		return
	}
	t.Fatalf("timed out waiting for peer connection: %v\nconnections=%+v\n%s", lastErr, last, readNodeLog(n.home))
}

func awaitGlobalKnown(t *testing.T, n *virtualFileNode, folder, file string) {
	t.Helper()
	path := fmt.Sprintf("/rest/db/file?folder=%s&file=%s", url.QueryEscape(folder), url.QueryEscape(file))
	awaitCondition(t, virtualFilesTimeout, "global file visible", func() error {
		_, err := n.proc.Get(path)
		return err
	})
}

func awaitVirtualFileState(t *testing.T, n *virtualFileNode, file string, state model.PresenceState) {
	t.Helper()
	awaitCondition(t, virtualFilesTimeout, "virtual file state", func() error {
		presence, status, err := n.proc.VirtualFilePresence(virtualFilesFolderID, file)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("unexpected status %d", status)
		}
		if presence.State != state {
			return fmt.Errorf("got state %s", presence.State)
		}
		return nil
	})
}

func awaitVirtualFileIPFSHealth(t *testing.T, n *virtualFileNode, file string, healthy bool) model.FilePresence {
	t.Helper()
	var last model.FilePresence
	awaitCondition(t, virtualFilesTimeout, "virtual file ipfs health", func() error {
		presence, status, err := n.proc.VirtualFilePresence(virtualFilesFolderID, file)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("unexpected status %d", status)
		}
		last = presence
		if presence.IPFSContentAddressableHealth.Healthy != healthy {
			return fmt.Errorf("healthy=%v reason=%q", presence.IPFSContentAddressableHealth.Healthy, presence.IPFSContentAddressableHealth.Reason)
		}
		return nil
	})
	return last
}

func awaitVirtualFileMissing(t *testing.T, n *virtualFileNode, file string) {
	t.Helper()
	awaitCondition(t, virtualFilesTimeout, "virtual file cleared", func() error {
		_, status, err := n.proc.VirtualFilePresence(virtualFilesFolderID, file)
		if err != nil {
			return err
		}
		if status != http.StatusNotFound {
			return fmt.Errorf("expected 404, got %d", status)
		}
		return nil
	})
}

func awaitNeedFiles(t *testing.T, n *virtualFileNode, expected int) {
	t.Helper()
	awaitCondition(t, virtualFilesTimeout, "need accounting", func() error {
		status, err := n.proc.Model(virtualFilesFolderID)
		if err != nil {
			return err
		}
		if status.NeedFiles != expected {
			return fmt.Errorf("need files = %d", status.NeedFiles)
		}
		return nil
	})
}

func awaitDirExists(t *testing.T, path string) {
	t.Helper()
	awaitCondition(t, virtualFilesTimeout, "directory exists", func() error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	})
}

func awaitCondition(t *testing.T, timeout time.Duration, desc string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %v", desc, lastErr)
}

func awaitNodeAPIReady(t *testing.T, home string, p *rc.Process) {
	t.Helper()
	deadline := time.Now().Add(virtualFilesTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-p.Stopped():
			t.Fatalf("syncthing exited before API became ready\n%s", readNodeLog(home))
		default:
		}
		if _, err := p.SystemVersion(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for syncthing API readiness: %v\n%s", lastErr, readNodeLog(home))
}

func readNodeLog(home string) string {
	bs, err := os.ReadFile(filepath.Join(home, "syncthing.log"))
	if err != nil {
		return fmt.Sprintf("failed to read syncthing log: %v", err)
	}
	return string(bs)
}

func describeVirtualFile(n *virtualFileNode, file string) string {
	presence, status, err := n.proc.VirtualFilePresence(virtualFilesFolderID, file)
	path := nodePath(n, file)
	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		return fmt.Sprintf("presenceStatus=%d presence=%+v presenceErr=%v pathExists=true mode=%s", status, presence, err, info.Mode())
	case os.IsNotExist(statErr):
		return fmt.Sprintf("presenceStatus=%d presence=%+v presenceErr=%v pathExists=false", status, presence, err)
	default:
		return fmt.Sprintf("presenceStatus=%d presence=%+v presenceErr=%v statErr=%v", status, presence, err, statErr)
	}
}

func describeFetchFailure(n *virtualFileNode, file string) string {
	path := fmt.Sprintf("/rest/debug/virtual-file/fetch?folder=%s&file=%s", url.QueryEscape(virtualFilesFolderID), url.QueryEscape(file))
	bs, status, err := n.proc.PostWithStatus(path, nil)
	return fmt.Sprintf("fetchStatus=%d fetchErr=%v fetchBody=%q", status, err, string(bs))
}

func fetchVirtualFileUntilReady(t *testing.T, n *virtualFileNode, file string) model.FilePresence {
	t.Helper()
	path := fmt.Sprintf("/rest/debug/virtual-file/fetch?folder=%s&file=%s", url.QueryEscape(virtualFilesFolderID), url.QueryEscape(file))
	deadline := time.Now().Add(virtualFilesTimeout)
	var lastStatus int
	var lastErr error
	var lastBody []byte
	for time.Now().Before(deadline) {
		bs, status, err := n.proc.PostWithStatus(path, nil)
		lastStatus, lastErr, lastBody = status, err, bs
		if err == nil && status == http.StatusOK {
			var presence model.FilePresence
			if err := json.Unmarshal(bs, &presence); err != nil {
				t.Fatalf("failed to decode fetch presence: %v", err)
			}
			return presence
		}
		if err == nil && status == http.StatusConflict && bytes.Contains(bs, []byte("no connected device has the required version")) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		if err == nil && status == http.StatusNotFound && bytes.Contains(bs, []byte("no such file")) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		t.Fatalf("fetch failed: status=%d err=%v body=%q (%s)", status, err, string(bs), describeVirtualFile(n, file))
	}
	t.Fatalf("timed out waiting for fetch readiness: status=%d err=%v body=%q (%s)", lastStatus, lastErr, string(lastBody), describeVirtualFile(n, file))
	return model.FilePresence{}
}

func assertCASObjectExists(t *testing.T, n *virtualFileNode, data []byte) {
	t.Helper()
	sum := sha256.Sum256(data)
	key := fmt.Sprintf("%x", sum)
	path := filepath.Join(n.home, "virtualfiles-cas", "objects", key[:2], key)
	awaitCondition(t, virtualFilesTimeout, "CAS object", func() error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Size() != int64(len(data)) {
			return fmt.Errorf("CAS object size = %d", info.Size())
		}
		return nil
	})
}

func peerOutBytes(t *testing.T, n *virtualFileNode, peerID protocol.DeviceID) int64 {
	t.Helper()
	conns, err := n.proc.Connections()
	if err != nil {
		t.Fatal(err)
	}
	stats, ok := conns[peerID.String()]
	if !ok {
		t.Fatalf("peer %s not present in connection stats", peerID)
	}
	return stats.OutBytesTotal
}

func writeNodeFile(t *testing.T, n *virtualFileNode, name string, data []byte) {
	t.Helper()
	path := nodePath(n, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRescan(t *testing.T, n *virtualFileNode, sub string) {
	t.Helper()
	var err error
	if sub == "" {
		err = n.proc.Rescan(virtualFilesFolderID)
	} else {
		err = n.proc.RescanSub(virtualFilesFolderID, sub, 0)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func nodePath(n *virtualFileNode, rel string) string {
	return filepath.Join(n.folder, filepath.FromSlash(rel))
}

func removePathOrFatal(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, got err=%v", path, err)
	}
}

func assertFileContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected file contents in %s: %q", path, got)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
