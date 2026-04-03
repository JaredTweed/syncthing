// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

var (
	ErrVirtualFileNotMetadataOnly    = errors.New("file is not metadata-only")
	ErrVirtualFileFetchUnsupported   = errors.New("explicit virtual-file fetch is not supported for this folder")
	ErrVirtualFileContentUnavailable = errors.New("no connected device has the required version")
)

type virtualFileMaterializer interface {
	materializeVirtualFile(ctx context.Context, file protocol.FileInfo) error
}

// FetchVirtualFile materializes a metadata-only record into a real local file.
// Experimental: This is the Phase 3 explicit lazy-fetch API.
func (m *model) FetchVirtualFile(ctx context.Context, folder, file string) (FilePresence, error) {
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

	metadataOnly, err := m.isMetadataOnly(folder, name)
	if err != nil {
		return FilePresence{}, err
	}
	if !metadataOnly {
		return FilePresence{}, ErrVirtualFileNotMetadataOnly
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

	backend := m.currentContentBackend()
	metadataPresence := newFilePresence(folder, name, MetadataOnly, backend)
	m.notifyVirtualFileHooks(m.newVirtualFileEvent(VirtualFileEventFetchStarted, metadataPresence, nil))

	if err := backend.Materialize(ctx, m, cfg, global); err != nil {
		m.notifyVirtualFileHooks(m.newVirtualFileEvent(VirtualFileEventFetchFailed, metadataPresence, err))
		return FilePresence{}, err
	}

	presence, err := m.VirtualFilePresence(folder, name)
	if err != nil {
		m.notifyVirtualFileHooks(m.newVirtualFileEvent(VirtualFileEventFetchFailed, metadataPresence, err))
		return FilePresence{}, err
	}

	m.notifyVirtualFileHooks(m.newVirtualFileEvent(VirtualFileEventFetchCompleted, presence, nil))
	return presence, nil
}

func (m *model) materializeLocalFile(ctx context.Context, folder config.FolderConfiguration, file protocol.FileInfo) error {
	m.mut.RLock()
	runner, ok := m.folderRunners.Get(folder.ID)
	m.mut.RUnlock()
	if !ok {
		return ErrFolderNotRunning
	}

	materializer, ok := runner.(virtualFileMaterializer)
	if !ok {
		return ErrVirtualFileFetchUnsupported
	}

	if err := materializer.materializeVirtualFile(ctx, file); err != nil {
		switch {
		case errors.Is(err, errNoDevice), errors.Is(err, errNotAvailable):
			return ErrVirtualFileContentUnavailable
		default:
			return fmt.Errorf("materializing %s: %w", file.Name, err)
		}
	}

	return nil
}
