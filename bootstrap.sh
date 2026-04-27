#!/usr/bin/env bash
# bootstrap.sh — raph-server-installer Phase 1 entrypoint.
#
# Public face of the installer. Invoked by the operator from the LayerStack
# (or any cloud-init) "user-data" box on a fresh Ubuntu 24.04 VPS:
#
#   curl -fsSL https://raw.githubusercontent.com/jazari-akuna/raph-server-installer/main/bootstrap.sh \
#     | DOMAIN=example.com bash
#
# Phase 1 responsibilities:
#   - Validate env, install base packages.
#   - Clone the installer repo to /opt/raph-server-installer.
#   - Render templates, run host hardening + Docker install.
#   - Schedule Phase 2 (post-reboot) via bootstrap-continue.service.
#   - Reboot.
#
# Phase 2 is in scripts/bootstrap-phase2.sh — it runs once after the reboot
# (DKMS-loaded kernel) and brings the Docker stacks up so the operator's
# browser can reach <domain>/setup.
#
# Idempotent: if /srv/store/.bootstrap-phase1-complete exists, skip the
# heavy steps and just (re-)arm the continue unit + reboot.
#
# Logs: tee'd to stdout (LayerStack console) AND
# /var/log/raph-installer/phase1.log so the operator can inspect after reboot.

set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────
# Constants
# ──────────────────────────────────────────────────────────────────────────

REPO_URL="${REPO_URL:-https://github.com/jazari-akuna/raph-server-installer.git}"
REPO_BRANCH="${REPO_BRANCH:-main}"
REPO_DIR="/opt/raph-server-installer"
STACKS_DIR="/opt/stacks"
ENV_FILE="${STACKS_DIR}/.env"
STATE_DIR="/srv/store"
PHASE1_DONE="${STATE_DIR}/.bootstrap-phase1-complete"
LOG_DIR="/var/log/raph-installer"
LOG_FILE="${LOG_DIR}/phase1.log"
TOKEN_DIR="/etc/raph-installer"
TOKEN_FILE="${TOKEN_DIR}/setup-token"
LUKS_DIR="/etc/luks"
CONTINUE_UNIT="bootstrap-continue.service"

# ──────────────────────────────────────────────────────────────────────────
# Bootstrap logging — tee everything to LOG_FILE + stdout
# ──────────────────────────────────────────────────────────────────────────

# Set up logging BEFORE any user-visible work so even early failures land
# in the file. Re-exec under a tee so subshells / sub-processes inherit
# the redirection.
if [[ -z "${RAPH_BOOTSTRAP_LOG_INITIALIZED:-}" ]]; then
  install -d -m 0755 "$LOG_DIR" 2>/dev/null || mkdir -p "$LOG_DIR"
  export RAPH_BOOTSTRAP_LOG_INITIALIZED=1
  # Append (don't truncate) so a re-run preserves the prior log.
  exec > >(tee -a "$LOG_FILE") 2>&1
fi

ts() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
log()  { printf '[bootstrap %s] %s\n' "$(ts)" "$*"; }
fail() { printf '[bootstrap %s] FATAL: %s\n' "$(ts)" "$*" >&2; exit 1; }

log "===== raph-server-installer bootstrap (phase 1) starting ====="
log "log: $LOG_FILE"

# ──────────────────────────────────────────────────────────────────────────
# Preflight
# ──────────────────────────────────────────────────────────────────────────

[[ ${EUID:-$(id -u)} -eq 0 ]] || fail "must run as root"

if [[ -r /etc/os-release ]]; then
  # shellcheck disable=SC1091
  . /etc/os-release
  [[ "${ID:-}" == "ubuntu" ]] || fail "Ubuntu only; found ID=${ID:-unknown}"
  if [[ "${VERSION_CODENAME:-}" != "noble" ]]; then
    log "WARNING: expected Ubuntu 24.04 (noble), got '${VERSION_CODENAME:-?}' — continuing"
  fi
else
  fail "/etc/os-release missing — cannot identify distro"
fi

# ──────────────────────────────────────────────────────────────────────────
# Required env (DOMAIN). Defaults for the rest.
# ──────────────────────────────────────────────────────────────────────────

DOMAIN="${DOMAIN:-}"
[[ -n "$DOMAIN" ]] || fail "DOMAIN env var is required (e.g. DOMAIN=example.com)"
[[ "$DOMAIN" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]] \
  || fail "DOMAIN '$DOMAIN' does not look like a valid lowercase domain"
export DOMAIN

# ADMIN_USERS defaults to "admin" for the turnkey flow (one-admin install).
# Operators wanting to pre-seed multiple admins can pass a whitespace-
# separated list. ADMIN_USERS_SSH defaults to ADMIN_USERS.
export ADMIN_USERS="${ADMIN_USERS:-admin}"
export ADMIN_USERS_SSH="${ADMIN_USERS_SSH:-$ADMIN_USERS}"

