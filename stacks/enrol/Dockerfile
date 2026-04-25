# enrol — peer-management web UI for the gw0 gateway.
#
# Multi-stage build:
#   stage 1: golang:1.23 to build a static binary
#   stage 2: debian:trixie-slim (glibc 2.41) so we can directly run the
#            host's bind-mounted /usr/bin/awg (Ubuntu 24.04 host, glibc
#            2.39). Bookworm slim ships glibc 2.36 which is too old.
#
# We rely on bind-mounts of the host's /usr/bin/awg + /usr/bin/awg-quick
# for the reload pipeline — vanilla wg-quick from upstream Debian does
# NOT understand the AmneziaWG-specific `Jc=` / `Jmin=` / `H1..H4=` lines
# in gw0.conf and rejects them with "Line unrecognized". With
# network_mode: host, the binaries operate on the host's already-shared
# net namespace and do not need nsenter.

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
FROM debian:trixie-slim

# wireguard-tools provides `wg` (used as the fall-back if the bind-mounted
# host /usr/bin/awg is missing). util-linux gives us nsenter for backward
# compatibility when ENROL_RELOAD_NSENTER=true. iproute2 + qrencode +
# wget are runtime deps.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        wireguard-tools \
        util-linux \
        iproute2 \
        qrencode \
        wget \
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
