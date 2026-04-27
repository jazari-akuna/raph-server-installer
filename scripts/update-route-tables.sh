#!/usr/bin/env bash
# update-route-tables.sh — manage the chnroutes2 IPv4 complement.
#
# The regional split lives entirely in the client peer config's AllowedIPs:
# "everything except mainland-CN" → routed through gw0; CN-destined packets
# fall back to the system default route. The server is unaware of this split.
#
# Modes:
#   --refresh             (default) ensure ~/.cache/raph/chnroutes.txt is
#                          fresh (re-fetch only if older than 25 days), then
#                          recompute the complement → ~/.cache/raph/allowed-ips.txt
#   --print-allowed-ips    cat the cached complement
#   --regenerate-peers     rewrite peers/*.conf AllowedIPs lines from the cache
#
# Source list: https://github.com/misakaio/chnroutes2 (chnroutes.txt)
# chnroutes2 is published under the MIT License — see the upstream repo's
# LICENSE file. We redistribute nothing; we fetch the .txt at runtime.
#
# IPv4-complement implementation note:
#   Computing 0.0.0.0/0 minus ~5000 CIDRs is not safe in pure shell. We use
#   Python's stdlib `ipaddress` module (collapse_addresses + address_exclude)
#   which is correct, fast, and ships with every modern distro. No pip deps.
#
# Idempotent. set -euo pipefail.

set -euo pipefail

# --- paths --------------------------------------------------------------------

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PEERS_DIR="$REPO_ROOT/peers"

if [[ "${EUID:-$(id -u)}" -eq 0 ]]; then
    CACHE_DIR="/var/cache/raph"
else
    CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/raph"
fi

CHNROUTES_FILE="$CACHE_DIR/chnroutes.txt"
ALLOWED_IPS_FILE="$CACHE_DIR/allowed-ips.txt"
CHNROUTES_URL="https://raw.githubusercontent.com/misakaio/chnroutes2/master/chnroutes.txt"
MAX_AGE_DAYS=25

MARKER="# AllowedIPs managed by update-route-tables.sh"

mkdir -p "$CACHE_DIR"

# --- helpers ------------------------------------------------------------------

log()  { printf '[update-route-tables] %s\n' "$*" >&2; }
die()  { printf '[update-route-tables] error: %s\n' "$*" >&2; exit 1; }

needs_refresh() {
    local f="$1"
    [[ -s "$f" ]] || return 0
    # Re-fetch if older than MAX_AGE_DAYS days
    if find "$f" -mtime -"$MAX_AGE_DAYS" -print -quit | grep -q .; then
        return 1
    fi
    return 0
}

fetch_chnroutes() {
    log "fetching chnroutes2 → $CHNROUTES_FILE"
    local tmp
    tmp="$(mktemp "${CHNROUTES_FILE}.XXXXXX")"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 --retry-delay 2 -o "$tmp" "$CHNROUTES_URL"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$tmp" "$CHNROUTES_URL"
    else
        rm -f "$tmp"
        die "neither curl nor wget is installed"
    fi
    [[ -s "$tmp" ]] || { rm -f "$tmp"; die "downloaded chnroutes is empty"; }
    mv "$tmp" "$CHNROUTES_FILE"
}

