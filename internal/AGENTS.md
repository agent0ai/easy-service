# Purpose

Own the implementation contracts for revision selection, installation, isolation and HTTP service supervision.

## Ownership

- This file owns package boundaries and verification across the deployment flow.
- Each child owns its implementation and regression tests; the command owns process wiring.
- Flow: configuration defaults/overrides → revision/image → preparation → sandbox launch/output capture → health → atomic config/state publication → proxy cutover → drain → recovery.
- Pending settings and retained logs remain independent of sandbox and cache cleanup.

## Local Contracts

- Use the serialized deployment engine for revision updates, memory replacement, CLI actions and recovery.
- Preserve exact revision/image identity and private writable filesystem ownership throughout the flow.
- Owned fakes must preserve the production boundary's lifetime, errors, filesystem effects and completion semantics.

## Work Guidance

- Fix shared defects at the owning package and verify both a regression and the affected integration path.
- Use standard library and native platform behavior before introducing dependencies or duplicated policy.

## Verification

- `go vet ./...` and `go test -race ./...` run from the repository root.
- Integration, optional real OCI and load checks are owned by their child packages.

## Child DOX Index

- [config/AGENTS.md](config/AGENTS.md): public defaults, environment validation and durable staging.
- [revision/AGENTS.md](revision/AGENTS.md): Git/GitHub selection and exact checkouts.
- [logs/AGENTS.md](logs/AGENTS.md): daily instance output files, rotation, retention and writer lifetime.
- [image/AGENTS.md](image/AGENTS.md): digest resolution, OCI copy/unpack and image cache.
- [process/AGENTS.md](process/AGENTS.md): bounded helper execution and process groups.
- [sandbox/AGENTS.md](sandbox/AGENTS.md): PRoot/Linux UID ownership, filesystems and CPU/RAM observation.
- [health/AGENTS.md](health/AGENTS.md): readiness and liveness probes.
- [proxy/AGENTS.md](proxy/AGENTS.md): HTTP transport, streaming, upgrades and request drain.
- [state/AGENTS.md](state/AGENTS.md): atomic accepted configuration/state, shared cleanup bounds and startup reconciliation.
- [supervisor/AGENTS.md](supervisor/AGENTS.md): deployment, cutover, recovery, private control/status and integration/load checks.
