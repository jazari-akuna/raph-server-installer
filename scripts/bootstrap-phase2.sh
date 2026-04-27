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
#   - Seed the `setup.${DOMAIN}` proxy host in NPM so the wizard answers
#     at `http://setup.${DOMAIN}/` (subdomain root, NOT apex `/setup`).
#   - Touch /srv/store/.bootstrap-phase2-complete.
#   - Disable bootstrap-continue.service so it doesn't re-fire.
#
# Idempotent: re-runs are safe — `docker compose up -d` is idempotent;
# enable/disable of the unit is too.
#
# Logs: tee'd to /var/log/raph-installer/phase2.log + stdout.

# ──────────────────────────────────────────────────────────────────────────
# Strict mode + structured failure reporting (shared lib)
# ──────────────────────────────────────────────────────────────────────────

# shellcheck source=lib/strict.sh
SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
. "$SCRIPT_DIR/lib/strict.sh"
strict_enable
STRICT_SCRIPT_NAME="bootstrap-phase2.sh"

# ──────────────────────────────────────────────────────────────────────────
# Constants
# ──────────────────────────────────────────────────────────────────────────

REPO_DIR="${REPO_DIR:-/opt/raph-server-installer}"
STACKS_DIR="${STACKS_DIR:-/opt/stacks}"
ENV_FILE="${ENV_FILE:-${STACKS_DIR}/.env}"
STATE_DIR="/srv/store"
PHASE1_DONE="${STATE_DIR}/.bootstrap-phase1-complete"
PHASE2_DONE="${STATE_DIR}/.bootstrap-phase2-complete"
LOG_DIR="/var/log/raph-installer"
LOG_FILE="${LOG_DIR}/phase2.log"
TOKEN_FILE="/etc/raph-installer/setup-token"
CONTINUE_UNIT="bootstrap-continue.service"

# ──────────────────────────────────────────────────────────────────────────
# Logging
# ──────────────────────────────────────────────────────────────────────────

if [[ -z "${RAPH_PHASE2_LOG_INITIALIZED:-}" ]]; then
  install -d -m 0755 "$LOG_DIR"   # why: log dir must exist before tee
  export RAPH_PHASE2_LOG_INITIALIZED=1
  exec > >(tee -a "$LOG_FILE") 2>&1
fi
strict_set_log "$LOG_FILE"

ts() { date -u '+%Y-%m-%dT%H:%M:%SZ'; }
log()  { printf '[phase2 %s] %s\n' "$(ts)" "$*"; }
fail() { printf '[phase2 %s] FATAL: %s\n' "$(ts)" "$*" >&2; exit 1; }

log "===== raph-server-installer bootstrap (phase 2) starting ====="
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  log "TEST_MODE=1: irreversible actions will be short-circuited"
fi

# ──────────────────────────────────────────────────────────────────────────
# Preflight
# ──────────────────────────────────────────────────────────────────────────

strict_step "preflight"
require_root

# External commands the script actually invokes below.
require_cmd docker curl jq install systemctl tee tail openssl
# In TEST_MODE the iptables/ufw probes are skipped; only require them outside.
if [[ "${TEST_MODE:-0}" != "1" ]]; then
  require_cmd iptables
fi

# Phase 1 sentinel must exist AND be non-empty (a zero-byte one is left
# behind by an interrupted phase 1 — refuse to honour it).
require_sentinel "$PHASE1_DONE"
require_file "$PHASE1_DONE"

# /opt/stacks symlink and .env file: phase 1 wrote them.
require_dir "$STACKS_DIR"
require_file "$ENV_FILE"

# Source .env so DOMAIN etc. are available.
strict_step "source $ENV_FILE"
# shellcheck disable=SC1090
set -a; . "$ENV_FILE"; set +a
require_env DOMAIN

