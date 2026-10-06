# DOX framework

- DOX is highly performant AGENTS.md hierarchy installed here
- Agent must follow DOX instructions across any edits

## Core Contract

- AGENTS.md files are binding work contracts for their subtrees
- Work products, source materials, instructions, records, assets, and durable docs must stay understandable from the nearest applicable AGENTS.md plus every parent AGENTS.md above it

## Read Before Editing

1. Read the root AGENTS.md
2. Identify every file or folder you expect to touch
3. Walk from the repository root to each target path
4. Read every AGENTS.md found along each route
5. If a parent AGENTS.md lists a child AGENTS.md whose scope contains the path, read that child and continue from there
6. Use the nearest AGENTS.md as the local contract and parent docs for repo-wide rules
7. If docs conflict, the closer doc controls local work details, but no child doc may weaken DOX

Do not rely on memory. Re-read the applicable DOX chain in the current session before editing.

## Update After Editing

Every meaningful change requires a DOX pass before the task is done.

Update the closest owning AGENTS.md when a change affects:

- purpose, scope, ownership, or responsibilities
- durable structure, contracts, workflows, or operating rules
- required inputs, outputs, permissions, constraints, side effects, or artifacts
- user preferences about behavior, communication, process, organization, or quality
- AGENTS.md creation, deletion, move, rename, or index contents

Update parent docs when parent-level structure, ownership, workflow, or child index changes. Update child docs when parent changes alter local rules. Remove stale or contradictory text immediately. Small edits that do not change behavior or contracts may leave docs unchanged, but the DOX pass still must happen.

## Hierarchy

- Root AGENTS.md is the DOX rail: project-wide instructions, global preferences, durable workflow rules, and the top-level Child DOX Index
- Child AGENTS.md files own domain-specific instructions and their own Child DOX Index
- Each parent explains what its direct children cover and what stays owned by the parent
- The closer a doc is to the work, the more specific and practical it must be

## Child Doc Shape

- Create a child AGENTS.md when a folder becomes a durable boundary with its own purpose, rules, responsibilities, workflow, materials, or quality standards
- Work Guidance must reflect the current standards of the project or user instructions; if there are no specific standards or instructions yet, leave it empty
- Verification must reflect an existing check; if no verification framework exists yet, leave it empty and update it when one exists

Default section order:
- Purpose
- Ownership
- Local Contracts
- Work Guidance
- Verification
- Child DOX Index

## Style

- Keep docs concise, current, and operational
- Document stable contracts, not diary entries
- Put broad rules in parent docs and concrete details in child docs
- Prefer direct bullets with explicit names
- Do not duplicate rules across many files unless each scope needs a local version
- Delete stale notes instead of explaining history
- Trim obvious statements, repeated rules, misplaced detail, and warnings for risks that no longer exist

## Closeout

1. Re-check changed paths against the DOX chain
2. Update nearest owning docs and any affected parents or children
3. Refresh every affected Child DOX Index
4. Remove stale or contradictory text
5. Run existing verification when relevant
6. Report any docs intentionally left unchanged and why

## User Preferences

- Keep the core lean, simple and performant; prefer standard library/native features over added dependencies.
- Verify reliability with bad revisions, stalls, crashes, resource pressure and sustained traffic; document material verification limits.
- Support local Git directories for quick integration tests and normal HTTP streaming, SSE and WebSocket traffic.
- Fix the shared owner of a defect and verify its regression plus the affected end-to-end path; do not hide shared defects with caller coercions, retries, fallbacks or duplicated logic.
- Keep this DOX hierarchy current with meaningful changes.
- Runtime configuration uses staged `config set`/`unset`, full or filtered `config show`, and explicit `config apply`; Docker environment values are defaults beneath durable overrides.
- Retain instance stdout/stderr independently of sandbox cleanup, in daily paginated logs with configurable age, file and total size limits. Keep config, logs and caches in separate folders without mandatory volume mappings.
- Keep README.md simple: lead with the five basic launch settings, show the container CLI, then list optional settings.
- Launch gVisor directly as 8020 does, sharing the outer container's network with an automatically assigned PORT per instance; do not add RootlessKit/slirp4netns, TUN access or subordinate UID/GID setup.

## Child DOX Index

