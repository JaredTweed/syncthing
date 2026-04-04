# Experimental Virtual Files Design

This document describes Phase 0 analysis and the Phase 1 scaffolding for an experimental local-first decentralized cloud filesystem mode built on top of Syncthing.

The work is intentionally gated behind `experimentalVirtualFiles` and does not change standard Syncthing behavior by default.

## Current Architecture Summary

### Folder and File Model

- Folder configuration lives in `lib/config`.
- Each folder resolves to a concrete `lib/fs.Filesystem` through `config.FolderConfiguration.Filesystem()`.
- Runtime folder behavior lives in `lib/model`.
- File state is represented primarily by `protocol.FileInfo` plus block lists and local flags.

### Index and Database Structures

- The model persists folder state through `internal/db.DB`.
- The current SQLite backend stores:
  - announced files in `files`
  - serialized `FileInfo` payloads in `fileinfos`
  - deduplicated block lists in `blocklists`
  - local block maps in `blocks`
- Global state and needed state are derived from `files.local_flags`.
- There is no separate metadata plane and content plane today.

### Block Fetch Flow

- Remote indexes arrive through `model.handleIndex()`.
- The send-receive folder puller iterates `AllNeededGlobalFiles`.
- Blocks are fetched through:
  - `sendReceiveFolder.pullerRoutine()`
  - `sendReceiveFolder.pullBlock()`
  - `model.RequestGlobal()`
  - remote `Connection.Request()`
- Incoming block requests are served by `model.Request()`, which reads directly from the folder filesystem.

### Filesystem Abstraction Points

- `lib/fs` abstracts local filesystem access.
- Folder scanning, request serving, temporary files, and finishing all assume local filesystem-backed content.
- There is already an `internal/blob.Store` abstraction used by some infrastructure services, but it is not part of the Syncthing folder sync path today.

### REST, GUI, and Config Touchpoints Relevant to Placeholders

- Config lives in `lib/config`.
- REST and debug endpoints live in `lib/api`.
- Folder summaries and needed-file views in the GUI consume `/rest/db/status`, `/rest/db/need`, `/rest/db/file`, and `/rest/db/browse`.
- Existing REST and GUI code assumes normal locally materialized files.

## Proposed Architecture

### Goals

- Preserve standard Syncthing behavior when `experimentalVirtualFiles` is disabled.
- Introduce a file presence abstraction with three states:
  - `FullLocal`
  - `MetadataOnly`
  - `PartialLocal`
- Separate the future metadata plane from the content plane without forcing a database rewrite in Phase 1.
- Add an internal content backend seam so future lazy fetch and content-addressed backends can be attached without hard-coding a new dependency.

### Phase 1 Architecture

- Add `options.experimentalVirtualFiles`, default `false`.
- Add a new internal `PresenceState` abstraction in `lib/model`.
- Add a new internal content backend interface in `lib/model` with a default implementation that reflects current local-filesystem behavior.
- Keep request serving, scanning, and pulling semantics unchanged.
- Add a debug endpoint exposing presence state for a file.
- Report all current local files as `FullLocal`.

### Future Architecture Direction

- Metadata plane:
  - local knowledge that a file exists
  - file attributes
  - block map or object references
  - presence state
- Content plane:
  - current local filesystem
  - future partial-local storage
  - future content-addressed or remote object backends
- Future request and materialization paths should route through the content backend seam instead of directly assuming the folder filesystem contains full file bytes.

## Exact Packages and Files Likely to Change

### Phase 1

- `docs/experimental-virtual-files-design.md`
- `lib/config/optionsconfiguration.go`
- `lib/config/config_test.go`
- `lib/model/model.go`
- `lib/model/virtualfiles.go`
- `lib/model/model_test.go`
- `lib/api/api.go`
- `lib/api/api_test.go`

### Likely Later Phases

