# easy-service

`easy-service` is the simple 80|20 solution for keeping a stateless HTTP service
alive and automatically updated on a single Linux VPS. It watches one GitHub
repository, prepares exact revisions in rootless gVisor sandboxes, and uses
healthy blue/green cutovers with minimal operational machinery. It is designed
for Docker Compose or Portainer and supports stateless HTTP APIs only.

The outer process keeps the current healthy revision serving while it fetches,
checks out, installs, starts, and probes a candidate. Cutover is atomic for each
new request. Existing requests, streaming responses, and WebSocket connections
remain assigned to the old backend for up to 30 seconds before that process tree
is terminated. Healthy updates aim to keep service uninterrupted, but this is
not an absolute zero-downtime guarantee: crashes, broken revisions, host
failures, and recovery windows can still cause outages. With no healthy backend,
the proxy returns 503 while polling and recovery continue.

## Configuration

All durations are positive numeric seconds.

| Variable | Default | Meaning |
|---|---:|---|
| `GIT_URL` | required | Credential-free HTTPS `github.com` repository URL |
| `GIT_TOKEN` | empty | Git and GitHub API token; never enters a workload |
| `GIT_BRANCH` | `main` | Branch watched in commit mode |
| `UPDATE_METHOD` | `commit` | `commit`, `tag`, or `release` |
| `UPDATE_PATTERN` | `*` | Whole tag wildcard (`*`, `?`); ignored for commits |
| `POLL_INTERVAL` | `300` | Poll period |
| `RUNTIME_IMAGE` | required | OCI workload image; digest references are supported |
| `SETUP_COMMAND` | empty | Optional `/bin/sh -c` preparation command |
| `RUN_COMMAND` | required | Foreground `/bin/sh -c` server command |
| `SERVICE_PORT` | `80` | Port inside the workload sandbox |
| `HEALTH_PATH` | `/` | GET readiness/liveness path; only 2xx passes |
| `STARTUP_TIMEOUT` | `60` | Readiness deadline after setup |
| `HEALTH_INTERVAL` | `10` | Probe and failed-recovery pacing interval |
| `HEALTH_FAILURES` | `3` | Consecutive liveness failures |
| `DATA_DIR` | `/data` | Persistent supervisor image/Git/preparation state |
| `SERVICE_MEMORY_LIMIT` | `512M` | Soft active-sandbox replacement threshold; empty or `0` disables monitoring |

Variables named `APP_*` are forwarded with `APP_` removed. The workload also
gets `PORT=SERVICE_PORT` and a minimal `PATH`/`HOME`; it does not inherit Git
credentials or the supervisor environment. The runtime image must contain
`/bin/sh` and all application runtime tools.

Memory limits are integer byte sizes with an optional binary `K`, `M`, `G`, or
`T` suffix. When enabled, the supervisor samples the resident memory of the
active rootlesskit/runsc process tree about once per second. This is not a hard
cgroup limit. Exceeding it requests a normal private same-revision blue/green
deployment: the original keeps serving through candidate readiness, then the
usual atomic cutover and 30-second drain apply. A rejected candidate is removed
and the original enters normal restart/redeploy recovery; if the original dies
during replacement, candidate readiness is cancelled immediately and ordinary
recovery takes ownership.

Commit mode resolves the watched remote branch. Tag mode orders matching tags
by annotated-tag timestamp or lightweight-tag commit timestamp and then tag
name. Release mode paginates GitHub releases, excludes drafts/prereleases,
orders by publication time, and matches `tag_name`. Every checkout is detached
at the resolved 40-character commit SHA and has its Git metadata removed.

## Portainer / Compose

The included `compose.yaml` is the minimal shape:

```yaml
services:
  easy-service:
    image: agent0ai/easy-service:v1.0.0 # replace with the Git tag to deploy
    restart: unless-stopped
    ports: ["8080:80"]
    environment:
      GIT_URL: https://github.com/example/stateless-api.git
      RUNTIME_IMAGE: docker.io/library/node:22-bookworm-slim
      RUN_COMMAND: node server.js
      SERVICE_MEMORY_LIMIT: 512M
    volumes: [easy-service-data:/data]
    devices: [/dev/net/tun:/dev/net/tun]
    security_opt: [seccomp=unconfined]
volumes:
  easy-service-data:
```

