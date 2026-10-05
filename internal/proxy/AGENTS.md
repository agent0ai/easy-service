# Purpose

Own HTTP forwarding and the lifetime of requests pinned to each backend.

## Ownership

- `proxy.go` owns backend selection, the shared transport/buffers, reverse proxies and in-flight drain counts.
- `protocol_test.go` owns real-socket HTTP/SSE/WebSocket checks.
- The supervisor owns drain deadlines and sandbox termination.

## Local Contracts

- Select and count the backend atomically per request; existing requests stay pinned through cutover.
- Guard backend reads, swaps and request admission with one selection read/write lock; do not add a second atomic-pointer mechanism.
- Track requests through streaming completion, cancellation and upgraded-connection closure; earlier idle periods cannot satisfy a later drain.
- Use Go's standard reverse proxy for HTTP semantics, immediate flushing and bidirectional upgrades.
- Enable HTTP/1 full duplex so a response can arrive while an upload continues.
- Preserve body bytes, statuses, query/path encoding, compression, repeated headers, informational responses and trailers.
- Share live request trailers through the reverse-proxy rewrite so values populated at upload EOF reach the backend.
- Retain trusted upstream forwarding headers and append the peer to X-Forwarded-For; ingress must establish trustworthy forwarding values.
- Reuse concurrent backend keepalive connections within the shared transport's global idle limit.
- SSE and WebSockets can remain on the retiring backend only until the engine's configured drain deadline.
- Return 503 without a backend and 502 for pre-response backend errors; interrupted responses abort only that request.
- The production listener is plain HTTP; TLS and client HTTP/2/3 termination belong to a trusted upstream.

## Work Guidance

- Fix protocol handling in this shared proxy without caller-specific adapters.
- Use real sockets to verify flushing, streaming cancellation and RFC 6455 frames; keep protocol behavior in the standard library.

## Verification

- `go test -race ./internal/proxy -count=20`.
- `go test ./internal/proxy -run '^$' -bench . -benchmem` measures request allocations.
- Supervisor integration verifies routing through deployment and drain.

## Child DOX Index

No child docs.
