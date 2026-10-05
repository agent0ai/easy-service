# Purpose

Own rootless gVisor isolation, private application filesystems and sandbox processes.

## Ownership

- `sandbox.go` owns prerequisite checks, OCI specs, preparation, start/restart, stop, logging and process-tree RSS.
- Recovery decisions belong to the supervisor engine; image unpack belongs to `internal/image`.

## Local Contracts

- Require rootlesskit/runsc/slirp4netns, nested user namespaces, TUN and cgroup v2; fail closed.
- Never add host Docker-socket access or substitute runc/host execution in production.
- Disable slirp access to supervisor host-loopback ports in preflight and every workload/setup launch.
- Mount an owned read-only resolv.conf targeting the default slirp DNS endpoint (10.0.2.3); OCI images may omit resolver configuration. Change DNS and rootless network addressing together.
- Copy disk-backed private rootfs trees; replace image `/app` before copying checkout content so host copies cannot follow its symlink.
- Setup is bounded to fifteen minutes and stops before failed preparation is removed.
- Separate filesystem-operation cancellation from running sandbox lifetime.
- `Done` is a closed broadcast; repeated `Wait` calls preserve the exit result.
- Do not signal completed instances. Stop escalates from TERM to KILL when its grace context expires and waits up to two additional seconds for process/output completion before reporting success.
- Workload environment is only forwarded application values plus controlled PATH/HOME/PORT; no supervisor credentials/environment inheritance.
- RSS is a soft observation of the active owned process tree, including children from all threads; memory replacement policy stays in the engine.

## Work Guidance

- Process/filesystem fakes must preserve lifecycle and isolation contract arguments.
- Do not broaden process scanning in the approximately one-second memory sampler.

## Verification

- `go test -race ./internal/sandbox`.
- `go test ./internal/sandbox -run '^$' -bench . -benchmem` measures RSS sampling.
- Real gVisor startup requires the host prerequisites in `../../README.md`; passing fakes does not prove host isolation works.

## Child DOX Index

No child docs.