# ADMIN_EMAIL is collected by the wizard ultimately, but if the operator
# pre-seeds it we honour it. Default to the conventional postmaster@.
export ADMIN_EMAIL="${ADMIN_EMAIL:-admin@${DOMAIN}}"

# Defaults for placeholder-required vars so render-templates.sh doesn't
# blow up before the wizard supplies the real values. The wizard re-renders
# templates at the end of Phase 2 (via render-templates.sh) with the real
# values; until then these are intentionally placeholders.
export QEDGE_PASSWORD="${QEDGE_PASSWORD:-bootstrap-placeholder-rotate-in-wizard}"
export AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH="${AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH:-\$pbkdf2-sha512\$bootstrap-placeholder}"

# Setup token: persistent random secret printed to console at end of
# Phase 2; the wizard requires it as an out-of-band check that whoever's
# at /setup is the operator who pasted the bootstrap command.
install -d -m 0700 "$TOKEN_DIR"
if [[ -n "${SETUP_TOKEN:-}" ]]; then
  printf '%s\n' "$SETUP_TOKEN" > "$TOKEN_FILE"
elif [[ ! -s "$TOKEN_FILE" ]]; then
  # 32 url-safe chars: tr -dc with /dev/urandom. head -c keeps it bounded.
  SETUP_TOKEN="$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)"
  printf '%s\n' "$SETUP_TOKEN" > "$TOKEN_FILE"
fi
chmod 0600 "$TOKEN_FILE"
SETUP_TOKEN="$(cat "$TOKEN_FILE")"
export SETUP_TOKEN

log "DOMAIN=$DOMAIN ADMIN_USERS='$ADMIN_USERS' SETUP_TOKEN=(in $TOKEN_FILE)"

# ──────────────────────────────────────────────────────────────────────────
# Persistent host config — write /etc/server-domain so every script that
# reads it (provision-peer.sh, cert-renewal-hook.sh, etc.) Just Works.
# ──────────────────────────────────────────────────────────────────────────

install -d -m 0755 /etc
printf '%s\n' "$DOMAIN" > /etc/server-domain
chmod 0644 /etc/server-domain
printf '%s\n' "$ADMIN_EMAIL" > /etc/server-admin-email
chmod 0644 /etc/server-admin-email

# Coordination with Parcel 2B: shared LUKS keyfile lives under /etc/luks/.
# Both phase 1 and the shared-volume scripts expect this dir to exist with
# strict perms. Idempotent.
install -d -m 0700 -o root -g root "$LUKS_DIR"

# /srv/store is the persistent state anchor (sentinels + data dirs).
install -d -m 0755 /srv/store
install -d -m 0755 /srv/store/data
install -d -m 0755 /srv/store/mnt
install -d -m 0755 /srv/secrets

# ──────────────────────────────────────────────────────────────────────────
# Idempotency: short-circuit if Phase 1 has already completed.
# ──────────────────────────────────────────────────────────────────────────

if [[ -f "$PHASE1_DONE" ]]; then
  log "Phase 1 sentinel present ($PHASE1_DONE) — skipping heavy steps"
  log "Re-arming bootstrap-continue.service in case it was disabled by hand"
  if [[ -f "/etc/systemd/system/${CONTINUE_UNIT}" ]]; then
    systemctl daemon-reload
    systemctl enable "${CONTINUE_UNIT}" >/dev/null 2>&1 || true
  fi
  log "Phase 1 idempotent re-run complete; rebooting in 10s to fire Phase 2"
  sleep 10
  systemctl reboot
  exit 0
fi

# ──────────────────────────────────────────────────────────────────────────
# Phase 1a — base packages (minimal; Docker etc. comes from install-docker.sh).
# ──────────────────────────────────────────────────────────────────────────

log "==> apt-get update + base packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y \
  git \
  curl \
  ca-certificates \
  gnupg \
  jq \
  gettext-base \
  cryptsetup \
  e2fsprogs \
  rsync \
  python3 \
  iproute2 \
  iptables \
  ufw

# ──────────────────────────────────────────────────────────────────────────
# Phase 1b — clone (or update) the installer repo.
# ──────────────────────────────────────────────────────────────────────────

if [[ -d "${REPO_DIR}/.git" ]]; then
  log "==> repo already cloned, fetching latest from origin/${REPO_BRANCH}"
  git -C "$REPO_DIR" fetch --depth=1 origin "$REPO_BRANCH"
  git -C "$REPO_DIR" reset --hard "origin/${REPO_BRANCH}"
