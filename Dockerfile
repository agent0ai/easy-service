# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/easy-service ./cmd/easy-service

FROM debian:bookworm-slim AS runsc
ARG TARGETARCH
ARG GVISOR_RELEASE=release/latest
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl zstd && rm -rf /var/lib/apt/lists/* \
 && case "$TARGETARCH" in amd64) arch=x86_64;; arm64) arch=aarch64;; *) exit 1;; esac \
 && base="https://storage.googleapis.com/gvisor/releases/${GVISOR_RELEASE}/${arch}" \
 && curl -fsSLo /gvisor.tar.zstd "$base/gvisor.tar.zstd" \
 && curl -fsSLo /gvisor.tar.zstd.sha512 "$base/gvisor.tar.zstd.sha512" \
 && (cd / && sha512sum -c gvisor.tar.zstd.sha512) \
 && mkdir /out && tar --zstd -xf /gvisor.tar.zstd -C /out \
 && rm -f /out/containerd-shim-runsc-v1

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git skopeo umoci tini coreutils libcap2-bin \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10000 --create-home --shell /usr/sbin/nologin easyservice
COPY --from=build /out/easy-service /usr/local/bin/easy-service
COPY --from=runsc /out/ /usr/local/bin/
RUN setcap cap_net_bind_service=+ep /usr/local/bin/easy-service \
 && apt-get purge -y libcap2-bin \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir /data /config /logs && chown easyservice:easyservice /data /config /logs
USER easyservice
EXPOSE 80
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/easy-service"]
