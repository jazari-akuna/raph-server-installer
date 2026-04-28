#!/usr/bin/env bash
# diag-against-existing-tunnel.sh — measure download/upload + diagnose
# bottleneck against an ALREADY-CONNECTED AmneziaWG tunnel. Use this
# when diag-from-client.sh's auto-install path fails (broken apt
# mirror, exotic distro, kernel without DKMS support, etc.).
#
# Workflow:
#   1. Install the AmneziaVPN GUI client (cross-distro AppImage — no
#      kernel module, no apt deps):
#         https://amnezia.org/downloads → "Linux"
#         chmod +x AmneziaVPN-*.AppImage
#         ./AmneziaVPN-*.AppImage
#      Or use the official "AmneziaWG" app if you prefer the
#      lighter-weight client.
#   2. In the GUI, import your amnezia.conf and click Connect.
#   3. Open a terminal, cd to wherever you saved THIS script:
#         sudo ./diag-against-existing-tunnel.sh
#      It will auto-detect the tunnel interface (anything routing
#      10.99.0.1) and run the full diagnostic battery against it.
#
# Required commands: ip, ping, dig (or nslookup), iperf3, ss, awk,
#   mtr (optional), speedtest-cli (optional).
# These are minor — install via your package manager if missing
# (apt/dnf/pacman/etc).

set -uo pipefail

SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
RUN_TS="$(date -u +%Y%m%dT%H%M%SZ)"
REPORT="${SCRIPT_DIR}/amnezia-diag-existing-${RUN_TS}.txt"
SERVER_IP="10.99.0.1"

log()  { printf '\n=== %s ===\n' "$*" | tee -a "$REPORT"; }
out()  { printf '%s\n' "$*" | tee -a "$REPORT"; }
run()  { printf '$ %s\n' "$*" | tee -a "$REPORT"; eval "$@" 2>&1 | tee -a "$REPORT"; printf '\n' | tee -a "$REPORT"; }

if [[ $EUID -ne 0 ]]; then
  echo "ERROR: must run as root (sudo $0)" >&2
  exit 1
fi
> "$REPORT"
log "diag-against-existing-tunnel — ${RUN_TS}"
out "host: $(hostnamectl hostname 2>/dev/null || hostname)"
out "kernel: $(uname -r)"
out "distro: $(lsb_release -d 2>/dev/null | cut -f2- || cat /etc/os-release | grep PRETTY_NAME | cut -d= -f2 | tr -d '\"')"
out "report: ${REPORT}"

# ----- 0. detect the tunnel interface ---------------------------------------
log "auto-detect tunnel interface (looking for the iface that routes to ${SERVER_IP})"
# `ip route get` reports which iface the kernel will use to reach SERVER_IP.
# If a tunnel is up, it'll be the tunnel iface; if not, it's the default route.
ROUTE_INFO="$(ip route get "$SERVER_IP" 2>&1 || true)"
out "ip route get ${SERVER_IP}:"
out "  $ROUTE_INFO"
IFACE="$(printf '%s\n' "$ROUTE_INFO" | awk '{for(i=1;i<NF;i++) if($i=="dev") print $(i+1)}' | head -n1)"

if [[ -z "${IFACE:-}" ]]; then
  out "ERROR: could not determine the iface for ${SERVER_IP}."
  out "       The tunnel doesn't seem to be up. Connect via the AmneziaVPN"
  out "       GUI app first, then re-run this script."
  exit 1
fi

# Sanity check: if the iface is the default route's iface (e.g. wlan0,
# eth0), the tunnel really is down — we'd be sending traffic in the
# clear out the public NIC and ofc 10.99.0.1 would not respond.
DEFAULT_IFACE="$(ip -o route show default | awk '{print $5; exit}')"
if [[ "$IFACE" == "$DEFAULT_IFACE" ]]; then
  out "WARN: ${SERVER_IP} routes via the default iface (${DEFAULT_IFACE})."
  out "      That means there is no tunnel between you and ${SERVER_IP}."
  out "      Open the AmneziaVPN app and click Connect, then re-run."
  exit 1
fi

out "  → tunnel iface: ${IFACE}"
run "ip link show ${IFACE}"
run "ip addr show ${IFACE}"
# awg show works only if amneziawg-tools is installed; the GUI app does
# NOT install them, so swallow the error.
run "awg show ${IFACE} 2>&1 || echo '(awg-tools not installed — skipping handshake details)'"

# ----- 1. baseline kernel state --------------------------------------------
log "kernel TCP knobs (receiver-side caps the achievable download)"
run "sysctl net.ipv4.tcp_congestion_control net.core.default_qdisc net.ipv4.tcp_rmem net.ipv4.tcp_wmem net.core.rmem_max net.core.wmem_max net.ipv4.tcp_window_scaling net.ipv4.tcp_mtu_probing"

