# easy-service

Run a service from Git. Easy Service installs it, checks that it is healthy,
keeps it running, and deploys new commits automatically.

## Launch

From this repository, edit `compose.yaml` for your application:

```yaml
services:
  easy-service:
    build: .
    restart: unless-stopped
    stop_grace_period: 90s
    ports: ["8080:80"]
    environment:
      GIT_URL: https://github.com/you/your-service.git
      GIT_TOKEN: ${GIT_TOKEN:-}
      RUNTIME_IMAGE: node:22-bookworm-slim
      SETUP_COMMAND: npm ci
      RUN_COMMAND: node server.js
```

For a private repository, set `GIT_TOKEN` in your shell or Portainer environment.
Leave it empty for a public repository. Then run:

```sh
docker compose up -d --build
```

Your service is available at **http://localhost:8080**.

Only `GIT_URL` and `RUN_COMMAND` are required. `RUNTIME_IMAGE` defaults to
`debian:bookworm-slim`; choose an image with your language installed, such as
`node:22-bookworm-slim` or `python:3.12-slim`. Short image names work, and you can
also use full registry names or pinned digests. `SETUP_COMMAND` installs your
application's dependencies and can be omitted if none are needed.
Without setup, easy-service caches the checkout alone and copies the runtime
image directly into each fresh instance.

Commands run in `/app` using `/bin/sh`. Keep the server running in the foreground
and make it listen on `$PORT`, assigned automatically for each instance. Pass
application settings as `APP_NAME=value`; the application receives `NAME=value`.
Image environment defaults are preserved; `APP_*` overrides them, including
`PATH` and `HOME`. `PORT` is always assigned by the supervisor. If the image has
no `HOME`, it defaults to `/root`. Git credentials stay in the supervisor.
Setup has a 15-minute timeout by default. Files installed under `$HOME` or `/tmp` survive
into the running instance; both use its private writable filesystem.

## Inspect and control

```sh
docker compose exec easy-service easy-service status
docker compose exec easy-service easy-service redeploy
docker compose exec easy-service easy-service restart
docker compose exec easy-service easy-service kill-draining
docker compose logs -f easy-service
```

- `status` shows the revision, image, health state, CPU and RAM for each instance.
  It shows `not running` when no instance is active and `unavailable` if sampling
  fails. Measurements include detached child processes. CPU is a sampled percentage; 100% means one fully used CPU core.
- `redeploy` checks Git immediately and creates a fresh writable installation,
  even if the revision has not changed. It reuses the prepared dependencies.
- `restart` restarts the current revision with its existing writable files and
  reserved Linux user, without copying or recursively changing file ownership.
  This briefly interrupts service.
- `kill-draining` immediately force-stops all retired instances, even while
  `apply` or `redeploy` waits for them to drain. It leaves the current instance
  running. Retired logs remain subject to the normal retention rules.

Inside the container, use `easy-service status`, `easy-service redeploy`, or
`easy-service restart` directly. `restart` and `redeploy` wait for completion
and report failures.
During startup, status shows `waiting`. The CLI uses a private Unix socket;
there is no extra network port to expose.

## Change settings

In the container's console:

```sh
easy-service config show
easy-service config show APP_A0_API_URL APP_VENICE_API_KEY
easy-service config set APP_A0_API_URL=https://api.example.com APP_VENICE_API_KEY=new-key
easy-service config set SERVICE_MEMORY_LIMIT=768M HEALTH_FAILURES=5
easy-service config apply
```

`show` prints all pending settings, or only the names you list, as sorted
`NAME=value` lines without headings. Values containing whitespace, quotes or
backslashes are quoted and escaped so each stays on one line. Successful `set`
prints the changed settings in the same format. `set` accepts `NAME=value` arguments and
saves the whole batch without changing the running service. Set several batches,
then run `apply` once. `config unset NAME...` removes saved overrides and restores
the values supplied by Docker, or the built-in defaults.

