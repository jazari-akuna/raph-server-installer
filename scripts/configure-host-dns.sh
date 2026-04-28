#!/usr/bin/env bash
# configure-host-dns.sh — set up systemd-resolved as the gw0-side caching
# resolver for VPN peers, and write /var/cache/raph/dns-servers.txt so
# enrol's renderClientConf points new peer .conf files at 10.99.0.1.
#
# Why: peer .conf default of "DNS = 1.1.1.1, 8.8.8.8" makes EVERY peer
# round-trip across the public internet from the VPS for every lookup,
# even cache-eligible ones. With the host's systemd-resolved binding to
# 10.99.0.1 (in addition to the default 127.0.0.53), all peers share a
# single warm cache. Cold-lookup latency unchanged; warm lookups go from
# 5-40ms to <1ms. A typical web page makes 10-30 lookups so this saves
# 50-500ms per warm visit.
#
# Idempotent: re-running on an already-configured host is a no-op.
#
# Usage:
#   sudo ./configure-host-dns.sh
#
# Optional env:
#   GW0_ADDR             default 10.99.0.1
#   UPSTREAM_DNS         default "1.1.1.1 8.8.8.8"
#   CACHE_DIR            default /var/cache/raph
#   RESOLVED_DROPIN      default /etc/systemd/resolved.conf.d/raph-gw0.conf

set -euo pipefail

GW0_ADDR="${GW0_ADDR:-10.99.0.1}"
UPSTREAM_DNS="${UPSTREAM_DNS:-1.1.1.1 8.8.8.8}"
CACHE_DIR="${CACHE_DIR:-/var/cache/raph}"
RESOLVED_DROPIN="${RESOLVED_DROPIN:-/etc/systemd/resolved.conf.d/raph-gw0.conf}"
DNS_FILE="${CACHE_DIR}/dns-servers.txt"

log() { printf '[configure-host-dns] %s\n' "$*"; }
die() { printf '[configure-host-dns] ERROR: %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root"

# Sanity: systemd-resolved must be present + active. Most Ubuntu LTS
# images have it on by default; older Debian or container hosts may not.
if ! systemctl is-active --quiet systemd-resolved; then
  die "systemd-resolved is not active — install systemd-resolved or set DNS manually"
fi

# Drop-in config: bind the stub resolver to GW0_ADDR (in addition to
# the default 127.0.0.53) and pin the upstream resolvers + cache.
mkdir -p "$(dirname "$RESOLVED_DROPIN")"
desired=$(cat <<EOF
# Managed by raph-server-installer. Do not edit by hand.
# Re-run scripts/configure-host-dns.sh to refresh.
[Resolve]
DNS=${UPSTREAM_DNS}
DNSStubListenerExtra=${GW0_ADDR}
Cache=yes
DNSStubListener=yes
EOF
)

if [[ -f "$RESOLVED_DROPIN" ]] && diff -q <(printf '%s\n' "$desired") "$RESOLVED_DROPIN" >/dev/null; then
  log "drop-in already current at $RESOLVED_DROPIN"
else
  tmp="$(mktemp)"
  printf '%s\n' "$desired" > "$tmp"
  chmod 0644 "$tmp"
  mv "$tmp" "$RESOLVED_DROPIN"
  log "wrote $RESOLVED_DROPIN"
  systemctl restart systemd-resolved
  log "systemd-resolved restarted"
fi

# Verify the new listener is up. Loop because the restart is async; ss
# reports columns: state recv-q send-q LOCAL_ADDR PEER_ADDR — the local
# address is the 4th field, not the 5th.
for i in 1 2 3 4 5 6 7 8 9 10; do
  if ss -ulnH 2>/dev/null | awk '{print $4}' | grep -qx "${GW0_ADDR}:53"; then
    log "listener bound on ${GW0_ADDR}:53"
    break
  fi
  sleep 1
  [[ $i -eq 10 ]] && die "systemd-resolved did not bind to ${GW0_ADDR}:53 — check 'systemctl status systemd-resolved'"
done

# Quick functional check.
if ! getent hosts -- example.com >/dev/null 2>&1; then
  log "warning: getent hosts example.com failed — upstream resolution may be broken"
fi

# Write the override file enrol reads from. Atomic to avoid half-writes
# if something kills us mid-write.
mkdir -p "$CACHE_DIR"
chmod 0755 "$CACHE_DIR"
tmp="${DNS_FILE}.tmp"
printf '%s\n' "${GW0_ADDR}" > "$tmp"
chmod 0644 "$tmp"
mv "$tmp" "$DNS_FILE"
log "wrote ${DNS_FILE} = ${GW0_ADDR}"

log "done — new peers will get DNS = ${GW0_ADDR}; existing peers must be re-created or update their .conf manually"
