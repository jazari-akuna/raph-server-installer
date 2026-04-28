#!/usr/bin/env bash
# retrofit-enrol-forward-auth.sh — patch the production NPM proxy hosts
# (auth/enrol/console) on an existing finalized deployment so they inject
# X-Forward-Auth-Secret on every proxied request.
#
# Why this script exists
# ----------------------
# Earlier installer revisions wired the proxy hosts WITHOUT the
# forward-auth secret (because the secret didn't exist yet). Recent enrol
# revisions fail closed on missing/wrong X-Forward-Auth-Secret — so on
# upgrade, every protected endpoint behind those hosts (the enrol admin
# UI, Portainer UI, anything fronted by Authelia forward-auth) starts
# returning 401 until each proxy host's advanced_config is re-rendered.
#
# Cloud (Nextcloud) is intentionally NOT retrofitted: per ADR-003 it
# uses Authelia OIDC (not forward-auth), the rule is `bypass`, and the
# advanced_config is `npmAdvNextcloudTmpl` (no forward-auth header).
#
# This script does the re-render, in place, on a live deployment:
#   1. Verifies /etc/raph-installer/enrol-forward-auth.secret exists
#      (running scripts/generate-enrol-forward-auth-secret.sh if not).
#   2. Reads $DOMAIN from /opt/stacks/.env.
#   3. Logs into NPM with the bootstrap admin creds at
#      /etc/raph-installer/npm-bootstrap.pass.
#   4. For each of auth.${DOMAIN} / enrol.${DOMAIN} / console.${DOMAIN},
#      GETs the existing proxy host and PUTs it back with an updated
#      advanced_config carrying the secret. The advanced_config
#      templates here are byte-for-byte the same as the ones in
#      stacks/enrol/setup.go (npmAdvFwdAuthTmpl / npmAdvAuthPortalTmpl).
#      For the auth host we use the auth-portal template; for the other
#      two we use the regular template.
#
# Idempotent: re-running on an already-retrofitted host is a no-op (PUT
# with the same body is fine). The script never deletes hosts — that
# would invalidate the attached letsencrypt cert binding and force a
# full reissue.
#
# Operator usage:
#   sudo /opt/raph-server-installer/scripts/retrofit-enrol-forward-auth.sh
#
# Requires the deployment to be FINALIZED (sentinel /srv/store/.setup-complete
# present). On a fresh install the wizard's finalizeWireNPM runs the
# equivalent of this code path automatically.

# ──────────────────────────────────────────────────────────────────────────
# Strict mode + structured failure reporting (shared lib)
# ──────────────────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib/strict.sh
. "$SCRIPT_DIR/lib/strict.sh"
strict_enable
STRICT_SCRIPT_NAME="retrofit-enrol-forward-auth.sh"

require_root
require_cmd curl jq cat grep tr printf

# ──────────────────────────────────────────────────────────────────────────
# Constants
# ──────────────────────────────────────────────────────────────────────────

REPO_DIR="${REPO_DIR:-/opt/raph-server-installer}"
ENV_FILE="${ENV_FILE:-/opt/stacks/.env}"
SETUP_COMPLETE_SENTINEL="${SETUP_COMPLETE_SENTINEL:-/srv/store/.setup-complete}"
SECRET_FILE="${FORWARD_AUTH_SECRET_FILE:-/etc/raph-installer/enrol-forward-auth.secret}"
NPM_URL="${NPM_URL:-http://127.0.0.1:81}"
NPM_DEFAULT_EMAIL="admin@example.com"
NPM_DEFAULT_PASS="changeme"
NPM_BOOTSTRAP_FILE="${NPM_BOOTSTRAP_FILE:-/etc/raph-installer/npm-bootstrap.pass}"
NPM_BOOTSTRAP_EMAIL="${NPM_BOOTSTRAP_EMAIL:-bootstrap@local}"

log()  { printf '[retrofit-enrol-forward-auth] %s\n' "$*"; }
warn() { printf '[retrofit-enrol-forward-auth] WARNING: %s\n' "$*" >&2; }
die()  { printf '[retrofit-enrol-forward-auth] ERROR: %s\n' "$*" >&2; exit 1; }

# ──────────────────────────────────────────────────────────────────────────
# Preflight
# ──────────────────────────────────────────────────────────────────────────

strict_step "preflight"

if [[ ! -e "$SETUP_COMPLETE_SENTINEL" ]]; then
  die "deployment not finalized — sentinel $SETUP_COMPLETE_SENTINEL absent. Use the wizard's finalize step instead."
fi

require_file "$ENV_FILE"

# ──────────────────────────────────────────────────────────────────────────
# Load DOMAIN from the canonical .env
# ──────────────────────────────────────────────────────────────────────────

