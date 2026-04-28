#!/usr/bin/env bash
# diag-from-client.sh — Ubuntu 24.04 client-side diagnostic for AmneziaWG
# tunnel performance.
#
# What it does:
#   1. Installs the AmneziaWG kernel module + userspace tools (idempotent).
#   2. Looks for `amnezia.conf` next to this script (waits up to 60s if
#      absent; tells you where to drop it).
#   3. Captures BASELINE network state with the tunnel DOWN: speedtest,
#      MTR, kernel TCP sysctls, NIC info, congestion-control settings.
#   4. Brings the tunnel UP on a unique iface name (`amndiag`) so it
#      doesn't collide with anything else you might be running.
#   5. Captures IN-TUNNEL state: handshake confirmation, MTU + PMTU
#      probe, single-stream download, single-stream upload, parallel
#      4-stream download, ss -tin snapshots during the download (BBR
#      bandwidth/cwnd/rtt to identify whether loss or rwnd is the cap).
#   6. Captures POST-TUNNEL speedtest to confirm the baseline restored.
#   7. Tears the tunnel down (cleanup-on-EXIT trap; survives Ctrl-C).
#   8. Writes a self-contained report next to this script:
#        amnezia-diag-<UTC-timestamp>.txt
#
# Send the resulting report file back. It contains no secrets — the
# AmneziaWG private key is NOT included; we extract only the public key
# for handshake correlation.
#
# Usage:
#   1. Drop this script + amnezia.conf into the same folder.
#   2. sudo ./diag-from-client.sh
#   3. Send the amnezia-diag-*.txt file that's written.
#
# Tested on Ubuntu 24.04 LTS (noble). May work on 22.04 with some
# package-name drift; not exercised.

set -uo pipefail

SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
CONF_SRC="${SCRIPT_DIR}/amnezia.conf"
RUN_TS="$(date -u +%Y%m%dT%H%M%SZ)"
REPORT="${SCRIPT_DIR}/amnezia-diag-${RUN_TS}.txt"
IFACE="amndiag"
SERVER_IP="10.99.0.1"
TMPCONF="/tmp/${IFACE}.conf"

log()  { printf '\n=== %s ===\n' "$*" | tee -a "$REPORT"; }
out()  { printf '%s\n' "$*" | tee -a "$REPORT"; }
run()  { printf '$ %s\n' "$*" | tee -a "$REPORT"; eval "$@" 2>&1 | tee -a "$REPORT"; printf '\n' | tee -a "$REPORT"; }
runq() { eval "$@" 2>&1; }

cleanup() {
  printf '\n=== cleanup ===\n' | tee -a "$REPORT" 2>/dev/null || true
  if ip link show "$IFACE" >/dev/null 2>&1; then
    awg-quick down "$TMPCONF" 2>&1 | tee -a "$REPORT" 2>/dev/null || true
  fi
  rm -f "$TMPCONF"
}
trap cleanup EXIT INT TERM

# ----- 0. preflight ---------------------------------------------------------
if [[ $EUID -ne 0 ]]; then
  echo "ERROR: must run as root (sudo $0)" >&2
  exit 1
fi
> "$REPORT"
log "raph-server-installer client diag — ${RUN_TS}"
out "host: $(hostnamectl hostname 2>/dev/null || hostname)"
out "kernel: $(uname -r)"
out "distro: $(lsb_release -d 2>/dev/null | cut -f2- || cat /etc/os-release | grep PRETTY_NAME | cut -d= -f2 | tr -d '\"')"
out "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | sed 's/^ *//')"
out "report: ${REPORT}"

# ----- 1. wait for amnezia.conf --------------------------------------------
log "waiting for amnezia.conf"
for i in 1 2 3 4 5 6; do
  if [[ -f "$CONF_SRC" ]]; then
    out "found ${CONF_SRC} ($(stat -c%s "$CONF_SRC") bytes)"
    break
  fi
  out "  not yet at ${CONF_SRC} (try ${i}/6 — drop it there now)"
  sleep 10
