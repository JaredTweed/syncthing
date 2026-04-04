// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build integration
// +build integration

package integration

import (
	"net/http"
	"testing"

	"github.com/syncthing/syncthing/lib/model"
	"github.com/syncthing/syncthing/lib/protocol"
)

func TestExperimentalVirtualFilesBinaryIPFSReuseAndRestart(t *testing.T) {
	kubo := newRealKuboHarness(t)

	leftID, _ := protocol.DeviceIDFromString(id1)
	rightID, _ := protocol.DeviceIDFromString(id2)
	baseDir := t.TempDir()

	left := newVirtualFileNode(t, baseDir, "ipfs-left", "h1", leftID, rightID, true, 0)
	right := newVirtualFileNode(t, baseDir, "ipfs-right", "h2", rightID, leftID, true, placeholderPullDelayS)
	enableNodeIPFS(left, kubo.apiURL, true)
	enableNodeIPFS(right, kubo.apiURL, true)
	linkNodes(t, left, right)

	left.start()
	right.start()
	left.resume()
	right.resume()
	awaitPeerConnected(t, left, right.myID)
	awaitPeerConnected(t, right, left.myID)

	file := "ipfs-reuse.txt"
	data := []byte("binary ipfs reuse through real kubo")
	ref := prepareIPFSBackedPlaceholder(t, kubo, left, right, file, data)
	if ref.Backend != "ipfsCAS" {
		t.Fatalf("unexpected ref: %+v", ref)
	}

	left.stop()

	presence := fetchVirtualFileUntilReady(t, right, file)
	if presence.State != model.FullLocal {
		t.Fatalf("expected FullLocal after IPFS fetch, got %+v", presence)
	}
	if presence.ContentAddressableBackend != "ipfsCAS" {
		t.Fatalf("expected ipfsCAS as active backend when healthy, got %+v", presence)
	}
	assertFileContents(t, nodePath(right, file), data)

	right.restart()
	right.resume()
	awaitVirtualFileState(t, right, file, model.FullLocal)
	assertFileContents(t, nodePath(right, file), data)
}

