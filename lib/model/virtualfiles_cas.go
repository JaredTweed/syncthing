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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
)

const (
	virtualFilesCASDirName         = "virtualfiles-cas"
	virtualFilesCASObjectMetaKV    = "virtualfiles/cas/object-meta"
	virtualFilesCASFileRefKVPrefix = "virtualfiles/cas/file-refs"
)

var (
	ErrContentAddressableBackendUnavailable = errors.New("content-addressed backend unavailable")
	ErrContentAddressableObjectMissing      = errors.New("content-addressed object missing")
	ErrContentAddressableCorrupt            = errors.New("content-addressed object corrupt")
	ErrContentAddressableDigestMismatch     = errors.New("content-addressed digest mismatch")
	ErrContentAddressableReferenceInvalid   = errors.New("content-addressed reference invalid")
)

// ContentAddressableReference describes a future content-addressed object
// without coupling Syncthing to any specific backend implementation.
type ContentAddressableReference struct {
	Scheme  string `json:"scheme"`
	Key     string `json:"key"`
	Size    int64  `json:"size"`
	Backend string `json:"backend,omitempty"`
	Locator string `json:"locator,omitempty"`
}

type ContentAddressableBackendHealth struct {
	Backend    string    `json:"backend"`
	Configured bool      `json:"configured"`
	Healthy    bool      `json:"healthy"`
	Reason     string    `json:"reason,omitempty"`
	Checked    time.Time `json:"checked,omitempty"`
}

// ContentAddressableBackend is the Phase 5 seam for future object stores such
// as IPFS. The default implementation is a local no-op backend.
type ContentAddressableBackend interface {
	BackendType() string
	SupportsContentAddressing() bool
	Health(ctx context.Context) ContentAddressableBackendHealth
	ReferenceForFile(folder config.FolderConfiguration, file protocol.FileInfo) (ContentAddressableReference, bool, error)
	AssociateFile(folder config.FolderConfiguration, file protocol.FileInfo, ref ContentAddressableReference) error
	ForgetFile(folder, file string) error
	Get(ctx context.Context, ref ContentAddressableReference) (io.ReadCloser, error)
	Put(ctx context.Context, ref ContentAddressableReference, src io.Reader) (ContentAddressableReference, error)
}

type noopContentAddressableBackend struct{}

type localContentAddressableBackend struct {
	root       string
	sdb        db.DB
	objectMeta *db.Typed
	mut        sync.Mutex
}

