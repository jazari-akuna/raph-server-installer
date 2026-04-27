#!/usr/bin/env bash
# bootstrap-npm-setup-route.sh — wire the bare minimum NPM proxy host so
# the setup wizard at http://setup.${DOMAIN}/ is reachable.
#
# Run during phase 2, AFTER `ingress` (NPM) is up and `enrol` has its
# host-mode listener bound on 172.17.0.1:8080. Idempotent: if the host
# already exists with the right forward target it logs "updating".
#
# Why this script exists
# ----------------------
# NPM ships with a default admin (admin@example.com / changeme) and an
# empty proxy-host list. With no proxy host, NPM serves its own "Welcome
# to nginx" placeholder on :80 — the wizard is unreachable.
#
# The full wizard finalisation (stacks/enrol/npm_client.go) handles
# rotating the admin credentials, issuing the wildcard cert, and wiring
# the four production hosts (auth, enrol, cloud, console) over HTTPS.
# We can NOT depend on that here because the operator hasn't reached
# the wizard yet — we need a way IN.
#
# So: bootstrap a minimal HTTP-only proxy host whose Host header is
# `setup.${DOMAIN}` and which forwards `/` to enrol at 172.17.0.1:8080.
# In setup mode, enrol's setupRouteGate dispatches `/` to the wizard root
# handler (which 303s to /setup/<step>). Internal step paths (/setup/dns
# etc.) stay on the same host. This is plain HTTP — no certs. The wizard
# runs over HTTP until the operator finishes step 3 (cert issuance), at
# which point npm_client.go rotates the proxy host onto HTTPS and removes
# the bootstrap setup.${DOMAIN} entry.
#
# IMPORTANT — scope:
#   This script registers EXACTLY ONE hostname: `setup.${DOMAIN}`. It
#   does NOT register the apex `${DOMAIN}` (no apex-path-prefix wizard
#   URL) and it does NOT register `enrol.${DOMAIN}` (that's a finalize-
#   step concern, fronted with HTTPS + forward-auth). Keep it tight.
#
# Auth: NPM's default-admin password is rotated via the API on first
# login. We re-rotate to a known value stored at /etc/raph-installer/
# npm-bootstrap.pass (mode 0600 root:root) so subsequent re-runs of this
# script (and the wizard's npm_client.go) can log back in. The wizard
# overwrites both with operator-supplied creds at finalisation time.

# shellcheck source=lib/strict.sh
SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
. "$SCRIPT_DIR/lib/strict.sh"
strict_enable
STRICT_SCRIPT_NAME="bootstrap-npm-setup-route.sh"

require_root
require_cmd curl jq install chmod

NPM_URL="${NPM_URL:-http://127.0.0.1:81}"
NPM_DEFAULT_EMAIL="admin@example.com"
NPM_DEFAULT_PASS="changeme"
# Rotated bootstrap admin: the wizard owns this file and replaces it
# with operator-supplied creds at finalize time.
NPM_BOOTSTRAP_FILE="${NPM_BOOTSTRAP_FILE:-/etc/raph-installer/npm-bootstrap.pass}"
NPM_BOOTSTRAP_EMAIL="${NPM_BOOTSTRAP_EMAIL:-bootstrap@local}"

# DOMAIN sourced from /opt/stacks/.env (preferred) or /etc/server-domain.
if [[ -z "${DOMAIN:-}" && -f /opt/stacks/.env ]]; then
  # shellcheck disable=SC1091
  set -a; . /opt/stacks/.env; set +a
fi
if [[ -z "${DOMAIN:-}" && -r /etc/server-domain ]]; then
  DOMAIN="$(tr -d '[:space:]' </etc/server-domain)"
fi
require_env DOMAIN

# Forward target for the wizard. enrol publishes ENROL_LISTEN on the docker
# bridge gateway IP (172.17.0.1:8080). NPM proxies need a literal IP rather
# than a /etc/hosts alias like host.docker.internal — openresty's resolver
# does not consult /etc/hosts and would log "host not found", returning 502.
FORWARD_HOST="172.17.0.1"
FORWARD_PORT="8080"
FORWARD_SCHEME="http"

log()  { printf '[bootstrap-npm-setup-route] %s\n' "$*"; }
warn() { printf '[bootstrap-npm-setup-route] WARNING: %s\n' "$*" >&2; }
die()  { printf '[bootstrap-npm-setup-route] ERROR: %s\n' "$*" >&2; exit 1; }

# --- wait for NPM to come up ---------------------------------------------

strict_step "wait for NPM API"
log "waiting for $NPM_URL/api"
for i in $(seq 1 30); do
  # NPM returns a JSON envelope with version info on /api when alive.
  if curl -fsS --max-time 2 "$NPM_URL/api" >/dev/null 2>&1; then
    log "NPM API responsive after ${i}s"
    break
  fi
  if (( i == 30 )); then
    die "NPM API did not respond within 30s — is the ingress container up?"
  fi
  sleep 1
done

