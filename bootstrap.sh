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
# browser can reach the wizard at http://setup.<domain>/.
#
# Idempotent: if /srv/store/.bootstrap-phase1-complete exists, skip the
# heavy steps and just (re-)arm the continue unit + reboot.
#
# Logs: tee'd to stdout (LayerStack console) AND
# /var/log/raph-installer/phase1.log so the operator can inspect after reboot.

# ──────────────────────────────────────────────────────────────────────────
# Strict mode + structured failure reporting (shared lib)
# ──────────────────────────────────────────────────────────────────────────
#
# bootstrap.sh is unique: it can be piped from `curl | bash` on a fresh
# host where the repo isn't on disk yet, so the lib may not exist on the
# very first invocation. We try to source it from a few likely locations.
# Until the lib is loaded we use plain `set -euo pipefail` and a minimal
# inline ERR trap so even early failures surface clearly. After Phase 1b
# (clone) we re-source the lib from the cloned tree.

set -euo pipefail
trap 'rc=$?; printf "\nFATAL: bootstrap.sh line %s: %s (rc=%s)\n" "${BASH_LINENO[0]}" "${BASH_COMMAND}" "$rc" >&2; exit "$rc"' ERR

_strict_lib=""
for _candidate in \
    "$(dirname -- "${BASH_SOURCE[0]:-$0}" 2>/dev/null)/scripts/lib/strict.sh" \
    "/opt/raph-server-installer/scripts/lib/strict.sh"; do
  if [[ -r "$_candidate" ]]; then
    _strict_lib="$_candidate"
    break
  fi
done
if [[ -n "$_strict_lib" ]]; then
  # shellcheck source=scripts/lib/strict.sh
  . "$_strict_lib"
  strict_enable
  STRICT_SCRIPT_NAME="bootstrap.sh"
fi
unset _strict_lib _candidate

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

# Tell the strict-lib trap (if loaded) where our log lives so failures
# print the last 20 lines of context. No-op when lib isn't sourced yet.
if declare -F strict_set_log >/dev/null 2>&1; then
  strict_set_log "$LOG_FILE"
fi

ts() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
log()  { printf '[bootstrap %s] %s\n' "$(ts)" "$*"; }
fail() { printf '[bootstrap %s] FATAL: %s\n' "$(ts)" "$*" >&2; exit 1; }

log "===== raph-server-installer bootstrap (phase 1) starting ====="
log "log: $LOG_FILE"

# TEST_MODE: when set to 1 (by the tests/ harness), the installer skips
# irreversible host-mutating actions — actual reboots, real cert issuance,
# real DKMS builds. Each skip emits a `TEST_MODE: skipping <thing>` line so
# failures are debuggable. The script otherwise behaves identically.
# Production runs leave TEST_MODE unset.
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  log "TEST_MODE=1: irreversible actions will be short-circuited (search 'TEST_MODE: skipping')"
fi

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

# App opt-outs. SKIP_CLOUD=1 leaves out the file-storage stack (cloud);
# SKIP_TASK=1 leaves out the project-tracker stack (task). Both default
# to installed. Persisted to /opt/stacks/.env below so Phase 2 (which
# runs post-reboot from systemd, without the operator's shell env) and
# the enrol wizard see the same choice.
export SKIP_CLOUD="${SKIP_CLOUD:-0}"
export SKIP_TASK="${SKIP_TASK:-0}"

# Defaults for placeholder-required vars so render-templates.sh doesn't
# blow up before the wizard supplies the real values. The wizard re-renders
# templates at the end of Phase 2 (via render-templates.sh) with the real
# values; until then these are intentionally placeholders.
export QEDGE_PASSWORD="${QEDGE_PASSWORD:-bootstrap-placeholder-rotate-in-wizard}"
export AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH="${AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH:-\$pbkdf2-sha512\$bootstrap-placeholder}"
export AUTHELIA_OIDC_CLOUD_CLIENT_SECRET_HASH="${AUTHELIA_OIDC_CLOUD_CLIENT_SECRET_HASH:-\$pbkdf2-sha512\$bootstrap-placeholder}"
export AUTHELIA_OIDC_TASK_CLIENT_SECRET_HASH="${AUTHELIA_OIDC_TASK_CLIENT_SECRET_HASH:-\$pbkdf2-sha512\$bootstrap-placeholder}"

