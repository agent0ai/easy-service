# Purpose

Own the supervisor executable and its process lifetime.

## Ownership

- `easy-service/main.go` wires configuration, prerequisite validation, persisted state, image preparation, controller, proxy and shutdown.
- `easy-service/cli.go` implements the same binary's `status`, `redeploy`, `restart` and help commands through the supervisor's Unix socket; CLI dispatch precedes configuration loading and sandbox validation.
- `runtimeAdapter` bridges the sandbox implementation to the supervisor runtime contract.
- `easy-service/real_test.go` owns the opt-in lifecycle check with actual Git, OCI tooling and a pinned BusyBox image, direct rootless gVisor, persistent setup writes/Docker DNS, overlapping revision ports, readiness, routing, bad-candidate rejection, memory sampling and orphan reconciliation.
- Lifecycle policy belongs to `internal/supervisor`; isolation belongs to `internal/sandbox`.

## Local Contracts

- Fail startup if rootless gVisor prerequisites or restart reconciliation fail.
- Runtime adapters pass commands/environment to direct runsc; sandbox owns PORT allocation and shared-network endpoints.
- Keep runtime process lifetime separate from polling/signal cancellation so HTTP drain finishes before sandbox termination.
- Propagate fatal owner errors to the process; keep transient image/revision failures retryable through their owning subsystem.
- Use `State.Recoverable` for prior revision recovery; both configured Git source and runtime image must match. Cached image recovery depends only on the runtime image.
- Bind the private control socket before reconciliation or image preparation, refusing a second live supervisor for the same DATA_DIR before it mutates owned state. Status remains available while images prepare; actions enter the controller once initialization completes.
- Actions use the existing serialized controller; CLI requests do not own sandbox or watcher lifetime.
- The CLI uses DATA_DIR (default /data), reports action failures with a nonzero exit status, and prints status without exposing Git credentials or application environment.

## Work Guidance

- Trace main, HTTP shutdown and engine shutdown together when changing cancellation.
- Do not add caller-specific recovery or isolation fallbacks.

## Verification

- `go test -race ./cmd/easy-service` checks reconciliation error propagation.
- `TestCLICommandsOverUnixSocket` checks CLI command methods, readable status, argument validation, action failures and missing-supervisor errors without requiring deployment configuration.
- `TestExistingControlOwnerPreventsReconciliation` checks that a second supervisor cannot reach state reconciliation or replace the existing live control socket.
- `make check` covers wiring and cross-package contracts.
- As an unprivileged user with the documented tools/host prerequisites: `go test ./cmd/easy-service -run TestRealGVisorLifecycle -timeout 5m -v -args -real-gvisor`.
- A production startup check requires the host prerequisites in `../README.md`.

## Child DOX Index

No child docs. This contract covers `easy-service/`.