# --- log in (rotate default admin on first run) --------------------------

# Try the rotated bootstrap password first; fall back to default.
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

strict_step "obtain NPM admin token"
ROTATED_PASS=""
if [[ -s "$NPM_BOOTSTRAP_FILE" ]]; then
  ROTATED_PASS="$(cat "$NPM_BOOTSTRAP_FILE")"
fi
if [[ -n "$ROTATED_PASS" ]] && try_login "$NPM_BOOTSTRAP_EMAIL" "$ROTATED_PASS"; then
  log "logged in with rotated bootstrap credentials"
elif try_login "$NPM_DEFAULT_EMAIL" "$NPM_DEFAULT_PASS"; then
  log "logged in with default admin — rotating credentials now"

  # Generate a new password and stash it. 32 b64url chars.
  NEW_PASS="$(openssl rand -base64 24 | tr -d '\n=' | tr '+/' '-_')"

  # Find user id of the default admin.
  USER_ID="$(curl -fsS -H "Authorization: Bearer $TOKEN" \
              "$NPM_URL/api/users?expand=permissions" \
              | jq -r --arg e "$NPM_DEFAULT_EMAIL" '.[] | select(.email==$e) | .id')"
  if [[ -z "$USER_ID" || "$USER_ID" == "null" ]]; then
    die "could not locate default admin in NPM user list"
  fi

  # Update the user's email + name to the bootstrap identity. NPM treats
  # an email change as just-another-update; the underlying account is the
  # same and current sessions stay valid.
  USER_PAYLOAD="$(jq -n \
      --arg name "Bootstrap" \
      --arg nick "bootstrap" \
      --arg email "$NPM_BOOTSTRAP_EMAIL" \
      '{name:$name, nickname:$nick, email:$email, roles:["admin"], is_disabled:false}')"
  curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" \
        -H 'Content-Type: application/json' \
        -d "$USER_PAYLOAD" \
        "$NPM_URL/api/users/$USER_ID" >/dev/null

  # Rotate the password.
  PASS_PAYLOAD="$(jq -n --arg cur "$NPM_DEFAULT_PASS" --arg new "$NEW_PASS" \
      '{type:"password", current:$cur, secret:$new}')"
  curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" \
        -H 'Content-Type: application/json' \
        -d "$PASS_PAYLOAD" \
        "$NPM_URL/api/users/$USER_ID/auth" >/dev/null

  # Persist the new password for re-runs and for the wizard's
  # npm_client.go to find. wizard rewrites both at finalize time.
  install -d -m 0700 -o root -g root /etc/raph-installer
  printf '%s' "$NEW_PASS" | install_secret_file 0600 "$NPM_BOOTSTRAP_FILE"
  ROTATED_PASS="$NEW_PASS"

  # Re-login under the rotated credentials so the rest of this script
  # (and any subsequent invocation) uses the canonical token.
  if ! try_login "$NPM_BOOTSTRAP_EMAIL" "$ROTATED_PASS"; then
    die "post-rotation re-login failed — rotation may not have stuck"
  fi
  log "rotated NPM admin: ${NPM_BOOTSTRAP_EMAIL} (password at $NPM_BOOTSTRAP_FILE)"
else
  die "could not log in as $NPM_BOOTSTRAP_EMAIL or $NPM_DEFAULT_EMAIL — investigate $NPM_URL"
fi

AUTH_HEADER=(-H "Authorization: Bearer $TOKEN")

# --- upsert the setup proxy host -----------------------------------------

# Single hostname: setup.${DOMAIN}. The apex and enrol.${DOMAIN} are
# intentionally NOT registered here — the wizard URL is the subdomain
# root, full stop. (apex-path-prefix wizard URLs were the prior design;
# they're dropped.)
SETUP_HOST="setup.$DOMAIN"
DOMAIN_NAMES_JSON="$(jq -nc --arg setup "$SETUP_HOST" '[$setup]')"

PAYLOAD="$(jq -nc \
    --argjson names "$DOMAIN_NAMES_JSON" \
    --arg host "$FORWARD_HOST" \
    --arg port "$FORWARD_PORT" \
    --arg scheme "$FORWARD_SCHEME" \
    '{
        domain_names: $names,
        forward_scheme: $scheme,
        forward_host: $host,
        forward_port: ($port|tonumber),
        block_exploits: false,
        caching_enabled: false,
        allow_websocket_upgrade: true,
        access_list_id: 0,
        certificate_id: 0,
        ssl_forced: false,
        http2_support: false,
        hsts_enabled: false,
        hsts_subdomains: false,
        advanced_config: "",
        meta: {letsencrypt_agree: false, dns_challenge: false},
        locations: []
    }')"

strict_step "upsert NPM proxy host for setup wizard"
EXISTING_ID="$(curl -fsS "${AUTH_HEADER[@]}" \
                "$NPM_URL/api/nginx/proxy-hosts" \
                | jq -r --arg d "$SETUP_HOST" \
                      'first(.[] | select(.domain_names | index($d)) | .id) // empty')"