done
if [[ ! -f "$CONF_SRC" ]]; then
  out "ERROR: ${CONF_SRC} not found after 60s — abort"
  exit 1
fi

# Sanity-check the conf
if ! grep -q '^\[Interface\]' "$CONF_SRC" || ! grep -q '^PrivateKey = ' "$CONF_SRC"; then
  out "ERROR: ${CONF_SRC} doesn't look like an AmneziaWG config (no [Interface]/PrivateKey)"
  exit 1
fi
PEER_PUB="$(awk '/^\[Peer\]/{p=1;next} p && /^PublicKey/ {gsub(/PublicKey *= */,""); print; exit}' "$CONF_SRC")"
out "peer public key (server identity): ${PEER_PUB:0:16}…"

# ----- 2. install AmneziaWG ------------------------------------------------
log "install AmneziaWG (kernel module if available, userspace fallback otherwise)"
RUNNING_KERNEL="$(uname -r)"
CODENAME="$(lsb_release -cs 2>/dev/null || awk -F= '/UBUNTU_CODENAME/{print $2}' /etc/os-release | tr -d '\"')"
out "  running kernel: ${RUNNING_KERNEL}"
out "  ubuntu codename: ${CODENAME:-<unknown>}"

# ppa:amnezia/ppa publishes packages for these series only:
#   trusty xenial bionic focal jammy noble oracular plucky
# (verified at https://launchpad.net/~amnezia/+archive/ubuntu/ppa).
# Anything else (questing 25.10, future releases) — go straight to source build.
PPA_SUPPORTED="trusty xenial bionic focal jammy noble oracular plucky"
USE_PPA=0
case " $PPA_SUPPORTED " in *" ${CODENAME} "*) USE_PPA=1 ;; esac

# Ensure 'universe' is enabled (provides dkms, golang-go, build-essential
# on minimal images where it's disabled by default).
if ! command -v add-apt-repository >/dev/null 2>&1; then
  out "  installing software-properties-common (provides add-apt-repository)"
  apt-get update -qq
  apt-get install -y software-properties-common 2>&1 | tail -3 | tee -a "$REPORT"
fi
out "  enabling universe (provides dkms, golang-go, build-essential)"
add-apt-repository -y universe 2>&1 | tail -3 | tee -a "$REPORT" || true
apt-get update -qq

INSTALL_OK=0  # set to 1 once awg + awg-quick are both on PATH

# ===== Path A: PPA + DKMS kernel module (preferred — fastest) ==============
if [[ $USE_PPA -eq 1 ]]; then
  log "path A: ppa:amnezia/ppa + DKMS kernel module"
  if ! grep -rq 'amnezia' /etc/apt/sources.list.d/ 2>/dev/null; then
    out "  adding ppa:amnezia/ppa"
    add-apt-repository -y ppa:amnezia/ppa 2>&1 | tail -3 | tee -a "$REPORT"
    apt-get update 2>&1 | tail -3 | tee -a "$REPORT"
  fi

  out "  installing linux-headers-${RUNNING_KERNEL} + build-essential + dkms"
  if apt-get install -y \
       "linux-headers-${RUNNING_KERNEL}" build-essential dkms 2>&1 | tail -5 | tee -a "$REPORT"; then
    out "  installing amneziawg-tools + amneziawg-dkms"
    if apt-get install -y amneziawg-tools amneziawg-dkms 2>&1 | tail -8 | tee -a "$REPORT"; then
      dkms autoinstall 2>&1 | tail -5 | tee -a "$REPORT" || true
      if modinfo amneziawg >/dev/null 2>&1 && modprobe amneziawg 2>&1 | tee -a "$REPORT"; then
        if command -v awg >/dev/null 2>&1 && command -v awg-quick >/dev/null 2>&1; then
          INSTALL_OK=1
          out "  path A succeeded — kernel module loaded, userspace tools on PATH"
        else
          out "  path A: kernel module loaded but userspace binaries missing — falling back"
        fi
      else
        out "  path A: DKMS build did not yield a loadable module — falling back to userspace"
      fi
    else
      out "  path A: amneziawg-{tools,dkms} apt install failed — falling back to source"
    fi
  else
    out "  path A: linux-headers-${RUNNING_KERNEL} install failed — falling back to source"
  fi
