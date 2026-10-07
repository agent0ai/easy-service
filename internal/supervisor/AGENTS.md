# Purpose

Own the serialized deployment, cutover, drain and recovery engine.

## Ownership

- `supervisor.go` owns desired-revision polling, preparation cache identity, deployment, memory replacement, watchers, recovery, shutdown and the HTTP listener.
- `settings.go` owns apply classification, immutable candidate plans and policy publication; `settings_test.go` checks stage/apply/restart persistence, rollback, app-only SHA preservation, source/image changes and streaming.
- `control.go` owns command execution through the engine, live status and the private Unix control socket; `control_test.go` checks commands through actual local Git, filesystems, sockets and HTTP with sandbox execution replaced.
- `integration_test.go` uses real local Git/filesystem/HTTP boundaries with owned sandbox execution fakes.
- `soak_test.go` owns opt-in deployment/traffic/resource stress verification.

## Local Contracts

- Keep the healthy active revision serving while a private candidate prepares and becomes ready; publish state before routing changes.
- Deployment, memory replacement and recovery share `Engine.publishWith` for durable state, backend publication and watcher retirement; readiness, restart allowance and drain remain in their existing policy paths.
- CLI actions enter Controller.Run. Redeploy requests immediate selection from the existing polling worker, discards older queued samples and forces ordinary deployment even for the same revision; prepared dependencies remain reusable.
- An unconfigured redeploy reports missing Git/run settings immediately; it must not wait for an absent polling selector.
- Explicit restart reuses the recovery path and current writable installation, consuming the automatic restart allowance. Repeated explicit restarts are allowed; failed restarts enter existing automatic recovery.
- Request cancellation bounds command work; persistent controller/runtime contexts own published watchers and sandboxes. Committed cutover drain still completes.
- Serve status concurrently with actions. Limit control clients to one pending/running action; use a 0700 control directory and 0600 Unix socket under DATA_DIR, recover stale sockets, and refuse to replace live sockets or ordinary files.
- Status reports service state, short instance identity, pending configuration, active revision, runtime image/digest and CPU/RAM usage, limit or sampling error for all live setup/candidate/running/draining instances; keep internal lifecycle bookkeeping out of the status payload.
- Save the configured Git source URL on deployment and recovery so startup cannot reuse a revision from another repository sharing the data directory.
- Preparation identity includes format version 3, full SHA, image digest, setup command and application environment. The version invalidates installations prepared under the previous runtime ownership model. Runtime-assigned ports and run-command-only changes reuse preparation.
- Reject invalid revision selections before filesystem/preparation work.
- Cancel candidate work if the serving instance exits or fails health; never route an exited/cancelled candidate.
- Memory pressure requests same-revision deployment through the same cutover/readiness/drain engine.
- Retire watcher goroutines and stale generations; failed candidate retries cannot starve active recovery.
- Recovery grants one restart of the writable installation, then fresh deployment; health success does not restore the restart allowance.
- Coalesce polled updates without blocking and pace failed retries by the health interval.
- Preserve committed drain work across operation cancellation; stop the old process only after completion or the drain deadline.
- Clean failed/private deployment filesystems and bound retained prepared caches; exclude every live setup filesystem from invalidation and collection until termination is confirmed.
- Retain filesystems if a stop cannot be confirmed; recovery cannot reuse a live installation or consume the restart allowance before stopping succeeds.
- Use `state.Within` for owned filesystem cleanup bounds.
- set/unset durably stage batches without deploying. apply validates the whole candidate, freezes its settings and saves accepted overrides with the revision before routing changes. Failed validation/preparation/readiness/persistence leaves accepted state untouched.
- App variables, commands, image, health path and source-selection changes use the existing deployment/drain flow. App-only updates preserve the accepted SHA and digest; memory/polling/health-timing/timeouts/log policy update in place. Validate new Git credentials against the source.
- Watchers capture their committed settings. Policy publication advances generation without replacing the backend; stale watcher events cannot affect the refreshed generation.
- Stop and join old-source polling before apply; restart it with immutable committed settings and without an immediate unsolicited Git update.
- Register retired instance ownership before routing cutover. kill-draining bypasses the serialized action lock, cancels drain waits and SIGKILLs only registered retirees in parallel; never kill the active instance. Keep failed-stop retirees reachable until exit is confirmed, and retain their logs.
- Instance IDs are twelve random lowercase hexadecimal characters, checked against retained logs and owned live paths. Register metadata before setup/start/restart; discard unused registrations on failure.
- Configure retention only after acceptance; collect on apply and report/retry logging failures. Failed apply cleanup retains accepted filesystem/cache ownership.

## Work Guidance

- Trace revision selection through preparation, readiness, routing, drain and recovery before changing a caller.
- Keep test fakes faithful to production exit broadcasts, lifetimes and private filesystem effects.
- Separate short timeout tests from long stress runs so race-detector scheduling has headroom.

## Verification

- `go test -race ./internal/supervisor -count=20`.
- `TestStreamingDeploymentCutoverAndDrain` checks real SSE continuity through failed durable-state publication, successful cutover and cleanup on client disconnect or drain expiry.
- `TestControlActionsWithLocalGit` checks repeated explicit restarts, surviving watchers, same-revision streaming redeploy, immediate Git updates, rejected candidates and failed-restart recovery. `TestControlSocketOwnershipAndCancellation` checks socket ownership, shutdown and cancelled actions.
- `go test -race ./internal/supervisor -run TestDeploymentSoak -timeout 10m -v -args -soak=3m` exercises sustained traffic, updates, bad/stalled commits, replacement, repeated crashes and resource bounds.
- TestKillAllRetiredInstancesKeepsActiveAndCleansFailedStops and TestControlActionsWithLocalGit verify urgent force-stop while redeploy is blocked by a live SSE stream.
- Real OCI/PRoot execution under default Docker policies remains a separate check.

## Child DOX Index

No child docs.
