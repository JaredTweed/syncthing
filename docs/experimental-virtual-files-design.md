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
- The default backend reuses Syncthing's existing block request and hash verification logic, writes a temp file, renames it into place, updates the local index, and clears the metadata-only record on success.
- Failures leave the metadata-only record in place and do not create an OS-visible placeholder file.

### Phase 4: Desktop Virtual Filesystem Integration Hooks

- Add hooks for platform-specific virtual filesystem adapters.
- Expose enough metadata and fetch APIs to support desktop integration without embedding OS-specific code into the core sync model.

Phase 4 implementation notes:

- Desktop integration is represented by an internal `VirtualFilesystemHook` interface and `VirtualFileEvent` lifecycle callbacks.
- The current hooks report placeholder creation, explicit fetch start/completion/failure, and placeholder clearing when a real local file update supersedes a placeholder.
- No platform-specific filesystem driver, kernel integration, or OS placeholder exposure is included in this phase.

### Phase 5: Optional Content-Addressed Backend Interface

- Extend the backend abstraction for object references and content-addressed storage.
- Keep IPFS or similar systems optional and pluggable.
- Avoid introducing any hard dependency into the default Syncthing build.

Phase 5 implementation notes:

- A `ContentAddressableBackend` interface now exists alongside the main content backend seam.
- The default implementation is a local no-op backend (`localNoop`) that reports content addressing as unsupported and adds no mandatory dependencies.
- The debug virtual-file responses expose the current content backend type and the content-addressed backend capability so future adapters can be validated without changing the wire protocol.
