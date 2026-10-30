# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie AS toolchain

FROM toolchain AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/easy-service ./cmd/easy-service

FROM toolchain AS tools
ARG TARGETOS TARGETARCH
WORKDIR /src
# Rebuild OCI tools with patched Go and dependency fixes newer than their releases.
RUN curl -fsSLo skopeo.tar.gz https://codeload.github.com/containers/skopeo/tar.gz/refs/tags/v1.24.1 \
 && echo '6a3f342a327ec7684198eb0e15114940b6101506db48d9a96590f5942cb3b335  skopeo.tar.gz' | sha256sum -c - \
 && mkdir skopeo && tar -xzf skopeo.tar.gz -C skopeo --strip-components=1 \
 && cd skopeo \
 && go get github.com/go-jose/go-jose/v4@v4.1.6 golang.org/x/crypto@v0.58.0 golang.org/x/net@v0.62.0 google.golang.org/grpc@v1.83.2 \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -trimpath -ldflags='-s -w' \
      -tags 'containers_image_openpgp exclude_graphdriver_btrfs' -o /out/skopeo ./cmd/skopeo
RUN curl -fsSLo umoci.tar.gz https://codeload.github.com/opencontainers/umoci/tar.gz/refs/tags/v0.6.0 \
 && echo '400a26c5f7ac06e40af907255e0e23407237d950e78e8d7c9043a1ad46da9ae5  umoci.tar.gz' | sha256sum -c - \
 && mkdir umoci && tar -xzf umoci.tar.gz -C umoci --strip-components=1 \
 && cd umoci \
 && go get github.com/klauspost/compress@v1.18.7 github.com/moby/sys/user@v0.4.1 golang.org/x/crypto@v0.58.0 \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -trimpath -ldflags='-s -w' -o /out/umoci ./cmd/umoci

FROM debian:trixie-slim AS proot
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl build-essential pkg-config libtalloc-dev \
 && curl -fsSLo /proot.tar.gz https://codeload.github.com/proot-me/proot/tar.gz/refs/tags/v5.5.0 \
 && echo '287cfb27f58e100b0153006cb352268225bb9ad9d41d9b52ed56d01a22c13e6f  /proot.tar.gz' | sha256sum -c - \
 && mkdir /src && tar -xzf /proot.tar.gz -C /src --strip-components=1 \
 && make -C /src/src loader.elf build.h && make -C /src/src proot

FROM debian:trixie-slim
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
      ca-certificates git tini coreutils libtalloc2 \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir /data /config /logs && chmod 700 /data /config /logs
COPY --from=build /out/easy-service /usr/local/bin/easy-service
COPY --from=tools /out/skopeo /out/umoci /usr/local/bin/
COPY --from=tools /src/skopeo/default-policy.json /etc/containers/policy.json
COPY --from=tools /src/skopeo/LICENSE /usr/share/doc/skopeo/copyright
COPY --from=tools /src/umoci/COPYING /usr/share/doc/umoci/copyright
COPY --from=proot /src/src/proot /usr/local/bin/proot
EXPOSE 80
ENTRYPOINT ["/usr/bin/tini","--","/usr/local/bin/easy-service"]
