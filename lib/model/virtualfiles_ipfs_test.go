// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/scanner"
)

type fakeIPFSServer struct {
	t *testing.T

	server *httptest.Server

	versionStatus int
	addStatus     int
	catStatus     map[string]int
	catBodies     map[string][]byte
	objects       map[string][]byte
}

func newFakeIPFSServer(t *testing.T) *fakeIPFSServer {
	t.Helper()

	srv := &fakeIPFSServer{
		t:             t,
		versionStatus: http.StatusOK,
		addStatus:     http.StatusOK,
		catStatus:     make(map[string]int),
		catBodies:     make(map[string][]byte),
		objects:       make(map[string][]byte),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/version", srv.handleVersion)
	mux.HandleFunc("/api/v0/add", srv.handleAdd)
	mux.HandleFunc("/api/v0/cat", srv.handleCat)
	srv.server = httptest.NewServer(mux)
	t.Cleanup(srv.server.Close)
	return srv
}

func (s *fakeIPFSServer) handleVersion(w http.ResponseWriter, _ *http.Request) {
	if s.versionStatus != http.StatusOK {
		http.Error(w, "version unavailable", s.versionStatus)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"Version": "0.27.0"})
}

func (s *fakeIPFSServer) handleAdd(w http.ResponseWriter, r *http.Request) {
	if s.addStatus != http.StatusOK {
		http.Error(w, "add unavailable", s.addStatus)
		return
	}

	reader, err := r.MultipartReader()
	if err != nil {
		s.t.Fatalf("creating multipart reader: %v", err)
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.t.Fatalf("reading multipart: %v", err)
		}
		if part.FormName() != "file" {
			continue
		}
		data, err := io.ReadAll(part)
		if err != nil {
			s.t.Fatalf("reading multipart file: %v", err)
		}
		sum := sha256.Sum256(data)
		cid := "cid-" + hex.EncodeToString(sum[:8])
		s.objects[cid] = data
		_ = json.NewEncoder(w).Encode(map[string]string{"Hash": cid})
		return
	}

	http.Error(w, "missing file", http.StatusBadRequest)
}

func (s *fakeIPFSServer) handleCat(w http.ResponseWriter, r *http.Request) {
	arg := r.URL.Query().Get("arg")
	if status, ok := s.catStatus[arg]; ok {
		http.Error(w, "cat unavailable", status)
		return
	}
	if body, ok := s.catBodies[arg]; ok {
		_, _ = w.Write(body)
		return
	}
	body, ok := s.objects[arg]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, _ = w.Write(body)
}

