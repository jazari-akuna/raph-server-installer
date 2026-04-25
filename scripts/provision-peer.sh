#!/usr/bin/env bash
# provision-peer.sh — generate a per-device gw0 peer config.
#
# Usage:
#   scripts/provision-peer.sh <peer-name>
#       e.g. scripts/provision-peer.sh sagan-phone
#            scripts/provision-peer.sh marcus-laptop
#
# Environment:
#   AWG_HOST    SSH target for the VPS (user@host or alias). Required when
#               run from the laptop. If unset and we appear to be on the VPS
#               (gw0.conf is locally readable), we operate locally.
#   AWG_ENDPOINT  Public endpoint to put in the client config.
#               Default: gw.antarctica-engineering.com:51820
#   AWG_DIR     Server-side AmneziaWG config dir.
#               Default: /etc/amnezia/amneziawg
#   AWG_IFACE   Interface name. Default: gw0
#   PEER_SUBNET Peer subnet. Default: 10.99.0.0/24
#   PEER_START  First peer IP host octet (low IPs reserved). Default: 10
#
# Runs on either the laptop or the VPS:
#   - On the VPS: reads/writes /etc/amnezia/amneziawg/* directly (needs root).
#   - On the laptop: reads/writes via `ssh root@$AWG_HOST`. Requires that the
#     SSH user can read gw0_public.key, gw0.conf, and append to gw0.conf.
#
# Output (always on the laptop side, always under the repo, gitignored):
#   peers/<name>.conf        client config (mode 0600)
#   peers/<name>.qr.png      QR for mobile import
#   plus an ANSI-UTF8 QR printed to stdout for quick on-screen scanning.
#
# The server-side AllowedIPs for this peer is just the peer's own /32 — the
# regional split is entirely client-side, encoded into the client's AllowedIPs
# via update-route-tables.sh (chnroutes2 complement).
#
# set -euo pipefail. Idempotent (re-running with the same name reuses the
# server-side peer entry if the public key matches; otherwise refuses).

set -euo pipefail

# --- args & defaults ----------------------------------------------------------