fi

# ===== Path B: build from source (works on any distro) =====================
if [[ $INSTALL_OK -eq 0 ]]; then
  log "path B: building amneziawg-tools + amneziawg-go from source"
  out "  this works on any distro/kernel; ~30% slower than the kernel module."
  out "  good enough for download-throughput diagnosis."
  out "  installing build deps (build-essential, golang-go, git, libmnl-dev, pkg-config)"
  # build-essential pulls in libc6-dev (string.h, arpa/inet.h) which a bare
  # 'gcc' install on minimal Ubuntu images does NOT pull in.
  apt-get install -y build-essential golang-go git libmnl-dev pkg-config 2>&1 | tail -3 | tee -a "$REPORT"

  if ! command -v go >/dev/null 2>&1; then
    out "ERROR: golang-go install did not yield 'go' on PATH. Cannot build amneziawg-go."
    exit 5
  fi

  SRC_ROOT="/tmp/amnezia-src"
  rm -rf "$SRC_ROOT"
  mkdir -p "$SRC_ROOT"

  out "  cloning + building amneziawg-tools (provides awg, awg-quick)"
  if ! git clone --depth 1 https://github.com/amnezia-vpn/amneziawg-tools "$SRC_ROOT/tools" 2>&1 | tail -3 | tee -a "$REPORT"; then
    out "ERROR: git clone of amneziawg-tools failed."
    exit 5
  fi
  if ! ( cd "$SRC_ROOT/tools/src" && make -j"$(nproc)" 2>&1 | tail -10 ) | tee -a "$REPORT"; then
    out "ERROR: amneziawg-tools build failed."
    exit 5
  fi
  ( cd "$SRC_ROOT/tools/src" && make install 2>&1 | tail -5 ) | tee -a "$REPORT"

  out "  cloning + building amneziawg-go (userspace tunnel implementation)"
  if ! git clone --depth 1 https://github.com/amnezia-vpn/amneziawg-go "$SRC_ROOT/go" 2>&1 | tail -3 | tee -a "$REPORT"; then
    out "ERROR: git clone of amneziawg-go failed."
    exit 5
  fi
  if ! ( cd "$SRC_ROOT/go" && make -j"$(nproc)" 2>&1 | tail -10 ) | tee -a "$REPORT"; then
    out "ERROR: amneziawg-go build failed."
    exit 5
  fi
  install -m 0755 "$SRC_ROOT/go/amneziawg-go" /usr/local/bin/amneziawg-go

  # awg-quick auto-detects amneziawg-go from PATH when the kernel module
  # isn't loaded (verified in awg-quick source: WG_QUICK_USERSPACE_IMPLEMENTATION
  # defaults to "amneziawg-go"). No env-var setup required.

  if command -v awg >/dev/null 2>&1 && command -v awg-quick >/dev/null 2>&1 \
       && command -v amneziawg-go >/dev/null 2>&1; then
    INSTALL_OK=1
    out "  path B succeeded — userspace tunnel implementation ready"
  else
    out "ERROR: source build finished but binaries are not on PATH:"
    out "       awg=$(command -v awg || echo MISSING)"
    out "       awg-quick=$(command -v awg-quick || echo MISSING)"
    out "       amneziawg-go=$(command -v amneziawg-go || echo MISSING)"
    exit 5
  fi
fi

if [[ $INSTALL_OK -ne 1 ]]; then
  out "ERROR: neither install path succeeded."
  exit 5
fi
run 'awg --version'
run 'command -v awg-quick && awg-quick --help 2>&1 | head -3 || true'

