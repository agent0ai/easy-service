# Purpose

Resolve, verify and cache the workload's OCI filesystem without a Docker socket.

## Ownership

- `image.go` owns Skopeo/umoci orchestration and ready metadata.
- `real_test.go` owns the opt-in real registry/rootless unpack check.
- `internal/state.AtomicWrite` owns durable metadata publication.

## Local Contracts

- Resolve Linux/current-architecture images by immutable SHA-256 digest; reject pin mismatches.
- Pass Docker image names to Skopeo directly: its native parser accepts official-image shorthand, Docker Hub namespaces, explicit registries and digest pins. Do not duplicate that parser.
- Remove an image tag before forming a digest copy reference, preserving registry ports.
- A source multi-platform index can differ from the copied architecture/converted OCI manifest; verify against Skopeo's copied digest.
- Unpack with `umoci --rootless`; cache metadata must refer to its owned rootfs path.
- Bound helper operations to ten minutes and remove failed partial copies.

## Work Guidance

- Tool fakes must emit valid digests and digest files and require the production rootless unpack arguments.
- Verify real-tool contract changes with the unprivileged check when tools/network are available.

## Verification

- `go test -race ./internal/image`.
- As an unprivileged user with Skopeo/umoci and registry access: `go test ./internal/image -run TestRealRootlessOCI -args -real-oci`.
- The real OCI check uses shorthand with a pinned multi-platform digest through resolution, immutable copy and rootless unpack.

## Child DOX Index

No child docs.