Use a Portainer Git stack or paste the Compose YAML. Put `GIT_TOKEN` in
Portainer's environment/secret handling, never in `GIT_URL` or the YAML.

## Mandatory host and isolation prerequisites

This product deliberately fails closed if rootless gVisor cannot start. It does
not use the Docker socket and never falls back to runc or an unsandboxed child.
The supported baseline is a modern x86-64 or arm64 Linux kernel (5.15 or later),
Docker Engine 24+ with cgroup v2, unprivileged user namespaces and subordinate
UID/GID mappings, and `/dev/net/tun`. Portainer must preserve the Compose device
and security options. Docker's default seccomp profile blocks nested namespace
operations, hence the explicit `seccomp=unconfined`; do not add `privileged` or
mount the host Docker socket. The outer container itself runs as UID 10000.

Before deployment, verify on the host:

```sh
test "$(cat /proc/sys/kernel/unprivileged_userns_clone)" = 1
test -c /dev/net/tun
docker compose run --rm easy-service runsc --version
docker compose run --rm easy-service rootlesskit --net=slirp4netns -- true
```

Startup diagnostics name a missing helper or disabled user namespace. Some VPS
providers disable nested user namespaces or TUN devices; that configuration is
unsupported. The default gVisor systrap platform avoids a KVM dependency.
Each deployment gets its own rootlesskit network namespace and slirp port
forward, its own gVisor process/filesystem view, and a private disk-backed rootfs
copy. Runtime image pulls are architecture-specific and cached by resolved
digest. Setup is bounded to 15 minutes; Git operations to 2 minutes; image
resolution/pull/unpack to 10 minutes; GitHub HTTP calls to 30 seconds.

## Lifecycle and limits

A fresh deployment has one restart using its existing writable filesystem.
After that allowance is used, another process/health failure makes a private
fresh filesystem from the prepared installation. Successful probes reset only
the consecutive failure count. Recovery has no retry cap or rollback and is
paced by `HEALTH_INTERVAL`; a newer polled revision supersedes a broken one.
Prepared and running filesystems are disk-backed under `/data`; application
writes are intentionally ephemeral across redeployments.

State is written by atomic rename plus directory sync. On supervisor restart,
the recorded process group and orphan instance directories are reconciled, then
the selected revision is deployed again. This can create an outage after an
outer-container crash. Uninterrupted service also cannot be guaranteed during
repeated launch failures, broken code, host failure, or the recovery window.
There is no dashboard, TLS termination, database management, automatic rollback,
permanent standby, or required webhook. Terminate TLS in a trusted upstream.

Logs include deployment ID, phase, and stream for setup/application output.
Stderr is transported as a stream label, not interpreted as severity. Successful
health checks are not logged.

## Development and verification

Run `make check`. Tests own fakes at Git/GitHub, OCI tooling, process, clock, and
sandbox boundaries. A real Docker image build needs Docker; a real gVisor smoke
test additionally needs the host prerequisites above. Multi-platform images can
be built with `docker buildx build --platform linux/amd64,linux/arm64 ...`.

## Docker Hub publication

Pushing a Git tag publishes the root Dockerfile for `linux/amd64` and
`linux/arm64` as `agent0ai/easy-service:<git-tag>`. The Git tag is reused
exactly; incompatible Docker tag names fail instead of being rewritten, and the
workflow does not publish `latest`. Configure the GitHub Actions secrets
`DOCKERHUB_ORG` and `DOCKERHUB_OAT_TOKEN` to provide the Docker Hub credentials.

## Credits

<p align="center">
  Created by <strong><a href="https://www.agent-zero.ai/">Agent Zero</a></strong><br>
  Open-source agentic AI framework<br>
  <a href="https://www.agent-zero.ai/">Website</a> &middot; <a href="https://github.com/agent0ai/agent-zero">GitHub repository</a>
</p>