# Idempotent short-circuit. Reject zero-byte sentinel leftover from a crash.
require_sentinel "$PHASE2_DONE"
if [[ -s "$PHASE2_DONE" ]]; then
  log "Phase 2 sentinel present ($PHASE2_DONE) — nothing to do"
  if command -v systemctl >/dev/null 2>&1; then
    # why: continue unit is one-shot; disable so it doesn't re-arm on next boot.
    systemctl disable "${CONTINUE_UNIT}" >/dev/null 2>&1 || true
  fi
  exit 0
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 1 — verify AmneziaWG kernel module is loadable.
# Only meaningful when gw0 (regional-split) is opted in; the default
# turnkey flow neither installs nor needs amneziawg, so checking for it
# would always fail and abort phase 2 before any docker stack came up.
# ──────────────────────────────────────────────────────────────────────────

SKIP_GW0="${SKIP_GW0:-1}"

if [[ "$SKIP_GW0" != "1" ]]; then
  strict_step "verify amneziawg kernel module"
  log "==> verifying amneziawg kernel module"
  if [[ "${TEST_MODE:-0}" == "1" ]]; then
    log "    TEST_MODE: skipping modinfo/modprobe amneziawg (DKMS not built in container)"
  else
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
  fi
else
  log "==> SKIP_GW0=1 — skipping amneziawg module verification (gw0 is opt-in)"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 2 — gw0 install (opt-in via SKIP_GW0=0).
# ──────────────────────────────────────────────────────────────────────────

# Default: SKIP. Regional-split + qedge are opt-in features (blueprint § G1).
# The wizard can re-run install-gw0.sh later if the operator turns it on.
if [[ "$SKIP_GW0" == "1" ]]; then
  log "==> SKIP_GW0=1 — skipping install-gw0.sh (regional-split is opt-in)"
else
  strict_step "install-gw0.sh"
  log "==> running scripts/install-gw0.sh (SKIP_GW0=0)"
  run_subscript "$REPO_DIR/scripts/install-gw0.sh"
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
  strict_step "create shared volume"
  log "==> creating shared volume via $SHARED_SCRIPT"
  # We deliberately do NOT abort phase 2 if the shared volume fails:
  # cloud (copyparty) starts fine without /shared, the wizard finalises
  # the volume later. We DO surface the rc clearly.
  set +e
  bash "$SHARED_SCRIPT"
  shared_rc=$?
  set -e
  if (( shared_rc != 0 )); then
    log "    WARNING: $SHARED_SCRIPT exited rc=$shared_rc; cloud will start"
    log "             without /shared. Investigate before announcing setup-ready"
    log "             (see 'docker logs cloud' and 'cryptsetup status store_shared')."
  fi
  # Install + enable the boot-time auto-mount unit so /shared comes back
  # after every reboot. We do NOT --now: the create script already left
  # the volume mounted in this boot; `start` would fail because the
  # mapper is open. The unit fires cleanly on next reboot.
  if [[ -f "$SHARED_UNIT_SRC" ]]; then
    install -d -m 0755 /etc/systemd/system   # why: standard systemd dir, idempotent
    install -m 0644 "$SHARED_UNIT_SRC" "$SHARED_UNIT_DST"
    if [[ "${TEST_MODE:-0}" == "1" ]] && ! command -v systemctl >/dev/null 2>&1; then
      log "    TEST_MODE: skipping systemctl daemon-reload + enable shared-store.service"
    else
      systemctl daemon-reload
      if ! systemctl enable shared-store.service >/dev/null 2>&1; then
        log "    WARNING: enable shared-store.service failed; volume won't auto-mount on reboot"
      fi
    fi
  fi
else
  log "==> $SHARED_SCRIPT not present (Parcel 2B); skipping shared volume"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 3.5 — generate Authelia secrets BEFORE compose-up authelia.
# ──────────────────────────────────────────────────────────────────────────
#
# stacks/authelia/docker-compose.yml has top-level `secrets:` blocks
# whose `file:` keys reference ./secrets/{jwt,session,storage,oidc}-secret
# and a bind-mount of ./secrets/oidc-key.pem. Compose v2 refuses to start
# the stack at all if any of those bind sources is missing. We generate
# them once, idempotently (re-runs leave existing secrets in place); the
# wizard never rotates them.