func TestExperimentalVirtualFilesBinaryIPFSFallbacksAndHealth(t *testing.T) {
	kubo := newRealKuboHarness(t)
	kubo.requireManaged(t)

	leftID, _ := protocol.DeviceIDFromString(id1)
	rightID, _ := protocol.DeviceIDFromString(id2)
	baseDir := t.TempDir()

	left := newVirtualFileNode(t, baseDir, "ipfs-fallback-left", "h1", leftID, rightID, true, 0)
	right := newVirtualFileNode(t, baseDir, "ipfs-fallback-right", "h2", rightID, leftID, true, placeholderPullDelayS)
	enableNodeIPFS(left, kubo.apiURL, true)
	enableNodeIPFS(right, kubo.apiURL, true)
	linkNodes(t, left, right)

	left.start()
	right.start()
	left.resume()
	right.resume()
	awaitPeerConnected(t, left, right.myID)
	awaitPeerConnected(t, right, left.myID)

	missingFile := "ipfs-missing.txt"
	missingData := []byte("fallback after missing kubo object")
	missingRef := prepareIPFSBackedPlaceholder(t, kubo, left, right, missingFile, missingData)
	if err := kubo.removeBlock(missingRef.Locator); err != nil {
		t.Fatalf("removing kubo block: %v", err)
	}
	awaitPeerConnected(t, left, right.myID)
	awaitPeerConnected(t, right, left.myID)
	beforeMissing := peerOutBytes(t, left, right.myID)
	presence := fetchVirtualFileUntilReady(t, right, missingFile)
	if presence.State != model.FullLocal {
		t.Fatalf("expected FullLocal after missing-object fallback, got %+v", presence)
	}
	afterMissing := peerOutBytes(t, left, right.myID)
	if afterMissing <= beforeMissing {
		t.Fatalf("expected remote traffic after missing IPFS object, before=%d after=%d", beforeMissing, afterMissing)
	}
	assertFileContents(t, nodePath(right, missingFile), missingData)
	assertCASObjectExists(t, right, missingData)

	mismatchFile := "ipfs-mismatch.txt"
	expectedData := []byte("expected bytes after kubo mismatch")
	wrongData := []byte("wrong kubo bytes")
	writeNodeFile(t, left, mismatchFile, expectedData)
	mustRescan(t, left, mismatchFile)
	awaitGlobalKnown(t, right, virtualFilesFolderID, mismatchFile)
	wrongLocator, err := kubo.addObject(wrongData)
	if err != nil {
		t.Fatal(err)
	}
	right.stop()
	injectIPFSPlaceholderState(t, right, mismatchFile, expectedData, ipfsReferenceForBytes(expectedData, wrongLocator))
	right.start()
	right.resume()
	awaitPeerConnected(t, left, right.myID)
	awaitPeerConnected(t, right, left.myID)
	awaitVirtualFileState(t, right, mismatchFile, model.MetadataOnly)
	awaitVirtualFileIPFSHealth(t, right, mismatchFile, true)
	beforeMismatch := peerOutBytes(t, left, right.myID)
	presence = fetchVirtualFileUntilReady(t, right, mismatchFile)
	if presence.State != model.FullLocal {
		t.Fatalf("expected FullLocal after mismatch fallback, got %+v", presence)
	}
	afterMismatch := peerOutBytes(t, left, right.myID)
	if afterMismatch <= beforeMismatch {
		t.Fatalf("expected remote traffic after mismatched IPFS content, before=%d after=%d", beforeMismatch, afterMismatch)
	}
	assertFileContents(t, nodePath(right, mismatchFile), expectedData)

	downFile := "ipfs-down.txt"
	downData := []byte("remote fallback when kubo disappears")
	writeNodeFile(t, left, downFile, downData)
	mustRescan(t, left, downFile)
	awaitGlobalKnown(t, right, virtualFilesFolderID, downFile)
	if _, status, err := right.proc.SetMetadataOnly(virtualFilesFolderID, downFile); err != nil || status != http.StatusOK {
		t.Fatalf("SetMetadataOnly(down) failed: status=%d err=%v", status, err)
	}
	awaitVirtualFileIPFSHealth(t, right, downFile, true)

	kubo.stop()
	presence = awaitVirtualFileIPFSHealth(t, right, downFile, false)
	if presence.ContentAddressableBackend != "localCAS" {
		t.Fatalf("expected localCAS to remain active when kubo is unavailable, got %+v", presence)
	}

	beforeDown := peerOutBytes(t, left, right.myID)
	presence = fetchVirtualFileUntilReady(t, right, downFile)
	if presence.State != model.FullLocal {
		t.Fatalf("expected FullLocal after kubo-down fallback, got %+v", presence)
	}
	afterDown := peerOutBytes(t, left, right.myID)
	if afterDown <= beforeDown {
		t.Fatalf("expected remote traffic after kubo outage, before=%d after=%d", beforeDown, afterDown)
	}
	assertFileContents(t, nodePath(right, downFile), downData)
	assertCASObjectExists(t, right, downData)
}

func TestExperimentalVirtualFilesBinaryIPFSNoOverwriteLocalWriter(t *testing.T) {
	kubo := newRealKuboHarness(t)

	leftID, _ := protocol.DeviceIDFromString(id1)
	rightID, _ := protocol.DeviceIDFromString(id2)
	baseDir := t.TempDir()

	left := newVirtualFileNode(t, baseDir, "ipfs-overwrite-left", "h1", leftID, rightID, true, 0)
	right := newVirtualFileNode(t, baseDir, "ipfs-overwrite-right", "h2", rightID, leftID, true, placeholderPullDelayS)
	enableNodeIPFS(left, kubo.apiURL, true)
	enableNodeIPFS(right, kubo.apiURL, true)
	linkNodes(t, left, right)

	left.start()
	right.start()
	left.resume()
	right.resume()
	awaitPeerConnected(t, left, right.myID)
	awaitPeerConnected(t, right, left.myID)

	file := "ipfs-local-writer.txt"
	data := []byte("real kubo should never overwrite a local writer")
	prepareIPFSBackedPlaceholder(t, kubo, left, right, file, data)

	left.stop()
	writeNodeFile(t, right, file, []byte("local-writer-wins"))

	_, status, err := right.proc.FetchVirtualFile(virtualFilesFolderID, file)
	if err != nil || status != http.StatusConflict {
		t.Fatalf("expected conflict for preexisting local file, got status=%d err=%v", status, err)
	}
	assertFileContents(t, nodePath(right, file), []byte("local-writer-wins"))
}

