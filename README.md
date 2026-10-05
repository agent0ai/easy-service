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
    ports: ["8080:80"]
    environment:
      GIT_URL: https://github.com/you/your-service.git
      GIT_TOKEN: ${GIT_TOKEN:-}
      RUNTIME_IMAGE: node:22-bookworm-slim
      SETUP_COMMAND: npm ci
      RUN_COMMAND: node server.js
    volumes: [easy-service-data:/data]
    devices: [/dev/net/tun:/dev/net/tun]
    security_opt: [seccomp=unconfined]
volumes:
  easy-service-data:
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

Commands run in `/app` using `/bin/sh`. Keep the server running in the foreground
and make it listen on `$PORT` (80 by default). Pass application settings as
`APP_NAME=value`; the application receives `NAME=value`. Git credentials stay
in the supervisor.

## Inspect and control

```sh
docker compose exec easy-service easy-service status
docker compose exec easy-service easy-service redeploy
docker compose exec easy-service easy-service restart
docker compose logs -f easy-service
```

- `status` shows the current revision, image, health state, and memory usage.
- `redeploy` checks Git immediately and creates a fresh writable installation,
  even if the revision has not changed. It reuses the prepared dependencies.
- `restart` restarts the current revision with its existing writable files.
  This briefly interrupts service.

Inside the container, use `easy-service status`, `easy-service redeploy`, or
`easy-service restart` directly. Actions wait for completion and report failures.
During startup, status shows `waiting`. The CLI uses a private Unix socket;
there is no extra network port to expose.

## Additional settings

All of these are optional. Times are in seconds.

| Variable | Default | What it does |
|---|---|---|
| `SERVICE_MEMORY_LIMIT` | `512M` | Replace the running service when its memory exceeds this soft threshold. Set `0` or empty to disable. |
| `GIT_BRANCH` | `main` | Branch to watch. |
| `UPDATE_METHOD` | `commit` | Deploy by `commit`, `tag`, or GitHub `release`. |
| `UPDATE_PATTERN` | `*` | Tag/release name filter, using `*` and `?`. |
| `POLL_INTERVAL` | `300` | How often to check Git. |
| `SERVICE_PORT` | `80` | Port your application listens on. Also passed as `PORT`. |
| `HEALTH_PATH` | `/` | URL path that must return a 2xx response. |
| `STARTUP_TIMEOUT` | `60` | How long to wait for a new service to become healthy after setup. |
| `HEALTH_INTERVAL` | `10` | Time between health checks and recovery retries. |
| `HEALTH_FAILURES` | `3` | Consecutive failed checks before recovery. |
| `DATA_DIR` | `/data` | Where the supervisor stores its Git, image, and installation cache. |

Memory sizes accept bytes or `K`, `M`, `G`, and `T` suffixes (1024-based).
The memory threshold is checked about once per second and includes the sandbox
process tree. It triggers a normal deployment rather than enforcing a hard cap.

`GIT_URL` can also be a local Git directory mounted into the container and
readable by UID 10000. Only committed files are deployed; local repositories
support commit and tag updates.

## What to expect

A healthy update starts the new version before switching traffic. Failed
candidates leave the healthy version running. Existing requests, SSE streams,
and WebSockets have up to 30 seconds to finish on the old version.

Crashes and failed health checks trigger one restart, followed by a fresh
installation on another failure. Recovery keeps retrying. Application writes
are temporary across redeployments; keep databases and durable files elsewhere.
Restarting the outer container can cause an outage.

Streaming uploads, streamed responses, SSE, and WebSockets are supported over
HTTP/1.1. Use a trusted reverse proxy for HTTPS and client HTTP/2 or HTTP/3.
Native HTTP/2 gRPC and forward-proxy CONNECT are unsupported.

Use a Linux host with Docker 24+, kernel 5.15+, cgroup v2, unprivileged user
namespaces, subordinate UID/GID mappings, and `/dev/net/tun`. Keep the Compose
device and security settings shown above. Workloads run in rootless gVisor;
startup fails if the host cannot provide it.

## Development

Run `make check` with Go 1.23+. The [DOX instructions](AGENTS.md) document each
package's ownership and tests, including optional real OCI/gVisor and load checks.

Git tags publish Docker images as `agent0ai/easy-service:<git-tag>` for amd64 and
arm64. You can use a published tag in Portainer instead of `build: .`.

## Credits

Created by [Agent Zero](https://www.agent-zero.ai/), the
[open-source agentic AI framework](https://github.com/agent0ai/agent-zero).
