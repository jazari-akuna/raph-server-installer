#!/usr/bin/env bash
#
# install-docker.sh — Step 3 of docs/design.md
#
# Installs Docker CE from the official Docker APT repo on Ubuntu 24.04 (noble),
# adds admins (taken from ADMIN_USERS env var) to the `docker` group, writes a
# conservative /etc/docker/daemon.json (json-file logging with rotation,
# live-restore), and creates the shared `edge` Docker network that all
# ingress-fronted stacks join.
#
# Idempotent. Safe to re-run. Run as root on the VPS.
#
# Naming: the network is called `edge` per the plan; we never write any of the
# camouflaged words (vpn/wireguard/amnezia/hysteria/tailscale/luks/vault/crypt)
# into user-facing names. "docker" is a package/runtime name and is in scope.

set -euo pipefail

# ---------- guards --------------------------------------------------------

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
    echo "ERROR: must be run as root." >&2
    exit 1
fi

. /etc/os-release
if [[ "${ID:-}" != "ubuntu" ]]; then
    echo "ERROR: this script targets Ubuntu (found ID=${ID:-unknown})." >&2
    exit 1
fi
if [[ "${VERSION_CODENAME:-}" != "noble" ]]; then
    echo "WARNING: expected Ubuntu 24.04 'noble', found '${VERSION_CODENAME:-unknown}'." >&2
fi

# ADMIN_USERS is provided by the bootstrap orchestrator (whitespace-separated).
# Empty is acceptable here — we'll just skip the docker-group step.
read -r -a ADMINS <<<"${ADMIN_USERS:-}"
KEYRING=/etc/apt/keyrings/docker.asc
SOURCES_LIST=/etc/apt/sources.list.d/docker.list
DAEMON_JSON=/etc/docker/daemon.json
EDGE_NET=edge

log()  { printf '[install-docker] %s\n' "$*"; }
warn() { printf '[install-docker] WARNING: %s\n' "$*" >&2; }

# ---------- 1. APT repo + GPG key ----------------------------------------

log "installing prerequisites (ca-certificates, curl)"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y ca-certificates curl

log "ensuring /etc/apt/keyrings exists"
install -d -m 0755 /etc/apt/keyrings

if [[ ! -s "$KEYRING" ]]; then
    log "fetching Docker GPG key into $KEYRING"
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o "$KEYRING"
    chmod 0644 "$KEYRING"
else
    log "Docker GPG key already present at $KEYRING"
    chmod 0644 "$KEYRING"
fi

ARCH="$(dpkg --print-architecture)"
DESIRED_LIST_LINE="deb [arch=${ARCH} signed-by=${KEYRING}] https://download.docker.com/linux/ubuntu noble stable"

if [[ ! -f "$SOURCES_LIST" ]] || ! grep -qxF "$DESIRED_LIST_LINE" "$SOURCES_LIST"; then
    log "writing $SOURCES_LIST"
    printf '%s\n' "$DESIRED_LIST_LINE" > "$SOURCES_LIST"
    chmod 0644 "$SOURCES_LIST"
    apt-get update -y
else
    log "$SOURCES_LIST already up to date"
fi

# ---------- 2. install Docker --------------------------------------------

log "installing docker-ce + plugins"
apt-get install -y \
    docker-ce \
    docker-ce-cli \
    containerd.io \
    docker-buildx-plugin \
    docker-compose-plugin

systemctl enable --now docker

# ---------- 3. add admins to docker group --------------------------------

for u in "${ADMINS[@]}"; do
    if id -u "$u" >/dev/null 2>&1; then
        if id -nG "$u" | tr ' ' '\n' | grep -qx docker; then
            log "user '$u' already in docker group"
        else
            log "adding '$u' to docker group"
            usermod -aG docker "$u"
        fi
    else
        warn "user '$u' does not exist on this host; skipping"
    fi
done

# ---------- 4. /etc/docker/daemon.json -----------------------------------

install -d -m 0755 /etc/docker

DESIRED_DAEMON_JSON='{
    "log-driver": "json-file",
    "log-opts": {
        "max-size": "10m",
        "max-file": "5"
    },
    "live-restore": true
}
'

daemon_changed=0
if [[ ! -f "$DAEMON_JSON" ]]; then
    log "writing new $DAEMON_JSON"
    printf '%s' "$DESIRED_DAEMON_JSON" > "$DAEMON_JSON"
    chmod 0644 "$DAEMON_JSON"
    daemon_changed=1
else
    # Compare semantically if jq is available; fall back to byte-compare.
    if command -v jq >/dev/null 2>&1; then
        current_canon="$(jq -S . "$DAEMON_JSON" 2>/dev/null || echo '__INVALID__')"
        desired_canon="$(printf '%s' "$DESIRED_DAEMON_JSON" | jq -S .)"
        if [[ "$current_canon" != "$desired_canon" ]]; then
            log "updating $DAEMON_JSON (content differs)"
            cp -a "$DAEMON_JSON" "${DAEMON_JSON}.bak.$(date +%s)"
            printf '%s' "$DESIRED_DAEMON_JSON" > "$DAEMON_JSON"
            chmod 0644 "$DAEMON_JSON"
            daemon_changed=1
        else
            log "$DAEMON_JSON already matches desired config"
        fi
    else
        if ! diff -q <(printf '%s' "$DESIRED_DAEMON_JSON") "$DAEMON_JSON" >/dev/null 2>&1; then
            log "updating $DAEMON_JSON (byte-level diff; jq not installed)"
            cp -a "$DAEMON_JSON" "${DAEMON_JSON}.bak.$(date +%s)"
            printf '%s' "$DESIRED_DAEMON_JSON" > "$DAEMON_JSON"
            chmod 0644 "$DAEMON_JSON"
            daemon_changed=1
        else
            log "$DAEMON_JSON already matches desired config"
        fi
    fi
fi

if [[ "$daemon_changed" -eq 1 ]]; then
    log "restarting docker (daemon.json changed)"
    systemctl restart docker
fi

# ---------- 5. edge network ----------------------------------------------

if docker network inspect "$EDGE_NET" >/dev/null 2>&1; then
    log "docker network '$EDGE_NET' already exists"
else
    log "creating docker network '$EDGE_NET' (bridge)"
    docker network create --driver bridge "$EDGE_NET"
fi

# ---------- 6. verification ----------------------------------------------

echo
echo "===== docker version ====="
docker version || true
echo
echo "===== docker network ls ====="
docker network ls
echo
echo "===== docker info | grep -i 'logging driver' ====="
docker info 2>/dev/null | grep -i 'logging driver' || true
echo
log "done."