func enableExperimentalIPFS(t *testing.T, wrapper config.Wrapper, apiURL string, prefer bool) {
	t.Helper()
	waiter, err := wrapper.Modify(func(cfg *config.Configuration) {
		cfg.Options.ExperimentalVirtualFiles = true
		cfg.Options.ExperimentalVirtualFilesIPFSEnabled = true
		cfg.Options.ExperimentalVirtualFilesIPFSAPIURL = apiURL
		cfg.Options.ExperimentalVirtualFilesIPFSTimeoutS = 1
		cfg.Options.ExperimentalVirtualFilesIPFSHealthIntervalS = 1
		cfg.Options.ExperimentalVirtualFilesIPFSPrefer = prefer
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.Wait()
}

func TestIPFSContentAddressableBackendPutGet(t *testing.T) {
	srv := newFakeIPFSServer(t)
	wrapper, _ := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, srv.server.URL, true)

	m := setupModel(t, wrapper)
	defer cleanupModel(m)

	ipfs := currentIPFSCASBackend(t, m)
	data := []byte("ipfs cas object payload")
	ref := contentAddressableReferenceForBytes(data)

	storedRef, err := ipfs.Put(context.Background(), ref, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if storedRef.Backend != "ipfsCAS" || storedRef.Locator == "" {
		t.Fatalf("unexpected stored IPFS ref: %+v", storedRef)
	}
	if !ipfs.Health(context.Background()).Healthy {
		t.Fatal("expected IPFS backend to report healthy")
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
}

func TestIPFSContentAddressableBackendDigestMismatchFails(t *testing.T) {
	srv := newFakeIPFSServer(t)
	wrapper, _ := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, srv.server.URL, true)

	m := setupModel(t, wrapper)
	defer cleanupModel(m)

	ipfs := currentIPFSCASBackend(t, m)
	ref := contentAddressableReferenceForBytes([]byte("expected"))
	if _, err := ipfs.Put(context.Background(), ref, bytes.NewReader([]byte("different"))); err == nil || err != ErrContentAddressableDigestMismatch {
		t.Fatalf("expected digest mismatch, got %v", err)
	}
}

func TestSetMetadataOnlyReferencesExistingIPFSContentWhenLocalCASMissing(t *testing.T) {
	srv := newFakeIPFSServer(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, srv.server.URL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("metadata only from ipfs")
	fc.addFile("ipfsref.txt", 0o644, protocol.FileInfoTypeFile, data)
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

	record, ok, err := m.virtualFileRecord(fcfg.ID, file.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || record.ContentRef == nil {
		t.Fatal("expected metadata-only record to retain IPFS reference")
	}
	if record.ContentRef.Backend != "ipfsCAS" {
		t.Fatalf("expected IPFS content ref, got %+v", record.ContentRef)
	}
}

func TestFetchVirtualFileUsesExistingIPFSContentWithoutRedownload(t *testing.T) {
	srv := newFakeIPFSServer(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, srv.server.URL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("fetch directly from ipfs")
	fc.addFile("ipfscached.txt", 0o644, protocol.FileInfoTypeFile, data)
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
		t.Fatal("IPFS-backed fetch should not request remote blocks")
		return nil, nil
	})

	presence, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name)
	if err != nil {
		t.Fatal(err)
	}
	if presence.State != FullLocal {
		t.Fatalf("expected FullLocal after IPFS fetch, got %+v", presence)
	}
	if fc.RequestCallCount() != 0 {
		t.Fatalf("expected no remote block requests, got %d", fc.RequestCallCount())
	}
}

func TestFetchVirtualFileFallsBackWhenIPFSObjectMissing(t *testing.T) {
	srv := newFakeIPFSServer(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, srv.server.URL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("remote after missing ipfs")
	fc.addFile("missingipfs.txt", 0o644, protocol.FileInfoTypeFile, data)
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
	delete(srv.objects, storedRef.Locator)

	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	beforeRequests := fc.RequestCallCount()
	if _, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}
	if fc.RequestCallCount() <= beforeRequests {
		t.Fatal("expected remote fallback after missing IPFS object")
	}
	if gotRef, ok, err := currentLocalCASBackend(t, m).ReferenceForFile(fcfg, file); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("expected local CAS association after remote fallback")
	} else if gotRef.Backend != "localCAS" {
		t.Fatalf("unexpected local CAS ref after fallback: %+v", gotRef)
	}
}

func TestFetchVirtualFileFallsBackWhenIPFSObjectCorrupt(t *testing.T) {
	srv := newFakeIPFSServer(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, srv.server.URL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("remote after corrupt ipfs")
	fc.addFile("corruptipfs.txt", 0o644, protocol.FileInfoTypeFile, data)
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
	srv.catBodies[storedRef.Locator] = []byte("corrupt")

	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	beforeRequests := fc.RequestCallCount()
	if _, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}
	if fc.RequestCallCount() <= beforeRequests {
		t.Fatal("expected remote fallback after corrupt IPFS object")
	}
}

func TestStoreVirtualFileContentInIPFSMirrorFailureKeepsLocalCAS(t *testing.T) {
	srv := newFakeIPFSServer(t)
	srv.addStatus = http.StatusInternalServerError

	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, srv.server.URL, true)

	m, fc := setupModelWithConnectionFromWrapper(t, wrapper)
	defer cleanupModel(m)

	data := []byte("local cas survives ipfs mirror failure")
	fc.addFile("mirrorfail.txt", 0o644, protocol.FileInfoTypeFile, data)
	file := fc.files[0]
	if err := m.sdb.Update(fcfg.ID, device1, []protocol.FileInfo{prepareFileInfoForIndex(file)}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.SetMetadataOnly(fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := m.FetchVirtualFile(context.Background(), fcfg.ID, file.Name); err != nil {
		t.Fatal(err)
	}

	if ref, ok, err := currentLocalCASBackend(t, m).ReferenceForFile(fcfg, file); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("expected local CAS ref after mirror failure")
	} else if ref.Backend != "localCAS" {
		t.Fatalf("unexpected local CAS ref %+v", ref)
	}
	if ref, ok, err := currentIPFSCASBackend(t, m).ReferenceForFile(fcfg, file); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatalf("expected no stored IPFS ref after mirror failure, got %+v", ref)
	}
}