- `internal/db/interface.go`
- `internal/db/sqlite/...`
- `lib/model/folder.go`
- `lib/model/folder_sendrecv.go`
- `lib/model/sharedpullerstate.go`
- `lib/api/api.go`
- `gui/default/syncthing/core/syncthingController.js`
- possibly `lib/fs/...`
- possibly `internal/blob/...` or a new content backend package

## Risks to Data Safety

- Accidentally reusing existing local flags for presence state could corrupt current global and need accounting.
- Serving reads through a future backend before the metadata/content split is correct could return stale or incomplete data.
- Allowing placeholders to look like fully materialized files too early could confuse scanners, pullers, or user-visible API behavior.
- Mixing metadata-only records with current local file semantics without careful gating could trigger deletions, rescans, or conflicts incorrectly.
- Any future content-addressed backend must preserve Syncthing's validation guarantees and not bypass block hash verification.

## Phased Plan

### Phase 1: Compileable Scaffolding Only

- Add `experimentalVirtualFiles: false` to config.
- Add `PresenceState` abstraction.
- Add a default local-filesystem content backend interface and implementation.
- Add a debug endpoint exposing presence state.
- Keep all behavior unchanged.

### Phase 2: Metadata-Only Placeholder Records

- Add a durable representation for placeholder presence state.
- Extend database schema or metadata storage for local presence without full content.
- Keep placeholders opt-in and feature-gated.

Phase 2 implementation notes:

- Placeholder presence is stored locally in the database KV layer under an experimental metadata plane.
- The wire protocol is unchanged. Remote devices do not need to understand placeholders.
- Metadata-only placeholders do not create files on the OS filesystem in this phase.
- When `experimentalVirtualFiles` is enabled, metadata-only placeholders are excluded from the local pull queue and local need accounting.
- The explicit mutation API is a debug endpoint: `POST /rest/debug/virtual-file/metadata-only?folder=<id>&file=<path>`.

### Phase 3: Lazy Fetch on Explicit API Request

- Add an explicit API to fetch or materialize a file on demand.
- Route fetch/materialization through the content backend seam.
- Keep implicit background behavior unchanged unless explicitly requested.

Phase 3 implementation notes:

- Explicit fetch is exposed as `POST /rest/debug/virtual-file/fetch?folder=<id>&file=<path>`.
- The fetch path is still fully feature-gated by `experimentalVirtualFiles`.
- Only the explicitly requested metadata-only file is materialized; the normal background pull queue remains unchanged.
- The default backend reuses Syncthing's existing block request and hash verification logic, writes a temp file, publishes it without replacement, and then reconciles the result through a targeted local rescan before clearing the metadata-only record.
- Failures leave the metadata-only record in place and do not create an OS-visible placeholder file.
- The explicit fetch finisher is now separate from the normal pull/conflict finisher to avoid reusing scan-queue side effects in a one-shot API path.
- Fetch revalidates that the current global file is still the same regular-file version before making the content visible locally.
- Final publish now uses an atomic no-replace publish helper under Syncthing's rename lock.
- The helper uses a hard-link style publish on local basic filesystems, so an out-of-band local writer can win by creating the destination first, but it cannot be overwritten by the virtual-file materialization path.
- There is no overwrite-capable fallback in the explicit virtual-file publish path. If the filesystem cannot support the safe publish primitive, fetch fails and leaves the metadata-only record intact.

### Phase 4: Desktop Virtual Filesystem Integration Hooks

- Add hooks for platform-specific virtual filesystem adapters.
- Expose enough metadata and fetch APIs to support desktop integration without embedding OS-specific code into the core sync model.

Phase 4 implementation notes:

- Desktop integration is represented by an internal `VirtualFilesystemHook` interface and `VirtualFileEvent` lifecycle callbacks.
- The current hooks report placeholder creation, explicit fetch start/completion/failure, and placeholder clearing when a real local file update supersedes a placeholder.
- No platform-specific filesystem driver, kernel integration, or OS placeholder exposure is included in this phase.
- Hook dispatch is synchronous for ordering, but now recovers from hook panics so experimental integrations cannot crash the core model.
- Hook dispatch intentionally remains synchronous. Reordering or buffering these lifecycle events behind a background queue would make it harder for a future desktop bridge to reason about placeholder creation, fetch completion, and placeholder clearing relative to the state transition that triggered them.

