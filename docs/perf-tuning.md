# gw0 Performance Tuning

Operator-facing reference for the AmneziaWG gateway (`gw0`). Companion to `docs/design.md` Step 8 and `scripts/install-gw0.sh`. Target: **>=50 Mbps per peer sustained, <=20% throughput overhead** vs raw VPS bandwidth.

## TL;DR

1. Kernel module beats userspace by ~30%. Make DKMS work — don't accept the userspace fallback as default.
2. On Ubuntu 24.04, the GA 6.8 kernel is the default risk; HWE (6.11/6.14) is reported as more reliable for this DKMS module.
3. The sysctl block in `host/sysctl/99-host.conf` is required, not optional, for a UDP tunnel gateway.
4. MTU 1380 is the conservative starting point. If `iperf3` looks good, leave it; if PMTUD is failing, that's where to look first.

## Kernel choice for AmneziaWG

Findings from upstream (citations at end):

- The kernel module's compat layer was originally written against 3.10–5.5 with patchwork additions for newer kernels. There is **no public CI** asserting "this module builds on Ubuntu 24.04 GA / HWE."
- Reports of build failures on **6.8.0-45-generic** (Ubuntu 24.04 GA) — the most common cause is missing/mismatched `linux-headers-$(uname -r)`. Fixed by ensuring headers match the running kernel.
- Reports of success on **6.12** with headers-only (no full kernel source). The README's "you need full kernel source" instruction is outdated for modern kernels.
- HWE 6.11 issue #72 was a misconfigured PPA suite (focal vs noble), not a real incompatibility.
- Upstream PPA ships **`amneziawg-linux-kmod`** for noble alongside `amneziawg-dkms` — `install-gw0.sh` prefers the prebuilt kmod when available.

### Decision flow

```
running kernel = $(uname -r)
            |
            v
linux-headers-${RUNNING_KERNEL} installed?  -- no -->  apt install it, retry
            |
           yes
            |
            v
modinfo amneziawg works?  -- yes -->  DONE (use kernel module)
            |
            no
            |
            v
prebuilt amneziawg-linux-kmod available + loadable?  -- yes -->  DONE
            |
            no
            |
            v
on GA 6.8?  -- yes -->  install linux-generic-hwe-24.04, REBOOT, retry
            |
            no (already on HWE / DKMS still failing)
            |
            v
build amneziawg-go from source, set
WG_QUICK_USERSPACE_IMPLEMENTATION=amneziawg-go
in /etc/default/awg-quick. Accept ~30% perf hit.
```

`install-gw0.sh` automates the first three branches and prints the remediation block for the last two.

### DKMS troubleshooting cheatsheet

| Symptom | First check | Fix |
|---|---|---|
| `dkms autoinstall` exits non-zero | `tail -n 50 /var/lib/dkms/amneziawg/*/build/make.log` | usually missing headers |
| "Bad return status for module build on kernel: X" where X != `uname -r` | kernel was upgraded but not booted | `reboot` then retry |
| "linux-headers-X is not supported" (realtime / oem kernel) | non-stock kernel flavour | switch to `linux-generic` or `linux-generic-hwe-24.04` |
| `modinfo amneziawg` says not found, but DKMS exited 0 | apt hook lied | `dkms autoinstall` again, watch for compile errors |
| `wg-quick` succeeds but no traffic | iface up but module not loaded | `lsmod | grep amneziawg`; if empty, DKMS didn't actually build |
| Everything builds, peer can't handshake | not a DKMS issue | check `awg show gw0`, ufw NAT, peer's `Endpoint` reachability |

## Network sysctl rationale

All applied via `host/sysctl/99-host.conf` (one file, single source of truth).