func TestVirtualFilePresenceReportsIPFSHealth(t *testing.T) {
	wrapper, _ := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, "http://127.0.0.1:1", false)

	m := setupModel(t, wrapper)
	defer cleanupModel(m)

	presence := newFilePresence("default", "foo", MetadataOnly, m.currentContentBackend())
	if presence.ContentAddressableBackend != "localCAS" {
		t.Fatalf("expected localCAS to remain the active backend, got %+v", presence)
	}
	if presence.ContentAddressableHealth.Backend != "localCAS" || !presence.ContentAddressableHealth.Healthy {
		t.Fatalf("unexpected active CAS health: %+v", presence.ContentAddressableHealth)
	}
	if presence.IPFSContentAddressableHealth.Backend != "ipfsCAS" || presence.IPFSContentAddressableHealth.Healthy || !presence.IPFSContentAddressableHealth.Configured {
		t.Fatalf("unexpected IPFS health payload: %+v", presence.IPFSContentAddressableHealth)
	}
}

func TestIPFSContentAddressableStatePersistsAcrossRestart(t *testing.T) {
	srv := newFakeIPFSServer(t)
	wrapper, fcfg := newDefaultBasicCfgWrapper(t)
	enableExperimentalIPFS(t, wrapper, srv.server.URL, true)

	dbDir := t.TempDir()
	m := newModelWithDBDir(t, wrapper, myID, nil, dbDir)
	m.ServeBackground()
	if err := m.ScanFolder(fcfg.ID); err != nil {
		t.Fatal(err)
	}

	data := []byte("restart from ipfs cas")
	blockSize := protocol.BlockSize(int64(len(data)))
	blocks, err := scanner.Blocks(context.Background(), bytes.NewReader(data), blockSize, int64(len(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	file := protocol.FileInfo{
		Name:         "ipfscasrestart.txt",
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

	m = newModelWithDBDir(t, wrapper, myID, nil, dbDir)
	m.ServeBackground()
	defer cleanupModel(m)

	presence, err := m.VirtualFilePresence(fcfg.ID, file.Name)
	if err != nil {
		t.Fatal(err)
	}
	if presence.State != MetadataOnly {
		t.Fatalf("expected metadata-only presence after restart, got %+v", presence)
	}
	if ref, ok, err := currentIPFSCASBackend(t, m).ReferenceForFile(fcfg, file); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("expected IPFS ref after restart")
	} else if ref.Backend != "ipfsCAS" {
		t.Fatalf("unexpected IPFS ref after restart: %+v", ref)
	}
}

func TestIPFSBackendMultipartContract(t *testing.T) {
	srv := newFakeIPFSServer(t)
	reqBody := &bytes.Buffer{}
	writer := multipart.NewWriter(reqBody)
	part, err := writer.CreateFormFile("file", "payload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, srv.server.URL+"/api/v0/add", reqBody)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rr := httptest.NewRecorder()
	srv.handleAdd(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rr.Code)
	}
}
