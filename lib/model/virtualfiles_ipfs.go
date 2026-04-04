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
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/internal/db"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
)

const (
	virtualFilesIPFSObjectMetaKV    = "virtualfiles/ipfs/object-meta"
	virtualFilesIPFSFileRefKVPrefix = "virtualfiles/ipfs/file-refs"
)

type ipfsContentAddressableBackend struct {
	apiURL         string
	timeout        time.Duration
	healthInterval time.Duration
	sdb            db.DB
	objectMeta     *db.Typed
	client         *http.Client

	mut        sync.Mutex
	lastHealth ContentAddressableBackendHealth
}

type ipfsAddResponse struct {
	Hash string `json:"Hash"`
}

func newIPFSContentAddressableBackend(m *model) ContentAddressableBackend {
	opts := m.cfg.Options()
	apiURL := strings.TrimSpace(opts.ExperimentalVirtualFilesIPFSAPIURL)
	if apiURL == "" {
		apiURL = "http://127.0.0.1:5001"
	}
	timeout := time.Duration(opts.ExperimentalVirtualFilesIPFSTimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	healthInterval := time.Duration(opts.ExperimentalVirtualFilesIPFSHealthIntervalS) * time.Second
	if healthInterval <= 0 {
		healthInterval = 30 * time.Second
	}
	return &ipfsContentAddressableBackend{
		apiURL:         apiURL,
		timeout:        timeout,
		healthInterval: healthInterval,
		sdb:            m.sdb,
		objectMeta:     db.NewTyped(m.sdb, virtualFilesIPFSObjectMetaKV),
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

func (b *localFilesystemContentBackend) localContentAddressableBackend() ContentAddressableBackend {
	if b == nil || b.model == nil || !b.model.experimentalVirtualFilesEnabled() {
		return noopContentAddressableBackend{}
	}
	b.localOnce.Do(func() {
		b.localCAS = newLocalContentAddressableBackend(b.model)
	})
	if b.localCAS == nil {
		return noopContentAddressableBackend{}
	}
	return b.localCAS
}

func (b *localFilesystemContentBackend) ipfsContentAddressableBackend() (ContentAddressableBackend, bool) {
	if b == nil || b.model == nil || !b.model.experimentalVirtualFilesEnabled() {
		return nil, false
	}
	opts := b.model.cfg.Options()
	if !opts.ExperimentalVirtualFilesIPFSEnabled {
		return nil, false
	}

	key := fmt.Sprintf("%s|%d|%d", strings.TrimSpace(opts.ExperimentalVirtualFilesIPFSAPIURL), opts.ExperimentalVirtualFilesIPFSTimeoutS, opts.ExperimentalVirtualFilesIPFSHealthIntervalS)
	b.ipfsMut.Lock()
	defer b.ipfsMut.Unlock()
	if b.ipfsCAS == nil || b.ipfsConfig != key {
		b.ipfsCAS = newIPFSContentAddressableBackend(b.model)
		b.ipfsConfig = key
	}
	return b.ipfsCAS, b.ipfsCAS != nil
}

func (b *localFilesystemContentBackend) referenceBackends(ctx context.Context) []ContentAddressableBackend {
	local := b.localContentAddressableBackend()
	backends := make([]ContentAddressableBackend, 0, 2)
	if local != nil && local.SupportsContentAddressing() {
		backends = append(backends, local)
	}
	if ipfs, ok := b.ipfsContentAddressableBackend(); ok && ipfs.SupportsContentAddressing() {
		if health := ipfs.Health(ctx); health.Healthy {
			backends = append(backends, ipfs)
		}
	}
	return backends
}

func (b *localFilesystemContentBackend) contentAddressableBackendForRef(ref ContentAddressableReference) ContentAddressableBackend {
	ref = normalizeContentAddressableReference(ref)
	switch ref.Backend {
	case "ipfsCAS":
		if ipfs, ok := b.ipfsContentAddressableBackend(); ok {
			return ipfs
		}
	case "", "localCAS":
		return b.localContentAddressableBackend()
	}
	return noopContentAddressableBackend{}
}

func (*ipfsContentAddressableBackend) BackendType() string {
	return "ipfsCAS"
}

func (*ipfsContentAddressableBackend) SupportsContentAddressing() bool {
	return true
}

func (b *ipfsContentAddressableBackend) Health(ctx context.Context) ContentAddressableBackendHealth {
	now := time.Now().UTC()

	b.mut.Lock()
	cached := b.lastHealth
	if !cached.Checked.IsZero() && now.Sub(cached.Checked) < b.healthInterval {
		b.mut.Unlock()
		return cached
	}
	b.mut.Unlock()

	status := ContentAddressableBackendHealth{
		Backend:    b.BackendType(),
		Configured: true,
		Healthy:    false,
		Checked:    now,
	}

	reqCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	resp, err := b.do(reqCtx, http.MethodPost, "/api/v0/version", nil, nil, "")
	if err != nil {
		status.Reason = err.Error()
	} else {
		_, statusCode, readErr := (*ProcesslessResponseReader)(nil).read(resp)
		if readErr != nil {
			status.Reason = readErr.Error()
		} else if statusCode != http.StatusOK {
			status.Reason = http.StatusText(statusCode)
		} else {
			status.Healthy = true
		}
	}

	b.mut.Lock()
	b.lastHealth = status
	b.mut.Unlock()
	return status
}

func (b *ipfsContentAddressableBackend) ReferenceForFile(folder config.FolderConfiguration, file protocol.FileInfo) (ContentAddressableReference, bool, error) {
	record, ok, err := b.fileRefRecord(folder.ID, file.Name)
	if err != nil || !ok {
		return ContentAddressableReference{}, false, err
	}
	if !record.matches(file) {
		if err := b.ForgetFile(folder.ID, file.Name); err != nil {
			return ContentAddressableReference{}, false, err
		}
		return ContentAddressableReference{}, false, nil
	}
	ref := record.Ref
	if ref.Backend == "" {
		ref.Backend = b.BackendType()
	}
	if ref.Backend != b.BackendType() || ref.Locator == "" || validateContentAddressableReference(ref) != nil {
		if err := b.ForgetFile(folder.ID, file.Name); err != nil {
			return ContentAddressableReference{}, false, err
		}
		return ContentAddressableReference{}, false, nil
	}
	if !b.Health(context.Background()).Healthy {
		return ContentAddressableReference{}, false, nil
	}
	return ref, true, nil
}

func (b *ipfsContentAddressableBackend) AssociateFile(folder config.FolderConfiguration, file protocol.FileInfo, ref ContentAddressableReference) error {
	if err := validateIPFSContentAddressableReference(ref); err != nil {
		return err
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

func (b *ipfsContentAddressableBackend) ForgetFile(folder, file string) error {
	b.mut.Lock()
	defer b.mut.Unlock()

	record, ok, err := b.fileRefRecord(folder, file)
	if err != nil || !ok {
		return err
	}
	if err := b.fileRefStore(folder).Delete(file); err != nil {
		return err
	}
	return b.adjustObjectRefCountHint(record.Ref, -1)
}

func (b *ipfsContentAddressableBackend) Get(ctx context.Context, ref ContentAddressableReference) (io.ReadCloser, error) {
	if err := validateIPFSContentAddressableReference(ref); err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, b.timeout)
	resp, err := b.do(reqCtx, http.MethodPost, "/api/v0/cat", url.Values{"arg": []string{ref.Locator}}, nil, "")
	if err != nil {
		cancel()
		return nil, ErrContentAddressableBackendUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		cancel()
		if isIPFSNotFound(body, resp.StatusCode) {
			return nil, ErrContentAddressableObjectMissing
		}
		return nil, ErrContentAddressableBackendUnavailable
	}

	return &verifyingReadCloser{
		ReadCloser: &contextCancelReadCloser{ReadCloser: resp.Body, cancel: cancel},
		hasher:     sha256.New(),
		expected:   ref,
	}, nil
}

func (b *ipfsContentAddressableBackend) Put(ctx context.Context, ref ContentAddressableReference, src io.Reader) (ContentAddressableReference, error) {
	ref.Backend = b.BackendType()
	if err := validateContentAddressableReference(ref); err != nil {
		return ContentAddressableReference{}, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	resultCh := make(chan struct {
		written int64
		sum     string
		err     error
	}, 1)

	go func() {
		defer close(resultCh)
		defer pw.Close()

		part, err := writer.CreateFormFile("file", ref.Key)
		if err != nil {
			_ = pw.CloseWithError(err)
			resultCh <- struct {
				written int64
				sum     string
				err     error
			}{err: err}
			return
		}

		hasher := sha256.New()
		written, err := io.Copy(io.MultiWriter(part, hasher), src)
		closeErr := writer.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = pw.CloseWithError(err)
			resultCh <- struct {
				written int64
				sum     string
				err     error
			}{err: err}
			return
		}
		resultCh <- struct {
			written int64
			sum     string
			err     error
		}{
			written: written,
			sum:     hex.EncodeToString(hasher.Sum(nil)),
		}
	}()

	params := url.Values{
		"pin":         []string{"false"},
		"cid-version": []string{"1"},
		"raw-leaves":  []string{"true"},
		"hash":        []string{"sha2-256"},
	}
	resp, err := b.do(reqCtx, http.MethodPost, "/api/v0/add", params, pr, writer.FormDataContentType())
	result := <-resultCh
	if result.err != nil {
		return ContentAddressableReference{}, result.err
	}
	if err != nil {
		return ContentAddressableReference{}, ErrContentAddressableBackendUnavailable
	}
	body, status, err := (*ProcesslessResponseReader)(nil).read(resp)
	if err != nil {
		return ContentAddressableReference{}, ErrContentAddressableBackendUnavailable
	}
	if result.written != ref.Size || result.sum != ref.Key {
		return ContentAddressableReference{}, ErrContentAddressableDigestMismatch
	}
	if status != http.StatusOK {
		return ContentAddressableReference{}, ErrContentAddressableBackendUnavailable
	}

	locator, err := parseIPFSAddResponse(body)
	if err != nil {
		return ContentAddressableReference{}, err
	}
	ref.Locator = locator
	if err := b.ensureObjectMeta(ref); err != nil {
		return ContentAddressableReference{}, err
	}
	return ref, nil
}

func (b *ipfsContentAddressableBackend) fileRefStore(folder string) *db.Typed {
	return db.NewTyped(b.sdb, virtualFilesIPFSFileRefKVPrefix+"/"+folder)
}

func (b *ipfsContentAddressableBackend) fileRefRecord(folder, file string) (contentAddressableFileRefRecord, bool, error) {
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

func (b *ipfsContentAddressableBackend) objectMetaRecord(ref ContentAddressableReference) (contentAddressableObjectMeta, bool, error) {
	bs, ok, err := b.objectMeta.Bytes(b.objectMetaKey(ref))
	if err != nil || !ok {
		return contentAddressableObjectMeta{}, ok, err
	}

	var meta contentAddressableObjectMeta
	if err := json.Unmarshal(bs, &meta); err != nil {
		if deleteErr := b.objectMeta.Delete(b.objectMetaKey(ref)); deleteErr != nil {
			return contentAddressableObjectMeta{}, false, deleteErr
		}
		return contentAddressableObjectMeta{}, false, nil
	}
	return meta, true, nil
}

func (b *ipfsContentAddressableBackend) objectMetaKey(ref ContentAddressableReference) string {
	return ref.Locator
}

func (b *ipfsContentAddressableBackend) putObjectMeta(ref ContentAddressableReference, meta contentAddressableObjectMeta) error {
	bs, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return b.objectMeta.PutBytes(b.objectMetaKey(ref), bs)
}

func (b *ipfsContentAddressableBackend) ensureObjectMeta(ref ContentAddressableReference) error {
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

func (b *ipfsContentAddressableBackend) adjustObjectRefCountHint(ref ContentAddressableReference, delta int) error {
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

func (b *ipfsContentAddressableBackend) do(ctx context.Context, method, endpoint string, params url.Values, body io.Reader, contentType string) (*http.Response, error) {
	apiURL, err := url.Parse(b.apiURL)
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
	return b.client.Do(req)
}

func parseIPFSAddResponse(body []byte) (string, error) {
	lines := bytes.Split(bytes.TrimSpace(body), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var resp ipfsAddResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return "", err
		}
		if resp.Hash == "" {
			return "", ErrContentAddressableCorrupt
		}
		return resp.Hash, nil
	}
	return "", ErrContentAddressableCorrupt
}

func validateIPFSContentAddressableReference(ref ContentAddressableReference) error {
	if err := validateContentAddressableReference(ref); err != nil {
		return err
	}
	if ref.Backend != "" && ref.Backend != "ipfsCAS" {
		return ErrContentAddressableReferenceInvalid
	}
	if strings.TrimSpace(ref.Locator) == "" {
		return ErrContentAddressableReferenceInvalid
	}
	return nil
}

func isIPFSNotFound(body []byte, status int) bool {
	if status == http.StatusNotFound {
		return true
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "not found") || strings.Contains(msg, "key not found") || strings.Contains(msg, "no link named")
}

type contextCancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *contextCancelReadCloser) Close() error {
	defer r.cancel()
	return r.ReadCloser.Close()
}

type ProcesslessResponseReader struct{}

func (*ProcesslessResponseReader) read(resp *http.Response) ([]byte, int, error) {
	bs, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	return bs, resp.StatusCode, err
}

func (b *ipfsContentAddressableBackend) String() string {
	return fmt.Sprintf("ipfsCAS(%s)", b.apiURL)
}

func fileSystemHasFile(filesystem fs.Filesystem, name string) bool {
	_, err := filesystem.Lstat(name)
	return err == nil
}