| Setting | Value | Why for a UDP tunnel |
|---|---|---|
| `net.core.rmem_max` / `wmem_max` | 25 MiB | Stock 200 KiB is far too small for sustained 50+ Mbps UDP. Overflow causes silent kernel-side packet drops that look like "VPN feels laggy". |
| `net.core.rmem_default` / `wmem_default` | 2.5 MiB | Default starting point for any new socket; pulls AmneziaWG's listening socket up without needing per-app tuning. |
| `net.core.netdev_max_backlog` | 5000 | Per-CPU queue between NIC softirq and protocol stack. Tunnel gateways burst higher than desktops. |
| `net.ipv4.udp_rmem_min` / `udp_wmem_min` | 8 KiB | Floor that prevents the kernel from auto-shrinking UDP buffers under memory pressure. |
| `net.ipv4.tcp_rmem` / `tcp_wmem` | 4K / 64–87K / 16 MiB | TCP autotuning bounds for TCP-over-tunnel (Git, downloads). 16 MiB ceiling matches the UDP tuning. |
| `net.ipv4.tcp_mtu_probing` | 1 | **Tunnel-specific.** Encapsulated TCP has a smaller effective MSS than the physical link; ICMP needed-frag is often filtered on CN routes, breaking classic PMTUD. Probing lets the stack discover the working MSS empirically. |
| `net.ipv4.tcp_notsent_lowat` | 16 KiB | Reduces small-packet syscall overhead on bulk transfers; harmless for interactive flows. |
| `net.ipv4.tcp_congestion_control` | `bbr` | Better throughput on long-fat-network (CN2-GIA backbone). Already in the file. |
| `net.core.default_qdisc` | `fq` | BBR's recommended pacing qdisc. Already in the file. |

Apply: `systemctl restart systemd-sysctl` (or reboot). Verify: `sysctl net.core.rmem_max` should print 26214400.

## IRQ handling on 2 vCPU

With only 2 vCPUs, single-flow UDP throughput is bottlenecked by the one core handling the NIC's softirq. Two cheap mitigations, both reversible, both off by default:

- `irqbalance` — apt-installed by `bootstrap-host.sh`. Spreads device IRQs across cores. On 2 vCPU it gives a measurable bump only if the NIC has multiple TX/RX queues; KVM virtio-net usually does.
- **RPS (Receive Packet Steering)** — software-side fan-out; lets core 0 take the IRQ but distribute packet processing to core 1. Enable per-NIC:

```bash
# replace eth0 with the actual default-route interface
echo 3 > /sys/class/net/eth0/queues/rx-0/rps_cpus     # both cores
echo 32768 > /sys/class/net/eth0/queues/rx-0/rps_flow_cnt
echo 32768 > /proc/sys/net/core/rps_sock_flow_entries
```

Persist via a systemd-tmpfiles drop-in if you decide to keep it. **Don't enable RPS speculatively** — measure first, the 2-core case can go either way depending on cache behaviour.

## MTU strategy

- Plan default in `gw0.conf`: **MTU 1380**.
- Math: physical link 1500 - IPv4 (20) - UDP (8) - WireGuard outer (32) - AmneziaWG junk-header padding margin = ~1380. Conservative; leaves headroom for the randomised init headers.
- When to bump: if `iperf3 -u` over `gw0` shows clean throughput at 1380 and you control both endpoints, try MTU 1420 (stock WireGuard default). Only bump on the server **and** the peer config — a mismatch will fragment.
- When to lower: if peers report stalls on specific sites and `tracepath` over the tunnel shows a smaller path MTU, drop to 1280 (IPv6 minimum, always works).
- Verify path: `tracepath <vps-ip>` from the peer; the last reported `pmtu` is the effective max.

## Crypto check

WireGuard / AmneziaWG use **ChaCha20-Poly1305**, not AES-GCM, so AES-NI is *not* the bottleneck. But AES-NI presence is a useful proxy for "modern crypto-capable CPU" (AVX2 generally implies decent ChaCha20 throughput too):

```bash
grep -o 'aes' /proc/cpuinfo | head -1     # 'aes' if present, empty if not
grep -o 'avx2' /proc/cpuinfo | head -1    # 'avx2' if present
lscpu | grep -E 'Model name|Flags'
```

LayerStack HK plans typically run on Xeon E5-26xx / E-23xx hosts; both have AES-NI and AVX2. If `grep -o 'aes' /proc/cpuinfo` returns empty on this VPS, escalate with the provider — that's an old or atypically restricted host.

## iperf3 verification

Run from a peer client (not from the VPS itself — that bypasses the tunnel).

