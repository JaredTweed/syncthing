// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
)

// PresenceState describes how much of a file is available locally.
// Experimental: This is Phase 1 scaffolding for experimentalVirtualFiles.
type PresenceState string

const (
	FullLocal    PresenceState = "FullLocal"
	MetadataOnly PresenceState = "MetadataOnly"
	PartialLocal PresenceState = "PartialLocal"
)

const virtualFilesPresenceKVPrefix = "virtualfiles/presence"

var (
	ErrExperimentalVirtualFilesDisabled = errors.New("experimentalVirtualFiles is disabled")
	ErrVirtualFileAlreadyLocal          = errors.New("file already exists locally")
	ErrVirtualFileNotSupported          = errors.New("metadata-only placeholders are only supported for regular files")
)

// FilePresence is returned by internal debug plumbing for virtual-file
// experiments. Phase 2 supports MetadataOnly records in the local metadata
// plane while keeping the OS filesystem untouched.
type FilePresence struct {
	Folder  string        `json:"folder"`
	File    string        `json:"file"`
	State   PresenceState `json:"state"`
	Backend string        `json:"backend"`
}

// ContentReadRequest is reserved for future backend-routed reads. Phase 1
// keeps request serving unchanged and direct filesystem backed.
type ContentReadRequest struct {
	Name          string
	Offset        int64
	Size          int
	FromTemporary bool
}

// ContentBackend abstracts future file-content storage from the folder model.
// Experimental: Phase 1 only wires the seam and a default local-filesystem
// implementation. Request serving and pulling still use the existing paths.
//
// TODO(experimentalVirtualFiles): Add metadata-plane lookups for files that do
// not have local content.
// TODO(experimentalVirtualFiles): Route explicit fetch and materialization
// through this interface in later phases.
type ContentBackend interface {
	BackendType() string
	PresenceForFile(folder config.FolderConfiguration, file protocol.FileInfo) PresenceState
	ReadAt(ctx context.Context, folder config.FolderConfiguration, req ContentReadRequest, buf []byte) (int, error)
}

type localFilesystemContentBackend struct{}

type virtualFileRecord struct {
	State   PresenceState `json:"state"`
	Updated time.Time     `json:"updated"`
}

func newContentBackend() ContentBackend {
	return localFilesystemContentBackend{}
}

func (localFilesystemContentBackend) BackendType() string {
	return "localFilesystem"
}

func (localFilesystemContentBackend) PresenceForFile(_ config.FolderConfiguration, _ protocol.FileInfo) PresenceState {
	return FullLocal
}

func (localFilesystemContentBackend) ReadAt(_ context.Context, folder config.FolderConfiguration, req ContentReadRequest, buf []byte) (int, error) {
	name := req.Name
	if req.FromTemporary {
		name = fs.TempName(name)
	}
	return readOffsetIntoBuf(folder.Filesystem(), name, req.Offset, buf)
}

func (m *model) experimentalVirtualFilesEnabled() bool {
	return m.cfg.Options().ExperimentalVirtualFiles
}

func normalizeVirtualFilePath(file string) (string, error) {
	return fs.Canonicalize(file)
}

func virtualFileRecordPrefix(folder string) string {
	return virtualFilesPresenceKVPrefix + "/" + folder
}

func (m *model) virtualFileRecordStore(folder string) *db.Typed {
	return db.NewTyped(m.sdb, virtualFileRecordPrefix(folder))
}

func (m *model) virtualFileRecord(folder, file string) (virtualFileRecord, bool, error) {
	bs, ok, err := m.virtualFileRecordStore(folder).Bytes(file)
	if err != nil || !ok {
		return virtualFileRecord{}, ok, err
	}

	var record virtualFileRecord
	if err := json.Unmarshal(bs, &record); err != nil {
		return virtualFileRecord{}, false, err
	}
	return record, true, nil
}

func (m *model) metadataOnlyNames(folder string) (map[string]struct{}, error) {
	names := make(map[string]struct{})
	prefix := virtualFileRecordPrefix(folder) + "/"

	it, errFn := m.sdb.PrefixKV(prefix)
	for kv := range it {
		file := strings.TrimPrefix(kv.Key, prefix)
		if file == "" {
			continue
		}

		var record virtualFileRecord
		if err := json.Unmarshal(kv.Value, &record); err != nil {
			continue
		}
		if record.State == MetadataOnly {
			names[file] = struct{}{}
		}
	}
	return names, errFn()
}

func countForFileInfo(file protocol.FileInfo) db.Counts {
	var counts db.Counts
	switch {
	case file.IsDeleted():
		counts.Deleted = 1
	case file.IsDirectory():
		counts.Directories = 1
	case file.IsSymlink():
		counts.Symlinks = 1
	default:
		counts.Files = 1
		counts.Bytes = file.Size
	}
	return counts
}

func subtractCounts(total, subtract db.Counts) db.Counts {
	total.Files -= subtract.Files
	total.Directories -= subtract.Directories
	total.Symlinks -= subtract.Symlinks
	total.Deleted -= subtract.Deleted
	total.Bytes -= subtract.Bytes

	if total.Files < 0 {
		total.Files = 0
	}
	if total.Directories < 0 {
		total.Directories = 0
	}
	if total.Symlinks < 0 {
		total.Symlinks = 0
	}
	if total.Deleted < 0 {
		total.Deleted = 0
	}
	if total.Bytes < 0 {
		total.Bytes = 0
	}
	return total
}

