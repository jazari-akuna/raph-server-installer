#!/usr/bin/env bash
# raph-tunnel-tunables.sh — runtime-only kernel knobs for the gw0
# AmneziaWG tunnel that can't be expressed via sysctl.d (qdisc, RPS,
# ethtool offload toggles). Installed to /usr/local/sbin/ and invoked
# by raph-tunnel-tunables.service after network-online.target so the
# WAN interface and gw0 both exist.
#
# Why each knob is here (not just "what"):
#
# 1) gw0 root qdisc: noqueue -> fq.
#    AmneziaWG creates gw0 with qdisc=noqueue. With BBR as the host's
#    TCP CC, BBR computes a pacing rate that the kernel needs to honor
#    via a real qdisc. With noqueue, packets generated locally that
#    egress via gw0 (e.g. iperf3 -s -B 10.99.0.1, but more importantly
#    *any* server-to-tunnel-peer TCP stream) get no pacing — bursts
#    overrun the WG encrypt path's effective queue and BBR can't sense
#    the bottleneck. fq gives BBR the per-flow pacing it expects and
#    matches the qdisc on ens3 (also fq), so the inner-TCP and
#    outer-UDP both pace cleanly.
#
# 2) RPS on ens3 (single-queue virtio).
#    /proc/interrupts shows virtio0-input.0 pinned to CPU0 with
#    millions of IRQs and zero on CPU1-3 (single-queue virtio NIC,
#    irq-balance can't help). Under heavy WG decrypt, all incoming
#    packet processing softirq runs on CPU0 -> single-core ceiling
#    around the AES throughput one core can sustain. RPS dispatches
#    received packets to other CPUs after the IRQ via IPIs, lifting
#    the ceiling. Mask f = CPUs 0-3.
#
# 3) RFS (rps_sock_flow_entries + rps_flow_cnt).
#    With RPS alone, a flow can ping-pong between CPUs and trash the
#    L1/L2 cache. RFS pins each flow to the CPU running the consuming
#    socket. 32k entries / 16k per-queue is the canonical setting for
#    a small box.
#
# 4) ethtool -K ens3 rx-udp-gro-forwarding on.
#    The encrypted UDP packets coming from peers are *forwarded* into
#    the kernel WG socket, not delivered to a userspace UDP listener.
#    GRO coalescing of UDP datagrams is OFF for the forwarding path
#    by default (kernel 5.10+ feature). Turning it on lets the stack
#    process N WG packets per syscall instead of one, cutting per-
#    packet overhead on the upload (peer->server) direction.
#
# Idempotent. Safe to re-run. Logs each step to stderr.

set -euo pipefail

log() { printf '[tunnel-tunables] %s\n' "$*" >&2; }

# Detect WAN interface from default route. Survives a NIC rename.
WAN="$(ip -o route show default | awk '{print $5; exit}')"
if [[ -z "$WAN" ]]; then
  log "FATAL: no default route, cannot detect WAN interface"
  exit 1
fi
log "WAN=$WAN"

# 1) gw0 qdisc -> fq (only if the interface exists; awg-quick@gw0 may not
# be up yet on first boot, so we are tolerant).
if ip link show gw0 >/dev/null 2>&1; then
  current_qdisc="$(tc qdisc show dev gw0 | awk '{print $2; exit}')"
  if [[ "$current_qdisc" != "fq" ]]; then
    log "gw0: qdisc $current_qdisc -> fq"
    tc qdisc replace dev gw0 root fq
  else
    log "gw0: qdisc already fq"
  fi
else
  log "gw0 not present yet; skipping qdisc tune (will be re-applied next boot)"
fi

# 2) RPS on WAN interface — spread softirq across all online CPUs.
NCPU="$(nproc)"
# Build a hex CPU mask covering all CPUs (e.g. 4 CPUs -> "f", 8 -> "ff").
CPUMASK="$(printf '%x' $(( (1 << NCPU) - 1 )))"
for q in /sys/class/net/"$WAN"/queues/rx-*; do
  [[ -e "$q/rps_cpus" ]] || continue
  current="$(cat "$q/rps_cpus")"
  # Strip leading zeros for comparison.
  if [[ "${current##0*0}" != "$CPUMASK" && "$(echo "$current" | tr -d 0,)" == "" && "$CPUMASK" != "0" ]]; then
    log "$q/rps_cpus: $current -> $CPUMASK"
    echo "$CPUMASK" > "$q/rps_cpus"
  elif [[ "$current" != "$CPUMASK" ]]; then
    log "$q/rps_cpus: $current -> $CPUMASK"
    echo "$CPUMASK" > "$q/rps_cpus"
  else
    log "$q/rps_cpus: already $CPUMASK"
  fi
done

# 3) RFS. rps_sock_flow_entries is set via sysctl.d; per-queue counter is
# runtime-only. Use floor(rps_sock_flow_entries / num_rx_queues) per the
# kernel docs; with one queue we use half (16k of 32k) to leave headroom.
NQ="$(ls -d /sys/class/net/"$WAN"/queues/rx-* 2>/dev/null | wc -l)"
RFS_PER_Q=$(( 32768 / (NQ > 0 ? NQ : 1) / 2 ))
for q in /sys/class/net/"$WAN"/queues/rx-*; do
  [[ -e "$q/rps_flow_cnt" ]] || continue
  log "$q/rps_flow_cnt -> $RFS_PER_Q"
  echo "$RFS_PER_Q" > "$q/rps_flow_cnt"
done

# 4) UDP GRO forwarding on the WAN interface — speeds up WG decrypt
# path (peer -> server upload). Some virtio drivers reject this; ignore
# failure rather than crashing the whole unit.
if ethtool -k "$WAN" 2>/dev/null | grep -q '^rx-udp-gro-forwarding'; then
  if ethtool -K "$WAN" rx-udp-gro-forwarding on 2>/dev/null; then
    log "$WAN: rx-udp-gro-forwarding -> on"
  else
    log "$WAN: rx-udp-gro-forwarding toggle not accepted (driver limitation, non-fatal)"
  fi
fi

log "done"
