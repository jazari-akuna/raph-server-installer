#!/usr/bin/env bash
# install-backup-timer.sh — install/refresh the nightly raph-backup systemd timer.
#
# Idempotent: re-running on an already-installed VPS is a no-op (no error,
# no duplicate units). Used by:
#   1. scripts/bootstrap-phase2.sh on first deploy.
#   2. Operator on an existing VPS to retrofit the backup timer
#      (after pulling a repo update that introduced this feature).
#
# Steps:
#   1. Generate /etc/raph-installer/restic-password if missing.
#   2. Initialise /srv/store/enrol-backups/restic if missing.
#   3. Copy the .service + .timer to /etc/systemd/system/, daemon-reload,
#      enable + start the timer.

set -euo pipefail

# Resolve REPO_DIR from the script location so this works whether invoked
# directly from the repo, via a symlink, or from a different cwd. The
# script lives at <repo>/scripts/install-backup-timer.sh; the systemd
# units live at <repo>/host/systemd/.
SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_DIR="$(cd -P -- "$SCRIPT_DIR/.." && pwd -P)"
UNITS_SRC_DIR="$REPO_DIR/host/systemd"
UNITS_DST_DIR="/etc/systemd/system"

PASSWORD_DIR="/etc/raph-installer"
PASSWORD_FILE="$PASSWORD_DIR/restic-password"
RESTIC_REPO_PARENT="/srv/store/enrol-backups"
RESTIC_REPO_DIR="$RESTIC_REPO_PARENT/restic"
RESTIC_REPO_CONFIG="$RESTIC_REPO_DIR/config"
RESTIC_IMAGE="restic/restic:0.16.5"

log() { printf '[install-backup-timer] %s\n' "$*"; }

# Resolve sudo: if we're already root, skip the indirection so this works
# in environments without sudo installed (e.g. minimal containers).
if [[ "$(id -u)" -eq 0 ]]; then
  SUDO=""
else
  SUDO="sudo"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 1 — restic password file
# ──────────────────────────────────────────────────────────────────────────

if [[ -f "$PASSWORD_FILE" ]]; then
  log "restic password already exists, skipping"
else
  $SUDO install -d -m 0700 "$PASSWORD_DIR"
  openssl rand -base64 64 | $SUDO tee "$PASSWORD_FILE" >/dev/null
  $SUDO chmod 0600 "$PASSWORD_FILE"
  log "restic password generated"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 2 — restic repository init
# ──────────────────────────────────────────────────────────────────────────

# Parent directory must exist + be root-owned 0700 BEFORE init so the
# repo inherits a tight perm baseline (encrypted, but defence in depth).
$SUDO install -d -m 0700 -o root -g root "$RESTIC_REPO_PARENT"

if [[ -f "$RESTIC_REPO_CONFIG" ]]; then
  log "restic repo already initialised at $RESTIC_REPO_DIR, skipping"
else
  # Prefer running through the live enrol container so we use the same
  # restic binary version Agent A's code path uses. Falls back to a
  # transient docker run on fresh installs where enrol isn't up yet
  # (bootstrap ordering puts this script after compose-up enrol, but
  # operators retrofitting on a stopped stack should still succeed).
  if docker ps --filter name=^enrol$ --format '{{.Names}}' | grep -q '^enrol$'; then
    log "initialising restic repo via 'docker exec enrol restic init'"
    $SUDO docker exec enrol restic \
      -r /srv/store/enrol-backups/restic \
      --password-file /etc/raph-installer/restic-password \
      init
  else
    log "enrol container not running; initialising restic repo via transient '$RESTIC_IMAGE' container"
    $SUDO docker run --rm \
      -v /etc/raph-installer:/etc/raph-installer:ro \
      -v /srv/store/enrol-backups:/srv/store/enrol-backups \
      "$RESTIC_IMAGE" \
      init \
      -r /srv/store/enrol-backups/restic \
      --password-file /etc/raph-installer/restic-password
  fi
  log "restic repo initialised at $RESTIC_REPO_DIR"
fi

# ──────────────────────────────────────────────────────────────────────────
# Step 3 — install systemd units, daemon-reload, enable+start the timer
# ──────────────────────────────────────────────────────────────────────────

units_changed=0
for unit in raph-backup.service raph-backup.timer; do
  src="$UNITS_SRC_DIR/$unit"
  dst="$UNITS_DST_DIR/$unit"
  if [[ ! -f "$src" ]]; then
    log "ERROR: source unit missing: $src"
    exit 1
  fi
  if [[ -f "$dst" ]] && cmp -s "$src" "$dst"; then
    log "$unit unchanged, skipping copy"
  else
    $SUDO install -m 0644 -o root -g root "$src" "$dst"
    log "$unit installed to $dst"
    units_changed=1
  fi
done

if (( units_changed == 1 )); then
  $SUDO systemctl daemon-reload
  log "systemctl daemon-reload (units changed)"
else
  log "no unit changes, skipping daemon-reload"
fi

# enable --now is idempotent: a no-op if the timer is already enabled
# and active. We always run it so a manually-disabled timer gets re-armed.
$SUDO systemctl enable --now raph-backup.timer >/dev/null
log "raph-backup.timer enabled and started"
