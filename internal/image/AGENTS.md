# Purpose

Resolve, verify and cache the workload's OCI filesystem without a Docker socket.

## Ownership

- `image.go` owns Skopeo/umoci orchestration and ready metadata.
- `real_test.go` owns the opt-in real registry/rootless unpack check.
- `internal/state.AtomicWrite` owns durable metadata publication.

## Local Contracts

- Resolve Linux/current-architecture images by immutable SHA-256 digest; reject pin mismatches. Prepared images return the filesystem, digest and OCI process environment.
- Pass Docker image names to Skopeo directly: its native parser accepts official-image shorthand, Docker Hub namespaces, explicit registries and digest pins. Do not duplicate that parser.
- Remove an image tag before forming a digest copy reference, preserving registry ports.
- A source multi-platform index can differ from the copied architecture/converted OCI manifest; verify against Skopeo's copied digest.
- Unpack with `umoci --rootless`; cache metadata must refer to its owned rootfs path. Read environment defaults from the retained bundle/config.json, including on offline cache recovery; do not duplicate them in ready metadata.
- Prune only after a usable image/deployment is selected, or a failed apply returns to the accepted image. Keep that image's rootfs/configuration, remove its unused OCI archive/digest file and all other image caches, and enforce owned cleanup paths. Failed image resolution/preparation must leave earlier usable images available.
- Bound helper operations to ten minutes and remove failed partial copies.

## Work Guidance

- Tool fakes must emit valid digests and digest files and require the production rootless unpack arguments.
- Verify real-tool contract changes with the unprivileged check when tools/network are available.

## Verification

- `go test -race ./internal/image`.
- `TestPruneKeepsSelectedImageAndOfflineEnvironment` checks archive/stale-cache removal, cleanup bounds and offline environment recovery; malformed OCI configuration rejects and removes partial preparation.
- As an unprivileged user with Skopeo/umoci and registry access: `go test ./internal/image -run TestRealRootlessOCI -args -real-oci`.
- The real OCI check uses shorthand with a pinned multi-platform digest through resolution, immutable copy and rootless unpack.

## Child DOX Index

No child docs.