# ----- 2. reachability + PMTU ----------------------------------------------
log "reachability + PMTU"
run "ping -c 5 -W 2 ${SERVER_IP}"
out "  PMTU probe at server inner-MTU 1420 (ping payload 1392 + 28 ICMP overhead):"
run "ping -M do -s 1392 -c 3 -W 2 ${SERVER_IP} 2>&1 | tail -5"
out "  PMTU probe one byte above (should fail — that's the hard ceiling):"
run "ping -M do -s 1393 -c 3 -W 2 ${SERVER_IP} 2>&1 | tail -5"
out "  DNS via tunnel"
run "dig @${SERVER_IP} +short +tries=1 +time=3 example.com 2>&1 || nslookup example.com ${SERVER_IP} 2>&1 | tail -5"

# ----- 3. iperf3 (in-tunnel — the gateway runs iperf3 -s on 10.99.0.1) ----
if ! command -v iperf3 >/dev/null 2>&1; then
  out "iperf3 not installed — install it (apt install iperf3 / dnf install iperf3) and re-run for the throughput measurements."
else
  HAS_JQ=0
  command -v jq >/dev/null 2>&1 && HAS_JQ=1

  log "iperf3 SINGLE-STREAM DOWNLOAD (server→client, 30s, 1s interval)"
  if [[ $HAS_JQ -eq 1 ]]; then
    run "iperf3 -c ${SERVER_IP} -t 30 -i 1 -R -O 2 --json > /tmp/iperf-down.json && jq -r '.intervals[] | \"\(.sum.start | floor)-\(.sum.end | floor)s  \(.sum.bits_per_second/1000000 | round) Mbps  retr=\(.sum.retransmits)\"' /tmp/iperf-down.json && echo --- && jq -r '.end | \"summary: \(.sum_received.bits_per_second/1000000 | round) Mbps received, \(.sum_sent.retransmits) total retransmits\"' /tmp/iperf-down.json"
  else
    run "iperf3 -c ${SERVER_IP} -t 30 -i 1 -R"
  fi

  log "iperf3 SINGLE-STREAM UPLOAD (client→server, 30s, 1s interval)"
  if [[ $HAS_JQ -eq 1 ]]; then
    run "iperf3 -c ${SERVER_IP} -t 30 -i 1 -O 2 --json > /tmp/iperf-up.json && jq -r '.intervals[] | \"\(.sum.start | floor)-\(.sum.end | floor)s  \(.sum.bits_per_second/1000000 | round) Mbps  retr=\(.sum.retransmits)\"' /tmp/iperf-up.json && echo --- && jq -r '.end | \"summary: \(.sum_sent.bits_per_second/1000000 | round) Mbps sent, \(.sum_sent.retransmits) total retransmits\"' /tmp/iperf-up.json"
  else
    run "iperf3 -c ${SERVER_IP} -t 30 -i 1"
  fi

  log "iperf3 PARALLEL 4-STREAM DOWNLOAD (does it scale?)"
  if [[ $HAS_JQ -eq 1 ]]; then
    run "iperf3 -c ${SERVER_IP} -t 20 -i 5 -R -P 4 -O 2 --json > /tmp/iperf-down-p4.json && jq -r '.end | \"summary 4-stream: \(.sum_received.bits_per_second/1000000 | round) Mbps received total, \(.sum_sent.retransmits) total retransmits\"' /tmp/iperf-down-p4.json"
  else
    run "iperf3 -c ${SERVER_IP} -t 20 -i 5 -R -P 4"
  fi

  log "TCP socket internals during a 20s download (snapshots @ 5,10,15s)"
  iperf3 -c "$SERVER_IP" -t 20 -R --json > /tmp/iperf-down-ssprobe.json 2>&1 &
  IPERF_PID=$!
  for s in 5 10 15; do
    sleep 5
    out "  --- ss -tin dst ${SERVER_IP}  (t=${s}s) ---"
    ss -tin "dst ${SERVER_IP}" 2>&1 | tee -a "$REPORT" || true
  done
  wait $IPERF_PID 2>/dev/null || true
fi

# ----- 4. external speedtest (real-world ↓↑) -------------------------------
log "external speedtest (in-tunnel — your real-world download experience)"
if command -v speedtest-cli >/dev/null 2>&1; then
  run "speedtest-cli --simple"
elif command -v speedtest >/dev/null 2>&1; then
  run "speedtest --simple"
else
  out "  speedtest-cli not installed — skipping. (apt install speedtest-cli)"
fi

# ----- 5. counters after the load -----------------------------------------
log "counters after the load (drops / retransmits / udp errors)"
run "nstat -a -z 2>&1 | grep -E 'TcpRetransSegs|TcpExtTCPRetransFail|TcpExtTCPLost|TcpExtTCPSACKReneging|UdpRcvbufErrors|UdpInErrors|UdpNoPorts'"
run "tc -s qdisc show dev ${IFACE}"
run "ip -s link show ${IFACE}"
run "mtr -c 10 -r -w ${SERVER_IP} 2>/dev/null || echo 'mtr not installed (apt install mtr-tiny)'"

log "DONE"
out "report: ${REPORT}"
out "send that file back to diagnose."