# Compute the IPv4 complement of the chnroutes set: 0.0.0.0/0 minus all CN
# CIDRs. Output: comma-separated CIDRs, no whitespace, suitable for an
# AllowedIPs line.
compute_complement() {
    python3 - "$CHNROUTES_FILE" <<'PY' > "$ALLOWED_IPS_FILE.tmp"
import sys
from ipaddress import IPv4Network, collapse_addresses

src = sys.argv[1]
cn = []
with open(src) as fh:
    for line in fh:
        line = line.strip()
        if not line or line.startswith('#'):
            continue
        try:
            cn.append(IPv4Network(line, strict=False))
        except ValueError:
            continue

# Collapse adjacent/overlapping CN networks first — keeps exclusion fast.
cn = list(collapse_addresses(cn))

remaining = [IPv4Network('0.0.0.0/0')]
for excl in cn:
    nxt = []
    for net in remaining:
        if not net.overlaps(excl):
            nxt.append(net)
            continue
        if excl.subnet_of(net):
            nxt.extend(net.address_exclude(excl))
        else:
            # Partial overlap: shouldn't happen with well-formed input, but
            # be defensive — drop pieces that fall inside excl.
            for piece in net.address_exclude(excl) if excl.subnet_of(net) else [net]:
                if not piece.overlaps(excl):
                    nxt.append(piece)
    remaining = nxt

remaining = sorted(collapse_addresses(remaining), key=lambda n: (int(n.network_address), n.prefixlen))
sys.stdout.write(','.join(str(n) for n in remaining))
PY
    [[ -s "$ALLOWED_IPS_FILE.tmp" ]] || die "complement computation produced no output"
    mv "$ALLOWED_IPS_FILE.tmp" "$ALLOWED_IPS_FILE"
    log "wrote complement → $ALLOWED_IPS_FILE ($(wc -c < "$ALLOWED_IPS_FILE") bytes)"
}

mode_refresh() {
    if needs_refresh "$CHNROUTES_FILE"; then
        fetch_chnroutes
    else
        log "chnroutes cache is fresh (<${MAX_AGE_DAYS}d); skipping fetch"
    fi
    compute_complement
}

mode_print() {
    [[ -s "$ALLOWED_IPS_FILE" ]] || mode_refresh
    cat "$ALLOWED_IPS_FILE"
}

mode_regenerate_peers() {
    [[ -s "$ALLOWED_IPS_FILE" ]] || mode_refresh
    [[ -d "$PEERS_DIR" ]] || die "peers dir not found: $PEERS_DIR"

    local allowed_ips
    allowed_ips="$(cat "$ALLOWED_IPS_FILE")"

    shopt -s nullglob
    local conf count=0
    for conf in "$PEERS_DIR"/*.conf; do
        # Rewrite the line that follows the marker, or insert marker+line at
        # the end of [Interface] / start of [Peer] if missing. Using awk for
        # in-place rewrite via temp file (idempotent).
        local tmp
        tmp="$(mktemp "${conf}.XXXXXX")"
        MARKER="$MARKER" ALLOWED_IPS="$allowed_ips" awk '
            BEGIN {
                marker = ENVIRON["MARKER"]
                ips    = ENVIRON["ALLOWED_IPS"]
                seen_marker = 0
                replaced    = 0
            }
            {
                if ($0 == marker) {
                    print
                    print "AllowedIPs = " ips
                    seen_marker = 1
                    replaced = 1
                    # skip the next line if it is an AllowedIPs line
                    if ((getline nxt) > 0) {
                        if (nxt !~ /^[[:space:]]*AllowedIPs[[:space:]]*=/) {
                            print nxt
                        }
                    }
                    next
                }
                print
            }
            END {
                if (!seen_marker) {
                    # Marker absent: emit an [Peer]-adjacent block at EOF as a
                    # safe fallback. Operator should re-run provision-peer.sh
                    # for a clean config; this just keeps the file functional.
                    print ""
                    print marker
                    print "AllowedIPs = " ips
                }
            }
        ' "$conf" > "$tmp"
        mv "$tmp" "$conf"
        chmod 600 "$conf"
        count=$((count+1))
        log "rewrote AllowedIPs in $conf"
    done
    log "regenerated $count peer config(s)"
}

# --- main ---------------------------------------------------------------------

mode="${1:---refresh}"
case "$mode" in
    --refresh|"") mode_refresh ;;
    --print-allowed-ips) mode_print ;;
    --regenerate-peers) mode_regenerate_peers ;;
    -h|--help)
        sed -n '2,15p' "$0"
        ;;
    *) die "unknown mode: $mode (try --refresh | --print-allowed-ips | --regenerate-peers)" ;;
esac
