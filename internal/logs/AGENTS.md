# Purpose

Capture and retain application output independently of sandbox filesystems.

## Ownership

- `logs.go` owns daily pages, instance frontmatter, size/age collection and concurrent writers.
- The supervisor registers instance identity; sandbox output owns writer lifetime.

## Local Contracts

- Store only owned regular log files in LOG_DIR; never follow symlinks or remove unrelated files.
- Keep retired and failed instance logs until collection. Include frontmatter in size accounting.
- Rotate at UTC day boundaries and the configured file limit; collect oldest pages by age and total size.
- File logging failures must be reported to the console without stopping application output capture or the service.
- Serialize writers and policy updates; bound disk use during writes as well as periodic collection.

## Work Guidance

- Use the standard library. Keep instance metadata immutable and omit environment values and credentials.

## Verification

- `go test -race ./internal/logs` checks rotation, retention, concurrent writers, restart discovery and filesystem failures.

## Child DOX Index

No child docs.
