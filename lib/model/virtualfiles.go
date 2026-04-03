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
	ErrVirtualFileIgnored               = errors.New("file is ignored locally")
	ErrVirtualFileNotSupported          = errors.New("metadata-only placeholders are only supported for regular files")
)

// FilePresence is returned by internal debug plumbing for virtual-file
// experiments. Phase 2 supports MetadataOnly records in the local metadata
// plane while keeping the OS filesystem untouched.
type FilePresence struct {
	Folder                    string        `json:"folder"`
	File                      string        `json:"file"`
	State                     PresenceState `json:"state"`
	Backend                   string        `json:"backend"`
	ContentAddressableBackend string        `json:"contentAddressableBackend,omitempty"`
	ContentAddressableEnabled bool          `json:"contentAddressableEnabled"`
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
// Experimental: The default implementation keeps request serving and the
// normal puller on the existing paths. Phase 3 starts routing only explicit
// virtual-file materialization through this seam.
//
// TODO(experimentalVirtualFiles): Add metadata-plane lookups for files that do
// not have local content.
// TODO(experimentalVirtualFiles): Route more of the content lifecycle through
// this interface in later phases.
type ContentBackend interface {
	BackendType() string
	PresenceForFile(folder config.FolderConfiguration, file protocol.FileInfo) PresenceState
	ReadAt(ctx context.Context, folder config.FolderConfiguration, req ContentReadRequest, buf []byte) (int, error)
	Materialize(ctx context.Context, m *model, folder config.FolderConfiguration, file protocol.FileInfo) error
	ContentAddressableBackend() ContentAddressableBackend
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

func (localFilesystemContentBackend) Materialize(ctx context.Context, m *model, folder config.FolderConfiguration, file protocol.FileInfo) error {
	return m.materializeLocalFile(ctx, folder, file)
}

func (localFilesystemContentBackend) ContentAddressableBackend() ContentAddressableBackend {
	return noopContentAddressableBackend{}
}

func (m *model) experimentalVirtualFilesEnabled() bool {
	return m.cfg.Options().ExperimentalVirtualFiles
}

func (m *model) currentContentBackend() ContentBackend {
	m.mut.RLock()
	backend := m.contentBackend
	m.mut.RUnlock()
	if backend == nil {
		backend = newContentBackend()
	}
	return backend
}

func newFilePresence(folder, file string, state PresenceState, backend ContentBackend) FilePresence {
	if backend == nil {
		backend = newContentBackend()
	}

	casBackend := backend.ContentAddressableBackend()
	presence := FilePresence{
		Folder:  folder,
		File:    file,
		State:   state,
		Backend: backend.BackendType(),
	}
	if casBackend != nil {
		presence.ContentAddressableBackend = casBackend.BackendType()
		presence.ContentAddressableEnabled = casBackend.SupportsContentAddressing()
	}
	return presence
}

func normalizeVirtualFilePath(file string) (string, error) {
	name, err := fs.Canonicalize(file)
	if err != nil {
		return "", err
	}
	if name == "." || name == "" {
		return "", protocol.ErrInvalid
	}
	return name, nil
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

func (m *model) virtualFileRecordNames(folder string) ([]string, error) {
	names := make([]string, 0)
	prefix := virtualFileRecordPrefix(folder) + "/"

	it, errFn := m.sdb.PrefixKV(prefix)
	for kv := range it {
		file := strings.TrimPrefix(kv.Key, prefix)
		if file == "" {
			continue
		}
		names = append(names, file)
	}
	return names, errFn()
}

func (m *model) virtualFileIgnored(folder, file string) bool {
	m.mut.RLock()
	ignores := m.folderIgnores[folder]
	m.mut.RUnlock()
	return ignores != nil && ignores.Match(file).IsIgnored()
}

func virtualFileExistsLocally(filesystem fs.Filesystem, file string) (bool, error) {
	_, err := filesystem.Lstat(file)
	switch {
	case err == nil, fs.IsErrCaseConflict(err):
		return true, nil
	case fs.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

func virtualFileMutexKey(folder, file string) string {
	return folder + "\x00" + file
}

func (m *model) metadataOnlyNames(folder string) (map[string]struct{}, error) {
	names := make(map[string]struct{})
	candidates, err := m.virtualFileRecordNames(folder)
	if err != nil {
		return nil, err
	}
	for _, file := range candidates {
		metadataOnly, err := m.isMetadataOnly(folder, file)
		if err != nil {
			return nil, err
		}
		if metadataOnly {
			names[file] = struct{}{}
		}
	}
	return names, nil
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
	if err != nil {
		_, clearErr := m.clearVirtualFilePresence(folder, file)
		return false, clearErr
	}
	if !ok {
		return false, nil
	}
	if record.State != MetadataOnly {
		return false, nil
	}
	if m.virtualFileIgnored(folder, file) {
		_, err := m.clearVirtualFilePresence(folder, file)
		return false, err
	}
	if local, ok, err := m.sdb.GetDeviceFile(folder, protocol.LocalDeviceID, file); err != nil {
		return false, err
	} else if ok && !local.IsDeleted() {
		_, err := m.clearVirtualFilePresence(folder, file)
		return false, err
	}
	if global, ok, err := m.sdb.GetGlobalFile(folder, file); err != nil {
		return false, err
	} else if !ok || global.IsDeleted() || global.IsDirectory() || global.IsSymlink() {
		_, err := m.clearVirtualFilePresence(folder, file)
		return false, err
	}
	return record.State == MetadataOnly, nil
}

func (m *model) clearVirtualFilePresence(folder, file string) (bool, error) {
	if !m.experimentalVirtualFilesEnabled() {
		return false, nil
	}

	store := m.virtualFileRecordStore(folder)
	if bs, ok, err := store.Bytes(file); err != nil {
		return false, err
	} else if !ok {
		return false, nil
	} else {
		var record virtualFileRecord
		if err := json.Unmarshal(bs, &record); err != nil {
			return true, store.Delete(file)
		}
		return true, store.Delete(file)
	}
}

func (m *model) clearVirtualFilePresenceBatch(folder string, files []string) ([]string, error) {
	cleared := make([]string, 0, len(files))
	for _, file := range files {
		removed, err := m.clearVirtualFilePresence(folder, file)
		if err != nil {
			return nil, err
		}
		if removed {
			cleared = append(cleared, file)
		}
	}
	return cleared, nil
}

func (m *model) SetMetadataOnly(folder, file string) (FilePresence, error) {
	if !m.experimentalVirtualFilesEnabled() {
		return FilePresence{}, ErrExperimentalVirtualFilesDisabled
	}

	name, err := normalizeVirtualFilePath(file)
	if err != nil {
		return FilePresence{}, protocol.ErrInvalid
	}

	cfg, ok := m.cfg.Folder(folder)
	if !ok {
		return FilePresence{}, ErrFolderMissing
	}

	lock := m.virtualFileMuts.Get(virtualFileMutexKey(folder, name))
	lock.Lock()
	defer lock.Unlock()

	if m.virtualFileIgnored(folder, name) {
		return FilePresence{}, ErrVirtualFileIgnored
	}

	if local, ok, err := m.sdb.GetDeviceFile(folder, protocol.LocalDeviceID, name); err != nil {
		return FilePresence{}, err
	} else if ok && !local.IsDeleted() {
		return FilePresence{}, ErrVirtualFileAlreadyLocal
	}
	if exists, err := virtualFileExistsLocally(cfg.Filesystem(), name); err != nil {
		return FilePresence{}, err
	} else if exists {
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

	presence, err := m.VirtualFilePresence(folder, name)
	if err == nil {
		m.notifyVirtualFileHooks(m.newVirtualFileEvent(VirtualFileEventMetadataOnlySet, presence, nil))
	}
	return presence, err
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

	backend := m.currentContentBackend()

	fi, ok, err := m.sdb.GetDeviceFile(folder, protocol.LocalDeviceID, name)
	if err != nil {
		return FilePresence{}, err
	}
	if ok && !fi.IsDeleted() {
		return newFilePresence(folder, name, backend.PresenceForFile(cfg, fi), backend), nil
	}

	if metadataOnly, err := m.isMetadataOnly(folder, name); err != nil {
		return FilePresence{}, err
	} else if metadataOnly {
		return newFilePresence(folder, name, MetadataOnly, backend), nil
	}

	return FilePresence{}, protocol.ErrNoSuchFile
}

func (m *model) pruneVirtualFilePresenceAfterRemoteUpdate(folder string, files []protocol.FileInfo, update bool) error {
	if update {
		for _, file := range files {
			if _, err := m.isMetadataOnly(folder, file.Name); err != nil {
				return err
			}
		}
		return nil
	}

	names, err := m.virtualFileRecordNames(folder)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := m.isMetadataOnly(folder, name); err != nil {
			return err
		}
	}
	return nil
}
