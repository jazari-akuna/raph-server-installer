#!/usr/bin/env bash
# bootstrap-phase2.sh — Phase 2 entrypoint, fired post-reboot by
# bootstrap-continue.service.
#
# Responsibilities (Wave 2A):
#   - Verify the AmneziaWG kernel module loaded post-reboot (DKMS done).
#     Fail loudly if not — the stacks won't function without it.
#   - Conditionally call install-gw0.sh (gated by SKIP_GW0; default 1 = skip).
#   - Bring up the Docker stacks in dependency order.
#   - Wait for enrol /healthz.
#   - Open port 80 long enough for the wizard to be reachable.
#   - Touch /srv/store/.bootstrap-phase2-complete.
#   - Disable bootstrap-continue.service so it doesn't re-fire.
#
# Idempotent: re-runs are safe — `docker compose up -d` is idempotent;
# enable/disable of the unit is too.
#
# Logs: tee'd to /var/log/raph-installer/phase2.log + stdout.

set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────
# Constants
# ──────────────────────────────────────────────────────────────────────────

REPO_DIR="${REPO_DIR:-/opt/raph-server-installer}"
STACKS_DIR="${STACKS_DIR:-/opt/stacks}"
ENV_FILE="${ENV_FILE:-${STACKS_DIR}/.env}"
STATE_DIR="/srv/store"
PHASE2_DONE="${STATE_DIR}/.bootstrap-phase2-complete"
LOG_DIR="/var/log/raph-installer"
LOG_FILE="${LOG_DIR}/phase2.log"
TOKEN_FILE="/etc/raph-installer/setup-token"
CONTINUE_UNIT="bootstrap-continue.service"

# ──────────────────────────────────────────────────────────────────────────
# Logging
# ──────────────────────────────────────────────────────────────────────────

if [[ -z "${RAPH_PHASE2_LOG_INITIALIZED:-}" ]]; then
  install -d -m 0755 "$LOG_DIR"
  export RAPH_PHASE2_LOG_INITIALIZED=1
  exec > >(tee -a "$LOG_FILE") 2>&1
fi

ts() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
log()  { printf '[phase2 %s] %s\n' "$(ts)" "$*"; }
fail() { printf '[phase2 %s] FATAL: %s\n' "$(ts)" "$*" >&2; exit 1; }

log "===== raph-server-installer bootstrap (phase 2) starting ====="

# ──────────────────────────────────────────────────────────────────────────
# Preflight
# ──────────────────────────────────────────────────────────────────────────

[[ ${EUID:-$(id -u)} -eq 0 ]] || fail "must run as root"

# Source .env so DOMAIN etc. are available.
if [[ -f "$ENV_FILE" ]]; then
  # shellcheck disable=SC1090
  set -a; . "$ENV_FILE"; set +a
else
  fail "$ENV_FILE missing — Phase 1 didn't complete"
fi
[[ -n "${DOMAIN:-}" ]] || fail "DOMAIN unset in $ENV_FILE"

# Idempotent short-circuit.
if [[ -f "$PHASE2_DONE" ]]; then
  log "Phase 2 sentinel present ($PHASE2_DONE) — nothing to do"
  systemctl disable "${CONTINUE_UNIT}" >/dev/null 2>&1 || true
  exit 0
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 1 — verify AmneziaWG kernel module is loadable.
# ──────────────────────────────────────────────────────────────────────────

log "==> verifying amneziawg kernel module"
if ! modinfo amneziawg >/dev/null 2>&1; then
  fail "amneziawg kernel module not present after reboot — DKMS build failed. \
Inspect /var/lib/dkms/amneziawg/*/build/make.log and the install-gw0.sh \
remediation block."
fi
log "    amneziawg modinfo OK"

# Don't load it pre-emptively — install-gw0.sh / awg-quick will pull it in
# when needed. modprobe-test it to surface any latent load failure now.
if ! modprobe -n amneziawg >/dev/null 2>&1; then
  log "    WARNING: modprobe -n amneziawg failed; the module may still load \
when awg-quick runs but this is suspicious"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 2 — gw0 install (opt-in via SKIP_GW0=0).
# ──────────────────────────────────────────────────────────────────────────

# Default: SKIP. Regional-split + qedge are opt-in features (blueprint § G1).
# The wizard can re-run install-gw0.sh later if the operator turns it on.
SKIP_GW0="${SKIP_GW0:-1}"
if [[ "$SKIP_GW0" == "1" ]]; then
  log "==> SKIP_GW0=1 — skipping install-gw0.sh (regional-split is opt-in)"
else
  log "==> running scripts/install-gw0.sh (SKIP_GW0=0)"
  bash "$REPO_DIR/scripts/install-gw0.sh"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 3 — shared volume (Parcel 2B).
# ──────────────────────────────────────────────────────────────────────────

# Coordinated with Parcel 2B: scripts/create-shared-volume.sh creates and
# mounts the shared LUKS-on-loopback volume that copyparty needs for its
# [/shared] block. If 2B's script isn't present yet, log and continue —
# cloud will start without /shared and the wizard can finalise later.
SHARED_SCRIPT="$REPO_DIR/scripts/create-shared-volume.sh"
SHARED_UNIT_SRC="$REPO_DIR/host/systemd/shared-store.service"
SHARED_UNIT_DST="/etc/systemd/system/shared-store.service"
if [[ -x "$SHARED_SCRIPT" ]]; then
  log "==> creating shared volume via $SHARED_SCRIPT"
  if ! bash "$SHARED_SCRIPT"; then
    log "    WARNING: $SHARED_SCRIPT exited non-zero; cloud may start \
without /shared. Investigate before announcing setup-ready."
  fi
  # Install + enable the boot-time auto-mount unit so /shared comes back
  # after every reboot. We do NOT --now: the create script already left
  # the volume mounted in this boot; `start` would fail because the
  # mapper is open. The unit fires cleanly on next reboot.
  if [[ -f "$SHARED_UNIT_SRC" ]]; then
    install -m 0644 "$SHARED_UNIT_SRC" "$SHARED_UNIT_DST"
    systemctl daemon-reload
    systemctl enable shared-store.service >/dev/null 2>&1 || \
      log "    WARNING: enable shared-store.service failed; volume won't auto-mount on reboot"
  fi
