# Purpose

Own persistent active state and reconciliation after supervisor restart.

## Ownership

- `state.go` owns state load/save/clear, atomic metadata publication, shared cleanup bounds and stale runtime/filesystem reconciliation.
- The command chooses recovery inputs; the engine owns state transitions.

## Local Contracts

- Publish state/metadata by file write, sync and close, atomic rename, then directory sync.
- Treat persisted state as untrusted input; cleanup paths must stay under the configured data directory.
- Keep only revision, digest, Git source URL and image reference in the active record; process IDs and filesystem paths are discovered from live runtimes instead of saved state. Ignore obsolete fields when reading older records.
- `Within` owns the shared lexical cleanup-boundary check used by reconciliation and the engine; exclude the root itself, sibling prefixes and paths outside the root.
- Verify live process ownership before signaling a process group; never trust a PID from older state.
- Discover unrecorded setup/candidate groups once at startup using same-user rootlesskit executables and exact owned runsc root/bundle paths.
- Kill owned orphan groups and wait for observed members to stop before deleting their filesystems; compare process start times to avoid waiting on reused PIDs.
- Sync the state directory after clearing the active record.
- Reconcile stale runtime state before a new deployment.
- Persist the configured Git URL with revision and image identity. `State.Recoverable` permits initial recovery only for the same non-empty source URL and runtime image; older records without source identity require fresh selection.
- Propagate malformed state and cleanup errors to the process owner.

## Work Guidance

- Keep reconciliation policy at this boundary rather than adding caller fallbacks.
- Exercise cleanup and atomic state round trips in temporary owned directories.
- Process scans belong to startup reconciliation, not the periodic sandbox memory sampler.

## Verification

- `go test -race ./internal/state ./cmd/easy-service`.
- Real helper-process regressions cover unrecorded/uncooperative runtimes and unrelated stale PIDs.

## Child DOX Index

No child docs.