To copy settings, paste the output from `show` into `set` on the other container:

```sh
easy-service config set <<'EOF'
APP_API_KEY=your-key
RUN_COMMAND="exec ./bin/gateway -env ''"
EOF
easy-service config apply
```

Without arguments, `set` reads one assignment per line until end of input.
It restores escaped values exactly and never expands shell expressions. You can
also pipe `config show` into `config set`. When copying all settings, directory
values must match the target container's Docker configuration.

`apply` validates and durably submits the current staged settings, prints an
apply ID, and returns immediately. Use `easy-service status` to see running and
queued apply IDs and the last result/error. For scripts that need the final
result, use `easy-service config apply --wait`; it waits through deployment and
drain and exits nonzero on failure or if a newer submission replaces it.
Closing the shell or interrupting a waiter does not cancel submitted work.

Only one apply runs at a time. While it runs, you can keep using `set`, `unset`
and `apply`. There is one pending snapshot: each newer apply replaces it, so
all settings submitted so far deploy together after the current deployment
finishes draining. Repeated identical submissions share an ID. Edits made after
the last apply stay staged until you explicitly submit them.

App variables, commands, runtime image and Git selection changes start a
candidate, check readiness, switch traffic, then drain the old instance. App-variable changes keep the current commit and
image digest. Memory, polling, health-check timing, timeout and log-retention
changes update the supervisor without replacing the app. Git credentials are
checked against the configured source. Failed applies keep the accepted settings
and healthy instance; pending changes remain available to correct and retry.
Changing app variables reruns setup because setup receives those variables too.

Settings survive stopping and restarting the same container. Docker environment
values are defaults; saved overrides take precedence. Staged settings also
survive restart. The supervisor first restores the accepted service, then
resumes the newest unfinished apply submission. Unsubmitted edits never deploy
automatically. Failed applies remain failed until you submit again; if recording
the result fails, status reports the storage problem and a restart can revisit
the unfinished submission. A container without `GIT_URL` or `RUN_COMMAND` stays
running in `waiting` so you can configure it using these commands.

The supervisor prints configuration changes, setup, startup, readiness, cutover,
drain, exits, recovery and log collection to the console. Configuration events
list variable names without printing their values. `status` also reports the
short instance ID, staged-change indicator and apply progress.
Runtime startup errors also appear in the console and retained instance logs.

## Additional settings

All of these are optional. Times are in seconds.

| Variable | Default | What it does |
|---|---|---|
| `SERVICE_MEMORY_LIMIT` | `512M` | Replace the running service when its memory exceeds this soft threshold. Set `0` or empty to disable. |
| `GIT_BRANCH` | `main` | Branch to watch in `commit` and `tag` modes. Ignored in GitHub `release` mode. |
| `UPDATE_METHOD` | `commit` | Deploy by `commit`, `tag`, or GitHub `release`. |
| `UPDATE_PATTERN` | `*` | Tag/release name filter, using `*` and `?`. |
| `POLL_INTERVAL` | `300` | How often to check Git. |
| `HEALTH_PATH` | `/` | URL path that must return a 2xx response. |
| `STARTUP_TIMEOUT` | `60` | How long to wait for a new service to become healthy after setup. |
| `HEALTH_INTERVAL` | `10` | Time between running health checks and recovery retries. Startup retries are capped at 250 ms. |
| `HEALTH_FAILURES` | `3` | Consecutive failed checks before recovery. |
| `SETUP_TIMEOUT` | `900` | Maximum setup time. |
| `DRAIN_TIMEOUT` | `30` | Time allowed for existing requests and streams to finish before stopping an old instance. |
| `DATA_DIR` | `/data` | Where the supervisor stores its Git, image, and installation cache. |
| `SANDBOX_MOUNTS` | empty | Comma-separated data directories to expose to the running app, as `/path` or `/source:/destination`. |
| `CONFIG_DIR` | `/config` | Accepted settings and pending overrides. |
| `LOG_DIR` | `/logs` | Application stdout and stderr from all instances. |
| `LOG_RETENTION_DAYS` | `30` | Days of application logs to retain. |
| `LOG_MAX_FILE_SIZE` | `10M` | Maximum size of each daily log page, including frontmatter. |
| `LOG_MAX_TOTAL_SIZE` | `1G` | Maximum combined size of retained application log pages. |

