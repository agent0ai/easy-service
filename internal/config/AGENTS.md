# Purpose

Parse and validate the public environment configuration.

## Ownership

- `config.go` owns defaults, input validation and deterministic `APP_*` forwarding.
- `../../README.md` owns the user-facing configuration table; update both together.

## Local Contracts

- Keep the documented environment-variable surface exact; add no hidden public options.
- Require GIT_URL and RUN_COMMAND; RUNTIME_IMAGE defaults to `debian:bookworm-slim` when unset or empty. Preserve explicit image names for native Skopeo parsing.
- Require credential-free GitHub HTTPS URLs or existing local Git directories; resolve local paths to absolute paths and reject local release mode.
- Accept only positive, representable durations of at least one nanosecond.
- Validate positive ports/failure counts and memory-size overflow; empty/zero memory limits disable observation.
- Pass only explicitly selected workload values; Git credentials stay in the supervisor.

## Work Guidance

- Validate at this boundary instead of coercing invalid inputs in callers.
- Environment tests must use `t.Setenv` rather than clearing unrelated process environment.

## Verification

- `go test -race ./internal/config`.
- `./scripts/contract-test.sh` checks the documented configuration surface.

## Child DOX Index

No child docs.
