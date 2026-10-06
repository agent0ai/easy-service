# Purpose

Own the supervisor executable and its process lifetime.

## Ownership

- `easy-service/main.go` wires configuration, prerequisite validation, persisted state, image preparation, controller, proxy and shutdown.
- `easy-service/cli.go` implements the same binary's `status`, `redeploy`, `restart`, `kill-draining`, `config` and help commands through the supervisor's Unix socket; CLI dispatch precedes configuration loading and sandbox validation.
- `runtimeAdapter` bridges the sandbox implementation to the supervisor runtime contract.
- `easy-service/real_test.go` owns opt-in checks with actual Git/OCI/direct rootless gVisor: BusyBox lifecycle, HOME/tmp setup persistence above 64 MiB, Docker DNS, overlapping ports, bad candidates, memory/orphan cleanup, and an ordinary Go-image build with image PATH and explicit HOME.
- Lifecycle policy belongs to `internal/supervisor`; isolation belongs to `internal/sandbox`.

## Local Contracts

- Fail startup if rootless gVisor prerequisites or restart reconciliation fail.
- Runtime adapters pass commands/environment to direct runsc; sandbox owns PORT allocation and shared-network endpoints.
- Keep runtime process lifetime separate from polling/signal cancellation. On shutdown, stop accepting HTTP requests while the engine drains/stops instances; engine completion bounds frontend shutdown, including slow clients. Honor configured drain and bounded sandbox-stop time.
- Propagate fatal owner errors to the process; keep transient image/revision failures retryable through their owning subsystem.
- Use `State.Recoverable` for prior revision recovery; both configured Git source and runtime image must match. Cached image recovery depends only on the runtime image.
- Supply immutable candidate Git/image/runtime settings through Controller.Configure. Resolve images lazily through the controller so initialization failures keep show/set/apply available. Reuse the current digest on app-only applies and keep offline cache fallback at initial recovery. Collect stale caches only after usable selection; log cleanup failures.
- Bind the private control socket before reconciliation or image preparation, refusing a second live supervisor for the same DATA_DIR before it mutates owned state. Status/show remain available during candidate work; initialization and actions run in the controller.
- Actions use the existing serialized controller; CLI requests do not own sandbox or watcher lifetime.
- The CLI uses DATA_DIR (default /data), reports action failures with a nonzero exit status, and prints status without exposing Git credentials or application environment. Explicit config show prints managed values; set requires NAME=value batches and rejects malformed batches before sending them, unset restores defaults, and apply activates pending changes.
- Show and successful set output sorted NAME=value lines without headings, quoting/escaping whitespace, quotes and backslashes to keep each value on one physical line. Set without arguments reads that format from stdin, decodes quoted values without shell expansion, accepts blank lines/CRLF and rejects malformed, duplicate, unreadable or oversized input before sending the batch. Argument values remain literal after normal shell parsing.
- Boot first with Docker defaults plus accepted CONFIG_DIR overrides; load only directory defaults before saved overrides so an overridden invalid Docker value cannot block startup. Missing Git/run settings keep the CLI available.
- Create /data, /config and /logs as UID 10000-owned writable directories in the image; require no Dockerfile VOLUME or default Compose volume mapping.
- Wire one log manager to setup/running instance output; lifecycle events log names/identity, never configuration values.

## Work Guidance

- Trace main, HTTP shutdown and engine shutdown together when changing cancellation.
- Do not add caller-specific recovery or isolation fallbacks.

## Verification

- `go test -race ./cmd/easy-service` checks reconciliation error propagation.
- `TestCLICommandsOverUnixSocket` checks CLI command methods, readable status, argument validation, action failures and missing-supervisor errors without requiring deployment configuration.
- `TestConfigTextRoundTrip` checks exported values through stdin import and durable staging over a real control socket, including quoting, whitespace, newlines and literal shell syntax.
- `TestExistingControlOwnerPreventsReconciliation` checks that a second supervisor cannot reach state reconciliation or replace the existing live control socket.
- `make check` covers wiring and cross-package contracts.
- As an unprivileged user with the documented tools/host prerequisites: `go test ./cmd/easy-service -run 'TestReal(GVisorLifecycle|GoImageEnvironmentAndSetup)' -timeout 10m -v -args -real-gvisor`.
- TestRealConfigurationCLIAndRestart uses a built EASY_SERVICE_REAL_BINARY with port-80 access to exercise empty-container CLI setup, real gVisor app output, pending/accepted restart persistence, bad candidates, urgent retirement kill and live log-policy trim.
- A production startup check requires the host prerequisites in `../README.md`.

## Child DOX Index

No child docs. This contract covers `easy-service/`.
