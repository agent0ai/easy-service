# Purpose

Own lightweight repository contract checks.

## Ownership

- `contract-test.sh` checks README/configuration, Compose defaults and absence of a Docker-socket dependency.
- The root Makefile runs the script after Go formatting, vet and race tests.

## Local Contracts

- Keep the script runnable with POSIX `sh` and standard tools.
- Check durable public contracts rather than duplicating implementation tests.
- Update configuration checks with `internal/config` and README changes.

## Work Guidance

- Keep checks small and fail with an actionable message.

## Verification

- `./scripts/contract-test.sh`.
- `make check` runs the complete existing workflow.

## Child DOX Index

No child docs.