else
  log "==> $SHARED_SCRIPT not present (Parcel 2B); skipping shared volume"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 4 — bring up Docker stacks in dependency order.
# ──────────────────────────────────────────────────────────────────────────

# Order matters:
#   ingress  — nginx-proxy-manager, owns the edge network and :80/:443.
#   authelia — SSO; cloud + console need it for forward-auth.
#   cloud    — copyparty (uses /shared if present).
#   console  — Portainer (OIDC via authelia).
#   enrol    — wizard + day-2 admin UI, exposes /healthz.
#   qedge    — only if explicitly opted in (SKIP_QEDGE=0); default off.
compose_up() {
  local stack="$1"
  local file="$STACKS_DIR/$stack/docker-compose.yml"
  if [[ ! -f "$file" ]]; then
    log "    skipping '$stack' — $file not present"
    return 0
  fi
  log "==> docker compose up -d ($stack)"
  # --remove-orphans cleans up containers from a previous compose layout.
  # Run inside the stack dir so any relative bind mounts resolve correctly.
  ( cd "$STACKS_DIR/$stack" && \
    docker compose --env-file "$ENV_FILE" -f "$file" up -d --remove-orphans )
}

compose_up ingress
compose_up authelia
compose_up cloud
compose_up console
compose_up enrol

SKIP_QEDGE="${SKIP_QEDGE:-1}"
if [[ "$SKIP_QEDGE" != "1" && -d "$STACKS_DIR/qedge" ]]; then
  log "==> SKIP_QEDGE=0; bringing up qedge"
  compose_up qedge
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 5 — wait for enrol /healthz.
# ──────────────────────────────────────────────────────────────────────────

# enrol runs in network_mode: host (per Wave 1A scrub). Health endpoint
# binds to 172.17.0.1:8080 in normal mode; in setup mode it's on :80
# directly. Try both endpoints so we don't depend on which mode it's in
# right now (the wizard finalisation flips the mode). 30s budget.
log "==> waiting for enrol /healthz (timeout 30s)"
ENROL_OK=0
for i in $(seq 1 30); do
  for url in \
    "http://127.0.0.1/healthz" \
    "http://172.17.0.1:8080/healthz" \
    "http://127.0.0.1:8080/healthz"; do
    if curl -fsS -o /dev/null --max-time 2 "$url" 2>/dev/null; then
      log "    enrol healthy at $url (after ${i}s)"
      ENROL_OK=1
      break 2
    fi
  done
  sleep 1
done
if [[ $ENROL_OK -eq 0 ]]; then
  log "    WARNING: enrol /healthz did not respond within 30s — wizard \
may not be reachable yet. Check 'docker compose -f \
$STACKS_DIR/enrol/docker-compose.yml logs'."
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 6 — open port 80 for the setup wizard.
# ──────────────────────────────────────────────────────────────────────────

# bootstrap-host.sh already staged `ufw allow 80/tcp` (and ufw is staged
# but disabled). At this point Docker has its own iptables chains in
# place, so we make sure port 80 is reachable. Belt-and-braces:
#   1. ensure ufw allows 80/tcp (no-op if already allowed; harmless if ufw
#      is inactive).
#   2. add a simple iptables INPUT ACCEPT rule on dport 80, if not already
#      present.
log "==> opening port 80 for setup wizard"
ufw allow 80/tcp >/dev/null 2>&1 || true
if command -v iptables >/dev/null 2>&1; then
  if ! iptables -C INPUT -p tcp --dport 80 -j ACCEPT 2>/dev/null; then
    iptables -I INPUT -p tcp --dport 80 -j ACCEPT
    log "    inserted iptables INPUT ACCEPT for tcp/80"
  else
    log "    iptables INPUT already accepts tcp/80"
  fi
fi
# Note: the wizard's finalisation step (Wave 3A) is responsible for closing
# port 80 once the wildcard cert is issued and NPM is fronting :443.

# ──────────────────────────────────────────────────────────────────────────
# Step 7 — finalise + announce.
# ──────────────────────────────────────────────────────────────────────────

date -u '+%Y-%m-%dT%H:%M:%SZ' > "$PHASE2_DONE"
log "Phase 2 sentinel: $PHASE2_DONE"

# Belt-and-suspenders: also disable the continue unit ourselves.
systemctl disable "${CONTINUE_UNIT}" >/dev/null 2>&1 || true

VPS_IP="$(curl -fsS --max-time 3 https://ifconfig.me 2>/dev/null \
  || ip -4 -o route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')"
TOKEN="$(cat "$TOKEN_FILE" 2>/dev/null || echo '<token-missing>')"

cat <<EOF

================================================================
Phase 2 complete. The setup wizard is now live.
================================================================

  URL  (preferred): http://${DOMAIN}/setup
  URL  (fallback):  http://${VPS_IP:-<vps-ip>}/setup

  Setup token:      ${TOKEN}
  Token saved at:   ${TOKEN_FILE}

  Phase 2 log:      ${LOG_FILE}

The wizard runs over plaintext HTTP until you finish step 3
(wildcard cert issuance). The setup token is the out-of-band
proof that you are the operator who pasted the bootstrap
command. After finalisation the wizard switches to HTTPS and
closes port 80.
================================================================
EOF
