#!/usr/bin/env bash
# configure-oidc.sh — flip Portainer's authentication mode to OAuth/OIDC
# pointing at the Authelia provider.
#
# Run this AFTER:
#   1. Authelia is up at https://auth.antarctica-engineering.com.
#   2. You generated a Portainer OIDC client_secret and put its hash in
#      /opt/stacks/authelia/.env (see authelia/README.md §2). The
#      plaintext is the value passed to this script via PORTAINER_CLIENT_SECRET.
#   3. The Portainer admin account exists (the bootstrap form was completed
#      within the 5-minute window — see ../README.md).
#   4. Portainer's web UI is reachable at https://127.0.0.1:9443 (loopback)
#      OR via the mesh overlay at https://<vps-magicdns>:9443.
#
# Usage:
#   PORTAINER_URL=https://127.0.0.1:9443 \
#   PORTAINER_USER=sagan \
#   PORTAINER_PASS='<sagan-portainer-admin-password>' \
#   PORTAINER_CLIENT_SECRET='<plaintext from authelia step>' \
#   ./configure-oidc.sh
#
# This wraps Portainer's Settings API. UI fields populated:
#   AuthenticationMethod    = 3 (OAuth)
#   OAuthSettings.ClientID  = console
#   OAuthSettings.AuthURL   = https://auth.antarctica-engineering.com/api/oidc/authorization
#   OAuthSettings.AccessURL = https://auth.antarctica-engineering.com/api/oidc/token
#   OAuthSettings.ResourceURL = https://auth.antarctica-engineering.com/api/oidc/userinfo
#   OAuthSettings.RedirectURL = https://console.antarctica-engineering.com
#   OAuthSettings.UserIdentifier = preferred_username
#   OAuthSettings.Scopes        = openid profile groups email
#   OAuthSettings.OAuthAutoCreateUsers = true
#
# Run is idempotent: re-running just overwrites the same settings.

set -euo pipefail

PORTAINER_URL="${PORTAINER_URL:-https://127.0.0.1:9443}"

require() { command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }; }
require curl
require jq

[[ -n "${PORTAINER_USER:-}" && -n "${PORTAINER_PASS:-}" ]] \
    || { echo "set PORTAINER_USER + PORTAINER_PASS" >&2; exit 1; }
[[ -n "${PORTAINER_CLIENT_SECRET:-}" ]] \
    || { echo "set PORTAINER_CLIENT_SECRET (plaintext)" >&2; exit 1; }

echo "[oidc] logging in to Portainer at $PORTAINER_URL as $PORTAINER_USER"
JWT="$(curl -sk -X POST "$PORTAINER_URL/api/auth" \
    -H 'Content-Type: application/json' \
    -d "$(jq -n --arg u "$PORTAINER_USER" --arg p "$PORTAINER_PASS" \
              '{username:$u, password:$p}')" \
    | jq -r '.jwt')"
[[ -n "$JWT" && "$JWT" != "null" ]] || { echo "login failed" >&2; exit 1; }

echo "[oidc] PUT /api/settings — switching to OAuth + Authelia"
curl -sk -X PUT "$PORTAINER_URL/api/settings" \
    -H "Authorization: Bearer $JWT" \
    -H 'Content-Type: application/json' \
    -d "$(jq -n \
        --arg cs "$PORTAINER_CLIENT_SECRET" \
        '{
            AuthenticationMethod: 3,
            OAuthSettings: {
                ClientID: "console",
                ClientSecret: $cs,
                AccessTokenURI: "https://auth.antarctica-engineering.com/api/oidc/token",
                AuthorizationURI: "https://auth.antarctica-engineering.com/api/oidc/authorization",
                ResourceURI: "https://auth.antarctica-engineering.com/api/oidc/userinfo",
                RedirectURI: "https://console.antarctica-engineering.com",
                UserIdentifier: "preferred_username",
                Scopes: "openid profile groups email",
                OAuthAutoCreateUsers: true,
                DefaultTeamID: 0,
                SSO: true,
                LogoutURI: "",
                KubeSecretKey: null
            }
        }')" \
    | jq -r '.OAuthSettings.AuthorizationURI // "no AuthorizationURI in response"' \
    | sed 's/^/[oidc] /'

echo "[oidc] done. Test by browsing https://console.antarctica-engineering.com/ in an incognito window."
echo "[oidc] If sign-in via OAuth fails, browse $PORTAINER_URL/#!/settings/auth and inspect the OAuth section."