if [[ "${TEST_MODE:-0}" == "1" ]]; then
  log "==> TEST_MODE: skipping Authelia secrets generation"
else
  strict_step "generate Authelia secrets"
  log "==> generating Authelia secret bundle (idempotent)"
  run_subscript "$REPO_DIR/scripts/generate-authelia-secrets.sh"
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
  strict_step "compose up $stack"
  log "==> docker compose up -d ($stack)"
  if [[ "${TEST_MODE:-0}" == "1" ]]; then
    # In test mode, log what would have run (the harness assertion library
    # greps these lines to verify each stack saw an up -d invocation).
    log "    TEST_MODE: skipping 'docker compose --env-file $ENV_FILE -f $file up -d --remove-orphans' for stack=$stack"
    if [[ -n "${TEST_COMPOSE_LOG:-}" ]]; then
      printf '%s\n' "$stack" >> "$TEST_COMPOSE_LOG"
    fi
    return 0
  fi
  # --remove-orphans cleans up containers from a previous compose layout.
  # Run inside the stack dir so any relative bind mounts resolve correctly.
  # Capture rc explicitly so a compose failure surfaces as a clear block
  # via fail() (the cd-and-compose sub-shell would otherwise just exit
  # silently and the strict ERR trap would point at the closing `)`,
  # not the failing compose-up).
  local rc=0
  set +e
  ( cd "$STACKS_DIR/$stack" && \
    docker compose --env-file "$ENV_FILE" -f "$file" up -d --remove-orphans )
  rc=$?
  set -e
  if (( rc != 0 )); then
    fail "docker compose up failed for stack '$stack' (rc=$rc). Check: docker compose --env-file $ENV_FILE -f $file config; docker logs $stack"
  fi
}

# Generate the NPM (ingress) initial admin password BEFORE bringing up
# ingress. NPM 2.14 only auto-creates an admin if INITIAL_ADMIN_EMAIL +
# INITIAL_ADMIN_PASSWORD are present in the container env at first boot;
# without them the user table stays empty and the bootstrap-npm-setup-route
# seeder has no way to authenticate. The script writes both to /opt/stacks/.env
# (idempotent) so compose substitutes them in.
if [[ "${TEST_MODE:-0}" != "1" ]]; then
  strict_step "generate NPM admin"
  log "==> generating NPM admin password (idempotent)"
  run_subscript "$REPO_DIR/scripts/generate-npm-admin.sh"
fi

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
# binds to 172.17.0.1:8080 always; the public wizard URL is fronted by an
# NPM proxy host on `setup.${DOMAIN}` that forwards `/` to that endpoint
# (see "Step 6.5" below). Try a few common loopback variants so a
# misconfigured listen address still surfaces health. 30s budget.
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  log "==> TEST_MODE: skipping enrol /healthz wait (no real containers running)"
else
  strict_step "wait for enrol /healthz"
  log "==> waiting for enrol /healthz (timeout 30s)"
  ENROL_OK=0
  for i in $(seq 1 30); do
    for url in \
      "http://172.17.0.1:8080/healthz" \
      "http://127.0.0.1:8080/healthz" \
      "http://127.0.0.1/healthz"; do
      if curl -fsS -o /dev/null --max-time 2 "$url" 2>/dev/null; then
        log "    enrol healthy at $url (after ${i}s)"
        ENROL_OK=1
        break 2
      fi
    done
    sleep 1
  done
  if [[ $ENROL_OK -eq 0 ]]; then
    log "    WARNING: enrol /healthz did not respond within 30s — wizard"
    log "             may not be reachable yet. Check 'docker logs enrol'"
    log "             and 'docker compose -f $STACKS_DIR/enrol/docker-compose.yml ps'."
  fi
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
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  log "    TEST_MODE: skipping ufw allow 80/tcp + iptables INPUT ACCEPT for tcp/80"
else
  strict_step "open port 80"
  # ufw may be inactive (default in this installer until step 3); the rule
  # is staged for later activation, so a non-zero rc here is informational.
  ufw allow 80/tcp >/dev/null 2>&1 || true   # why: ufw may not be active yet; staging is fine
  if command -v iptables >/dev/null 2>&1; then
    if ! iptables -C INPUT -p tcp --dport 80 -j ACCEPT 2>/dev/null; then
      iptables -I INPUT -p tcp --dport 80 -j ACCEPT
      log "    inserted iptables INPUT ACCEPT for tcp/80"
    else
      log "    iptables INPUT already accepts tcp/80"
    fi
  fi
