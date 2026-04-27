#!/usr/bin/env bash
# generate-npm-admin.sh — bootstrap the NPM (ingress) initial admin.
#
# NPM 2.14 dropped the legacy admin@example.com / changeme default user.
# An initial admin is now only auto-created when INITIAL_ADMIN_EMAIL +
# INITIAL_ADMIN_PASSWORD are set in the container's env, otherwise the
# user table stays empty and the API rejects every login attempt.
#
# This script:
#   1. Generates a random NPM admin password (32 alnum chars) on first run
#      and persists it root:root mode 0600 at /etc/raph-installer/npm-bootstrap.pass.
#   2. Appends NPM_INITIAL_ADMIN_EMAIL + NPM_INITIAL_ADMIN_PASSWORD to
#      /opt/stacks/.env so docker compose substitutes them into the
#      ingress container's environment block.
#
# Idempotent: re-runs reuse the existing password and only append .env
# entries that are missing.
#
# Hooked into bootstrap-phase2.sh BEFORE `compose_up ingress`.

# ──────────────────────────────────────────────────────────────────────────
# Strict mode + structured failure reporting (shared lib)
# ──────────────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib/strict.sh
. "$SCRIPT_DIR/lib/strict.sh"
strict_enable
STRICT_SCRIPT_NAME="generate-npm-admin.sh"

require_root
require_cmd install tr cat

ENV_FILE="${ENV_FILE:-/opt/stacks/.env}"
PASS_DIR="/etc/raph-installer"
PASS_FILE="${PASS_DIR}/npm-bootstrap.pass"
NPM_EMAIL="${NPM_INITIAL_ADMIN_EMAIL:-bootstrap@local}"

log() { printf '[generate-npm-admin] %s\n' "$*"; }

require_file "$ENV_FILE"

install -d -m 0700 -o root -g root "$PASS_DIR"

if [[ ! -s "$PASS_FILE" ]]; then
  # 32 alnum chars from urandom. Toggle pipefail off for the same SIGPIPE
  # reason bootstrap.sh documents at length.
  set +o pipefail
  pass="$(tr -dc 'A-Za-z0-9' </dev/urandom 2>/dev/null | head -c 32)"
  set -o pipefail
  printf '%s' "$pass" > "$PASS_FILE"
  chmod 0600 "$PASS_FILE"
  log "generated NPM admin password ($PASS_FILE)"
else
  log "reusing NPM admin password ($PASS_FILE)"
fi

pass="$(cat "$PASS_FILE")"

# Append to .env if not already present. We append rather than rewrite to
# preserve any operator-supplied lines. Single-quoted to survive bash
# sourcing AND docker-compose's .env reader (compose strips outer single
# quotes per the compose-spec env-file format).
if ! grep -q '^NPM_INITIAL_ADMIN_EMAIL=' "$ENV_FILE"; then
  printf "NPM_INITIAL_ADMIN_EMAIL='%s'\n" "$NPM_EMAIL" >> "$ENV_FILE"
  log "added NPM_INITIAL_ADMIN_EMAIL to $ENV_FILE"
fi
if ! grep -q '^NPM_INITIAL_ADMIN_PASSWORD=' "$ENV_FILE"; then
  printf "NPM_INITIAL_ADMIN_PASSWORD='%s'\n" "$pass" >> "$ENV_FILE"
  log "added NPM_INITIAL_ADMIN_PASSWORD to $ENV_FILE"
fi
