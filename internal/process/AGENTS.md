# Purpose

Own helper subprocess cancellation, descendant cleanup and bounded captured output.

## Ownership

- `process.go` provides `Command`, `Wait`, `Output` and `Runner`.
- Git, OCI and sandbox helpers share this boundary.

## Local Contracts

- Commands own a new process group; cancellation kills that group.
- Completion removes remaining descendants; inherited output pipes cannot keep waiting indefinitely.
- Captured combined output is capped at 1 MiB; overflow drains output and fails with truncated diagnostics.
- Stream application/setup logs through the sandbox log boundary instead of accumulating them here.

## Work Guidance

- Preserve `WaitDelay` and group cleanup when changing subprocess handling.
- Avoid promoted `io.ReaderFrom` methods that could bypass the capture limit.

## Verification

- `go test -race ./internal/process` uses real helper processes for stalled inherited pipes and output flooding.

## Child DOX Index

No child docs.

