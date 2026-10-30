# Purpose

Own persistent active state and reconciliation after supervisor restart.

## Ownership

- `runtime.go` owns root-only UID leases under /run/easy-service/users and their orphan cleanup; workload users cannot forge or release ownership records.
- `sandbox.go` owns protected per-filesystem ownership records, stopped-user reacquisition and filesystem deletion before UID release.
- `state.go` owns state load/save/clear, atomic metadata publication, shared cleanup bounds and stale runtime/filesystem reconciliation.
- The command chooses recovery inputs; the engine owns state transitions.

## Local Contracts

- Publish state/metadata through a fresh private temporary file, write/sync/close, atomic rename, then directory sync; never follow an existing temporary-file symlink.
- Treat persisted state as untrusted input; cleanup paths must stay under the configured data directory.
- Keep revision, digest, Git source URL, image reference, accepted overrides, sandbox basename and deployment-input hash in one CONFIG_DIR/active.json record. Saved sandbox names resolve only beneath DATA_DIR/instances; never persist/trust a process ID for recovery. Older records without sandbox identity take the normal deployment path.
- `Within` owns the shared lexical cleanup-boundary check used by reconciliation and the engine; exclude the root itself, sibling prefixes and paths outside the root.
- Never trust PIDs or UIDs supplied by accepted application state. Use protected runtime UID leases to identify owned orphan processes.
- Leases reserve unique UIDs in the 20000–59999 range before any process starts. Prepared and running sandbox filesystems retain their lease while stopped; release only after processes stop and the private filesystem is deleted. Legacy process-only leases release after stop. Never release a replaced lease.
- Root-owned 0600 .easy-service-owner.json records sit outside each guest rootfs. Reacquire only matching stopped ownership; reject symlinks, writable metadata, changed rootfs ownership and another installation's lease. Reconstruct retained reservations before allocations if /run was lost.
- At startup stop owned orphan processes, preserve only the accepted instance and prepared caches, and remove obsolete instance/runtime/deployment files. Leave other supervisors' leases untouched. Invalid saved filesystem ownership is discarded through bounded cleanup and rebuilt.
- Sync the state directory after clearing the active record.
- Reconcile stale runtime state before a new deployment. Preserve accepted configuration and its recovery identity; collect only owned numeric active/pending/apply temporary files from interrupted writes. Preserve durable apply submissions/results. Store instances, runtime roots and preparation data separately under DATA_DIR.
- Persist the configured Git URL with revision and image identity. `State.Recoverable` permits initial recovery only for the same non-empty source URL and runtime image; older records without source identity require fresh selection.
- Propagate malformed state and cleanup errors to the process owner.

## Work Guidance

- Keep reconciliation policy at this boundary rather than adding caller fallbacks.
- Exercise cleanup and atomic state round trips in temporary owned directories.
- Runtime UID process scans are shared with the sandbox observation/signaling boundary.

## Verification

- `go test -race ./internal/state ./cmd/easy-service` covers retained files/users, missing runtime leases, peer-lease rejection, ownership/path validation and existing state contracts.
- Real helper-process regressions cover orphan UID scopes, peer supervisors and unrelated stale PIDs.

## Child DOX Index

No child docs.