# Step e: install the diagnostic helpers.
out "  installing diagnostic helpers (iperf3, mtr-tiny, dnsutils, jq, iproute2, speedtest-cli)"
# iproute2 (ip), iputils-ping (ping), procps (sysctl), iptables (awg-quick fwmark) — minimal Ubuntu images often lack these.
apt-get install -y iperf3 mtr-tiny dnsutils iputils-ping iproute2 procps iptables curl jq 2>&1 | tail -3 | tee -a "$REPORT"
# speedtest-cli is in 'universe' on noble; tolerate its absence so the
# script still produces an in-tunnel iperf3 report on a minimal image.
if ! command -v speedtest-cli >/dev/null 2>&1; then
  apt-get install -y speedtest-cli 2>&1 | tail -3 | tee -a "$REPORT" || true
fi
HAS_SPEEDTEST=0
if command -v speedtest-cli >/dev/null 2>&1; then HAS_SPEEDTEST=1; out "  speedtest-cli available"
else                                                              out "  WARNING: speedtest-cli unavailable — will skip non-tunnel speedtest comparison (in-tunnel iperf3 still runs)"
fi

# ----- 3. baseline (no tunnel) ---------------------------------------------
log "BASELINE — no tunnel (your raw WAN to the public internet)"
DEFAULT_IFACE="$(ip -o route show default | awk '{print $5; exit}')"
out "default iface: ${DEFAULT_IFACE}"
run "ip -s link show ${DEFAULT_IFACE}"
run "ip route show"
run "resolvectl status 2>/dev/null | head -25 || cat /etc/resolv.conf"

# Kernel TCP receiver-side knobs (these directly cap how much we can
# pull DOWN at any RTT. Astrill being fast means these are probably OK,
# but we want to know.)
run "sysctl net.ipv4.tcp_congestion_control net.core.default_qdisc net.ipv4.tcp_rmem net.ipv4.tcp_wmem net.core.rmem_max net.core.wmem_max net.ipv4.tcp_window_scaling net.ipv4.tcp_mtu_probing"

if [[ "$HAS_SPEEDTEST" == "1" ]]; then
  out "  speedtest-cli (no tunnel) — picks nearest server, ~30s"
  run "speedtest-cli --simple"
fi
out "  mtr to gw.orgabots.com (10 cycles)"
run "mtr -c 10 -r -w gw.orgabots.com 2>/dev/null || echo 'mtr unavailable'"

# ----- 4. bring up the tunnel ----------------------------------------------
log "bring tunnel UP on iface '${IFACE}' (renamed from amnezia.conf)"
# Copy + rewrite: change the iface name (so we never collide with an
# existing 'amnezia' iface), strip the DNS line (resolvconf flow may not
# work everywhere; we test DNS explicitly with `dig @` below).
sed -e '/^DNS = /d' "$CONF_SRC" > "$TMPCONF"
chmod 600 "$TMPCONF"
out "  conf head (private key + magic numbers REDACTED in this report):"
sed -e 's/^PrivateKey = .*/PrivateKey = <REDACTED>/' \
    -e 's/^H[1234] = .*/H? = <REDACTED>/' \
    "$TMPCONF" | head -25 | tee -a "$REPORT"

if ! awg-quick up "$TMPCONF" 2>&1 | tee -a "$REPORT"; then
  out "ERROR: awg-quick up failed — abort"
  exit 1
fi
sleep 3

# ----- 5. in-tunnel diagnostics -------------------------------------------
log "IN-TUNNEL — handshake + interface state"
run "awg show ${IFACE}"
run "ip link show ${IFACE}"
run "ip route show table all | head -30"

log "IN-TUNNEL — reachability + PMTU probe"
out "  ping ${SERVER_IP} (5 echoes; smoke test)"
run "ping -c 5 -W 2 ${SERVER_IP}"
out "  PMTU probe: 1392 inner = 1420 with IP+ICMP overhead, should pass"
run "ping -M do -s 1392 -c 3 -W 2 ${SERVER_IP} 2>&1 | tail -5"
out "  PMTU probe: 1393 should fail — that's the hard ceiling"
run "ping -M do -s 1393 -c 3 -W 2 ${SERVER_IP} 2>&1 | tail -5"
out "  DNS via tunnel"
run "dig @${SERVER_IP} +short +tries=1 +time=3 example.com"

