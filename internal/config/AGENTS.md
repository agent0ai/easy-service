# Purpose

Parse and validate the public environment configuration.

## Ownership

- `config.go` owns the exact Defaults map, pure parsing, validation and deterministic `APP_*` forwarding.
- `settings.go` owns Docker defaults beneath accepted/pending overrides, batch staging, show values and change detection. Accepted records are published by the engine through `state.Store`.
- `../../README.md` owns the user-facing configuration table; update both together.

## Local Contracts

- Keep the documented environment-variable surface exact; add no hidden public options.
- Require GIT_URL and RUN_COMMAND on apply; allow an unconfigured supervisor to wait for CLI input at startup; RUNTIME_IMAGE defaults to `debian:bookworm-slim` when unset or empty. Preserve explicit image names for native Skopeo parsing.
- Require credential-free GitHub HTTPS URLs or existing local Git directories; resolve local paths to absolute paths and reject local release mode.
- Accept only positive, representable durations of at least one nanosecond.
- Validate positive failure counts and memory-size overflow; empty/zero memory limits disable observation. Runtime owns automatic PORT assignment; expose no fixed SERVICE_PORT option.
- Pass only explicitly selected workload values; Git credentials stay in the supervisor.
- Store pending overrides atomically in CONFIG_DIR/pending.json with mode 0600; set/unset never alters accepted settings. Unset restores Docker/built-in defaults. Stage incomplete values, then validate the full candidate on apply.
- Persist explicit overrides even when their effective value equals the Docker default. Pending changes survive restart but do not activate automatically.
- Show all managed variables and explicit APP_* values, or a requested subset, through the private control socket; values are intentionally visible to the authorized console user. Configuration events log names only.
- DATA_DIR, CONFIG_DIR and LOG_DIR are separate absolute boot-time locations selected by Docker; reject live location changes.
- Default logs to thirty days, 10M pages and 1G total; require positive retention, file sizes at least 1K and total size at least the file size. Bound size/duration arithmetic.
- Limit saved override batches to 1 MiB and reject unknown names, invalid APP_* names and NUL bytes.

## Work Guidance

- Validate at this boundary instead of coercing invalid inputs in callers.
- Environment tests must use `t.Setenv` rather than clearing unrelated process environment.

## Verification

- `go test -race ./internal/config`.
- `./scripts/contract-test.sh` checks the documented configuration surface.

## Child DOX Index

No child docs.
