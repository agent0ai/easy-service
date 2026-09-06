# Engineering contract

Trace defects through the complete revision-selection, preparation, sandbox,
health, routing, drain, and recovery flow. Fix behavior in the subsystem that
owns its contract; callers may not weaken gVisor isolation, duplicate recovery
policy, or leak supervisor credentials/environment into workloads. Keep the
public environment-variable surface exactly aligned with README.md and
internal/config. Production code must not add a host Docker-socket dependency.

Memory pressure is an approximately one-second soft observation of the active
rootlesskit/runsc process tree. It must enter the serialized deployment engine
as a same-revision blue/green replacement request; it may not create a second
cutover, readiness, drain, or recovery state machine.

Tests may replace process, image, GitHub, clock, and sandbox boundaries with
owned fakes. A fake must preserve the production contract it stands in for.
