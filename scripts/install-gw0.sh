#!/usr/bin/env bash
# install-gw0.sh — Build Sequence Step 8: gateway server (gw0).
#
# Run as root on the VPS, AFTER scripts/deploy.sh has rsync'd this repo's
# host/ tree to /root/host/ and Step 1 (bootstrap-host.sh) has staged ufw
# rules. Idempotent — safe to re-run.
#
# Out of scope:
#   - Generating peer configs (handled by scripts/provision-peer.sh in Step 9).
#   - Enabling ufw itself (deferred to Step 3 / ufw-docker integration).

# Strict mode + structured failure reporting (shared lib).
SCRIPT_DIR="$(cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=lib/strict.sh
. "$SCRIPT_DIR/lib/strict.sh"
strict_enable
STRICT_SCRIPT_NAME="install-gw0.sh"

# Default ENABLED — gw0 backs the enrol UI's /peers (devices) feature
# that every user touches. Skip only when SKIP_GW0=1 is set explicitly
# (operators who don't want a tunnel server: bare LAN deployments,
# environments where AmneziaWG conflicts with another VPN, etc.).
if [[ "${SKIP_GW0:-0}" == "1" ]]; then
  echo "[install-gw0] SKIP_GW0=1, skipping (operator opt-out)"
  exit 0
fi

# Preflight: only checks for the gw0=ON path. add-apt-repository and dkms
# are deliberately NOT required here — this script installs them itself
# (software-properties-common + the DKMS build prereqs below), so requiring
# them up-front would fail every fresh turnkey install.
require_root
require_cmd apt-get install awk sed od ip systemctl modinfo

REPO_HOST_DIR="/root/host"
# amneziawg-tools ships its config under /etc/amnezia/amneziawg/, not
# /etc/wireguard/ — confirmed against amneziawg-tools.spec (sysconfdir +
# 'amnezia/amneziawg'). The repo dir is named host/wireguard/ for
# convention only; nothing in that path is exposed publicly.
AWG_CONF_DIR="/etc/amnezia/amneziawg"
AWG_CONF="${AWG_CONF_DIR}/gw0.conf"
AWG_PRIV="${AWG_CONF_DIR}/gw0_private.key"
AWG_PUB="${AWG_CONF_DIR}/gw0_public.key"
TEMPLATE="${REPO_HOST_DIR}/wireguard/gw0.conf.template"
UFW_FRAGMENT="${REPO_HOST_DIR}/ufw/before.rules.fragment"
UFW_BEFORE="/etc/ufw/before.rules"
# Marker comment lives in the fragment itself; grepping for it makes splice
# detection independent of any other comment a sysadmin may have added.
UFW_MARKER="# BEGIN gw0-nat"

strict_step "amneziawg PPA + DKMS install"
echo "==> adding upstream PPA (ppa:amnezia/ppa)"
# add-apt-repository is idempotent; -y suppresses the interactive prompt.
# software-properties-common provides add-apt-repository on minimal Ubuntu.
apt-get install -y software-properties-common
add-apt-repository -y ppa:amnezia/ppa
apt-get update

echo "==> installing DKMS build prerequisites"
# #1 cause of amneziawg-dkms build failures on Ubuntu 24.04 is missing
# linux-headers for the *running* kernel (e.g., kernel was upgraded but
# the host hasn't rebooted; the running kernel's headers were autoremoved).
# Install matching headers + build tooling BEFORE pulling the DKMS package
# so the postinst trigger has everything it needs on first try.
# References:
#   https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/issues/56
#   https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/issues/72
RUNNING_KERNEL="$(uname -r)"
echo "    running kernel: ${RUNNING_KERNEL}"
apt-get install -y "linux-headers-${RUNNING_KERNEL}" build-essential dkms

echo "==> installing amneziawg-tools"
apt-get install -y amneziawg-tools

# Prefer the prebuilt kernel-module package if the PPA ships one for our
# kernel — it skips the DKMS compile entirely and avoids the whole class
# of "headers don't match" failures. The 'amneziawg-linux-kmod' source
# package is published in ppa:amnezia/ppa for noble (verified via
# https://launchpad.net/~amnezia/+archive/ubuntu/ppa/+packages); whether
# a binary that loads on the *running* kernel is available depends on
# whether Canonical's kernel-team-style hooks rebuilt it for that ABI.
# Branch on availability rather than assuming.
USE_PREBUILT=0
if apt-cache search --names-only '^amneziawg-linux-kmod' | grep -q amneziawg-linux-kmod; then
  echo "==> prebuilt kernel-module package available, attempting install"
  set +e
  apt-get install -y amneziawg-linux-kmod
  KMOD_RC=$?
  set -e
  if [[ $KMOD_RC -eq 0 ]] && modinfo amneziawg >/dev/null 2>&1; then
    echo "    prebuilt amneziawg-linux-kmod installed; skipping DKMS"
    USE_PREBUILT=1
  else
    echo "    prebuilt kmod did not yield a loadable module for ${RUNNING_KERNEL}, falling back to DKMS" >&2
  fi
