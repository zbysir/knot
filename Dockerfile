# Single image, two roles: `knot head` and `knot node`.
#
# sing-box ships as a separate binary rather than a linked library on purpose:
# its Go API changes between releases, while the CLI and config schema are the
# supported interface. Upgrading is a one-line version bump here.
ARG SINGBOX_VERSION=1.13.15

# Builder stages pin to BUILDPLATFORM and cross-compile via GOOS/GOARCH.
# Running them under QEMU emulation instead also works, but compiling sing-box
# that way takes tens of minutes.
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/knot .

# Built from source so the build tags are explicit and auditable -- this binary
# relays other machines' traffic.
#
# NOTE: do NOT add `with_reality_server`. As of 1.13 it was merged into
# `with_utls` and passing it is now a hard compile error. The official release
# binary therefore does include Reality server support, despite the tag not
# appearing in `sing-box version` output.
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS singbox
ARG SINGBOX_VERSION
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go install -trimpath -ldflags="-s -w" \
      -tags "with_gvisor,with_quic,with_utls" \
      github.com/sagernet/sing-box/cmd/sing-box@v${SINGBOX_VERSION} && \
    find /go/bin -name sing-box -exec cp {} /usr/local/bin/sing-box \;

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates iproute2 && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build   /out/knot               /usr/local/bin/knot
COPY --from=singbox /usr/local/bin/sing-box /usr/local/bin/sing-box
VOLUME /var/lib/knot
ENTRYPOINT ["/usr/local/bin/knot"]
CMD ["head"]