else
  log "==> cloning ${REPO_URL} (branch ${REPO_BRANCH}) -> ${REPO_DIR}"
  install -d -m 0755 "$(dirname "$REPO_DIR")"
  git clone --depth=1 --branch "$REPO_BRANCH" "$REPO_URL" "$REPO_DIR"
fi

# Compatibility symlinks: existing scripts (bootstrap-host.sh, install-gw0.sh)
# reference /root/host. Maintain that path without copying files.
ln -sfn "$REPO_DIR/host" /root/host
ln -sfn "$REPO_DIR/scripts" /root/scripts

# /opt/stacks is the canonical compose root referenced by docs and unit
# files. Symlink (not bind-mount) keeps `git pull` semantics intact.
install -d -m 0755 /opt
ln -sfn "$REPO_DIR/stacks" "$STACKS_DIR"

# ──────────────────────────────────────────────────────────────────────────
# Phase 1c — render /opt/stacks/.env from .env.example + collected vars.
# ──────────────────────────────────────────────────────────────────────────

log "==> writing $ENV_FILE"
# Honour any existing .env (operator may have pre-staged one); only overwrite
# if absent. Wave 2A keeps it simple: the wizard owns the long-lived .env.
if [[ -f "$ENV_FILE" ]]; then
  log "    $ENV_FILE exists; preserving (operator-supplied)"
else
  install -d -m 0750 "$STACKS_DIR"
  # Build a minimal env from the values we have at Phase 1. The wizard
  # appends/overwrites OVH creds, NPM admin, OIDC hash etc. in Phase 2.
  cat > "$ENV_FILE" <<EOF
# raph-server-installer — generated by bootstrap.sh phase 1
# Wizard (Phase 2) appends provider creds + secrets; do not hand-edit
# unless you know what you're doing.
DOMAIN=${DOMAIN}
ADMIN_EMAIL=${ADMIN_EMAIL}
ADMIN_USERS=${ADMIN_USERS}
ADMIN_USERS_SSH=${ADMIN_USERS_SSH}
ENROL_DOMAIN=${DOMAIN}
SETUP_TOKEN=${SETUP_TOKEN}
QEDGE_PASSWORD=${QEDGE_PASSWORD}
AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH=${AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH}
EOF
  chmod 0640 "$ENV_FILE"
  log "    wrote $ENV_FILE"
fi

# ──────────────────────────────────────────────────────────────────────────
# Phase 1d — render every .template into its rendered sibling.
# ──────────────────────────────────────────────────────────────────────────

log "==> rendering templates"
"$REPO_DIR/scripts/render-templates.sh" --env-file "$ENV_FILE"

# ──────────────────────────────────────────────────────────────────────────
# Phase 1e — base host hardening (existing script, unchanged surface).
# ──────────────────────────────────────────────────────────────────────────

log "==> running scripts/bootstrap-host.sh"
ADMIN_USERS="$ADMIN_USERS" \
  bash "$REPO_DIR/scripts/bootstrap-host.sh"

# ──────────────────────────────────────────────────────────────────────────
# Phase 1f — Docker.
# ──────────────────────────────────────────────────────────────────────────

log "==> running scripts/install-docker.sh"
ADMIN_USERS="$ADMIN_USERS" \
  bash "$REPO_DIR/scripts/install-docker.sh"

# ──────────────────────────────────────────────────────────────────────────
# Phase 1g — install bootstrap-continue.service (Phase 2 trigger).
# ──────────────────────────────────────────────────────────────────────────

log "==> installing ${CONTINUE_UNIT}"
install -m 0644 \
  "$REPO_DIR/host/systemd/${CONTINUE_UNIT}" \
  "/etc/systemd/system/${CONTINUE_UNIT}"
systemctl daemon-reload
systemctl enable "${CONTINUE_UNIT}"

# ──────────────────────────────────────────────────────────────────────────
# Phase 1 complete.
# ──────────────────────────────────────────────────────────────────────────

date -u '+%Y-%m-%dT%H:%M:%SZ' > "$PHASE1_DONE"
log "Phase 1 sentinel: $PHASE1_DONE"

cat <<EOF

================================================================
Phase 1 complete. Rebooting in 10 seconds.
================================================================
After reboot, ${CONTINUE_UNIT} runs scripts/bootstrap-phase2.sh
which brings up the Docker stacks and opens the setup wizard at:

    http://${DOMAIN}/setup
    (or http://<vps-ip>/setup if DNS hasn't propagated)

Setup token (saved at $TOKEN_FILE):
    ${SETUP_TOKEN}

Phase 1 log: $LOG_FILE
Phase 2 log will be: ${LOG_DIR}/phase2.log
================================================================
EOF

sleep 10
log "rebooting now"
systemctl reboot
