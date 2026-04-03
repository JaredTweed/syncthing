// Copyright (C) 2026 The Syncthing Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"context"
	"errors"
	"io"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
)

var ErrContentAddressableBackendUnavailable = errors.New("content-addressed backend unavailable")

// ContentAddressableReference describes a future content-addressed object
// without coupling Syncthing to any specific backend implementation.
type ContentAddressableReference struct {
	Scheme string `json:"scheme"`
	Key    string `json:"key"`
	Size   int64  `json:"size"`
}

// ContentAddressableBackend is the Phase 5 seam for future object stores such
// as IPFS. The default implementation is a local no-op backend.
type ContentAddressableBackend interface {
	BackendType() string
	SupportsContentAddressing() bool
	ReferenceForFile(folder config.FolderConfiguration, file protocol.FileInfo) (ContentAddressableReference, bool, error)
	Get(ctx context.Context, ref ContentAddressableReference) (io.ReadCloser, error)
	Put(ctx context.Context, ref ContentAddressableReference, src io.Reader) error
}

type noopContentAddressableBackend struct{}

func (noopContentAddressableBackend) BackendType() string {
	return "localNoop"
}

func (noopContentAddressableBackend) SupportsContentAddressing() bool {
	return false
}

func (noopContentAddressableBackend) ReferenceForFile(config.FolderConfiguration, protocol.FileInfo) (ContentAddressableReference, bool, error) {
	return ContentAddressableReference{}, false, nil
}

func (noopContentAddressableBackend) Get(context.Context, ContentAddressableReference) (io.ReadCloser, error) {
	return nil, ErrContentAddressableBackendUnavailable
}

func (noopContentAddressableBackend) Put(context.Context, ContentAddressableReference, io.Reader) error {
	return ErrContentAddressableBackendUnavailable
}