# Setup token: persistent random secret printed to console at end of
# Phase 2; the wizard requires it as an out-of-band check that whoever's
# at http://setup.${DOMAIN}/ is the operator who pasted the bootstrap
# command.
install -d -m 0700 "$TOKEN_DIR"
if [[ -n "${SETUP_TOKEN:-}" ]]; then
  printf '%s\n' "$SETUP_TOKEN" > "$TOKEN_FILE"
elif [[ ! -s "$TOKEN_FILE" ]]; then
  # 32 alphanumeric chars from /dev/urandom. We deliberately AVOID the
  # `tr -dc … </dev/urandom | head -c 32` idiom: under `set -o pipefail`
  # the early SIGPIPE that `head` raises on `tr` causes the entire pipeline
  # to be reported as failed, then `set -e` exits the script even though
  # the token was generated correctly. (Discovered by tests/ harness.)
  # Toggle pipefail off just for this one substitution; the rest of the
  # script keeps the strict mode.
  set +o pipefail
  SETUP_TOKEN="$(tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 32)"
  set -o pipefail
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

# /srv/store is the persistent state anchor (sentinels + per-stack
# bind-mount targets). Per ADR-001, every stack's persistent state lives
# under /srv/store/<stack>-* so backup is a single rsync per box.
install -d -m 0755 /srv/store
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
  if [[ "${TEST_MODE:-0}" == "1" ]]; then
    log "TEST_MODE: skipping reboot on idempotent re-run (touching .test-reboot-requested)"
    : > "${STATE_DIR}/.test-reboot-requested"
    exit 0
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
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  log "TEST_MODE: skipping apt-get update + install of base packages (git curl ca-certificates gnupg jq gettext-base rsync python3 iproute2 iptables ufw)"
else
  apt-get update -y
  apt-get install -y \
    git \
    curl \
    ca-certificates \
    gnupg \
    jq \
    gettext-base \
    rsync \
    python3 \
    iproute2 \
    iptables \
    ufw
fi

# ──────────────────────────────────────────────────────────────────────────
# Phase 1b — clone (or update) the installer repo.
# ──────────────────────────────────────────────────────────────────────────

if [[ "${TEST_MODE:-0}" == "1" && -n "${TEST_REPO_SRC:-}" ]]; then
  log "TEST_MODE: skipping git clone; symlinking ${TEST_REPO_SRC} -> ${REPO_DIR}"
  install -d -m 0755 "$(dirname "$REPO_DIR")"
  # Remove any prior symlink/dir so the new symlink takes effect.
  if [[ -L "$REPO_DIR" || -d "$REPO_DIR" ]]; then
    rm -rf "$REPO_DIR"
  fi
  ln -sfn "$TEST_REPO_SRC" "$REPO_DIR"
elif [[ -d "${REPO_DIR}/.git" ]]; then
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

# Re-source the strict lib now that the repo exists on disk. The early
# inline trap above is replaced by the richer structured one. No-op if
# the lib was already loaded (it guards against double-source).
if [[ -z "${RAPH_STRICT_LIB_LOADED:-}" && -r "$REPO_DIR/scripts/lib/strict.sh" ]]; then
  # shellcheck source=scripts/lib/strict.sh
  . "$REPO_DIR/scripts/lib/strict.sh"
  strict_enable
  STRICT_SCRIPT_NAME="bootstrap.sh"
  strict_set_log "$LOG_FILE"
fi

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
  # NOTE: values are single-quoted to survive both bash sourcing
  # (`set -a; . .env; set +a` from render-templates.sh) and docker-compose's
  # .env reader. Single quotes preserve `$pbkdf2-…` placeholders verbatim
  # rather than letting bash expand `$pbkdf2` into "". Compose strips the
  # outer single quotes per the compose-spec env-file format.
  cat > "$ENV_FILE" <<EOF