### Phase 5: Optional Content-Addressed Backend Interface

- Extend the backend abstraction for object references and content-addressed storage.
- Keep IPFS or similar systems optional and pluggable.
- Avoid introducing any hard dependency into the default Syncthing build.

Phase 5 implementation notes:

- A `ContentAddressableBackend` interface now exists alongside the main content backend seam.
- The first real implementation is a local, Syncthing-managed CAS store rooted beside Syncthing's SQLite database, with file references and GC hints stored in the metadata DB.
- Object layout:
  - `<db dir>/virtualfiles-cas/objects/<sha256-prefix>/<sha256>` stores the raw object bytes on disk
  - `virtualfiles/cas/object-meta/<sha256>` in the DB KV store holds object metadata and a reference-count hint
  - `virtualfiles/cas/file-refs/<folder>/<file>` in the DB KV store holds the current file-to-object association for the matching global file content
- Digest/address format:
  - `ContentAddressableReference{Scheme:"sha256", Key:<hex sha256 of full file bytes>, Size:<file size>}`
  - file references additionally record the expected block hash and file size so stale references can be rejected before use
- Write path:
  - remote explicit fetch still downloads into a staged temp file and verifies blocks using the existing Syncthing pull path
  - once the staged temp is complete, the bytes are hashed into a `sha256` address, streamed into local CAS, and the file reference is updated
  - only after that does materialization publish the final file through the existing no-overwrite path
- Read path:
  - explicit fetch first asks the CAS backend whether the current global file already has a valid local object reference
  - if so, the object is streamed out of CAS into a staged temp file, rehashed against the current block list, and then published through the same conservative materialization path
  - if the CAS object is missing or corrupt, the reference is treated as stale and fetch falls back to the normal remote block fetch path
- Reference tracking:
  - the file-reference store is the first live-reference scaffold for future GC
  - object metadata stores a best-effort reference-count hint so a later GC pass can identify apparently unreferenced objects without changing the object format
  - full GC is intentionally deferred; Phase 5 only provides the bookkeeping seams
- Failure handling:
  - digest mismatch on CAS read/write is treated as corruption and never published to the folder filesystem
  - missing or stale CAS references are self-healed by dropping the reference and falling back to remote fetch when possible
  - CAS storage failure does not relax publish safety or introduce overwrite semantics; it only disables the acceleration path for that fetch
- Future IPFS adapter boundary:
  - a future non-local adapter should implement the same `ContentAddressableBackend` contract while leaving file-reference validation, placeholder state, and conservative final publish semantics unchanged
- The debug virtual-file responses expose the current content backend type and the content-addressed backend capability so future adapters can be validated without changing the wire protocol.

Phase 5 extension: optional IPFS adapter

- The current local CAS remains the primary experimental backend.
- A second optional adapter, `ipfsCAS`, can mirror and reuse content through a local Kubo-compatible HTTP API.
- New experimental config under `options`:
  - `experimentalVirtualFilesIPFSEnabled`
  - `experimentalVirtualFilesIPFSAPIURL`
  - `experimentalVirtualFilesIPFSTimeoutS`
  - `experimentalVirtualFilesIPFSHealthIntervalS`
  - `experimentalVirtualFilesIPFSPrefer`
- Backend interface adjustments:
  - content-addressed references now carry backend-specific data (`Backend`, `Locator`) while still using a Syncthing-owned `sha256` key and expected size as the trust anchor
  - CAS backends report health through `ContentAddressableBackendHealth`
  - `Put` returns the backend-specific stored reference so adapters like IPFS can attach locators such as a CID