```bash
# 1. Baseline: peer to VPS public IP, OUTSIDE the tunnel.
iperf3 -c <vps-public-ip> -t 30
#   -> measures raw VPS network capacity, ceiling for tunnel throughput.

# 2. Through gw0: peer connects via tunnel to VPS internal IP (10.x.x.1).
iperf3 -c 10.x.x.1 -t 30
#   -> tunnel throughput. Ratio (2)/(1) = tunnel efficiency.
#      Target: >= 0.80 (kernel module). Userspace floor: ~0.55-0.65.

# 3. UDP, to detect drops at high pps:
iperf3 -c 10.x.x.1 -u -b 100M -t 30
#   -> watch for "Lost/Total Datagrams" in the summary. Anything over
#      ~0.1% means something downstream of the rmem buffers is dropping.

# 4. Bidirectional, to catch asymmetric tuning:
iperf3 -c 10.x.x.1 -t 30 --bidir
```

If TCP-via-tunnel is much slower than UDP-via-tunnel at the same bitrate, suspect MTU/MSS — try the `tcp_mtu_probing` setting (already on) or drop tunnel MTU.

## Refs

- amneziawg-linux-kernel-module README and issues — https://github.com/amnezia-vpn/amneziawg-linux-kernel-module
  - issue #56 (kernel 6.8 DKMS) — https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/issues/56
  - issue #72 (Ubuntu 24.04.2 / HWE 6.11) — https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/issues/72
  - issue #147 (headers-only build works on 6.12) — https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/issues/147
  - PR #62 (DKMS autoinstall fix, unmerged) — https://github.com/amnezia-vpn/amneziawg-linux-kernel-module/pull/62
- Upstream PPA — https://launchpad.net/~amnezia/+archive/ubuntu/ppa
- amneziawg-go (no published binary releases as of research date) — https://github.com/amnezia-vpn/amneziawg-go/releases
- Ubuntu kernel lifecycle / HWE — https://ubuntu.com/kernel/lifecycle

## Open questions for the operator (verify on the live box)

- `uname -r` on the actual VPS — confirms whether you're on GA 6.8 or already on HWE.
- `apt-cache policy amneziawg-linux-kmod` — confirms whether the prebuilt kmod is published for the running ABI.
- `grep -o 'aes' /proc/cpuinfo` — sanity check; should print `aes`.
- Real-world `iperf3` numbers from a peer in mainland CN (the latency profile dominates beyond what tuning can fix).

## VPS uplink ceiling (the hard limit nothing in this doc can fix)

Everything above tunes overhead inside the tunnel. None of it can move the ceiling set by the **carrier route from the VPS to the peer's network** — and on commodity HK plans, that ceiling is in the single-to-low-double-digit Mbps for mainland-China destinations.

Measured 2026-04-28 from a Layerstack standard HK plan, `speedtest --server <id>` against major CN carriers:

| Test server                 | VPS → server (peer download) | VPS ← server (peer upload) | RTT  |
|-----------------------------|------------------------------|----------------------------|------|
| Beijing Unicom (43752)      | **7.7 Mbps**                 | 44 Mbps                    | 80 ms|
| Shanghai Unicom (24447)     | **7.6 Mbps**                 | 67 Mbps                    | 66 ms|
| Hangzhou Telecom (59386)    | **14.7 Mbps**                | 110 Mbps                   | 68 ms|
| Auto-pick (regional/HK/SG)  | 506 Mbps                     | 822 Mbps                   | 1 ms |

Note the asymmetry: outbound from this VPS to mainland CN is throttled to the 7–15 Mbps range; inbound is roughly an order of magnitude higher. That asymmetry is exactly what an in-tunnel iperf3 from a CN peer will show (the peer's "download" travels the throttled VPS→CN direction, the peer's "upload" travels the unconstrained CN→VPS direction).

If you're seeing tunnel throughput in this range and the in-VPS speedtest above gives the same numbers, **stop tuning the tunnel** — protocol, MTU, sysctl, junk parameters, and even switching from `gw0` to `qedge` (Hysteria2) will all converge on the same ~15 Mbps because the bottleneck is the wire, not the tunnel. The fix is provider-side:

- **Same provider, premium tier.** Layerstack, Bandwagon, Vultr etc. sell "China-optimized" / CN2-GIA / CMI-routed plans for a 2-3× price uplift; these clear the 100+ Mbps barrier to CN.
- **Different provider with CN-direct transit.** Bandwagon CN2 GIA and similar SKUs are widely benchmarked.
- **Frontend in a CN-friendly region.** Keep the stack on the cheap VPS, add a thin proxy on a CN2-routed box. Adds RTT and a moving part.

Astrill and similar commercial VPNs hit 200+ Mbps to the same peers because they buy premium CN transit in bulk. That's the mechanism, not protocol cleverness.
