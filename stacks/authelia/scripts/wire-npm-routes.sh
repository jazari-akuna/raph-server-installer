#!/usr/bin/env bash
# wire-npm-routes.sh — create/update the four proxy hosts in NPM.
#
# Usage:
#   DOMAIN=example.com \
#   NPM_URL=http://127.0.0.1:81 \
#   NPM_EMAIL=admin@example.com \
#   NPM_PASS=changeme \
#   ./wire-npm-routes.sh
#
# Or pass NPM_TOKEN directly (skips the login step):
#   DOMAIN=example.com NPM_URL=http://127.0.0.1:81 \
#       NPM_TOKEN=eyJhbG... ./wire-npm-routes.sh
#
# Idempotent: if a proxy host with a given domain already exists, the
# script PUTs an update; otherwise POSTs a creation. The wildcard cert
# (NPM cert id = $NPM_CERT_ID, default 3) is required for SSL.
#
# Hosts created/updated (all under ${DOMAIN}):
#   1. auth.${DOMAIN}    → authelia:9091     (no fwd-auth)
#   2. enrol.${DOMAIN}   → enrol:8080        (fwd-auth)
#   3. cloud.${DOMAIN}   → cloud:3923        (fwd-auth)
#   4. console.${DOMAIN} → console:9443 https (fwd-auth)
#
# The forward-auth Advanced config is the same for all three protected
# hosts and references the snippets in /snippets/ (mounted into the NPM
# container by ../../ingress changes).

set -euo pipefail

NPM_URL="${NPM_URL:-http://127.0.0.1:81}"
NPM_CERT_ID="${NPM_CERT_ID:-3}"   # wildcard cert id for *.${DOMAIN}

# DOMAIN is the apex (e.g. example.com); fall back to /etc/server-domain
# for convenience when running the script on the VPS without explicit env.
if [[ -z "${DOMAIN:-}" && -r /etc/server-domain ]]; then
    DOMAIN="$(tr -d '[:space:]' </etc/server-domain)"
fi
[[ -n "${DOMAIN:-}" ]] || { echo "DOMAIN must be set (apex domain)" >&2; exit 1; }

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

# auth portal: when an already-logged-in visitor hits /, Authelia shows
# its built-in "authenticated" placeholder instead of redirecting. We
# rewrite a bare GET / into /?rd=enrol/, which makes Authelia auto-
# forward authenticated users to the launcher AND drives unauthenticated
# users through login → launcher.
#
# Additionally we route POST /api/firstfactor through enrol's
# /login-intercept proxy so enrol can auto-unlock the user's LUKS volume
# with the password they just typed. enrol forwards to Authelia verbatim
# and copies the response, so end-user-visible behaviour is unchanged.
# (172.17.0.1 is the docker0 bridge gateway — that's how the NPM ingress
# container reaches the host-network-mode enrol service.)
read -r -d '' ADV_AUTH_PORTAL <<EOF || true
location = / {
    if (\$arg_rd = "") {
        return 302 /?rd=https://enrol.${DOMAIN}/;
    }
    include conf.d/include/proxy.conf;
}

location = /api/firstfactor {
    proxy_pass http://172.17.0.1:8080/login-intercept;
    proxy_set_header Host \$host;
    proxy_set_header X-Real-IP \$remote_addr;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto \$scheme;
    proxy_set_header X-Forwarded-Host \$host;
    proxy_http_version 1.1;
}
EOF

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
#    Adv config rewrites bare GET / to /?rd=enrol/ so already-logged-in
#    visitors land on the launcher instead of the "Authenticated"
#    placeholder.
upsert_proxy_host \
    "auth.${DOMAIN}" \
    "authelia" 9091 "http" \
    "$ADV_AUTH_PORTAL"

# 2. enrol — peer manager. Forward-auth.
upsert_proxy_host \
    "enrol.${DOMAIN}" \
    "enrol" 8080 "http" \
    "$ADV_FWD_AUTH"

# 3. cloud — copyparty. Forward-auth (replaces existing host id 1).
upsert_proxy_host \
    "cloud.${DOMAIN}" \
    "cloud" 3923 "http" \
    "$ADV_FWD_AUTH"

# 4. console — Portainer. Forward-auth + scheme=https (Portainer's
#    self-signed cert is accepted because NPM hits the upstream at
#    https://console:9443 inside the docker network).
upsert_proxy_host \
    "console.${DOMAIN}" \
    "console" 9443 "https" \
    "$ADV_FWD_AUTH"

log "all four proxy hosts wired"
