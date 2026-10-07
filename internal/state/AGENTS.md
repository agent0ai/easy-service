# Purpose

Own persistent active state and reconciliation after supervisor restart.

## Ownership

- `runtime.go` owns root-only UID leases under /run/easy-service/users and their orphan cleanup; workload users cannot forge or release ownership records.
- `state.go` owns state load/save/clear, atomic metadata publication, shared cleanup bounds and stale runtime/filesystem reconciliation.
- The command chooses recovery inputs; the engine owns state transitions.

## Local Contracts

- Publish state/metadata through a fresh private temporary file, write/sync/close, atomic rename, then directory sync; never follow an existing temporary-file symlink.
- Treat persisted state as untrusted input; cleanup paths must stay under the configured data directory.
- Keep revision, digest, Git source URL, image reference and accepted overrides in one CONFIG_DIR/active.json record; process IDs and filesystem paths are discovered from live runtimes instead of saved state. Ignore obsolete fields when reading older records.
- `Within` owns the shared lexical cleanup-boundary check used by reconciliation and the engine; exclude the root itself, sibling prefixes and paths outside the root.
- Never trust PIDs or UIDs supplied by accepted application state. Use protected runtime UID leases to identify owned orphan processes.
- Leases reserve unique UIDs in the 20000–59999 range (within Docker's usual 65536-UID mapping) before any process starts, persist the exact DATA_DIR/runtime instance root, and are released only after all live UID processes stop.
- At startup kill every live process in leases belonging to this DATA_DIR, including detached/reparented children, before deleting instances/runtime/deployments; leave other supervisors' leases untouched.
- Sync the state directory after clearing the active record.
- Reconcile stale runtime state before a new deployment. Preserve accepted configuration and its recovery identity; collect only owned numeric active/pending temporary files from interrupted writes. Store instances, runtime roots and preparation data separately under DATA_DIR.
- Persist the configured Git URL with revision and image identity. `State.Recoverable` permits initial recovery only for the same non-empty source URL and runtime image; older records without source identity require fresh selection.
- Propagate malformed state and cleanup errors to the process owner.

## Work Guidance

- Keep reconciliation policy at this boundary rather than adding caller fallbacks.
- Exercise cleanup and atomic state round trips in temporary owned directories.
- Runtime UID process scans are shared with the sandbox observation/signaling boundary.

## Verification

- `go test -race ./internal/state ./cmd/easy-service`.
- Real helper-process regressions cover orphan UID scopes, peer supervisors and unrelated stale PIDs.

## Child DOX Index

No child docs.
