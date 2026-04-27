#!/usr/bin/env bash
# generate-authelia-secrets.sh — create the Authelia secret bundle.
#
# Idempotent. Each artifact is written ONCE; subsequent runs leave existing
# files alone but re-assert mode + ownership. Re-issues nothing — rotating
# a leaked secret is a deliberate operator step (delete the file + re-run).
#
# Outputs (all root:root):
#   /opt/stacks/authelia/secrets/jwt-secret               (0600, 64 b64 chars)
#   /opt/stacks/authelia/secrets/session-secret           (0600)
#   /opt/stacks/authelia/secrets/storage-encryption-key   (0600)
#   /opt/stacks/authelia/secrets/oidc-hmac-secret         (0600)
#   /opt/stacks/authelia/secrets/oidc-key.pem             (0600, RSA-2048 PKCS#8)
#
# Hooked into bootstrap-phase2.sh BEFORE `compose_up authelia`. Also safe
# to invoke standalone:  sudo bash scripts/generate-authelia-secrets.sh
#
# References:
#   stacks/authelia/.env.example  — generation-recipe-of-record
#   stacks/authelia/docker-compose.yml — consumer (top-level secrets:)

# shellcheck source=lib/strict.sh
SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
. "$SCRIPT_DIR/lib/strict.sh"
strict_enable
STRICT_SCRIPT_NAME="generate-authelia-secrets.sh"

require_root
require_cmd openssl install chmod chown stat mktemp dd

SECRETS_DIR="${SECRETS_DIR:-/opt/stacks/authelia/secrets}"
strict_step "ensure secrets dir"

install -d -m 0700 -o root -g root "$SECRETS_DIR"   # why: secret dir must not be world-readable

gen_random_b64() {
  # 64 raw bytes -> base64 -> stripped newlines. Authelia accepts any
  # high-entropy string for these; 64 b64 chars (~48 bytes of entropy) is
  # well above the documented 32-byte minimum.
  openssl rand -base64 64 | tr -d '\n'
}

ensure_random_secret() {
  local name="$1"
  local path="$SECRETS_DIR/$name"
  if [[ -s "$path" ]]; then
    chmod 0600 "$path"
    chown root:root "$path"
    printf '[generate-authelia-secrets] keeping existing %s\n' "$path"
    return 0
  fi
  strict_step "generate $name"
  printf '[generate-authelia-secrets] writing %s\n' "$path"
  gen_random_b64 | install_secret_file 0600 "$path"
}

ensure_random_secret jwt-secret
ensure_random_secret session-secret
ensure_random_secret storage-encryption-key
ensure_random_secret oidc-hmac-secret

# OIDC issuer key — PKCS#8, RSA-2048, unencrypted (Authelia's `secret`
# template func reads it raw).
OIDC_KEY="$SECRETS_DIR/oidc-key.pem"
if [[ -s "$OIDC_KEY" ]]; then
  chmod 0600 "$OIDC_KEY"
  chown root:root "$OIDC_KEY"
  printf '[generate-authelia-secrets] keeping existing %s\n' "$OIDC_KEY"
else
  strict_step "generate oidc-key.pem (RSA-2048 PKCS#8)"
  printf '[generate-authelia-secrets] writing %s\n' "$OIDC_KEY"
  TMP_RAW="$(mktemp)"
  TMP_PK8="$(mktemp)"
  trap 'rm -f "$TMP_RAW" "$TMP_PK8"' EXIT
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$TMP_RAW" >/dev/null 2>&1
  openssl pkcs8 -topk8 -inform PEM -outform PEM -nocrypt -in "$TMP_RAW" -out "$TMP_PK8" >/dev/null 2>&1
  install_secret_file 0600 "$OIDC_KEY" < "$TMP_PK8"
  rm -f "$TMP_RAW" "$TMP_PK8"
  trap - EXIT
fi

# users_database.yml — Authelia refuses to start if the file is absent.
# enrol writes a real one when the wizard creates the first admin; until
# then we drop a minimal placeholder that loads but contains no users.
USERS_DIR="/opt/stacks/authelia/users_db"
USERS_DB="$USERS_DIR/users_database.yml"
install -d -m 0750 -o root -g root "$USERS_DIR"
if [[ ! -s "$USERS_DB" ]]; then
  strict_step "seed users_database.yml placeholder"
  printf '[generate-authelia-secrets] writing placeholder %s\n' "$USERS_DB"
  cat > "$USERS_DB" <<'EOF'
# Placeholder users_database.yml.
# Real users are written by enrol when the wizard creates the first admin.
# This file MUST exist (Authelia validates the path at startup) but a
# bare empty `users:` mapping is accepted.
users: {}
EOF
  chmod 0640 "$USERS_DB"
  chown root:root "$USERS_DB"
fi

printf '[generate-authelia-secrets] done — %s contents:\n' "$SECRETS_DIR"
ls -la "$SECRETS_DIR"