func TestExperimentalVirtualFilesBinaryIPFSMixedPeerSafety(t *testing.T) {
	kubo := newRealKuboHarness(t)

	normID, _ := protocol.DeviceIDFromString(id1)
	expID, _ := protocol.DeviceIDFromString(id2)
	baseDir := t.TempDir()

	norm := newVirtualFileNode(t, baseDir, "ipfs-mixed-normal", "h1", normID, expID, false, 0)
	exp := newVirtualFileNode(t, baseDir, "ipfs-mixed-exp", "h2", expID, normID, true, placeholderPullDelayS)
	enableNodeIPFS(exp, kubo.apiURL, true)
	linkNodes(t, norm, exp)

	norm.start()
	exp.start()
	norm.resume()
	exp.resume()
	awaitPeerConnected(t, norm, exp.myID)
	awaitPeerConnected(t, exp, norm.myID)

	setPullerDelay(t, exp, 0)
	ordinaryBefore := "mixed-ordinary-before.txt"
	ordinaryBeforeData := []byte("ordinary sync before ipfs operations")
	writeNodeFile(t, norm, ordinaryBefore, ordinaryBeforeData)
	mustRescan(t, norm, ordinaryBefore)
	awaitSyncWithTimeout(t, virtualFilesFolderID, virtualFilesTimeout, norm.proc, exp.proc)
	assertFileContents(t, nodePath(exp, ordinaryBefore), ordinaryBeforeData)
	setPullerDelay(t, exp, placeholderPullDelayS)

	file := "mixed-ipfs.txt"
	data := []byte("ipfs-backed placeholder on experimental peer only")
	prepareIPFSBackedPlaceholder(t, kubo, norm, exp, file, data)

	norm.stop()

	presence := fetchVirtualFileUntilReady(t, exp, file)
	if presence.State != model.FullLocal {
		t.Fatalf("expected FullLocal after IPFS fetch, got %+v", presence)
	}
	assertFileContents(t, nodePath(exp, file), data)

	norm.start()
	norm.resume()
	exp.resume()
	awaitPeerConnected(t, norm, exp.myID)
	awaitPeerConnected(t, exp, norm.myID)

	setPullerDelay(t, exp, 0)
	ordinaryAfter := "mixed-ordinary-after.txt"
	ordinaryAfterData := []byte("ordinary sync still works after experimental ipfs fetch")
	writeNodeFile(t, norm, ordinaryAfter, ordinaryAfterData)
	mustRescan(t, norm, ordinaryAfter)
	awaitSyncWithTimeout(t, virtualFilesFolderID, virtualFilesTimeout, norm.proc, exp.proc)
	assertFileContents(t, nodePath(exp, ordinaryAfter), ordinaryAfterData)
	assertFileContents(t, nodePath(norm, file), data)

	if _, status, err := norm.proc.SetMetadataOnly(virtualFilesFolderID, ordinaryAfter); err != nil || status != http.StatusConflict {
		t.Fatalf("expected normal peer metadata-only API to remain disabled with 409, got status=%d err=%v", status, err)
	}
}

func prepareIPFSBackedPlaceholder(t *testing.T, kubo *realKuboHarness, sender, receiver *virtualFileNode, file string, data []byte) model.ContentAddressableReference {
	t.Helper()

	writeNodeFile(t, sender, file, data)
	mustRescan(t, sender, file)
	awaitGlobalKnown(t, receiver, virtualFilesFolderID, file)

	locator, err := kubo.addObject(data)
	if err != nil {
		t.Fatalf("adding kubo object for %s: %v", file, err)
	}
	ref := ipfsReferenceForBytes(data, locator)

	receiver.stop()
	injectIPFSPlaceholderState(t, receiver, file, data, ref)
	receiver.start()
	receiver.resume()
	if sender.proc != nil {
		awaitPeerConnected(t, sender, receiver.myID)
		awaitPeerConnected(t, receiver, sender.myID)
	}
	awaitVirtualFileState(t, receiver, file, model.MetadataOnly)
	awaitVirtualFileIPFSHealth(t, receiver, file, true)
	assertPathAbsent(t, nodePath(receiver, file))
	return ref
}
