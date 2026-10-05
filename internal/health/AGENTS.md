# Purpose

Own HTTP readiness/liveness observations of a running instance.

## Ownership

- `health.go` owns probe timeout calculation, startup probing and consecutive-failure observation.
- The supervisor owns restart/redeploy decisions and pacing.

## Local Contracts

- Only HTTP 2xx passes; do not follow redirects or use supervisor HTTP proxy environment.
- Probe timeout is half the interval, capped at three seconds and at the remaining startup time, with a one-nanosecond minimum.
- Process exit and context cancellation interrupt probes and waits.
- Successful probes reset only the consecutive-failure count.
- Startup timeout diagnostics retain the last probe error.

## Work Guidance

- Keep readiness and liveness on the same probe contract.
- Do not add recovery policy or caller-specific probe fallbacks.

## Verification

- `go test -race ./internal/health` checks timeout calculation, failure thresholds and exit interruption.
- Supervisor integration tests exercise bad and stalled HTTP health endpoints.

## Child DOX Index

No child docs.

