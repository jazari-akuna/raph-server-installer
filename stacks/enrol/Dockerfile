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
#   docker-compose-plugin — provides `docker compose` as a CLI subcommand.
#                           The wizard's finalize step shells out to
#                           `docker compose --profile finalize -f
#                           ingress/docker-compose.yml run --rm certbot ...`
#                           to issue the wildcard cert. Without the plugin
#                           the docker CLI doesn't know `compose` is a
#                           subcommand and parses `--profile` as a global
#                           flag → "unknown flag: --profile".
#   passwd / login        — useradd / userdel for host-side user
#                           management; provided by the `passwd` package
#                           in trixie.
#   gettext-base          — provides `envsubst`. The wizard's finalize
#                           step shells out to scripts/render-templates.sh
#                           (see setup.go finalizeRenderTemplates) which
#                           uses envsubst per-file. Without this, the
#                           re-render step fails inside the container with
#                           "envsubst: command not found" for every
#                           *.template file. Phase 1's apt install on
#                           the host is unrelated — finalize runs inside
#                           THIS container.
#   restic                — deduplicating encrypted backup engine used by
#                           the admin /backup page (ADR-010) and by the
#                           nightly raph-backup.timer systemd unit. The
#                           timer execs `enrol backup --scheduled
#                           --tag=daily` inside this container so the
#                           binary needs to be on PATH here. trixie ships
#                           restic 0.16.x which is fine for our usage
#                           (snapshot, restore, forget, dump, copy);
#                           we don't pin a specific apt version so a
#                           security update to the package picks up
#                           automatically on the next image rebuild.
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
        gettext-base \
        restic \
        sqlite3 \
 && rm -rf /var/lib/apt/lists/*

# docker-compose-plugin isn't in the Debian apt repo (it's only published in
# Docker's own apt repo, which we don't wire up in this image). Pull the
# static binary from the upstream release directly. Pinned version matches
# install-docker.sh's host-side compose plugin so finalize behaviour matches
# what the operator gets if they run `docker compose` on the host. The
# checksum is verified to fail-fast on a tampered download.
ARG DOCKER_COMPOSE_VERSION=v2.32.2
ARG DOCKER_COMPOSE_SHA256_X86_64=e746a42f33113ca1057a72adff5f07d584b38c94dd7cc8368f6a30c276367710
# aarch64 SHA from the same release's checksums.txt:
#   https://github.com/docker/compose/releases/download/v2.32.2/checksums.txt
# Without this pin, an arm64 build silently skipped sha256sum -c, so a tampered
# release CDN payload would land inside this privileged container == host root.
ARG DOCKER_COMPOSE_SHA256_AARCH64=c5b795b304410d46a754ecacfee36bf1f341e3bcd562a882525115e09ed90d6c
RUN set -eux; \
    arch="$(uname -m)"; \
    case "$arch" in \
      x86_64)  compose_arch=x86_64;  compose_sha="${DOCKER_COMPOSE_SHA256_X86_64}"  ;; \
      aarch64) compose_arch=aarch64; compose_sha="${DOCKER_COMPOSE_SHA256_AARCH64}" ;; \
      *) echo "unsupported arch $arch for docker-compose static binary" >&2; exit 1 ;; \
    esac; \
    install -d /usr/libexec/docker/cli-plugins; \
    wget -qO /usr/libexec/docker/cli-plugins/docker-compose \
      "https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-${compose_arch}"; \
    echo "${compose_sha}  /usr/libexec/docker/cli-plugins/docker-compose" | sha256sum -c -; \
    chmod +x /usr/libexec/docker/cli-plugins/docker-compose; \
    docker compose version

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