fi
# Note: the wizard's finalisation step (Wave 3A) is responsible for closing
# port 80 once the wildcard cert is issued and NPM is fronting :443.

# ──────────────────────────────────────────────────────────────────────────
# Step 6.5 — seed the wizard's `setup.${DOMAIN}` proxy host in NPM.
# ──────────────────────────────────────────────────────────────────────────

# The wizard URL is `http://setup.${DOMAIN}/` (subdomain + root path), NOT
# `http://${DOMAIN}/setup` (apex + path prefix). NPM is already up (Step 4),
# but it ships with no proxy hosts configured, so without this step the
# operator's browser would land on NPM's "Congratulations" fallback page.
#
# bootstrap-npm-setup-route.sh logs in to NPM with its hard-coded default
# admin credentials (admin@example.com / changeme), rotates them to a
# bootstrap-only set (stashed at /etc/raph-installer/npm-bootstrap.pass),
# and upserts a single proxy host for `setup.${DOMAIN}` that forwards to
# enrol on the docker0 bridge. The wizard's finalize step
# (stacks/enrol/npm_client.go § Bootstrap) replaces both the bootstrap
# admin and that proxy host with operator-supplied credentials and the
# four steady-state hosts (auth, enrol, cloud, console) over HTTPS.
SETUP_ROUTE_SCRIPT="$REPO_DIR/scripts/bootstrap-npm-setup-route.sh"
log "==> wiring NPM proxy host for setup.${DOMAIN}"
if [[ "${TEST_MODE:-0}" == "1" ]]; then
  log "    TEST_MODE: skipping bootstrap-npm-setup-route.sh"
elif [[ ! -x "$SETUP_ROUTE_SCRIPT" ]]; then
  log "    WARNING: $SETUP_ROUTE_SCRIPT missing or not executable; the wizard"
  log "             will be unreachable until you create the proxy host"
  log "             manually via NPM's UI at http://<vps>:81 (forward to"
  log "             http://host.docker.internal:8080)"
else
  strict_step "bootstrap NPM setup proxy host"
  run_subscript "$SETUP_ROUTE_SCRIPT"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 7 — finalise + announce.
# ──────────────────────────────────────────────────────────────────────────

strict_step "finalise"
date -u '+%Y-%m-%dT%H:%M:%SZ' > "$PHASE2_DONE"
chmod 0644 "$PHASE2_DONE"
log "Phase 2 sentinel: $PHASE2_DONE"

# Belt-and-suspenders: also disable the continue unit ourselves.
if command -v systemctl >/dev/null 2>&1; then
  # why: continue unit is single-shot; remove from boot to prevent re-fire.
  systemctl disable "${CONTINUE_UNIT}" >/dev/null 2>&1 || true
fi

VPS_IP="$(curl -fsS --max-time 3 https://ifconfig.me 2>/dev/null \
  || ip -4 -o route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}')"

cat <<EOF

================================================================
Phase 2 complete. The setup wizard is now live.
================================================================

  URL: https://setup.${DOMAIN}/

The wizard is fronted by a self-signed certificate until you
complete the finalize step (wildcard cert issuance via DNS-01).
Your browser will show a "not secure" / "your connection is not
private" warning on first visit — click "Advanced → Proceed".

If DNS hasn't propagated for setup.${DOMAIN} yet, you can reach
it directly at https://${VPS_IP:-<vps-ip>}/ provided you send
Host: setup.${DOMAIN} (e.g. via /etc/hosts or curl --resolve).
HTTP traffic on port 80 is permanently 301-redirected to HTTPS.

  Phase 2 log: ${LOG_FILE}
================================================================
EOF