type contentAddressableObjectMeta struct {
	Size         int64     `json:"size"`
	RefCountHint int       `json:"refCountHint"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
}

type contentAddressableFileRefRecord struct {
	Ref        ContentAddressableReference `json:"ref"`
	Size       int64                       `json:"size"`
	BlocksHash []byte                      `json:"blocksHash,omitempty"`
	Updated    time.Time                   `json:"updated"`
}

type verifyingReadCloser struct {
	io.ReadCloser
	hasher    hashWriter
	expected  ContentAddressableReference
	bytesRead int64
	verified  bool
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func newLocalContentAddressableBackend(m *model) ContentAddressableBackend {
	root, ok := localContentAddressableRoot(m.cfg, m.sdb)
	if !ok {
		return noopContentAddressableBackend{}
	}
	return &localContentAddressableBackend{
		root:       root,
		sdb:        m.sdb,
		objectMeta: db.NewTyped(m.sdb, virtualFilesCASObjectMetaKV),
	}
}

func localContentAddressableRoot(cfg config.Wrapper, sdb db.DB) (string, bool) {
	if provider, ok := sdb.(interface{ PathBase() string }); ok {
		if pathBase := provider.PathBase(); pathBase != "" {
			return filepath.Join(pathBase, virtualFilesCASDirName), true
		}
	}

	configPath := cfg.ConfigPath()
	if configPath == "" {
		return "", false
	}
	return filepath.Join(filepath.Dir(configPath), virtualFilesCASDirName), true
}

func (noopContentAddressableBackend) BackendType() string {
	return "localNoop"
}

func (noopContentAddressableBackend) SupportsContentAddressing() bool {
	return false
}

func (noopContentAddressableBackend) Health(context.Context) ContentAddressableBackendHealth {
	return ContentAddressableBackendHealth{
		Backend:    "localNoop",
		Configured: false,
		Healthy:    false,
		Reason:     "disabled",
		Checked:    time.Now().UTC(),
	}
}

func (noopContentAddressableBackend) ReferenceForFile(config.FolderConfiguration, protocol.FileInfo) (ContentAddressableReference, bool, error) {
	return ContentAddressableReference{}, false, nil
}

func (noopContentAddressableBackend) AssociateFile(config.FolderConfiguration, protocol.FileInfo, ContentAddressableReference) error {
	return ErrContentAddressableBackendUnavailable
}

func (noopContentAddressableBackend) ForgetFile(string, string) error {
	return nil
}

func (noopContentAddressableBackend) Get(context.Context, ContentAddressableReference) (io.ReadCloser, error) {
	return nil, ErrContentAddressableBackendUnavailable
}

func (noopContentAddressableBackend) Put(context.Context, ContentAddressableReference, io.Reader) (ContentAddressableReference, error) {
	return ContentAddressableReference{}, ErrContentAddressableBackendUnavailable
}

func (*localContentAddressableBackend) BackendType() string {
	return "localCAS"
}

func (*localContentAddressableBackend) SupportsContentAddressing() bool {
	return true
}

func (b *localContentAddressableBackend) Health(context.Context) ContentAddressableBackendHealth {
	status := ContentAddressableBackendHealth{
		Backend:    b.BackendType(),
		Configured: b != nil && b.root != "",
		Healthy:    b != nil && b.root != "",
		Checked:    time.Now().UTC(),
	}
	if !status.Healthy {
		status.Reason = "local CAS root unavailable"
	}
	return status
}

func (b *localContentAddressableBackend) ReferenceForFile(folder config.FolderConfiguration, file protocol.FileInfo) (ContentAddressableReference, bool, error) {
	record, ok, err := b.fileRefRecord(folder.ID, file.Name)
	if err != nil || !ok {
		return ContentAddressableReference{}, false, err
	}
	record.Ref = normalizeContentAddressableReference(record.Ref)
	if !record.matches(file) {
		if err := b.ForgetFile(folder.ID, file.Name); err != nil {
			return ContentAddressableReference{}, false, err
		}
		return ContentAddressableReference{}, false, nil
	}
	if err := validateContentAddressableReference(record.Ref); err != nil {
		if err := b.ForgetFile(folder.ID, file.Name); err != nil {
			return ContentAddressableReference{}, false, err
		}
		return ContentAddressableReference{}, false, nil
	}
	available, err := b.objectAvailable(record.Ref)
	if err != nil {
		return ContentAddressableReference{}, false, err
	}
	if !available {
		if err := b.ForgetFile(folder.ID, file.Name); err != nil {
			return ContentAddressableReference{}, false, err
		}
		return ContentAddressableReference{}, false, nil
	}
	return record.Ref, true, nil
}

func (b *localContentAddressableBackend) AssociateFile(folder config.FolderConfiguration, file protocol.FileInfo, ref ContentAddressableReference) error {
	ref = normalizeContentAddressableReference(ref)
	if err := validateContentAddressableReference(ref); err != nil {
		return err
	}
	available, err := b.objectAvailable(ref)
	if err != nil {
		return err
	}
	if !available {
		return ErrContentAddressableObjectMissing
	}

	record := contentAddressableFileRefRecord{
		Ref:        ref,
		Size:       file.Size,
		BlocksHash: fileBlocksHash(file),
		Updated:    time.Now().UTC(),
	}
	bs, err := json.Marshal(record)
	if err != nil {
		return err
	}

	b.mut.Lock()
	defer b.mut.Unlock()

	oldRecord, ok, err := b.fileRefRecord(folder.ID, file.Name)
	if err != nil {
		return err
	}
	if ok && !contentAddressableReferenceEqual(oldRecord.Ref, ref) {
		if err := b.adjustObjectRefCountHint(oldRecord.Ref, -1); err != nil {
			return err
		}
	}
	if !ok || !contentAddressableReferenceEqual(oldRecord.Ref, ref) {
		if err := b.adjustObjectRefCountHint(ref, 1); err != nil {
			return err
		}
	} else if err := b.ensureObjectMeta(ref); err != nil {
		return err
	}

	return b.fileRefStore(folder.ID).PutBytes(file.Name, bs)
}

func (b *localContentAddressableBackend) ForgetFile(folder, file string) error {
	b.mut.Lock()
	defer b.mut.Unlock()
	return b.forgetFileLocked(folder, file)
}

func (b *localContentAddressableBackend) Get(_ context.Context, ref ContentAddressableReference) (io.ReadCloser, error) {
	ref = normalizeContentAddressableReference(ref)
	if ref.Backend != "" && ref.Backend != b.BackendType() {
		return nil, ErrContentAddressableReferenceInvalid
	}
	if err := validateContentAddressableReference(ref); err != nil {
		return nil, err
	}

	path := b.objectPath(ref)
	fd, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrContentAddressableObjectMissing
		}
		return nil, err
	}

	info, err := fd.Stat()
	if err != nil {
		fd.Close()
		return nil, err
	}
	if info.Size() != ref.Size {
		fd.Close()
		return nil, ErrContentAddressableCorrupt
	}

	return &verifyingReadCloser{
		ReadCloser: fd,
		hasher:     sha256.New(),
		expected:   ref,
	}, nil
}

func (b *localContentAddressableBackend) Put(ctx context.Context, ref ContentAddressableReference, src io.Reader) (_ ContentAddressableReference, err error) {
	ref = normalizeContentAddressableReference(ref)
	ref.Backend = b.BackendType()
	ref.Locator = ""
	if err := validateContentAddressableReference(ref); err != nil {
		return ContentAddressableReference{}, err
	}

	b.mut.Lock()
	defer b.mut.Unlock()

	if err := b.ensureRoot(); err != nil {
		return ContentAddressableReference{}, err
	}

	finalPath := b.objectPath(ref)
	if err := b.ensureExistingObject(ref); err == nil {
		return ref, b.ensureObjectMeta(ref)
	} else if !errors.Is(err, ErrContentAddressableObjectMissing) {
		if !errors.Is(err, ErrContentAddressableCorrupt) {
			return ContentAddressableReference{}, err
		}
		if removeErr := os.Remove(finalPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return ContentAddressableReference{}, removeErr
		}
	}

	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ContentAddressableReference{}, err
	}

	tmp, err := os.CreateTemp(dir, "."+ref.Key+".tmp-*")
	if err != nil {
		return ContentAddressableReference{}, err
	}
	tmpName := tmp.Name()
	defer func() {
		if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
			err = removeErr
		}
	}()

	hasher := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(tmp, hasher), src)
	if err != nil {
		_ = tmp.Close()
		return ContentAddressableReference{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return ContentAddressableReference{}, err
	}
	if err := tmp.Close(); err != nil {
		return ContentAddressableReference{}, err
	}

	if written != ref.Size || hex.EncodeToString(hasher.Sum(nil)) != ref.Key {
		return ContentAddressableReference{}, ErrContentAddressableDigestMismatch
	}

	if err := os.Link(tmpName, finalPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return ContentAddressableReference{}, err
		}
		if err := b.ensureExistingObject(ref); err != nil {
			return ContentAddressableReference{}, err
		}
	}

	if err := syncDir(dir); err != nil {
		return ContentAddressableReference{}, err
	}
	return ref, b.ensureObjectMeta(ref)
}

func (r contentAddressableFileRefRecord) matches(file protocol.FileInfo) bool {
	return r.Size == file.Size && bytes.Equal(r.BlocksHash, fileBlocksHash(file))
}

func (b *localContentAddressableBackend) fileRefStore(folder string) *db.Typed {
	return db.NewTyped(b.sdb, virtualFilesCASFileRefKVPrefix+"/"+folder)
}

func (b *localContentAddressableBackend) fileRefRecord(folder, file string) (contentAddressableFileRefRecord, bool, error) {
	bs, ok, err := b.fileRefStore(folder).Bytes(file)
	if err != nil || !ok {
		return contentAddressableFileRefRecord{}, ok, err
	}

	var record contentAddressableFileRefRecord
	if err := json.Unmarshal(bs, &record); err != nil {
		if deleteErr := b.fileRefStore(folder).Delete(file); deleteErr != nil {
			return contentAddressableFileRefRecord{}, false, deleteErr
		}
		return contentAddressableFileRefRecord{}, false, nil
	}
	return record, true, nil
}

func (b *localContentAddressableBackend) objectMetaRecord(ref ContentAddressableReference) (contentAddressableObjectMeta, bool, error) {
	bs, ok, err := b.objectMeta.Bytes(ref.Key)
	if err != nil || !ok {
		return contentAddressableObjectMeta{}, ok, err
	}

	var meta contentAddressableObjectMeta
	if err := json.Unmarshal(bs, &meta); err != nil {
		if deleteErr := b.objectMeta.Delete(ref.Key); deleteErr != nil {
			return contentAddressableObjectMeta{}, false, deleteErr
		}
		return contentAddressableObjectMeta{}, false, nil
	}
	return meta, true, nil
}

func (b *localContentAddressableBackend) putObjectMeta(ref ContentAddressableReference, meta contentAddressableObjectMeta) error {
	bs, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return b.objectMeta.PutBytes(ref.Key, bs)
}

func (b *localContentAddressableBackend) ensureObjectMeta(ref ContentAddressableReference) error {
	meta, ok, err := b.objectMetaRecord(ref)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if !ok {
		meta = contentAddressableObjectMeta{
			Size:    ref.Size,
			Created: now,
		}
	}
	meta.Size = ref.Size
	meta.Updated = now
	return b.putObjectMeta(ref, meta)
}

func (b *localContentAddressableBackend) adjustObjectRefCountHint(ref ContentAddressableReference, delta int) error {
	meta, ok, err := b.objectMetaRecord(ref)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if !ok {
		meta = contentAddressableObjectMeta{
			Size:    ref.Size,
			Created: now,
		}
	}
	meta.Size = ref.Size
	meta.RefCountHint += delta
	if meta.RefCountHint < 0 {
		meta.RefCountHint = 0
	}
	meta.Updated = now
	return b.putObjectMeta(ref, meta)
}

func (b *localContentAddressableBackend) forgetFileLocked(folder, file string) error {
	record, ok, err := b.fileRefRecord(folder, file)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := b.fileRefStore(folder).Delete(file); err != nil {
		return err
	}
	return b.adjustObjectRefCountHint(record.Ref, -1)
}

func (b *localContentAddressableBackend) ensureRoot() error {
	return os.MkdirAll(filepath.Join(b.root, "objects"), 0o700)
}

func (b *localContentAddressableBackend) objectPath(ref ContentAddressableReference) string {
	return filepath.Join(b.root, "objects", ref.Key[:2], ref.Key)
}

func (b *localContentAddressableBackend) objectAvailable(ref ContentAddressableReference) (bool, error) {
	if err := validateContentAddressableReference(ref); err != nil {
		return false, err
	}

	info, err := os.Stat(b.objectPath(ref))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.Size() != ref.Size {
		return false, nil
	}
	return true, nil
}

func (b *localContentAddressableBackend) ensureExistingObject(ref ContentAddressableReference) error {
	path := b.objectPath(ref)
	fd, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrContentAddressableObjectMissing
		}
		return err
	}
	defer fd.Close()

	hasher := sha256.New()
	written, err := io.Copy(hasher, fd)
	if err != nil {
		return err
	}
	if written != ref.Size || hex.EncodeToString(hasher.Sum(nil)) != ref.Key {
		return ErrContentAddressableCorrupt
	}
	return nil
}

func validateContentAddressableReference(ref ContentAddressableReference) error {
	if ref.Scheme != "sha256" || ref.Size < 0 || len(ref.Key) != sha256.Size*2 {
		return ErrContentAddressableReferenceInvalid
	}
	if _, err := hex.DecodeString(ref.Key); err != nil {
		return ErrContentAddressableReferenceInvalid
	}
	return nil
}

func normalizeContentAddressableReference(ref ContentAddressableReference) ContentAddressableReference {
	if ref.Backend == "" && ref.Locator == "" {
		ref.Backend = "localCAS"
	}
	return ref
}

func contentAddressableReferenceEqual(a, b ContentAddressableReference) bool {
	a = normalizeContentAddressableReference(a)
	b = normalizeContentAddressableReference(b)
	return a.Scheme == b.Scheme && a.Key == b.Key && a.Size == b.Size && a.Backend == b.Backend && a.Locator == b.Locator
}

func fileBlocksHash(file protocol.FileInfo) []byte {
	if len(file.BlocksHash) > 0 {
		return append([]byte(nil), file.BlocksHash...)
	}
	if len(file.Blocks) == 0 {
		return nil
	}
	return protocol.BlocksHash(file.Blocks)
}

func contentAddressableReferenceForBytes(data []byte) ContentAddressableReference {
	sum := sha256.Sum256(data)
	return ContentAddressableReference{
		Scheme: "sha256",
		Key:    hex.EncodeToString(sum[:]),
		Size:   int64(len(data)),
	}
}

func (r *verifyingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.bytesRead += int64(n)
		_, _ = r.hasher.Write(p[:n])
	}
	if err == io.EOF {
		if verifyErr := r.verify(); verifyErr != nil {
			return n, verifyErr
		}
	}
	return n, err
}

func (r *verifyingReadCloser) Close() error {
	if err := r.ReadCloser.Close(); err != nil {
		return err
	}
	return nil
}

func (r *verifyingReadCloser) verify() error {
	if r.verified {
		return nil
	}
	r.verified = true
	if r.bytesRead != r.expected.Size {
		return ErrContentAddressableCorrupt
	}
	if hex.EncodeToString(r.hasher.Sum(nil)) != r.expected.Key {
		return ErrContentAddressableCorrupt
	}
	return nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 128<<10)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}

		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}

func syncDir(path string) error {
	fd, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fd.Close()
	return fd.Sync()
}

func contentAddressableReferenceFromFile(ctx context.Context, filesystem fs.Filesystem, name string) (ContentAddressableReference, error) {
	fd, err := filesystem.Open(name)
	if err != nil {
		return ContentAddressableReference{}, err
	}
	defer fd.Close()

	hasher := sha256.New()
	written, err := copyWithContext(ctx, hasher, fd)
	if err != nil {
		return ContentAddressableReference{}, err
	}
	return ContentAddressableReference{
		Scheme: "sha256",
		Key:    hex.EncodeToString(hasher.Sum(nil)),
		Size:   written,
	}, nil
}

func (b *localContentAddressableBackend) String() string {
	return fmt.Sprintf("localCAS(%s)", b.root)
}