- [cmd/AGENTS.md](cmd/AGENTS.md): executable wiring, container CLI, runtime adapter and process shutdown.
- [internal/AGENTS.md](internal/AGENTS.md): package boundaries, deployment flow and ten package-specific child contracts.
- [scripts/AGENTS.md](scripts/AGENTS.md): repository contract checks.
- [.github/AGENTS.md](.github/AGENTS.md): tagged multi-platform Docker publication.

The root owns README.md, Dockerfile, compose.yaml, Makefile, go.mod, .gitignore and DOX-LICENSE. Child docs own their named subtrees.

## Project Context and Verification

- easy-service supervises stateless HTTP workloads selected from Git, installed in rootless gVisor sandboxes and exposed on port 80.
- README.md is the public configuration, deployment and lifecycle reference. Keep it aligned with implementation.
- Run `make check` with Go 1.23 or later: formatting, vet, race tests and `scripts/contract-test.sh`.
- Cross-package tests may fake owned external boundaries; real OCI/gVisor and Docker build checks require their documented tools and host prerequisites.
- Docker runtime packaging retains runsc and its companion helpers while excluding the unused containerd shim; this service launches runsc directly.
- The same Docker binary starts the supervisor without arguments and provides `status`, `redeploy`, `restart`, `kill-draining`, `config` and `help` subcommands. Control stays on a private Unix socket under DATA_DIR.
- Runtime images default to `debian:bookworm-slim`; use Skopeo's native Docker image-name handling for shorthand names.
- Preserve image environment defaults and explicit APP_* overrides, reserving only PORT; HOME defaults to /root. Setup writes in HOME and /tmp stay on the private writable filesystem.
- Accepted configuration and revision/image identity share one atomic record in CONFIG_DIR; pending changes never activate on restart. An unconfigured container keeps its control CLI available.
- Apply through immutable candidate settings and the existing readiness/cutover/drain engine. Supervisor policy changes avoid app replacement. Urgent kill-draining bypasses a blocked drain and may force-stop only registered retired instances.
- Logs live in LOG_DIR and survive instance cleanup, defaulting to thirty days, 10 MiB pages and 1 GiB combined. Short random instance IDs remain unique against live runtimes and retained logs.
- Discard source archives and stale image/Git caches only after usable selection or return to the previously accepted deployment; preserve offline recovery inputs.
- Compose grants ninety seconds for outer-container shutdown so the supervisor can finish request drain and sandbox cleanup.
- Tagged Docker publication requires GitHub Actions secrets `DOCKERHUB_ORG` and `DOCKERHUB_OAT_TOKEN`; Git tags are preserved exactly and no `latest` tag is published.
- Follow the nearest child verification instructions for local edits. Documentation-only changes require link/index and source-contract review.

## GitHub Authentication

- For an explicitly requested authenticated GitHub operation, use an already exported non-empty GITHUB_TOKEN first.
- Otherwise check applicable .env files in this repository and its enclosing project/workspace, importing only GITHUB_TOKEN for that operation without modifying those files.
- Never display, log, diff, commit or otherwise expose .env contents or token values.
- Never put tokens in remote URLs, command-line arguments or persistent Git configuration; use scoped non-persistent credentials.

# Engineering contract

Trace defects through the complete revision-selection, preparation, sandbox,
health, routing, drain, and recovery flow. Fix behavior in the subsystem that
owns its contract; callers may not weaken gVisor isolation, duplicate recovery
policy, or leak supervisor credentials/environment into workloads. Keep the
public environment-variable surface exactly aligned with README.md and
internal/config. Production code must not add a host Docker-socket dependency.

Memory pressure is an approximately one-second soft observation of the active
runsc process tree. It must enter the serialized deployment engine
as a same-revision blue/green replacement request; it may not create a second
cutover, readiness, drain, or recovery state machine.

Tests may replace process, image, GitHub, clock, and sandbox boundaries with
owned fakes. A fake must preserve the production contract it stands in for.

## DOX Source

The framework above is adapted from [agent0ai/dox](https://github.com/agent0ai/dox/tree/765ae4ac02cc884eefcd41a3d0f71941721adb89), commit `765ae4ac02cc884eefcd41a3d0f71941721adb89`. Its license is preserved in [DOX-LICENSE](DOX-LICENSE).