if [[ -n "$EXISTING_ID" && "$EXISTING_ID" != "null" ]]; then
  PROXY_ID="$EXISTING_ID"
  log "updating existing proxy host id=$PROXY_ID ($SETUP_HOST → $FORWARD_SCHEME://$FORWARD_HOST:$FORWARD_PORT)"
  curl -fsS "${AUTH_HEADER[@]}" -X PUT \
        -H 'Content-Type: application/json' \
        -d "$PAYLOAD" \
        "$NPM_URL/api/nginx/proxy-hosts/$PROXY_ID" >/dev/null
else
  log "creating proxy host ($SETUP_HOST → $FORWARD_SCHEME://$FORWARD_HOST:$FORWARD_PORT)"
  PROXY_ID="$(curl -fsS "${AUTH_HEADER[@]}" -X POST \
        -H 'Content-Type: application/json' \
        -d "$PAYLOAD" \
        "$NPM_URL/api/nginx/proxy-hosts" | jq -r .id)"
fi

# ──────────────────────────────────────────────────────────────────────────
# Self-signed cert + force-SSL
# ──────────────────────────────────────────────────────────────────────────
#
# The wizard runs over plain HTTP until the operator finishes the cert
# step (wildcard issuance via DNS-01 in the finalize phase). But modern
# browsers silently upgrade typed URLs to https:// (HSTS, "HTTPS-First"
# mode in Chrome/Edge/Firefox), so a bare http:// proxy host is
# unreachable to operators who don't manually prefix the scheme.
#
# Mint a self-signed cert for setup.${DOMAIN}, attach it to the proxy
# host, and set ssl_forced=true so http:// → 301 → https://. Browsers
# will show a "not secure" interstitial on first visit; the operator
# clicks through. The wizard's finalize step (npm_client.go) replaces
# this cert with the real wildcard.
#
# Idempotent: reuse an existing self-signed cert if one is already
# attached to this proxy host.

strict_step "self-signed cert for $SETUP_HOST"

# Look up an existing bootstrap cert by nice_name (idempotency anchor that
# survives proxy-host PUTs which would otherwise reset certificate_id).
CERT_NICE_NAME="bootstrap-selfsigned-${SETUP_HOST}"
CURRENT_CERT_ID="$(curl -fsS "${AUTH_HEADER[@]}" "$NPM_URL/api/nginx/certificates" \
                    | jq -r --arg n "$CERT_NICE_NAME" \
                            'first(.[] | select(.nice_name == $n) | .id) // empty')"

if [[ -z "$CURRENT_CERT_ID" || "$CURRENT_CERT_ID" == "null" ]]; then
  log "minting self-signed cert for $SETUP_HOST"
  TMPDIR_CERT="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR_CERT"' EXIT
  openssl req -x509 -newkey rsa:2048 \
    -keyout "$TMPDIR_CERT/key.pem" \
    -out "$TMPDIR_CERT/cert.pem" \
    -days 365 -nodes \
    -subj "/CN=$SETUP_HOST" \
    -addext "subjectAltName=DNS:$SETUP_HOST" \
    >/dev/null 2>&1

  CERT_OBJ="$(curl -fsS "${AUTH_HEADER[@]}" -X POST \
                -H 'Content-Type: application/json' \
                -d "$(jq -nc --arg n "bootstrap-selfsigned-${SETUP_HOST}" \
                              --arg d "$SETUP_HOST" \
                              '{provider:"other", nice_name:$n, domain_names:[$d], meta:{}}')" \
                "$NPM_URL/api/nginx/certificates")"
  CERT_ID="$(echo "$CERT_OBJ" | jq -r .id)"
  if [[ -z "$CERT_ID" || "$CERT_ID" == "null" ]]; then
    die "NPM certificate create failed: $CERT_OBJ"
  fi

  curl -fsS "${AUTH_HEADER[@]}" -X POST \
       -F "certificate=@$TMPDIR_CERT/cert.pem" \
       -F "certificate_key=@$TMPDIR_CERT/key.pem" \
       "$NPM_URL/api/nginx/certificates/$CERT_ID/upload" >/dev/null

  rm -rf "$TMPDIR_CERT"
  trap - EXIT
  log "uploaded NPM certificate id=$CERT_ID"
else
  CERT_ID="$CURRENT_CERT_ID"
  log "reusing existing certificate id=$CERT_ID on proxy host"
fi

# Bind cert + force SSL on the proxy host.
curl -fsS "${AUTH_HEADER[@]}" -X PUT \
     -H 'Content-Type: application/json' \
     -d "$(jq -nc --argjson cid "$CERT_ID" \
              '{certificate_id:$cid, ssl_forced:true, http2_support:false, hsts_enabled:false}')" \
     "$NPM_URL/api/nginx/proxy-hosts/$PROXY_ID" >/dev/null

log "done — wizard reachable at https://$SETUP_HOST/ (HTTP 301→HTTPS, self-signed until finalize)"
