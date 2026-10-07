# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/easy-service ./cmd/easy-service

FROM debian:bookworm-slim AS proot
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl build-essential libtalloc-dev \
 && curl -fsSLo /proot.tar.gz https://codeload.github.com/proot-me/proot/tar.gz/refs/tags/v5.5.0 \
 && echo '287cfb27f58e100b0153006cb352268225bb9ad9d41d9b52ed56d01a22c13e6f  /proot.tar.gz' | sha256sum -c - \
 && mkdir /src && tar -xzf /proot.tar.gz -C /src --strip-components=1 \
 && make -C /src/src loader.elf build.h && make -C /src/src proot

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git skopeo umoci tini coreutils libtalloc2 \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir /data /config /logs && chmod 700 /data /config /logs
COPY --from=build /out/easy-service /usr/local/bin/easy-service
COPY --from=proot /src/src/proot /usr/local/bin/proot
EXPOSE 80
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/easy-service"]
