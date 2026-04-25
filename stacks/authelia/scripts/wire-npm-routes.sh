#!/usr/bin/env bash
# wire-npm-routes.sh — create/update the four proxy hosts in NPM.
#
# Usage:
#   NPM_URL=http://127.0.0.1:81 \
#   NPM_EMAIL=raphaelcasimir.inge@gmail.com \
#   NPM_PASS=changeme \
#   ./wire-npm-routes.sh
#
# Or pass NPM_TOKEN directly (skips the login step):
#   NPM_URL=http://127.0.0.1:81 NPM_TOKEN=eyJhbG... ./wire-npm-routes.sh
#
# Idempotent: if a proxy host with a given domain already exists, the
# script PUTs an update; otherwise POSTs a creation. The wildcard cert
# (NPM cert id = $NPM_CERT_ID, default 3) is required for SSL.
#
# Hosts created/updated:
#   1. auth.antarctica-engineering.com   → authelia:9091     (no fwd-auth)
#   2. enrol.antarctica-engineering.com  → enrol:8080        (fwd-auth)
#   3. cloud.antarctica-engineering.com  → cloud:3923        (fwd-auth)
#   4. console.antarctica-engineering.com→ console:9443 https (fwd-auth)
#
# The forward-auth Advanced config is the same for all three protected
# hosts and references the snippets in /snippets/ (mounted into the NPM
# container by ../../ingress changes).

set -euo pipefail

NPM_URL="${NPM_URL:-http://127.0.0.1:81}"
NPM_CERT_ID="${NPM_CERT_ID:-3}"   # wildcard cert id for *.antarctica-engineering.com

log()  { printf '[wire-npm-routes] %s\n' "$*" >&2; }
die()  { printf '[wire-npm-routes] error: %s\n' "$*" >&2; exit 1; }

require() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }
require curl
require jq

# ----- auth -----------------------------------------------------------
if [[ -z "${NPM_TOKEN:-}" ]]; then
    [[ -n "${NPM_EMAIL:-}" && -n "${NPM_PASS:-}" ]] \
        || die "set NPM_TOKEN or both NPM_EMAIL+NPM_PASS"
    log "logging in to NPM at $NPM_URL as $NPM_EMAIL"
    NPM_TOKEN="$(curl -fsS -X POST "$NPM_URL/api/tokens" \
        -H 'Content-Type: application/json' \
        -d "$(jq -n --arg id "$NPM_EMAIL" --arg s "$NPM_PASS" \
                '{identity:$id, secret:$s}')" \
        | jq -r '.token')"
    [[ -n "$NPM_TOKEN" && "$NPM_TOKEN" != "null" ]] || die "login failed"
fi
AUTH_HEADER=(-H "Authorization: Bearer $NPM_TOKEN")

# ----- forward-auth advanced config (shared) -------------------------
read -r -d '' ADV_FWD_AUTH <<'EOF' || true
include /snippets/authelia-location.conf;

location / {
    include /snippets/proxy.conf;
    include /snippets/authelia-authrequest.conf;
    proxy_pass $forward_scheme://$server:$port;
}
EOF

# Empty advanced config for hosts that should NOT be gated.
ADV_NONE=""

# ----- helper: create or update a proxy host -------------------------
# Args: domain forward_host forward_port forward_scheme advanced_config
upsert_proxy_host() {
    local domain="$1" host="$2" port="$3" scheme="$4" advanced="$5"

    local existing_id
    existing_id="$(curl -fsS "${AUTH_HEADER[@]}" \
        "$NPM_URL/api/nginx/proxy-hosts" \
        | jq -r --arg d "$domain" \
              '.[] | select(.domain_names | index($d)) | .id' \
        | head -n1)"

    local payload
    payload="$(jq -n \
        --arg domain "$domain" \
        --arg host "$host" \
        --arg port "$port" \
        --arg scheme "$scheme" \
        --arg advanced "$advanced" \
        --argjson cert_id "$NPM_CERT_ID" \
        '{
            domain_names: [$domain],
            forward_scheme: $scheme,
            forward_host: $host,
            forward_port: ($port|tonumber),
            block_exploits: true,
            caching_enabled: false,
            allow_websocket_upgrade: true,
            access_list_id: 0,
            certificate_id: $cert_id,
            ssl_forced: true,
            http2_support: true,
            hsts_enabled: true,
            hsts_subdomains: false,
            advanced_config: $advanced,
            meta: {letsencrypt_agree: false, dns_challenge: false},
            locations: []
        }')"

    if [[ -n "$existing_id" && "$existing_id" != "null" ]]; then
        log "updating proxy host id=$existing_id ($domain → $scheme://$host:$port)"
        curl -fsS "${AUTH_HEADER[@]}" -X PUT \
             -H 'Content-Type: application/json' \
             -d "$payload" \
             "$NPM_URL/api/nginx/proxy-hosts/$existing_id" >/dev/null
    else
        log "creating proxy host ($domain → $scheme://$host:$port)"
        curl -fsS "${AUTH_HEADER[@]}" -X POST \
             -H 'Content-Type: application/json' \
             -d "$payload" \
             "$NPM_URL/api/nginx/proxy-hosts" >/dev/null
    fi
}

# ----- the four hosts -------------------------------------------------
# 1. auth — Authelia portal itself. NO forward-auth (would loop).
upsert_proxy_host \
    "auth.antarctica-engineering.com" \
    "authelia" 9091 "http" \
    "$ADV_NONE"

# 2. enrol — peer manager. Forward-auth.
upsert_proxy_host \
    "enrol.antarctica-engineering.com" \
    "enrol" 8080 "http" \
    "$ADV_FWD_AUTH"

# 3. cloud — copyparty. Forward-auth (replaces existing host id 1).
upsert_proxy_host \
    "cloud.antarctica-engineering.com" \
    "cloud" 3923 "http" \
    "$ADV_FWD_AUTH"

# 4. console — Portainer. Forward-auth + scheme=https (Portainer's
#    self-signed cert is accepted because NPM hits the upstream at
#    https://console:9443 inside the docker network).
upsert_proxy_host \
    "console.antarctica-engineering.com" \
    "console" 9443 "https" \
    "$ADV_FWD_AUTH"

log "all four proxy hosts wired"
