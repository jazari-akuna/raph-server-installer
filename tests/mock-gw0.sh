#!/usr/bin/env bash
# mock-gw0.sh — drop-in replacement for scripts/install-gw0.sh used in
# `SKIP_GW0=0` test scenarios. We can't actually build the AmneziaWG DKMS
# module inside a container against a host kernel we don't control, so the
# mock pretends to install it: writes the expected sentinel files, logs the
# invocation, and exits 0.
#
# Usage: invoked by tests/scenarios.sh in scenarios that want to exercise
# the SKIP_GW0=0 code path without the real DKMS build.
#
# This file lives under tests/ and is NEVER copied into the production
# image. Production scripts are unmodified by its existence.

set -euo pipefail

LOG="${TEST_STUB_LOG:-/tmp/test-stubs.log}"
printf 'mock-gw0.sh %s\n' "$*" >> "$LOG"

AWG_CONF_DIR="/etc/amnezia/amneziawg"
AWG_CONF="${AWG_CONF_DIR}/gw0.conf"
AWG_PRIV="${AWG_CONF_DIR}/gw0_private.key"
AWG_PUB="${AWG_CONF_DIR}/gw0_public.key"

install -d -m 0755 "$AWG_CONF_DIR"

# Stub config — same shape as the real install-gw0.sh emits, with a banner
# making it clear this came from the mock.
cat > "$AWG_CONF" <<'EOF'
# MOCK gw0.conf produced by tests/mock-gw0.sh — NOT a real AmneziaWG config.
[Interface]
PrivateKey = MOCK_PRIVATE_KEY
Address    = 10.99.0.1/24
ListenPort = 51820
EOF
chmod 0600 "$AWG_CONF"

printf 'MOCK_PRIVATE_KEY\n' > "$AWG_PRIV"
chmod 0600 "$AWG_PRIV"
printf 'MOCK_PUBLIC_KEY\n'  > "$AWG_PUB"
chmod 0644 "$AWG_PUB"

echo "[mock-gw0] wrote stub $AWG_CONF, $AWG_PRIV, $AWG_PUB"
exit 0
