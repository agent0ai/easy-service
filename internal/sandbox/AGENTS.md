# Purpose

Own OCI filesystem execution through PRoot, Linux user isolation and whole-instance lifetime.

## Ownership

- `sandbox.go` owns preparation, launch/restart, ports, DNS, output, CPU/RAM sampling and live-instance discovery.
- `child.go` owns the internal launcher and guest re-exec boundary.
- `internal/state` owns root-protected UID leases and restart reconciliation; `internal/process` owns UID process observation/signaling; retained files belong to `internal/logs`.

## Local Contracts

- Run the supervisor as root inside Docker; execute each setup/run as a separate leased UID with cleared groups and no_new_privs. Use PRoot 5.5 without nested mounts, user namespaces, host policy changes or a Docker socket.
- PRoot supplies OCI path translation and apparent root; Linux UIDs/file permissions supply containment. Workloads share the outer network, devices and process information rather than a separate kernel sandbox.
- Keep OCI image and Git caches, configuration, control and logs inaccessible to workload users. Never inherit the supervisor environment. The inherited launch descriptor carries only app settings; close it before running the app.
- Preserve image environment and explicit APP_* values, reserving only assigned PORT; default HOME is /root. Keep tracer environment separate from the guest environment.
- Assign an available PORT for every setup/start/restart; endpoints must match it so overlapping instances can serve concurrently.
- Copy private writable rootfs trees, replace image /app and /tmp symlinks before host access, retain HOME/tmp setup writes, and never follow image symlinks during recursive ownership changes.
- DATA_DIR and managed instance parents permit traversal; rootfs and tracer temporary storage belong only to the leased UID. Root-owned launch settings stay private. Custom DATA_DIR ancestors must permit directory traversal.
- Supply an owned read-only resolver copy using PRoot path translation; guest resolver symlinks cannot redirect host writes.
- Setup honors SETUP_TIMEOUT and confirms termination before failed preparation is removed. Never reuse or validate a prepared filesystem still owned by live setup. Filesystem cancellation remains separate from app lifetime.
- Stop signals every process owned by the leased UID, including detached/reparented descendants, then escalates to KILL at the deadline. Cancellation also stops the whole UID; signal the UID before terminating the tracer. Confirm all live UID processes exited before release, Done or filesystem cleanup. Never reuse a live UID or signal completed instances.
- Close Done as a broadcast after process/output/descendant completion and before console reporting or file-writer retirement can block; preserve exit errors across repeated Wait calls.
- Capture registered stdout/stderr and runtime failures through the shared log owner; release file handles without deleting retained logs.
- Report sampled CPU and soft resident memory across the complete UID, including detached descendants. CPU 100% means one core. Memory replacement policy remains in the serialized engine.
- Live discovery covers setup, candidates, running and draining instances; remove completed instances from the process-local registry.

## Work Guidance

- Owned execution fakes must preserve credential dropping, no_new_privs, workload environment exclusion, exit broadcasts and complete UID cleanup.
- Qualify actual OCI/PRoot execution on the remote host under default security policies before relying on it.

## Verification

- `go test -race ./internal/sandbox` checks overlapping ports/UIDs, private file ownership, privilege restrictions, detached-child CPU/RAM, whole-instance kill, peer survival and retained failure output.
- Actual OCI/PRoot checks belong to cmd/easy-service/real_test.go; fakes do not verify OCI syscall compatibility.

## Child DOX Index

No child docs.