# raph-server-installer — generated by bootstrap.sh phase 1
# Wizard (Phase 2) appends provider creds + secrets; do not hand-edit
# unless you know what you're doing.
DOMAIN='${DOMAIN}'
ADMIN_EMAIL='${ADMIN_EMAIL}'
ADMIN_USERS='${ADMIN_USERS}'
ADMIN_USERS_SSH='${ADMIN_USERS_SSH}'
SKIP_CLOUD='${SKIP_CLOUD}'
SKIP_TASK='${SKIP_TASK}'
ENROL_DOMAIN='${DOMAIN}'
SETUP_TOKEN='${SETUP_TOKEN}'
QEDGE_PASSWORD='${QEDGE_PASSWORD}'
AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH='${AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH}'
AUTHELIA_OIDC_CLOUD_CLIENT_SECRET_HASH='${AUTHELIA_OIDC_CLOUD_CLIENT_SECRET_HASH}'
AUTHELIA_OIDC_TASK_CLIENT_SECRET_HASH='${AUTHELIA_OIDC_TASK_CLIENT_SECRET_HASH}'
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
if declare -F strict_step >/dev/null 2>&1; then strict_step "bootstrap-host.sh"; fi
if declare -F run_subscript >/dev/null 2>&1; then
  ADMIN_USERS="$ADMIN_USERS" \
    run_subscript "$REPO_DIR/scripts/bootstrap-host.sh"
else
  ADMIN_USERS="$ADMIN_USERS" \
    bash "$REPO_DIR/scripts/bootstrap-host.sh"
fi

# ──────────────────────────────────────────────────────────────────────────
# Phase 1f — Docker.
# ──────────────────────────────────────────────────────────────────────────

log "==> running scripts/install-docker.sh"
if declare -F strict_step >/dev/null 2>&1; then strict_step "install-docker.sh"; fi
if declare -F run_subscript >/dev/null 2>&1; then
  ADMIN_USERS="$ADMIN_USERS" \
    run_subscript "$REPO_DIR/scripts/install-docker.sh"
else
  ADMIN_USERS="$ADMIN_USERS" \
    bash "$REPO_DIR/scripts/install-docker.sh"
fi

# ──────────────────────────────────────────────────────────────────────────
# Phase 1g — install bootstrap-continue.service (Phase 2 trigger).
# ──────────────────────────────────────────────────────────────────────────

log "==> installing ${CONTINUE_UNIT}"
install -d -m 0755 /etc/systemd/system
install -m 0644 \
  "$REPO_DIR/host/systemd/${CONTINUE_UNIT}" \
  "/etc/systemd/system/${CONTINUE_UNIT}"
if [[ "${TEST_MODE:-0}" == "1" ]] && ! command -v systemctl >/dev/null 2>&1; then
  log "TEST_MODE: skipping systemctl daemon-reload + enable ${CONTINUE_UNIT}"
else
  systemctl daemon-reload
  systemctl enable "${CONTINUE_UNIT}"
fi

# ──────────────────────────────────────────────────────────────────────────
# Phase 1 complete.
# ──────────────────────────────────────────────────────────────────────────

date -u '+%Y-%m-%dT%H:%M:%SZ' > "$PHASE1_DONE"
chmod 0644 "$PHASE1_DONE"   # why: sentinel is parsed by phase 2 preflight; world-readable mtime is fine
log "Phase 1 sentinel: $PHASE1_DONE"

cat <<EOF

================================================================
Phase 1 complete. Rebooting in 10 seconds.
================================================================
After reboot, ${CONTINUE_UNIT} runs scripts/bootstrap-phase2.sh
which brings up the Docker stacks and opens the setup wizard at:

    https://setup.${DOMAIN}/

The wizard ships behind a self-signed cert until you finish the
finalize step, so your browser will show a "not secure" warning on
first visit — click through ("Advanced → Proceed"). The wizard's
finalize step issues a real wildcard cert via DNS-01 and replaces it.

Phase 1 log: $LOG_FILE
Phase 2 log will be: ${LOG_DIR}/phase2.log
================================================================
EOF

if [[ "${TEST_MODE:-0}" == "1" ]]; then
  log "TEST_MODE: skipping systemctl reboot (touching .test-reboot-requested instead)"
  : > "${STATE_DIR}/.test-reboot-requested"
  exit 0
fi

sleep 10
log "rebooting now"
systemctl reboot