**Upgrading from v0.3.0 or earlier:** Tag mode no longer watches tags across all branches.
Set `GIT_BRANCH` to the branch you deploy. Only matching tags on its tip or ancestors
are eligible, newest creator time first (descending tag name breaks ties). If the
branch is missing or no reachable tag matches, no new revision is selected.

Memory sizes accept bytes or `K`, `M`, `G`, and `T` suffixes (1024-based).
Log sizes use the same suffixes. The file limit must be at least `1K`, and the
total limit must be at least the file limit. Retention days must be positive.
The memory threshold is checked about once per second and includes the whole
instance, including detached children. It triggers a normal deployment rather than enforcing a hard cap.

Container dashboards may include reclaimable file cache from copying images.
`easy-service status` reports process RAM. The host kernel reclaims cache when
memory is needed; Easy Service does not routinely flush it.

`GIT_URL` can also be a local Git directory mounted into the container and
readable by the supervisor. Only committed files are deployed; local repositories
support commit and tag updates.

## Logs and persistence

App output goes to the console and files such as
`/logs/a1b2c3d4e5f6-2026-10-06-000001.log`. Each file starts with frontmatter
containing the instance ID, full commit, Git source, runtime image/digest,
deployment start time and UTC day. Output identifies setup/run and stdout/stderr.
Files rotate at midnight UTC or the size limit. Logs from stopped and failed
instances remain until collection removes them.

The collector runs every minute and on apply. It removes expired pages and the
oldest pages needed to stay under the total limit; writes enforce the size limit
too. Reducing the file limit also removes existing oversized pages. File logging
errors appear on the console and are retried without stopping the application.

No volume mapping is required. `/config`, `/logs` and `/data` are separate folders
inside the container. To retain them when **recreating or removing** the container,
add whichever mappings you need to your stack:

```yaml
volumes:
  - easy-service-config:/config
  - easy-service-logs:/logs
  - easy-service-cache:/data
```

Declare those named volumes at the top level of the stack. Host bind directories
must be writable by root. Directory locations are chosen through Docker's
`DATA_DIR`, `CONFIG_DIR` and `LOG_DIR` settings when the container starts;
the other settings can be changed through the CLI.

### Application data

Mount storage into the outer container with Docker, then opt in to exposing
each directory to the app. For example, add these entries to the easy-service
service in Compose.

```yaml
volumes:
  - ./database:/mounted-db
  - ./uploads:/srv/uploads
environment:
  SANDBOX_MOUNTS: /mounted-db:/var/lib/myapp,/srv/uploads
  APP_DB_PATH: /var/lib/myapp/app.db
```

The source is an existing absolute directory inside the outer container.
A single path uses the same location inside the sandbox. Use clean directory
paths without symlinks, and distinct, non-overlapping destinations. Supervisor
state and runtime paths are protected. Setup commands use the private image
filesystem. Apply changes with `config set SANDBOX_MOUNTS=...` and
`config apply --wait`.

Prepare each source directory with numeric group `19000` and mode `2770`, for
example `sudo chgrp 19000 ./database ./uploads` and
`sudo chmod 2770 ./database ./uploads`. Parent directories inside the container
must permit traversal. App instances with mounts receive group `19000` and umask
`0002`. Setgid directories preserve the group for new files. Easy-service
leaves the permissions and ownership of mounted contents to you.

For SQLite, mount the entire directory. Create a new database file with group
`19000` and mode `0660` before the app opens it, or grant the same access to the
existing database and sidecar files. Use local storage with SQLite-compatible
locking. The candidate and draining instance can use the same database during
deployment. Keep schema changes compatible with both versions during that
overlap, and take backups independently of deployment.