if [[ -z "${DOMAIN:-}" ]]; then
  # shellcheck disable=SC1090
  set -a; . "$ENV_FILE"; set +a
fi
require_env DOMAIN

# ──────────────────────────────────────────────────────────────────────────
# Ensure secret exists (run the generator if missing) + sanity-check shape
# ──────────────────────────────────────────────────────────────────────────

strict_step "load forward-auth secret"

if [[ ! -s "$SECRET_FILE" ]]; then
  log "secret file $SECRET_FILE absent — invoking generator"
  if [[ ! -x "$REPO_DIR/scripts/generate-enrol-forward-auth-secret.sh" ]]; then
    die "generator $REPO_DIR/scripts/generate-enrol-forward-auth-secret.sh not found or not executable"
  fi
  run_subscript "$REPO_DIR/scripts/generate-enrol-forward-auth-secret.sh"
fi
require_file "$SECRET_FILE"

# Read the secret into a local var. We never log or printf it.
FWD_SECRET="$(cat "$SECRET_FILE")"
if ! [[ "$FWD_SECRET" =~ ^[0-9a-f]{32,128}$ ]]; then
  die "$SECRET_FILE malformed — expected lowercase hex, 32–128 chars"
fi

# ──────────────────────────────────────────────────────────────────────────
# NPM admin login (mirrors bootstrap-npm-setup-route.sh's flow verbatim)
# ──────────────────────────────────────────────────────────────────────────

strict_step "obtain NPM admin token"

# Wait briefly for NPM to be reachable — operators may invoke this right
# after a `docker compose up` of the ingress stack.
for i in $(seq 1 30); do
  if curl -fsS --max-time 2 "$NPM_URL/api" >/dev/null 2>&1; then
    break
  fi
  if (( i == 30 )); then
    die "NPM API at $NPM_URL did not respond within 30s — is the ingress container up?"
  fi
  sleep 1
done

TOKEN=""
try_login() {
  local email="$1" pass="$2"
  local body resp
  body="$(jq -n --arg id "$email" --arg s "$pass" '{identity:$id, secret:$s}')"
  resp="$(curl -fsS -X POST "$NPM_URL/api/tokens" \
            -H 'Content-Type: application/json' \
            -d "$body" 2>/dev/null || true)"
  TOKEN="$(printf '%s' "$resp" | jq -r '.token // empty' 2>/dev/null)"
  [[ -n "$TOKEN" && "$TOKEN" != "null" ]]
}

# Try the bootstrap-rotated password first (this is the canonical path on
# any current deployment). Fall back to operator-supplied creds via env,
# then to the legacy default. The wizard rotates the admin off the
# bootstrap identity to the operator's chosen email at finalize time, so
# on a finalized deployment this file may have been replaced — operators
# can override via NPM_EMAIL / NPM_PASS env.
ROTATED_PASS=""
if [[ -s "$NPM_BOOTSTRAP_FILE" ]]; then
  ROTATED_PASS="$(cat "$NPM_BOOTSTRAP_FILE")"
fi

if [[ -n "${NPM_EMAIL:-}" && -n "${NPM_PASS:-}" ]] && try_login "$NPM_EMAIL" "$NPM_PASS"; then
  log "logged in with operator-supplied NPM_EMAIL / NPM_PASS"
elif [[ -n "$ROTATED_PASS" ]] && try_login "$NPM_BOOTSTRAP_EMAIL" "$ROTATED_PASS"; then
  log "logged in with rotated bootstrap credentials ($NPM_BOOTSTRAP_FILE)"
elif try_login "$NPM_DEFAULT_EMAIL" "$NPM_DEFAULT_PASS"; then
  log "logged in with NPM legacy default admin"
else
  die "could not log in to NPM — set NPM_EMAIL + NPM_PASS to the operator-rotated admin credentials and re-run"
fi

AUTH_HEADER=(-H "Authorization: Bearer $TOKEN")

# ──────────────────────────────────────────────────────────────────────────
# advanced_config templates — byte-for-byte the same as
# stacks/enrol/setup.go::npmAdvFwdAuthTmpl and npmAdvAuthPortalTmpl. If
# you change one, change the other too. nginx variables ($host, $scheme,
# $forward_scheme, $server, $port, etc.) are literal in the rendered
# config, NOT shell-interpolated.
# ──────────────────────────────────────────────────────────────────────────

# Forward-auth template (one %s slot, used twice via %[1]s in the Go
# template — bash printf doesn't have positional refs, so we pass the
# secret twice).
ADV_FWD_AUTH="$(printf 'include /snippets/authelia-location.conf;\n\nproxy_set_header X-Forward-Auth-Secret '"'"'%s'"'"';\n\nlocation / {\n    include /snippets/proxy.conf;\n    include /snippets/authelia-authrequest.conf;\n    proxy_set_header X-Forward-Auth-Secret '"'"'%s'"'"';\n    proxy_pass $forward_scheme://$server:$port;\n}\n' "$FWD_SECRET" "$FWD_SECRET")"

