#!/usr/bin/env bash
# generate-enrol-forward-auth-secret.sh — mint the shared secret that
# fronts enrol's protected routes.
#
# Why this script exists
# ----------------------
# enrol publishes its admin UI on 172.17.0.1:8080 — the docker bridge
# gateway IP. That address is reachable from EVERY container on the
# docker bridge (cloud, console, authelia, certbot, any future user-
# added stack, OR an in-container compromise of e.g. Nextcloud).
#
# Without a shared secret, the only thing standing between such a
# container and full enrol admin (root-equivalent on the host: enrol is
# privileged, holds the docker socket, can exec anywhere) is the
# Remote-User / Remote-Groups headers — which are trivially forgeable:
#
#     curl -H 'Remote-User: raph' -H 'Remote-Groups: admins' \
#          http://172.17.0.1:8080/users/raph/delete
#
# NPM is the only entity that should be allowed to forward Remote-User
# headers to enrol. We give NPM a shared secret to inject on every
# protected proxy host, and enrol refuses requests that don't carry it.
# This script mints that secret.
#
# Outputs:
#   /etc/raph-installer/enrol-forward-auth.secret  — 32 random bytes,
#                                                    hex-encoded, mode
#                                                    0600 root:root.
#   /opt/stacks/.env                               — append
#                                                    ENROL_FORWARD_AUTH_SECRET=
#                                                    so docker compose
#                                                    substitutes it into
#                                                    enrol's container
#                                                    environment.
#
# Idempotent: re-runs reuse the existing on-disk secret. The secret
# never rotates automatically — rotation breaks every NPM proxy host
# that doesn't get its `proxy_set_header` re-rendered in the same
# transaction. If you need to rotate, delete the secret file, re-run
# this script, then re-run the wizard's finalize step (or manually
# re-render every proxy host's advanced_config via NPM's UI).
#
# Hooked into bootstrap-phase2.sh BEFORE `compose_up enrol` and BEFORE
# `bootstrap-npm-setup-route.sh` (the setup proxy host needs the secret
# in advanced_config).

# ──────────────────────────────────────────────────────────────────────────
# Strict mode + structured failure reporting (shared lib)
# ──────────────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib/strict.sh
. "$SCRIPT_DIR/lib/strict.sh"
strict_enable
STRICT_SCRIPT_NAME="generate-enrol-forward-auth-secret.sh"

require_root
require_cmd install openssl cat grep printf awk

ENV_FILE="${ENV_FILE:-/opt/stacks/.env}"
SECRET_DIR="/etc/raph-installer"
SECRET_FILE="${SECRET_DIR}/enrol-forward-auth.secret"

log() { printf '[generate-enrol-forward-auth-secret] %s\n' "$*"; }

require_file "$ENV_FILE"

install -d -m 0700 -o root -g root "$SECRET_DIR"

if [[ ! -s "$SECRET_FILE" ]]; then
  # 32 random bytes hex-encoded → 64 chars. Hex (not base64) so the
  # value survives every shell quoting style without needing escapes;
  # nginx's proxy_set_header value, docker compose's .env reader, and
  # bash's single-quote literal all accept [0-9a-f]+ verbatim.
  secret="$(openssl rand -hex 32)"
  printf '%s' "$secret" > "$SECRET_FILE"
  chmod 0600 "$SECRET_FILE"
  chown root:root "$SECRET_FILE"
  log "generated enrol forward-auth secret ($SECRET_FILE)"
else
  log "reusing enrol forward-auth secret ($SECRET_FILE)"
fi

secret="$(cat "$SECRET_FILE")"

# Append to .env if not already present, otherwise replace the value
# in-place so a re-run of this script (e.g. after an operator manually
# edited .env) converges on the on-disk secret. We single-quote so
# compose's .env reader takes the literal value (compose strips outer
# single quotes per the compose-spec env-file format).
#
# Both branches use the same tmpfile + atomic-rename pattern. The awk END
# block emits the line if no match was found, so the same script handles
# the first-time-add case AND the in-place-rewrite case. A partial write
# (e.g. a SIGKILL mid-`>>` append) would leave the wizard / compose unable
# to read the env file, so we never `>>` the secret directly.
if grep -q '^ENROL_FORWARD_AUTH_SECRET=' "$ENV_FILE"; then
  was_present=1
else
  was_present=0
fi
tmp="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
awk -v s="$secret" '
  BEGIN { done=0 }
  /^ENROL_FORWARD_AUTH_SECRET=/ {
    printf "ENROL_FORWARD_AUTH_SECRET=\x27%s\x27\n", s
    done=1
    next
  }
  { print }
  END { if (!done) printf "ENROL_FORWARD_AUTH_SECRET=\x27%s\x27\n", s }
' "$ENV_FILE" > "$tmp"
chmod --reference="$ENV_FILE" "$tmp"
chown --reference="$ENV_FILE" "$tmp"
mv -f "$tmp" "$ENV_FILE"
if (( was_present )); then
  log "refreshed ENROL_FORWARD_AUTH_SECRET in $ENV_FILE"
else
  log "added ENROL_FORWARD_AUTH_SECRET to $ENV_FILE"
fi
