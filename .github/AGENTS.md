# Purpose

Own GitHub Actions publication automation.

## Ownership

- `workflows/publish-docker.yml` builds and publishes Docker images.
- `docker_release.py` owns revision selection, numeric version ordering and eligibility for `latest`; `docker_release_test.py` verifies that policy against real Git repositories.
- The root Dockerfile/Compose/README own runtime build and deployment contracts.

## Local Contracts

- Automatic tag pushes build only the highest stable version tag. Stable tags use `vMAJOR.MINOR[.PATCH]` or `MAJOR.MINOR[.PATCH]`; compare numeric components, with the exact tag name breaking equal-version ties.
- Manual dispatch requires a Git tag or an unambiguous 7–40 character commit hash. Preserve that input as the Docker tag; reject branches, invalid Docker tags and the reserved name `latest`.
- Build the resolved commit for Linux amd64 and arm64 as `agent0ai/easy-service:<tag-or-hash>`.
- Serialize publications without cancelling running or queued builds. Refresh Git tags after building; publish `latest` from the built manifest digest only if the selected tag is still the highest stable version and still points to the built commit. Commit-hash and prerelease builds never publish `latest`.
- Load automation from the default branch and source from the selected commit in separate checkout directories. Do not persist GitHub credentials in the Docker build context.
- Credentials come from `DOCKERHUB_ORG` and `DOCKERHUB_OAT_TOKEN` Actions secrets; never embed them in files or output.
- Keep repository permissions at the minimum required by the workflow.

## Work Guidance

- Check README publication instructions when changing workflow behavior.
- Publishing a tag/image is an external action and requires user authorization.

## Verification

- `make check` covers the shared repository contracts.
- `PYTHONDONTWRITEBYTECODE=1 python3 .github/docker_release_test.py` checks selection, annotated tags, commit inputs, invalid inputs and stale promotion.
- Inspect trigger/ref/tag/platform/secret wiring; a real image build/push requires Docker and authorized publication.

## Child DOX Index

No child docs. This contract covers `workflows/` and the publication helper and test.
