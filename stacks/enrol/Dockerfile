# enrol — peer-management web UI for the gw0 gateway.
#
# Multi-stage build:
#   stage 1: golang:1.23 to build a static binary
#   stage 2: debian:bookworm-slim with wireguard-tools (for `wg`/`wg-quick`)
#            and util-linux (for `nsenter`) so we can syncconf the host
#            interface from inside the container.
#
# Note: we use vanilla wireguard-tools binaries here; AmneziaWG's `awg`
# variant is not in upstream Debian, but `wg syncconf` understands the
# AmneziaWG interface fine because the kernel module is what reads the
# anti-DPI params (Jc/Jmin/.../H1-H4) — userspace only sees standard
# WireGuard fields plus passthrough. If the host interface name is `gw0`
# and was configured with awg-quick, we still reload it via nsenter into
# the host's PID 1 namespace, calling the host's own `awg`/`awg-quick`.

FROM golang:1.23-bookworm AS build

WORKDIR /src

# Copy go.mod first for layer caching.
COPY go.mod ./
# go.sum is produced by `go mod tidy`; if absent at build time the
# `go mod download` below seeds it. We don't commit go.sum because the
# laptop doesn't have network for `go mod tidy` in this flow — it'll be
# regenerated and committed in a follow-up if/when the build is exercised
# against the network. For now keep deps to stdlib + skip2/go-qrcode.
RUN go mod download || true

COPY . .

# Build a static binary. CGO disabled.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags='-s -w -extldflags=-static' \
        -o /out/enrol \
        .

###############################################################################
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        wireguard-tools \
        util-linux \
        iproute2 \
        qrencode \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/enrol /usr/local/bin/enrol
COPY web /app/web

WORKDIR /app

ENV ENROL_LISTEN=:8080 \
    ENROL_AWG_DIR=/etc/amnezia/amneziawg \
    ENROL_AWG_IFACE=gw0 \
    ENROL_AWG_ENDPOINT=gw.antarctica-engineering.com:51820 \
    ENROL_PEER_SUBNET=10.99.0.0/24 \
    ENROL_PEER_START=10 \
    ENROL_HEADER_USER=Remote-User \
    ENROL_HEADER_GROUPS=Remote-Groups \
    ENROL_REQUIRED_GROUP=admins \
    ENROL_TEMPLATES=/app/web/templates \
    ENROL_STATIC=/app/web/static

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/enrol"]