fi

DKMS_FAILED=0
if [[ $USE_PREBUILT -eq 0 ]]; then
  echo "==> installing amneziawg-dkms"
  # Capture apt rc so a build failure surfaces a remediation block
  # instead of aborting the script halfway through configuration.
  set +e
  apt-get install -y amneziawg-dkms
  APT_RC=$?
  set -e
  if [[ $APT_RC -ne 0 ]]; then
    DKMS_FAILED=1
  fi

  echo "==> running 'dkms autoinstall' explicitly"
  # apt's dkms hook sometimes exits 0 even when the module build itself
  # failed (the package is "installed", just not built). Running
  # autoinstall a second time is idempotent and gives us the canonical
  # build rc we can branch on.
  set +e
  dkms autoinstall
  DKMS_RC=$?
  set -e
  if [[ $DKMS_RC -ne 0 ]]; then
    DKMS_FAILED=1
  fi
  # Final ground-truth check: can we actually load the module?
  if ! modinfo amneziawg >/dev/null 2>&1; then
    DKMS_FAILED=1
  fi
fi

if [[ $DKMS_FAILED -eq 1 ]]; then
  echo "================================================================" >&2
  echo "gw0 install: amneziawg kernel module is NOT available." >&2
  echo "================================================================" >&2
  echo "" >&2
  echo "--- last 50 lines of dkms make.log (most recent build) ---" >&2
  # Sort by mtime so the newest build dir wins; tolerate the glob not matching.
  LATEST_LOG="$(ls -1t /var/lib/dkms/amneziawg/*/build/make.log 2>/dev/null | head -n1 || true)"
  if [[ -n "${LATEST_LOG}" ]]; then
    tail -n 50 "${LATEST_LOG}" >&2
  else
    echo "(no make.log found under /var/lib/dkms/amneziawg/*/build/)" >&2
  fi
  cat >&2 <<EOF

REMEDIATION (try in this order — least to most invasive):