if [[ $# -lt 1 ]]; then
    echo "usage: $0 <peer-name>" >&2
    exit 2
fi

PEER_NAME="$1"
[[ "$PEER_NAME" =~ ^[a-zA-Z0-9._-]+$ ]] \
    || { echo "peer name must match [a-zA-Z0-9._-]+" >&2; exit 2; }

AWG_DIR="${AWG_DIR:-/etc/amnezia/amneziawg}"
AWG_IFACE="${AWG_IFACE:-gw0}"
AWG_ENDPOINT="${AWG_ENDPOINT:-gw.antarctica-engineering.com:51820}"
PEER_SUBNET="${PEER_SUBNET:-10.99.0.0/24}"
PEER_START="${PEER_START:-10}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PEERS_DIR="$REPO_ROOT/peers"
ROUTES_SCRIPT="$REPO_ROOT/scripts/update-route-tables.sh"

mkdir -p "$PEERS_DIR"
chmod 700 "$PEERS_DIR"

# --- transport: local on VPS, ssh from laptop --------------------------------

# We pick a transport so the rest of the script can call `srv_*` helpers
# uniformly. If $AWG_HOST is set we always use ssh (lets the user force the
# laptop path even on the VPS for testing); otherwise we use local IO if
# gw0.conf is locally readable.

if [[ -n "${AWG_HOST:-}" ]]; then
    MODE="remote"
elif [[ -r "$AWG_DIR/${AWG_IFACE}.conf" ]]; then
    MODE="local"
else
    echo "AWG_HOST is unset and $AWG_DIR/${AWG_IFACE}.conf is not readable here." >&2
    echo "Set AWG_HOST=user@vps to run from the laptop." >&2
    exit 1
fi

srv_run() {
    # Run a command on the server. stdin → server stdin, stdout → server stdout.
    if [[ "$MODE" == "remote" ]]; then
        ssh -o BatchMode=yes "$AWG_HOST" "$@"
    else
        bash -c "$*"
    fi
}

srv_cat() {        # srv_cat <path>
    if [[ "$MODE" == "remote" ]]; then
        ssh -o BatchMode=yes "$AWG_HOST" "cat $1"
    else
        cat "$1"
    fi
}

srv_append() {     # srv_append <path>  (content on stdin)
    if [[ "$MODE" == "remote" ]]; then
        ssh -o BatchMode=yes "$AWG_HOST" "cat >> $1"
    else
        cat >> "$1"
    fi
}

srv_test_iface_up() {
    srv_run "ip link show ${AWG_IFACE} 2>/dev/null | grep -q 'state UP'" \
        && return 0 || return 1
}

# --- read server pubkey & anti-DPI params ------------------------------------

log() { printf '[provision-peer] %s\n' "$*" >&2; }

log "reading server public key from $AWG_DIR/${AWG_IFACE}_public.key"
SERVER_PUBKEY="$(srv_cat "$AWG_DIR/${AWG_IFACE}_public.key" | tr -d '[:space:]')"
[[ -n "$SERVER_PUBKEY" ]] || { echo "empty server public key" >&2; exit 1; }

log "reading server config $AWG_DIR/${AWG_IFACE}.conf"
SERVER_CONF="$(srv_cat "$AWG_DIR/${AWG_IFACE}.conf")"

# Extract the AmneziaWG anti-DPI params from [Interface] — they MUST match
# byte-for-byte on the client or the handshake will not succeed.
extract_param() {
    local key="$1"
    awk -v k="$key" '
        /^\[Interface\]/ { in_iface = 1; next }
        /^\[/            { in_iface = 0 }
        in_iface && $0 ~ "^[[:space:]]*"k"[[:space:]]*=" {
            sub("^[^=]*=[[:space:]]*", "", $0)
            sub("[[:space:]]*$", "", $0)
            print
            exit
        }
    ' <<<"$SERVER_CONF"
}

JC=$(extract_param Jc)
JMIN=$(extract_param Jmin)
JMAX=$(extract_param Jmax)
S1=$(extract_param S1)
S2=$(extract_param S2)
H1=$(extract_param H1)
H2=$(extract_param H2)
H3=$(extract_param H3)
H4=$(extract_param H4)

for v in JC JMIN JMAX S1 S2 H1 H2 H3 H4; do
    if [[ -z "${!v}" ]]; then
        echo "missing AmneziaWG param $v in server config; aborting" >&2
        exit 1
    fi
done

# --- pick next free peer IP --------------------------------------------------

# Parse all AllowedIPs entries from server [Peer] blocks; collect host octets
# in $PEER_SUBNET; pick the lowest unused at or above $PEER_START.

SUBNET_PREFIX="${PEER_SUBNET%.*}"   # 10.99.0.0/24 → 10.99.0
USED_OCTETS="$(awk -v pfx="$SUBNET_PREFIX" '
    /^\[Peer\]/   { in_peer = 1; next }
    /^\[/         { in_peer = 0 }
    in_peer && /^[[:space:]]*AllowedIPs[[:space:]]*=/ {
        sub("^[^=]*=", "", $0)
        n = split($0, parts, ",")
        for (i = 1; i <= n; i++) {
            ip = parts[i]
            gsub(/[[:space:]]/, "", ip)
            sub("/.*", "", ip)
            if (index(ip, pfx".") == 1) {
                octet = substr(ip, length(pfx)+2)
                print octet
            }
        }
    }
' <<<"$SERVER_CONF" | sort -un)"

next_octet=""
for candidate in $(seq "$PEER_START" 254); do
    if ! grep -qx "$candidate" <<<"$USED_OCTETS"; then
        next_octet="$candidate"
        break
    fi
done
[[ -n "$next_octet" ]] || { echo "no free peer IP in $PEER_SUBNET" >&2; exit 1; }

PEER_IP="${SUBNET_PREFIX}.${next_octet}"
log "assigning peer IP: $PEER_IP/32"

# --- generate peer keypair locally (umask 077) -------------------------------

OLD_UMASK=$(umask)
umask 077

PEER_PRIV_FILE="$(mktemp)"
trap 'shred -u "$PEER_PRIV_FILE" 2>/dev/null || rm -f "$PEER_PRIV_FILE"' EXIT

# Prefer awg (AmneziaWG userspace) — falls back to wg if awg isn't on the
# laptop. Both produce compatible Curve25519 keys.
if command -v awg >/dev/null 2>&1; then
    KEYGEN="awg"
elif command -v wg >/dev/null 2>&1; then
    KEYGEN="wg"
else
    echo "need either 'awg' or 'wg' on this host to generate keys" >&2
    exit 1
fi

"$KEYGEN" genkey > "$PEER_PRIV_FILE"
PEER_PRIVKEY="$(cat "$PEER_PRIV_FILE")"
PEER_PUBKEY="$("$KEYGEN" pubkey < "$PEER_PRIV_FILE")"

umask "$OLD_UMASK"

# --- compute client AllowedIPs (chnroutes2 complement) -----------------------

log "computing client AllowedIPs (chnroutes2 complement)"
CLIENT_ALLOWED_IPS="$("$ROUTES_SCRIPT" --print-allowed-ips)"
[[ -n "$CLIENT_ALLOWED_IPS" ]] || { echo "empty AllowedIPs from $ROUTES_SCRIPT" >&2; exit 1; }

# --- append [Peer] block to server gw0.conf (idempotent) ---------------------

# If a [Peer] block with our PublicKey already exists, do nothing.
if grep -qF "PublicKey = $PEER_PUBKEY" <<<"$SERVER_CONF"; then
    log "peer with this public key already in server config; skipping append"
else
    log "appending [Peer] block to $AWG_DIR/${AWG_IFACE}.conf"
    {
        printf '\n# peer: %s (added %s)\n' "$PEER_NAME" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        printf '[Peer]\n'
        printf 'PublicKey = %s\n' "$PEER_PUBKEY"
        printf 'AllowedIPs = %s/32\n' "$PEER_IP"
    } | srv_append "$AWG_DIR/${AWG_IFACE}.conf"
fi

# Live-update the running interface if it's up; otherwise the operator will
# reload gw0 manually (or via deploy.sh).
if srv_test_iface_up; then
    log "interface ${AWG_IFACE} is up; applying peer live via awg set"
    srv_run "awg set ${AWG_IFACE} peer ${PEER_PUBKEY} allowed-ips ${PEER_IP}/32" \
        || log "awg set failed (non-fatal); reload ${AWG_IFACE} manually"
else
    log "interface ${AWG_IFACE} is not up; appended only — reload required"
fi

# --- render client config ----------------------------------------------------

CLIENT_CONF="$PEERS_DIR/${PEER_NAME}.conf"
QR_PNG="$PEERS_DIR/${PEER_NAME}.qr.png"

umask 077
cat > "$CLIENT_CONF" <<EOF
# rarcus-server peer: ${PEER_NAME}
# generated $(date -u +%Y-%m-%dT%H:%M:%SZ)
# AllowedIPs = chnroutes2 complement (mainland-CN excluded; refresh monthly
#   via scripts/update-route-tables.sh --regenerate-peers)
[Interface]
PrivateKey = ${PEER_PRIVKEY}
Address = ${PEER_IP}/32
DNS = 1.1.1.1, 8.8.8.8
Jc = ${JC}
Jmin = ${JMIN}
Jmax = ${JMAX}
S1 = ${S1}
S2 = ${S2}
H1 = ${H1}
H2 = ${H2}
H3 = ${H3}
H4 = ${H4}

[Peer]
PublicKey = ${SERVER_PUBKEY}
Endpoint = ${AWG_ENDPOINT}
PersistentKeepalive = 25
# AllowedIPs managed by update-route-tables.sh
AllowedIPs = ${CLIENT_ALLOWED_IPS}
EOF
# The marker comment above is canonical: update-route-tables.sh
# --regenerate-peers searches for that exact line and rewrites the
# AllowedIPs immediately after it.
chmod 600 "$CLIENT_CONF"
umask "$OLD_UMASK"

# --- QR codes ----------------------------------------------------------------

if ! command -v qrencode >/dev/null 2>&1; then
    echo "qrencode not installed; skipping QR generation. Install with: apt install qrencode" >&2
else
    qrencode -o "$QR_PNG" < "$CLIENT_CONF"
    chmod 600 "$QR_PNG"
    echo
    qrencode -t ansiutf8 < "$CLIENT_CONF"
    echo
fi

# --- summary -----------------------------------------------------------------

cat <<EOF

provisioned peer:
  name        ${PEER_NAME}
  ip          ${PEER_IP}/32
  pubkey      ${PEER_PUBKEY}
  client conf ${CLIENT_CONF}
  qr png      ${QR_PNG}
  endpoint    ${AWG_ENDPOINT}
  server pub  ${SERVER_PUBKEY}

next steps:
  - import ${CLIENT_CONF} (or scan ${QR_PNG}) on the device
  - if ${AWG_IFACE} was not live-updated, reload it on the VPS:
      systemctl restart awg-quick@${AWG_IFACE}
EOF