log "IN-TUNNEL — iperf3 SINGLE-STREAM DOWNLOAD (server→client, 30s, 1s interval)"
# -R reverses sender/receiver: server is sender, we are receiver.
run "iperf3 -c ${SERVER_IP} -t 30 -i 1 -R -O 2 --json > /tmp/iperf-down.json && jq -r '.intervals[] | \"\(.sum.start | floor)-\(.sum.end | floor)s  \(.sum.bits_per_second/1000000 | round) Mbps  retr=\(.sum.retransmits)\"' /tmp/iperf-down.json && echo --- && jq -r '.end | \"summary: \(.sum_received.bits_per_second/1000000 | round) Mbps received, \(.sum_sent.retransmits) total retransmits\"' /tmp/iperf-down.json"

log "IN-TUNNEL — iperf3 SINGLE-STREAM UPLOAD (client→server, 30s, 1s interval)"
run "iperf3 -c ${SERVER_IP} -t 30 -i 1 -O 2 --json > /tmp/iperf-up.json && jq -r '.intervals[] | \"\(.sum.start | floor)-\(.sum.end | floor)s  \(.sum.bits_per_second/1000000 | round) Mbps  retr=\(.sum.retransmits)\"' /tmp/iperf-up.json && echo --- && jq -r '.end | \"summary: \(.sum_sent.bits_per_second/1000000 | round) Mbps sent, \(.sum_sent.retransmits) total retransmits\"' /tmp/iperf-up.json"

log "IN-TUNNEL — iperf3 PARALLEL 4-STREAM DOWNLOAD (does it scale?)"
run "iperf3 -c ${SERVER_IP} -t 20 -i 5 -R -P 4 -O 2 --json > /tmp/iperf-down-p4.json && jq -r '.end | \"summary 4-stream: \(.sum_received.bits_per_second/1000000 | round) Mbps received total, \(.sum_sent.retransmits) total retransmits\"' /tmp/iperf-down-p4.json"

log "IN-TUNNEL — TCP socket internals during a 20s download (snapshots @ 5,10,15s)"
# Background the iperf, snapshot ss -tin every 5s.
iperf3 -c "$SERVER_IP" -t 20 -R --json > /tmp/iperf-down-ssprobe.json 2>&1 &
IPERF_PID=$!
for s in 5 10 15; do
  sleep 5
  out "  --- ss -tin dst ${SERVER_IP}  (t=${s}s) ---"
  ss -tin "dst ${SERVER_IP}" 2>&1 | tee -a "$REPORT" || true
done
wait $IPERF_PID 2>/dev/null || true

log "IN-TUNNEL — speedtest-cli (real-world download)"
if [[ "$HAS_SPEEDTEST" == "1" ]]; then
  run "speedtest-cli --simple"
else
  out "  (speedtest-cli not installed)"
fi

log "IN-TUNNEL — counters after the load (drops / retransmits / udp errors)"
run "nstat -a -z 2>&1 | grep -E 'TcpRetransSegs|TcpExtTCPRetransFail|TcpExtTCPLost|TcpExtTCPSACKReneging|UdpRcvbufErrors|UdpInErrors|UdpNoPorts'"
run "tc -s qdisc show dev ${IFACE}"
run "ip -s link show ${IFACE}"
run "awg show ${IFACE} transfer"

# ----- 6. tear down + recheck baseline -------------------------------------
log "tear tunnel DOWN"
awg-quick down "$TMPCONF" 2>&1 | tee -a "$REPORT"

log "POST-TUNNEL — quick speedtest to confirm baseline restored"
if [[ "$HAS_SPEEDTEST" == "1" ]]; then
  run "speedtest-cli --simple"
fi

log "DONE"
out "report saved to: ${REPORT}"
out "send that file back to diagnose."
