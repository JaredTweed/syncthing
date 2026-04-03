// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package osutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/fs"
)

func TestRenameOrCopyNoReplaceExternalWriterWinsAtPublishPoint(t *testing.T) {
	filesystem := fs.NewFilesystem(fs.FilesystemTypeBasic, t.TempDir())

	fd, err := filesystem.Create("from")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.Write([]byte("remote")); err != nil {
		t.Fatal(err)
	}
	_ = fd.Close()

	beforeNoReplacePublishHook = func() {
		beforeNoReplacePublishHook = nil
		destPath := filepath.Join(filesystem.URI(), "to")
		if err := os.WriteFile(destPath, []byte("local"), 0o644); err != nil {
			t.Fatalf("creating local destination: %v", err)
		}
	}
	defer func() {
		beforeNoReplacePublishHook = nil
	}()

	err = RenameOrCopyNoReplace(fs.CopyRangeMethodStandard, filesystem, filesystem, "from", "to")
	if !fs.IsExist(err) {
		t.Fatalf("expected ErrExist, got %v", err)
	}

	buf, err := os.ReadFile(filepath.Join(filesystem.URI(), "to"))
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "local" {
		t.Fatalf("local writer content was overwritten: %q", buf)
	}

	buf, err = os.ReadFile(filepath.Join(filesystem.URI(), "from"))
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != "remote" {
		t.Fatalf("staged source content changed unexpectedly: %q", buf)
	}
}
