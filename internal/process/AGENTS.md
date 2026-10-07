# Purpose

Own helper subprocess cancellation, descendant cleanup and bounded captured output.

## Ownership

- `users.go` observes real Linux UIDs through /proc/status and stat and signals an entire leased UID through an unprivileged helper in the outer PID namespace.
- `process.go` provides `Command`, `Wait`, `Output` and `Runner`.
- Git, OCI and sandbox helpers share this boundary. UID lease persistence belongs to internal/state.

## Local Contracts

- Commands own a new process group; cancellation kills that group.
- Completion removes remaining descendants; inherited output pipes cannot keep waiting indefinitely.
- Captured combined output is capped at 1 MiB; overflow drains output and fails with truncated diagnostics.
- UID observation includes detached/reparented processes; UID signaling rejects system accounts and never runs kill(-1) as root. Serialize signal helpers so overlapping normal/urgent shutdown cannot kill each other's helpers. They receive a fixed minimal environment.
- Stream application/setup logs through the sandbox log boundary instead of accumulating them here.

## Work Guidance

- Preserve `WaitDelay` and group cleanup when changing subprocess handling.
- Avoid promoted `io.ReaderFrom` methods that could bypass the capture limit.

## Verification

- `go test -race ./internal/process` uses real helper processes for stalled inherited pipes and output flooding. Sandbox/state tests verify real UID monitoring and complete descendant cleanup.

## Child DOX Index

No child docs.