- IPFS adapter architecture:
  - `ipfsCAS` is an adapter layer under `lib/model`, not a core sync rewrite
  - it talks to a configured local HTTP API endpoint and keeps its own file-reference metadata in the Syncthing DB
  - local CAS and IPFS refs are tracked separately, so IPFS failure cannot invalidate the local CAS baseline
- Trust model:
  - IPFS content is always treated as untrusted transport or cache content
  - every IPFS read is verified locally against the expected full-file `sha256` and size before the materialization path can finish
  - materialization still runs through the existing staged temp file plus no-overwrite publish semantics
- Write path:
  - explicit fetch still downloads or reconstructs bytes locally first
  - successful explicit fetch stores into local CAS as the primary durable baseline
  - if IPFS is enabled and healthy, the same verified bytes are mirrored into `ipfsCAS` on a best-effort basis
  - IPFS mirror failure never invalidates a successful local fetch or local CAS store
- Read path:
  - explicit fetch prefers local CAS reuse first
  - if local CAS has no usable ref, the adapter may reuse a healthy IPFS-backed ref
  - stale, missing, unhealthy, or corrupt IPFS content is ignored or pruned and fetch falls back safely to local CAS or the normal remote block-fetch path
- Failure and fallback behavior:
  - IPFS health only influences the optional acceleration path
  - if IPFS is disabled, unhealthy, unreachable, slow, or returns corrupt data, the feature falls back to local CAS or normal remote fetch
  - transport and timeout failures now update cached IPFS health immediately so debug presence and later fetch attempts reflect live daemon loss more quickly than the normal periodic probe interval
  - IPFS never widens publish semantics and never creates an overwrite-capable fallback
- What is and is not decentralized yet:
  - this does not make Syncthing fully decentralized in the storage layer
  - the local node still owns metadata, safety checks, explicit fetch decisions, and final file adoption
  - IPFS is currently only an optional local content-addressed cache or mirror behind explicit virtual-file fetch
  - the wire protocol between Syncthing peers remains unchanged
- Real-Kubo validation path:
  - fake HTTP tests remain the default fast coverage for adapter semantics and timeout behavior
  - additional integration-tag tests can use a managed local `kubo`/`ipfs` daemon when the binary is installed, or skip cleanly when it is absent
  - the real-daemon suite is intended to catch behavioral differences between the fake API assumptions and an actual Kubo node without making IPFS a hard build or runtime dependency

## Hardening Notes

- Metadata-only records are now self-healing:
  - corrupt records are pruned on access
  - ignored paths are rejected and stale placeholder records for them are removed
  - remote deletes and unsupported type changes prune stale placeholder records
  - local index updates still clear placeholder state when a real local file supersedes it
- Explicit mutation and fetch reject empty or root-like paths (`""`, `"."`) as invalid.
- `SetMetadataOnly` now checks both the local index and the real filesystem, so an unscanned local file cannot be overwritten by a placeholder record.
- Fetch failure keeps the metadata-only record in place and cleans up the temporary file.
- If a real local file appears while an explicit fetch is in progress, fetch now fails with `ErrVirtualFileAlreadyLocal`, schedules a rescan, and leaves the local file in place.
- If the remote file version changes while an explicit fetch is in progress, fetch fails with `ErrVirtualFileStale` and leaves the placeholder record intact.
- Explicit fetch now adopts materialized content through a targeted local rescan instead of directly trusting the remote metadata after publish. This means a local writer that changes the file after publish but before adoption is reconciled from disk, not from the stale fetched metadata.
- Restart safety now relies on two conservative properties:
  - published files are adopted through the normal local scan path on restart, which clears stale placeholder metadata
  - if a crash happens after publish but before staged-temp cleanup, the leftover temp name remains ignored and the published final file is still reconciled safely on the next scan
- Remaining limitation: safe explicit fetch publish now depends on a no-replace primitive being available for the current local basic filesystem. On filesystems where hard-link publish is unsupported, explicit fetch fails conservatively instead of falling back to an overwrite-capable path.