# Auth-portal template (one slot: domain for the bare-GET 302). Two
# location blocks: bare-GET redirect to the enrol portal, and a
# catch-all `location /` that proxies static assets (/static/js/*,
# /static/css/*, etc.) verbatim to authelia. Without that catch-all
# NPM 404s on the SPA's static assets and the login page renders as a
# blank black screen.
#
# Note: the previous version of this template rewrote /api/firstfactor
# onto enrol's /login-intercept handler (which proxied to Authelia and
# auto-unlocked the user's LUKS volume on success). With per-user LUKS
# removed (ADR-004) and Nextcloud handling its own OIDC session
# (ADR-003), the rewrite is gone and Authelia's POST handler runs
# unmodified.
ADV_AUTH_PORTAL="$(printf 'location = / {\n    if ($arg_rd = "") {\n        return 302 /?rd=https://enrol.%s/;\n    }\n    include conf.d/include/proxy.conf;\n}\n\nlocation / {\n    include conf.d/include/proxy.conf;\n}\n' "$DOMAIN")"

# ──────────────────────────────────────────────────────────────────────────
# Walk the proxy-host list and PUT each match
# ──────────────────────────────────────────────────────────────────────────

strict_step "patch proxy hosts"

ALL_HOSTS_JSON="$(curl -fsS "${AUTH_HEADER[@]}" "$NPM_URL/api/nginx/proxy-hosts")"

retrofit_one() {
  local fqdn="$1" adv="$2" host_id host_obj payload http_code resp
  host_id="$(printf '%s' "$ALL_HOSTS_JSON" \
              | jq -r --arg d "$fqdn" \
                  'first(.[] | select(.domain_names | index($d)) | .id) // empty')"
  if [[ -z "$host_id" || "$host_id" == "null" ]]; then
    log "MISSING $fqdn — no NPM proxy host carries this name; skipping"
    return 0
  fi

  host_obj="$(printf '%s' "$ALL_HOSTS_JSON" \
                | jq --argjson id "$host_id" 'first(.[] | select(.id == $id))')"

  # Build the PUT payload: keep every field NPM cares about from the
  # existing host object, override advanced_config. The fields list
  # mirrors the schema NPM accepts on PUT (anything we omit gets reset
  # to defaults — most importantly certificate_id, which would unbind
  # the wildcard cert).
  payload="$(printf '%s' "$host_obj" | jq --arg adv "$adv" '{
      domain_names,
      forward_scheme,
      forward_host,
      forward_port,
      access_list_id: (.access_list_id // 0),
      certificate_id: (.certificate_id // 0),
      ssl_forced: (.ssl_forced // false),
      hsts_enabled: (.hsts_enabled // false),
      hsts_subdomains: (.hsts_subdomains // false),
      http2_support: (.http2_support // false),
      block_exploits: (.block_exploits // false),
      caching_enabled: (.caching_enabled // false),
      allow_websocket_upgrade: (.allow_websocket_upgrade // false),
      meta: (.meta // {}),
      locations: (.locations // []),
      advanced_config: $adv
  }')"

  # Capture HTTP status separately so we can tell apart "PUT 200 OK,
  # nothing changed" (idempotent re-run) from a 4xx/5xx that we should
  # surface. Output written to /dev/null — the response body contains
  # the (possibly long) updated host object and is not useful here.
  http_code="$(curl -sS -o /dev/null -w '%{http_code}' \
                  "${AUTH_HEADER[@]}" -X PUT \
                  -H 'Content-Type: application/json' \
                  -d "$payload" \
                  "$NPM_URL/api/nginx/proxy-hosts/$host_id" || true)"
  if [[ "$http_code" =~ ^2 ]]; then
    log "OK $fqdn (id=$host_id, advanced_config retrofitted)"
  else
    warn "FAIL $fqdn (id=$host_id, NPM returned HTTP $http_code)"
    return 1
  fi
}

rc=0
retrofit_one "auth.$DOMAIN"    "$ADV_AUTH_PORTAL"   || rc=1
retrofit_one "enrol.$DOMAIN"   "$ADV_FWD_AUTH"      || rc=1
retrofit_one "console.$DOMAIN" "$ADV_FWD_AUTH"      || rc=1

if (( rc == 0 )); then
  log "done — auth/enrol/console proxy hosts now inject X-Forward-Auth-Secret (cloud uses OIDC and is intentionally untouched per ADR-003)"
else
  die "one or more proxy hosts failed to update — see warnings above; re-run after investigating"
fi