func (m *model) metadataOnlyNeedCounts(folder string) (db.Counts, error) {
	names, err := m.metadataOnlyNames(folder)
	if err != nil {
		return db.Counts{}, err
	}

	var counts db.Counts
	for name := range names {
		if fi, ok, err := m.sdb.GetDeviceFile(folder, protocol.LocalDeviceID, name); err != nil {
			return db.Counts{}, err
		} else if ok && !fi.IsDeleted() {
			continue
		}

		fi, ok, err := m.sdb.GetGlobalFile(folder, name)
		if err != nil {
			return db.Counts{}, err
		}
		if !ok || fi.IsDeleted() {
			continue
		}

		counts = counts.Add(countForFileInfo(fi))
	}
	return counts, nil
}

func (m *model) effectiveNeedSize(folder string, device protocol.DeviceID) (db.Counts, error) {
	need, err := m.sdb.CountNeed(folder, device)
	if err != nil {
		return db.Counts{}, err
	}
	if device != protocol.LocalDeviceID || !m.experimentalVirtualFilesEnabled() {
		return need, nil
	}

	metadataOnly, err := m.metadataOnlyNeedCounts(folder)
	if err != nil {
		return db.Counts{}, err
	}
	return subtractCounts(need, metadataOnly), nil
}

func (m *model) isMetadataOnly(folder, file string) (bool, error) {
	if !m.experimentalVirtualFilesEnabled() {
		return false, nil
	}

	record, ok, err := m.virtualFileRecord(folder, file)
	if err != nil || !ok {
		return false, err
	}
	return record.State == MetadataOnly, nil
}

func (m *model) clearVirtualFilePresence(folder, file string) error {
	if !m.experimentalVirtualFilesEnabled() {
		return nil
	}
	return m.virtualFileRecordStore(folder).Delete(file)
}

func (m *model) clearVirtualFilePresenceBatch(folder string, files []string) error {
	for _, file := range files {
		if err := m.clearVirtualFilePresence(folder, file); err != nil {
			return err
		}
	}
	return nil
}

func (m *model) SetMetadataOnly(folder, file string) (FilePresence, error) {
	if !m.experimentalVirtualFilesEnabled() {
		return FilePresence{}, ErrExperimentalVirtualFilesDisabled
	}

	name, err := normalizeVirtualFilePath(file)
	if err != nil {
		return FilePresence{}, protocol.ErrInvalid
	}

	if _, ok := m.cfg.Folder(folder); !ok {
		return FilePresence{}, ErrFolderMissing
	}

	if local, ok, err := m.sdb.GetDeviceFile(folder, protocol.LocalDeviceID, name); err != nil {
		return FilePresence{}, err
	} else if ok && !local.IsDeleted() {
		return FilePresence{}, ErrVirtualFileAlreadyLocal
	}

	global, ok, err := m.sdb.GetGlobalFile(folder, name)
	if err != nil {
		return FilePresence{}, err
	}
	if !ok {
		return FilePresence{}, protocol.ErrNoSuchFile
	}
	if global.IsDeleted() || global.IsDirectory() || global.IsSymlink() {
		return FilePresence{}, ErrVirtualFileNotSupported
	}

	record := virtualFileRecord{
		State:   MetadataOnly,
		Updated: time.Now().UTC(),
	}
	bs, err := json.Marshal(record)
	if err != nil {
		return FilePresence{}, err
	}
	if err := m.virtualFileRecordStore(folder).PutBytes(name, bs); err != nil {
		return FilePresence{}, err
	}

	return m.VirtualFilePresence(folder, name)
}

func (m *model) VirtualFilePresence(folder, file string) (FilePresence, error) {
	name, err := normalizeVirtualFilePath(file)
	if err != nil {
		return FilePresence{}, protocol.ErrInvalid
	}

	cfg, ok := m.cfg.Folder(folder)
	if !ok {
		return FilePresence{}, ErrFolderMissing
	}

	m.mut.RLock()
	backend := m.contentBackend
	m.mut.RUnlock()
	if backend == nil {
		backend = newContentBackend()
	}

	fi, ok, err := m.sdb.GetDeviceFile(folder, protocol.LocalDeviceID, name)
	if err != nil {
		return FilePresence{}, err
	}
	if ok && !fi.IsDeleted() {
		return FilePresence{
			Folder:  folder,
			File:    name,
			State:   backend.PresenceForFile(cfg, fi),
			Backend: backend.BackendType(),
		}, nil
	}

	if metadataOnly, err := m.isMetadataOnly(folder, name); err != nil {
		return FilePresence{}, err
	} else if metadataOnly {
		return FilePresence{
			Folder:  folder,
			File:    name,
			State:   MetadataOnly,
			Backend: backend.BackendType(),
		}, nil
	}

	return FilePresence{}, protocol.ErrNoSuchFile
}
