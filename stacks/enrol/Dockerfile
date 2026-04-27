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

# Copy go.mod first for layer caching. go.sum may or may not be
# present in the source tree — the laptop has no Go installed so we
# can't `go mod tidy` there. The COPY pattern here uses a glob so a
# missing go.sum doesn't fail the layer.
COPY go.mod ./
COPY go.su[m] ./

COPY . .

# Generate go.sum if missing (laptop case), then download. This is
# slower than the cached download path but only runs once per image.
RUN test -f go.sum || go mod tidy
RUN go mod download

# Build a static binary. CGO disabled.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags='-s -w -extldflags=-static' \
        -o /out/enrol \
        .

###############################################################################
FROM debian:trixie-slim

# Runtime deps:
#   ca-certificates       — TLS roots (for `docker exec authelia …` over
#                           the docker socket; not strictly required but
#                           cheap; future-proofs HTTPS-talking code).
#   wireguard-tools       — `wg` fallback if host /usr/bin/awg is absent.
#   util-linux + mount    — `mount` / `umount` / `nsenter` / `findmnt`.
#   iproute2              — `ip` (rare debug; cheap).
#   qrencode              — render TOTP + peer-config QR PNGs.
#   wget                  — healthcheck only.
#   cryptsetup-bin        — luksFormat / open / close / luksAddKey /
#                           luksRemoveKey / luksDump for LUKS lifecycle.
#   e2fsprogs             — mkfs.ext4 / resize2fs / e2label.
#   coreutils             — `shred` for secure deletion of LUKS blobs.
#                           (already in -slim but listed explicitly so
#                           future image switches don't drop it.)
#   docker-cli            — used to call `docker exec authelia authelia
#                           storage user totp generate <user>` via the
#                           bind-mounted /var/run/docker.sock.
#   passwd / login        — useradd / userdel for host-side user
#                           management; provided by the `passwd` package
#                           in trixie.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        wireguard-tools \
        util-linux \
        mount \
        iproute2 \
        qrencode \
        wget \
        cryptsetup-bin \
        e2fsprogs \
        coreutils \
        docker-cli \
        passwd \
        login \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/enrol /usr/local/bin/enrol
COPY web /app/web

WORKDIR /app

ENV ENROL_LISTEN=:8080 \
    ENROL_AWG_DIR=/etc/amnezia/amneziawg \
    ENROL_AWG_IFACE=gw0 \
    ENROL_PEER_SUBNET=10.99.0.0/24 \
    ENROL_PEER_START=10 \
    ENROL_HEADER_USER=Remote-User \
    ENROL_HEADER_GROUPS=Remote-Groups \
    ENROL_REQUIRED_GROUP=admins \
    ENROL_TEMPLATES=/app/web/templates \
    ENROL_STATIC=/app/web/static
# ENROL_DOMAIN and ENROL_AWG_ENDPOINT are intentionally NOT defaulted in the
# image — the compose file (or the operator) must supply them at runtime so
# the image stays domain-agnostic.

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/enrol"]
