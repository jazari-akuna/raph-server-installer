# Shared VPS — Bring-Up Plan

## Context

You (sagan) and marcus are setting up a shared LayerStack VPS (HK / CN2-GIA backbone, public IPv4, 2 vCPU, 4 GB RAM, 150 GB disk, domain `antarctica-engineering.com`) as a multi-purpose server. It needs to do five things, each with constraints already agreed:

1. **Host Docker apps**, managed from a web UI (you don't want to SSH for everything).
2. **Reverse-proxy** those apps behind clean HTTPS, also managed from a web UI.
3. **Per-user encrypted file storage** via copyparty, with a per-user encrypted blob underneath (maintainer-blessed pattern: <https://ocv.me/doc/unix/portable-luks.sh>).
4. **Remote-access gateway** that lets you and marcus "use the internet as if you were on the server," with **client-side regional split** so that traffic destined to mainland-CN IP space bypasses the VPS and goes out the user's local ISP — only foreign traffic exits via the VPS. Throughput overhead ≤20%. Target: ≥50 Mbps per user.
5. **Recoverable encrypted backups**, pulled from your client machines on a schedule (server holds no backup credentials; encrypted blobs are opaque ciphertext to the backup script).

Two users only, both co-admins, shared Docker daemon, single host kernel. Threat model is "encrypted at rest from disk theft / hosting-provider snapshots / leaked backups," not "zero-trust against a co-admin with root."

A secondary requirement is **operational discretion**: the externally visible footprint (DNS, SNI, port banners) and on-disk artifact names should not advertise their function. Camouflage is at the names-and-paths layer; package names visible to a shell user are not in scope (would require building from source under aliases, not worth the cost).

## Project Repository

Editable source of truth lives on your laptop at `/home/sagan/Projects/rarcus-server/`, tracked in git, deployed to the VPS via `rsync`/`scp` (or a simple Makefile target). The repo holds the full configuration: compose stacks, host config files, systemd units, scripts, and this plan document. Generated artifacts (peer configs, certs, secrets) are gitignored.

```
/home/sagan/Projects/rarcus-server/
├── README.md                            # one-pager: what this is, how to deploy
├── docs/
│   └── plan.md                          # (this document — copied in as step 0)
├── stacks/                              # console-managed compose stacks
│   ├── ingress/docker-compose.yml       # NPM
│   ├── console/docker-compose.yml       # Portainer (bootstrap)
│   ├── cloud/docker-compose.yml         # copyparty
│   └── qedge/docker-compose.yml         # alternate ingress (off by default)
├── host/                                # files that go directly on the host fs
│   ├── sysctl/99-host.conf              # → /etc/sysctl.d/99-host.conf
│   ├── ufw/before.rules.fragment        # NAT MASQUERADE block to splice into /etc/ufw/before.rules
│   ├── wireguard/gw0.conf.template      # gateway server config (peers stripped)
│   └── systemd/
│       └── store-mount@.service         # → /etc/systemd/system/store-mount@.service
├── scripts/
│   ├── deploy.sh                        # rsync stacks/ host/ to VPS, reload systemd
│   ├── mount-stores.sh                  # interactive: prompts for passphrases on (re)boot
│   ├── update-route-tables.sh           # regenerates AllowedIPs complement
│   └── provision-peer.sh                # produces peer .conf + QR for a device
├── peers/                               # gitignored: generated peer configs
└── .gitignore                           # peers/, *.key, *.crt, *.env, secrets/
```

The deploy story is "edit on laptop, commit, `./scripts/deploy.sh`." No editing files directly on the server. This also keeps the VPS filesystem reproducible — if it gets nuked, you re-provision from the laptop repo.

## Architecture Overview

```
                       Internet
                          │
                          ▼
                    LayerStack VPS
        ┌──────────────────────────────────────────┐
        │ host: Ubuntu LTS, ufw, sysctl tuning     │
        │ ┌──────────────────────────────────────┐ │
        │ │   ingress (Nginx Proxy Manager)      │ │ :80/:443  ── all HTTP(S) ingress
        │ │   wildcard cert via DNS-01           │ │
        │ └──┬──────────┬──────────┬─────────────┘ │
        │    ▼          ▼          ▼               │
        │  console     cloud    user apps          │ Docker network "edge"
        │ (Portainer) (copyparty) (compose stacks) │
        │                ▲                         │
        │                │ bind-mounts             │
        │   /srv/store/mnt/{sagan,marcus}/  ←──────│ unlocked encrypted volume mountpoints
        │   /srv/store/data/{sagan,marcus}.img      │ encrypted blobs at rest
        │                                          │
        │ gw0   :51820/udp ── primary gateway, kernel module
        │ qedge :443/udp   ── alternate ingress (TLS-camouflaged QUIC), idle by default
        │ mesh                ── admin overlay network (private, no inbound port from internet)
        │ sshd  :22/tcp    ── key-auth only, fail2ban
        └──────────────────────────────────────────┘

                Clients (you + marcus)
                ──────────────────────
   gateway client (default) →  AllowedIPs = world − domestic CIDR set
   mesh client (optional)   →  reach internal services via overlay
   sing-box client (reserve) → alternate ingress + GeoIP-direct rules
   restic client (cron)     →  pulls /srv/store/data/$user.img over ssh
```

Single-host, single-Docker-daemon, ingress (NPM) is the only thing on :80/:443. Every other service is internal to a Docker network and only reachable through ingress (or via the overlay mesh). Encrypted blobs live on the host filesystem (not inside Docker volumes) so they survive container churn and can be backed up as plain files.

## Component Decisions

| Layer | Choice | Why |
|---|---|---|
| Host OS | Ubuntu 24.04 LTS (not 25.10) | LTS gets 5-yr support; kernel 6.8 is well-supported by every module below. |
| Container runtime | Docker CE + compose v2 | Already familiar, manageable from the web UI, vast image ecosystem. |
| Container UI | **Portainer CE** (referred to internally as `console`) | Mature, supports compose stacks, custom containers, image build, volume/network management. ~150 MB RAM. |
| Reverse proxy | **Nginx Proxy Manager** (referred to internally as `ingress`) | Click-driven UI, auto-Let's Encrypt with DNS-01 wildcard via OVH API (or any DNS-01-supported provider), per-host access lists. ~100 MB RAM. |
| File server | **copyparty** in Docker (referred to internally as `cloud`) | What you asked for; great UX; ACL'd per-user. |
| At-rest encryption | **LUKS2** sparse file per user, mounted at unlock time via systemd | Maintainer-recommended; portable single-file blob; ~0% perf overhead; trivial backup target. |
| Primary gateway | **AmneziaWG** kernel module via DKMS, interface `gw0` | WireGuard variant with anti-DPI junk packets + randomized init headers. Single-digit-% overhead. Survives "reliable for daily work" regional pressure. Interface name `gw0`, not `awg0`/`wg0`, to avoid the obvious banner. |
| Alternate ingress | **Hysteria2** (referred to internally as `qedge`) | QUIC on :443/udp, presents as ordinary TLS handshake. 50–500 Mbps single-stream on 2 vCPU, ~5–10% overhead. Stays stopped by default; switched on only when the primary path is being actively probed/blocked. |
| Admin overlay | **Tailscale** (referred to internally as `mesh`) | Useful for admins not currently behind heavy filtering; gives an out-of-band path to `console`/`ingress` admin UIs that never has to be exposed to the public internet. |
| Regional split routing | **chnroutes2** → `AllowedIPs` complement, refreshed monthly via cron | Client-side: gateway client adds domestic CIDRs as more-specific routes via the local gateway, defaults via VPS. Keeps domestic services fast and direct. |
| Backups | **restic over SSH (pulled from client)** | Client cron → `restic -r sftp:vps:… backup`. Server stores only ciphertext; client holds repo password. Off-host, deduped, snapshotted. |
| Firewall | **ufw** + `ufw-docker` for Docker-aware rules | Default-deny inbound; only :22/tcp, :80/tcp, :443/tcp, :443/udp, :51820/udp open. |
| Process supervision | systemd units for volume mount + gateway; everything else in Docker | Boundary between host concerns and app concerns stays clean. |

### Naming convention

The plan uses these neutral identifiers consistently:

| Internal name | What it actually is |
|---|---|
| `ingress` | Nginx Proxy Manager (reverse proxy + ACME) |
| `console` | Portainer (Docker management UI) |
| `cloud` | copyparty (file server) |
| `gw0` | AmneziaWG kernel interface |
| `qedge` | Hysteria2 QUIC service (kept stopped by default) |
| `mesh` | Tailscale (admin overlay) |
| `store` | the per-user encrypted volume system (LUKS blob + mountpoint) |

Subdomains, paths, container names, systemd units all follow this naming. Public-facing artifacts never use the words "vpn", "wireguard", "tunnel", "stealth", "vault", "luks", or "crypt".

### Things explicitly NOT in scope

- Hard multi-tenant isolation (LXC/VM per user). You're co-admins.
- Client-side end-to-end encryption (Cryptomator etc.). You picked encrypted-at-rest only.
- CDN / DNS-proxy fronting (Cloudflare orange-cloud, OVH AlwaysOn, etc.) on the apex or any subdomain. We use OVH for DNS only and DNS-01 ACME for wildcard certs; records stay direct (no proxy in front) — proxy-fronting breaks `gw0`/`qedge` ingress and adds an unwanted MITM.
- `qedge` as the daily driver. It stays stopped until the primary path is actively under pressure.

## DNS Layout (`antarctica-engineering.com`)

| Record | Type | Target | Purpose |
|---|---|---|---|
| `antarctica-engineering.com` | A | `VPS_IP` | apex (optional; can serve a generic landing page or 404) |
| `*.antarctica-engineering.com` | A | `VPS_IP` | wildcard for all subdomains |
| `gw.antarctica-engineering.com` | A (grey) | `VPS_IP` | gateway endpoint hostname (clients connect here) |
| `cdn.antarctica-engineering.com` | A (grey) | `VPS_IP` | alternate ingress hostname (used as SNI by `qedge`) |

Public app subdomains, created in `ingress` as needed:

| Subdomain | Behind | Notes |
|---|---|---|
| `cloud.antarctica-engineering.com` | `cloud` (copyparty) | public, password-protected by copyparty |
| user-app subdomains | user stacks | created on demand by either admin via `console` |

**Admin UIs are not on public DNS.** `console` (Portainer) and `ingress`'s own admin panel are reachable **only** via the `mesh` overlay or via SSH-tunnel — there's no public hostname for them. This matters because admin UIs are the highest-value compromise targets and exposing them publicly is the main thing that gets DIY self-hosters owned.

DNS-01 ACME against the OVH API gives `ingress` a wildcard cert covering everything, no per-subdomain HTTP-01 dance. SNI for `cdn.antarctica-engineering.com` (`qedge`) is presented by Hysteria2 itself with the same cert, so it looks like a perfectly ordinary CDN edge to a passive observer.

## Filesystem Layout

```
/etc/wireguard/gw0.conf                      # gateway server config (peers + AmneziaWG params)
/etc/wireguard/peers/{sagan,marcus}.conf     # generated peer configs
/etc/sysctl.d/99-host.conf                   # ip_forward=1, BBR, fs.inotify limits, etc.
/etc/ufw/before.rules                        # NAT MASQUERADE for gw0
/etc/systemd/system/store-mount@.service     # per-user volume unlock unit (template)
/etc/cron.monthly/update-route-tables        # refresh domestic-CIDR set

/srv/store/data/sagan.img                    # 50 GB sparse, LUKS2, your passphrase
/srv/store/data/marcus.img                   # 50 GB sparse, LUKS2, marcus's passphrase
/srv/store/mnt/sagan/                        # mountpoint when unlocked
/srv/store/mnt/marcus/                       # mountpoint when unlocked

/opt/stacks/                                 # console-managed compose stacks
  ingress/                                   # NPM
  console/                                   # Portainer (bootstrap-deployed)
  cloud/                                     # copyparty + bind-mounts /srv/store/mnt
  qedge/                                     # alternate ingress (off by default)

/opt/scripts/
  mount-stores.sh                            # interactive: prompts for passphrases on (re)boot
  update-route-tables.sh                     # regenerates AllowedIPs complement
  provision-peer.sh                          # produces peer .conf + QR for a device
```

No path on this filesystem contains the words `luks`, `vault`, `vpn`, `wireguard`, `amnezia`, `hysteria`, `tailscale`, or `stealth`. Backup blobs leak only as `${user}.img` files under a directory called `store`.

## Build Sequence

This is the order to actually do things; each step depends on the previous.

0. **Create project repo on the laptop** — `mkdir -p /home/sagan/Projects/rarcus-server/{docs,stacks/{ingress,console,cloud,qedge},host/{sysctl,ufw,wireguard,systemd},scripts,peers}`; copy this plan into `docs/plan.md`; `git init`; write the `.gitignore` (peers/, *.key, *.crt, *.env, secrets/); first commit. Everything else flows from this directory.
1. **Base host hardening** — fresh Ubuntu 24.04, non-root admin users (`sagan`, `marcus`), SSH key-only auth, `fail2ban`, `ufw` default-deny inbound, `unattended-upgrades`, swapfile (4 GB), enable BBR + `net.ipv4.ip_forward=1` in `/etc/sysctl.d/99-host.conf`.
2. **DNS** — create A + wildcard + `gw.` + `cdn.` records at OVH (no CDN / proxy in front); generate an OVH application token scoped to DNS read/write on the single zone (`ingress` will use this for DNS-01). See `stacks/ingress/README.md` §2 for the exact token grant list.
3. **Docker** — install Docker CE, add both users to the `docker` group, create the `edge` Docker network that all `ingress`-fronted services join.
4. **`console` (Portainer)** — bootstrap via one-shot `docker run`, then create admin accounts. From here on, every other stack is created via `console`.
5. **`ingress` (NPM) stack** — deploy via `console`; configure OVH DNS-01 with the application token; provision wildcard cert for `*.antarctica-engineering.com`; expose `cloud.antarctica-engineering.com` as the first proxy host (proves the stack works end-to-end). Do **not** create a public proxy host for `console` or for `ingress`'s own admin — restrict those to the `mesh` overlay.
6. **`store` volumes** — create `sagan.img` + `marcus.img` under `/srv/store/data/` (50 GB each, sparse, LUKS2, argon2id KDF); each user sets their own passphrase; install `store-mount@.service` systemd template that prompts via `systemd-ask-password` at unlock time (manual unlock on each reboot — passphrases never on disk).
7. **`cloud` (copyparty) stack** — deploy via `console` with bind-mounts to `/srv/store/mnt/sagan` and `/srv/store/mnt/marcus`; configure two copyparty user accounts mapped to those volumes; expose at `cloud.antarctica-engineering.com` through `ingress`. Verify: when a volume isn't mounted, copyparty just sees an empty directory (fail-closed by accident — fine).
8. **Gateway server (`gw0`)** — install `amneziawg-dkms` + `amneziawg-tools` from the upstream PPA; rename the interface to `gw0` in the unit; if DKMS doesn't build against the running kernel, fall back to the userspace `amneziawg-go` binary; configure anti-DPI parameters (`Jc/Jmin/Jmax/S1/S2/H1/H2/H3/H4`); add NAT MASQUERADE for `gw0` in `/etc/ufw/before.rules`; enable on boot.
9. **Generate peer configs** — `provision-peer.sh` produces a `.conf` whose `AllowedIPs` is the **complement** of the chnroutes2 CIDR set (everything except mainland CN). Output: one `.conf` per device + QR code for the mobile client. The same script schedules a monthly `update-route-tables.sh` to refresh the CIDR set.
10. **`mesh` (Tailscale)** — install + `tailscale up --ssh`; both admins join from their dev machines; the `mesh` interface is the only path to `console` and `ingress`'s admin UI from anywhere outside the box.
11. **`qedge` stack** — deploy in `console` but leave it stopped. Cert (covered by the wildcard) and obfuscation password documented in a sealed note. Procedure: if the primary `gw0` path goes silent during a heavy regional crackdown, start the container; the pre-prepared sing-box client config connects to `cdn.antarctica-engineering.com:443/udp` with built-in GeoIP-direct rules.
12. **Backups (client-side)** — on each admin's local machine: `restic init` against `sftp:vps:/srv/store/data` (restic stores its repo metadata client-side; the actual backup target is each user's `${user}.img`). Cron / systemd-timer runs `restic backup /srv/store/data/$user.img` daily, pulled from the server. Repo password lives only on the client. Test recovery: pull a snapshot to a scratch host, attach the recovered `.img` as a loop device, unlock with the passphrase, mount, verify a known file.
13. **Verification pass** — see Verification section.

## Regional Split Routing (the "domestic-direct" piece)

The whole trick is in the **client** `AllowedIPs` line — the server is unaware. Two ways to realize "default route via VPS, but domestic-CN traffic stays local":

- **AllowedIPs as complement of the domestic CIDR set** (preferred for the `gw0` client): `update-route-tables.sh` pulls <https://github.com/misakaio/chnroutes2> and produces the inverted set ("everything except CN") as ~5000 CIDRs. Drop into the client `.conf`. The client installs all of those as routes through `gw0`; CN-destined packets fall back to the system default route (local gateway). The routing table grows by ~5k entries — fine on Linux/Android/iOS/macOS; Windows handles it but uglier.

- **sing-box / clash-meta with GeoIP rules** (used by the `qedge` reserve client): `route.rules` includes `{"geoip": ["cn"], "outbound": "direct"}` and `{"outbound": "proxy"}` as the default. No CIDR list management; GeoIP DB is bundled. This is why sing-box is the recommended client for the alternate path even though the `gw0` path is daily.

`update-route-tables.sh` is the load-bearing script; it must regenerate peer configs (or update the `AllowedIPs` line via `wg syncconf`-style update) on a monthly cadence. CIDR drift is real but slow.

## Verification

End-to-end checks before declaring done. Run from outside the VPS unless noted.

1. **Public HTTPS reachability**
   - `curl -sI https://cloud.antarctica-engineering.com` → 200, valid Let's Encrypt cert covering `*.antarctica-engineering.com`.
   - `curl -sI https://console.antarctica-engineering.com` → connection refused or DNS NXDOMAIN (admin UI is **not** publicly exposed; this is the intended state).
2. **Encrypted volume round-trip**
   - With your volume unlocked: upload `test.bin` via `cloud`.
   - On host: `umount /srv/store/mnt/sagan && cryptsetup close store_sagan`.
   - Browse `cloud.antarctica-engineering.com` → volume appears empty / inaccessible.
   - Re-unlock → `test.bin` reappears.
3. **Gateway throughput (primary path)**
   - From a client outside heavy filtering: `iperf3 -c <vps>` over `gw0` → ≥800 Mbps (sanity check).
   - From a client inside mainland CN on CN2: `iperf3 -c <vps>` → ≥50 Mbps sustained.
4. **Regional split correctness (the important one)**
   - Client connected via `gw0`, in mainland China:
     - `curl -s ifconfig.me` → returns **VPS public IP** (foreign traffic exits via VPS).
     - `mtr -rwc 10 baidu.com` → first hop is the **local gateway**, not the VPS (CN traffic stays direct).
     - `mtr -rwc 10 github.com` → first hop is the **VPS** (foreign traffic transits VPS).
5. **Alternate-ingress drill**
   - Stop `gw0` on the server. Start the `qedge` stack from `console`.
   - Switch client to the sing-box profile → all the above checks should still pass.
   - Throughput delta < 20% vs `gw0`.
6. **Backup recoverability**
   - On client: `restic snapshots` → recent snapshot of `sagan.img` exists.
   - `restic restore latest --target /tmp/recover` → recovered `.img` mounts, unlocks with the passphrase, and the test file from check (2) is present.
7. **Resource budget sanity**
   - `free -m` on the server with everything running (`ingress`, `console`, `cloud`, `gw0`, `mesh`, `qedge` stopped): used RAM ≤ 2.5 GB, leaving ~1.5 GB headroom for user-deployed apps.
   - If headroom is tight, document which container memory-limits to set in `console`.

## Risks & Open Trade-offs

- **4 GB RAM is tight.** Every app you and marcus deploy via `console` eats into the same pool. Set per-container memory limits and treat the swapfile as a safety net, not a pool. RAM-heavy stacks (databases, AI inference) would warrant a VPS upgrade rather than tuning around it.
- **DKMS module against newer kernels.** Mitigation: pin Ubuntu 24.04 LTS (kernel 6.8) and accept the userspace fallback (~30% perf hit but still hits the 50 Mbps target).
- **`console` is a single point of compromise** for the Docker daemon. Keeping it off public DNS and reachable only via `mesh` (or SSH tunnel) is doing a lot of the heavy lifting here. Enable Portainer's MFA on top.
- **Manual volume unlock on every reboot.** Intentional — passphrases not on disk. If the VPS reboots while you're asleep, `cloud` is unavailable until someone unlocks. Acceptable for the threat model.
- **Wildcard cert via OVH DNS-01** means an OVH application token (application_key / application_secret / consumer_key triple) lives on the server. Scope it tightly (DNS read/write on the single zone), rotate yearly, store in `ingress`'s encrypted config — not in plain compose files.
- **CIDR list drift.** Updates monthly-ish. If a domestic service moves to a new prefix that isn't yet in the list, traffic to it briefly transits the VPS (correct destination, slower path, no breakage). Acceptable.
- **Camouflage is defense-in-depth, not invisibility.** Anyone with shell access on the box can `apt list --installed`, `ip -d link show gw0`, `ss -tulpn` and identify every component. The renamed paths/subdomains/units protect against passive DNS observation, casual SNI fingerprinting, banner grabs, and backup-blob filename leakage — not against an attacker who already has root.