## What to expect

A healthy update starts the new version before switching traffic. Failed
candidates leave the healthy version running. Existing requests, SSE streams,
and WebSockets have up to `DRAIN_TIMEOUT` seconds (30 by default) to finish on
the old version. Increase Docker's stop grace period when increasing this timeout.

Crashes and failed health checks trigger one restart, followed by a fresh
installation on another failure. Recovery keeps retrying the accepted revision;
publish a fix or revert in Git if it is faulty. Pending repairs can finish even
while the accepted revision repeatedly crashes. Writes to the private
filesystem are temporary across redeployments. Store durable app data in
`SANDBOX_MOUNTS` or an external service.
Restarting the outer container briefly interrupts service, then resumes the last
accepted sandbox with its existing files and reserved user. It uses the accepted
cached image digest before contacting the registry and checks Git after recovery.
Changed deployment settings or an unavailable/unhealthy saved sandbox use the
normal fresh-installation path. Older installations without resume metadata take
one normal startup before subsequent restarts can reuse their sandbox. No extra
configuration is needed. Recreating the container requires retaining both
`/config` and `/data` to resume it.
To deploy a newer runtime image, set `RUNTIME_IMAGE` to its new tag or digest and
apply the change; restarting preserves the previously accepted image.

Streaming uploads, streamed responses, SSE, and WebSockets are supported over
HTTP/1.1. Use a trusted reverse proxy for HTTPS and client HTTP/2 or HTTP/3.
Native HTTP/2 gRPC and forward-proxy CONNECT are unsupported.

Use a Linux host with Docker 24+ and kernel 5.15+. Default Docker permissions
are sufficient, including Hostinger's AppArmor and seccomp policies.

The supervisor runs as root inside Docker. Each app instance runs as a separate
unprivileged Linux user with a private writable filesystem. PRoot runs the
unpacked OCI image without mounting filesystems or creating user namespaces.
It shares Docker's network, devices and process information; Linux user and file
permissions protect supervisor settings and other instances. This is process
and file isolation, not a separate kernel sandbox. `DATA_DIR` parent directories
must allow directory traversal. The default `/data` works as supplied.

## Development

Run `make check` with Go 1.27.3+ and Python 3. The [DOX instructions](AGENTS.md)
document each package's ownership and tests, including optional real OCI/runtime
and load checks.

The supervisor image uses Debian 13 (trixie) with package updates applied at build
time. The service and bundled OCI tools use Go 1.27.3. Rebuild with
`docker compose build --pull --no-cache` to pick up base-image and Debian security
updates; existing images are not updated in place.

Pushing the highest version tag publishes `agent0ai/easy-service:<git-tag>` for
amd64 and arm64, plus `agent0ai/easy-service:latest`. Versions are compared as
numbers: `v0.1.10` is newer than `v0.1.9`. Older tag pushes are skipped. Stable
versions use `vMAJOR.MINOR[.PATCH]` or the same format without `v`.

To build a specific revision, open **Actions → Publish Docker image → Run
workflow** and enter a Git tag or commit hash in `ref`. The image uses the same
tag or hash you entered. Older tags, prerelease tags and commit hashes publish
their own image tag; only the highest stable version also updates `latest`.
You can use a published image in Portainer instead of `build: .`.

Set these repository secrets under **Settings → Secrets and variables → Actions**:

| Secret | Value |
| --- | --- |
| `DOCKERHUB_ORG` | `agent0ai` |
| `DOCKERHUB_OAT_TOKEN` | Docker Hub organization access token with image push access to `agent0ai/easy-service` |

GitHub supplies its own read-only repository token; no GitHub token secret is needed.

## Credits

Created by [Agent Zero](https://www.agent-zero.ai/), the
[open-source agentic AI framework](https://github.com/agent0ai/agent-zero).
