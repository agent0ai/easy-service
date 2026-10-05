# Purpose

Own GitHub Actions publication automation.

## Ownership

- `workflows/publish-docker.yml` publishes tagged Docker images.
- The root Dockerfile/Compose/README own runtime build and deployment contracts.

## Local Contracts

- Publish only on pushed Git tags; build the tagged revision.
- Use the exact Git tag as the Docker tag, rejecting incompatible names instead of rewriting them.
- Publish `agent0ai/easy-service:<git-tag>` for Linux amd64 and arm64; do not publish `latest`.
- Credentials come from `DOCKERHUB_ORG` and `DOCKERHUB_OAT_TOKEN` Actions secrets; never embed them in files or output.
- Keep repository permissions at the minimum required by the workflow.

## Work Guidance

- Check README publication instructions when changing workflow behavior.
- Publishing a tag/image is an external action and requires user authorization.

## Verification

- `make check` covers the shared repository contracts.
- Inspect trigger/ref/tag/platform/secret wiring; a real image build/push requires Docker and authorized publication.

## Child DOX Index

No child docs. This contract covers `workflows/`.

