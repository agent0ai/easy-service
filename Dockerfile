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
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/* \
 && case "$TARGETARCH" in amd64) arch=x86_64;; arm64) arch=aarch64;; *) exit 1;; esac \
 && base="https://storage.googleapis.com/gvisor/releases/${GVISOR_RELEASE}/${arch}" \
 && curl -fsSLo /runsc "$base/runsc" \
 && curl -fsSLo /runsc.sha512 "$base/runsc.sha512" \
 && (cd / && sha512sum -c runsc.sha512) \
 && chmod 0755 /runsc

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git skopeo umoci rootlesskit slirp4netns uidmap tini coreutils libcap2-bin \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10000 --create-home --shell /usr/sbin/nologin easyservice \
 && echo 'easyservice:100000:65536' >> /etc/subuid \
 && echo 'easyservice:100000:65536' >> /etc/subgid
COPY --from=build /out/easy-service /usr/local/bin/easy-service
COPY --from=runsc /runsc /usr/local/bin/runsc
RUN setcap cap_net_bind_service=+ep /usr/local/bin/easy-service \
 && apt-get purge -y libcap2-bin \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir /data && chown easyservice:easyservice /data
USER easyservice
EXPOSE 80
VOLUME ["/data"]
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/easy-service"]