(a) [LOW EFFORT] Verify linux-headers match the running kernel.
    The DKMS build needs headers for ${RUNNING_KERNEL} specifically.

      apt-get install -y linux-headers-${RUNNING_KERNEL}
      dkms autoinstall
      modinfo amneziawg

    If your kernel was upgraded since last boot, REBOOT first so
    \`uname -r\` matches the headers apt can actually install, then
    re-run this script.

(b) [MEDIUM EFFORT, requires reboot] Install the HWE kernel.
    The amneziawg DKMS build is reported as more reliable on HWE
    kernels (6.11/6.14 on noble) than on the 6.8 GA kernel — the
    upstream module's compat shims have better coverage for the
    newer source tree. Caveat: HWE swaps the kernel, requiring a
    reboot, and Canonical's HWE rolls forward (currently 6.14, soon
    6.17) which means slightly more churn.

      apt-get install -y linux-generic-hwe-24.04
      reboot
      # after reboot:
      apt-get install -y linux-headers-\$(uname -r)
      dkms autoinstall

(c) [LAST RESORT] Userspace fallback: amneziawg-go (~30% perf hit).
    Upstream does NOT publish prebuilt binaries
    (https://github.com/amnezia-vpn/amneziawg-go/releases shows
    "no releases"), so this is a build-from-source path:

      apt-get install -y golang-go git
      git clone https://github.com/amnezia-vpn/amneziawg-go /opt/src/amneziawg-go
      cd /opt/src/amneziawg-go && make
      install -m 0755 amneziawg-go /usr/local/bin/amneziawg-go

    Then enable userspace mode for awg-quick:

      mkdir -p /etc/default
      echo 'WG_QUICK_USERSPACE_IMPLEMENTATION=amneziawg-go' \\
        >> /etc/default/awg-quick

    Re-run this script after one of (a)/(b)/(c) is in place.

Refs:
  https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/issues/56
  https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/issues/72
  https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/pull/62
EOF
  exit 1
fi

# 'dkms status' shows "(WAITING FOR REBOOT)" when a module is built for a
# kernel that isn't currently running. Don't auto-reboot — print a notice
# and let the operator schedule it.
if dkms status 2>/dev/null | grep -qi 'reboot'; then
  echo "    NOTICE: dkms status indicates a reboot is required to load the new module" >&2
fi

strict_step "render gw0 conf + ufw splice"
echo "==> ensuring ${AWG_CONF_DIR} (mode 0700)"
install -d -m 0700 "$AWG_CONF_DIR"

echo "==> server keypair"
if [[ ! -s "$AWG_PRIV" ]]; then
  # umask 077 is belt-and-braces; the dir is already 0700 and tee+>
  # inherit those perms via install -d, but a stray cp later could widen
  # them. Setting umask here makes the keypair mode locally derivable.
  ( umask 077 && awg genkey | tee "$AWG_PRIV" | awg pubkey > "$AWG_PUB" )
  chmod 0600 "$AWG_PRIV" "$AWG_PUB"
  echo "    generated ${AWG_PRIV}"
else
  echo "    ${AWG_PRIV} exists, leaving in place"
fi

echo "==> rendering ${AWG_CONF}"
if [[ -f "$AWG_CONF" ]]; then
  # Skip render once a conf exists — provision-peer.sh appends [Peer]
  # blocks to it, and we must not clobber those.
  echo "    ${AWG_CONF} exists, skipping render (peers may be present)"
else
  if [[ ! -f "$TEMPLATE" ]]; then
    echo "    ERROR: template missing at $TEMPLATE — run deploy.sh first" >&2
    exit 1
  fi
  SERVER_PRIVATE_KEY="$(cat "$AWG_PRIV")"
  # Four random uint32s for H1..H4. od emits one number per call; printing
  # without a newline keeps the substitution clean.
  H1="$(od -An -N4 -tu4 /dev/urandom | tr -d ' \n')"
  H2="$(od -An -N4 -tu4 /dev/urandom | tr -d ' \n')"
  H3="$(od -An -N4 -tu4 /dev/urandom | tr -d ' \n')"
  H4="$(od -An -N4 -tu4 /dev/urandom | tr -d ' \n')"
  # envsubst would also work but pulls in gettext-base; sed with a fixed
  # variable list is fine and avoids surprises if the template ever uses
  # other $-prefixed strings.
  ( umask 077 && \
    sed \
      -e "s|\${SERVER_PRIVATE_KEY}|${SERVER_PRIVATE_KEY}|g" \
      -e "s|\${H1}|${H1}|g" \
      -e "s|\${H2}|${H2}|g" \
      -e "s|\${H3}|${H3}|g" \
      -e "s|\${H4}|${H4}|g" \
      "$TEMPLATE" > "$AWG_CONF" )
  chmod 0600 "$AWG_CONF"
  echo "    rendered ${AWG_CONF}"
fi

echo "==> splicing ufw fragment into ${UFW_BEFORE}"
if grep -qF "$UFW_MARKER" "$UFW_BEFORE"; then
  echo "    marker present, skipping splice"
else
  PUBLIC_IFACE="$(ip route show default | awk '/default/ {print $5; exit}')"
  if [[ -z "$PUBLIC_IFACE" ]]; then
    echo "    ERROR: could not detect default-route interface" >&2
    exit 1
  fi
  echo "    detected PUBLIC_IFACE=${PUBLIC_IFACE}"
  # Render the fragment with the iface substituted. Two splice points:
  #   - *nat block goes at the very top of before.rules (before *filter).
  #   - The two -A ufw-before-forward lines go inside *filter, before its
  #     COMMIT. The fragment file mixes both; we split it on the END of
  #     the *nat ... COMMIT section.
  RENDERED="$(mktemp)"
  trap 'rm -f "$RENDERED"' EXIT
  sed -e "s|\${PUBLIC_IFACE}|${PUBLIC_IFACE}|g" "$UFW_FRAGMENT" > "$RENDERED"
  # NAT block = lines from "*nat" through the first "COMMIT" inclusive,
  # plus the leading marker. Forward-rules block = the remaining
  # ufw-before-forward lines. Awk-based split keeps this readable.
  NAT_BLOCK="$(awk '
    /^# BEGIN gw0-nat/ {p=1}
    p {print}
    p && /^COMMIT$/ {exit}
  ' "$RENDERED")"
  FWD_BLOCK="$(awk '
    /^-A ufw-before-forward/ {print}
    /^# END gw0-nat/ {print; exit}
  ' "$RENDERED")"
  # Backup before mutating — a corrupted before.rules takes down ufw entirely.
  cp -a "$UFW_BEFORE" "${UFW_BEFORE}.bak.$(date +%s)"
  # Prepend NAT block at top of file. *filter forward rules: insert
  # immediately before the first COMMIT line in the *filter table. We rely
  # on the stock Ubuntu before.rules layout (single *filter, single COMMIT
  # at end). If the operator has heavily customised before.rules, the
  # marker check above fails open and they should splice manually.
  TMP="$(mktemp)"
  {
    printf '%s\n\n' "$NAT_BLOCK"
    awk -v fwd="$FWD_BLOCK" '
      # Insert forward rules just before the LAST COMMIT (the *filter one).
      # Two-pass: count commits first.
      NR==FNR { if ($0=="COMMIT") last=NR; next }
      FNR==last { print fwd; print; next }
      { print }
    ' "$UFW_BEFORE" "$UFW_BEFORE"
  } > "$TMP"
  install -m 0640 -o root -g root "$TMP" "$UFW_BEFORE"
  rm -f "$TMP"
  # Reload only if ufw is already active; bootstrap-host.sh leaves it staged
  # but disabled until Step 3.
  if ufw status | grep -q "Status: active"; then
    ufw reload
  fi
fi

echo "==> enabling awg-quick@gw0.service"
# Unit name confirmed against amneziawg-tools.spec: %{_unitdir}/awg-quick@.service.
# The package ships its own template, parallel to wg-quick@.service from
# stock wireguard-tools, which means both can coexist.
systemctl daemon-reload
systemctl enable awg-quick@gw0.service
# Restart (not just start) so re-running this script after a config change
# picks up the new conf. systemctl restart on a stopped unit is equivalent
# to start.
systemctl restart awg-quick@gw0.service

# Wire systemd-resolved as the in-tunnel caching resolver for peers. Must
# come AFTER awg-quick@gw0 is up — the resolver binds to 10.99.0.1 which
# only exists once the interface is configured. Idempotent.
strict_step "configure host DNS (systemd-resolved on 10.99.0.1)"
"${SCRIPT_DIR}/configure-host-dns.sh"

# Tunnel perf tunables: gw0 root qdisc (noqueue -> fq), RPS/RFS on the
# WAN NIC, UDP GRO forwarding, and the matching sysctl knobs. Background
# is documented in raph-tunnel-tunables.sh + 99-tunnel-tuning.conf. We
# install both, restart the sysctl drop-in, and start the systemd unit
# so the runtime knobs apply without waiting for a reboot.
strict_step "install tunnel perf tunables (qdisc, RPS, sysctl)"
install -m 0755 "${REPO_HOST_DIR}/raph-tunnel-tunables.sh" \
  /usr/local/sbin/raph-tunnel-tunables.sh
install -m 0644 "${REPO_HOST_DIR}/systemd/raph-tunnel-tunables.service" \
  /etc/systemd/system/raph-tunnel-tunables.service
install -m 0644 "${REPO_HOST_DIR}/sysctl/99-tunnel-tuning.conf" \
  /etc/sysctl.d/99-tunnel-tuning.conf
# Apply sysctl drop-in immediately (does not require reboot).
sysctl --system >/dev/null
systemctl daemon-reload
systemctl enable raph-tunnel-tunables.service
# Restart (not just start) so re-runs pick up edits to the script.
systemctl restart raph-tunnel-tunables.service

# iperf3 server bound to the tunnel IP — only reachable from peers, used
# by scripts/diag-from-client.sh to measure in-tunnel throughput in both
# directions when diagnosing per-peer download slowness. Tiny memory
# footprint, hardened systemd unit (User=nobody, ProtectSystem=strict).
strict_step "install iperf3-gw0 server (tunnel-only)"
apt-get install -y iperf3
install -m 0644 "${REPO_HOST_DIR}/systemd/iperf3-gw0.service" \
  /etc/systemd/system/iperf3-gw0.service
systemctl daemon-reload
systemctl enable iperf3-gw0.service
systemctl restart iperf3-gw0.service

echo "==> verification"
# Each command is a probe with independent failure modes; do not bail out
# on the first non-zero — surface all of them so the operator can triage.
set +e
echo "--- awg show gw0"
awg show gw0
echo "--- ip -d link show gw0"
ip -d link show gw0
echo "--- ss -ulpn :443 (gw0 listener; QUIC-shaped UDP/443 to dodge DPI)"
ss -ulpn | grep -E '[*0]:443\b' || echo "    (no listener on 443 — interface may be down)"
set -e

cat <<'EOF'

================================================================
Step 8 gw0 install: DONE
================================================================

NEXT (Step 9): generate peer configs (one per device).

    /opt/scripts/provision-peer.sh <device-name>

Each invocation appends a [Peer] block to /etc/amnezia/amneziawg/gw0.conf
and emits peers/<device-name>.conf + QR on the laptop side.

If `awg show gw0` printed nothing or `ip -d link` shows the interface
DOWN, check:
  - journalctl -u awg-quick@gw0
  - dmesg | tail -n 50              (DKMS module load errors)
  - cat /etc/amnezia/amneziawg/gw0.conf  (rendering correctness)
EOF
