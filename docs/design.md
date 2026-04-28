# raph-server-installer — Design Document

## Purpose

This document describes the system that `raph-server-installer` provisions
on a fresh Ubuntu 24.04 VPS. It is the canonical design reference: the
installer scripts, compose stacks, and setup wizard implement what is
described here. Anyone reading the repo cold should be able to start at
this document and follow the cross-references into the relevant code.

`README.md` (at the repo root) is the public-facing one-pager —
"what is this, how do I install it." This document is the layer below:
the architectural rationale and the build sequence it follows.

## Context

The installer targets a small operator who wants a self-hosted "private
cloud" on a single VPS without becoming a part-time DevOps engineer.
The reference deployment target is a 2 vCPU / 2–4 GB / 100+ GB Ubuntu
24.04 LTS host with a public IPv4 and a registered domain whose DNS
the operator controls. The stack delivers five things:

1. **Host Docker apps**, managed from a web UI (Portainer) — the operator
   should not need SSH for day-to-day operations.
2. **Reverse-proxy** those apps behind clean HTTPS via Nginx Proxy
   Manager with an automatic Let's Encrypt wildcard cert.
3. **Multi-user file storage and collaboration** via Nextcloud (with
   Talk for WebRTC calls and groupfolders for shared spaces), backed by
   Postgres + Redis. User data lives in plaintext on the host's
   encrypted-at-rest filesystem; per-user isolation is enforced at the
   application layer (Nextcloud's per-user data dir + ACLs). See
   `docs/architecture-decisions.md` ADR-004 for why per-user LUKS was
   removed and how to opt into Nextcloud Server-side Encryption if
   cold-disk protection becomes important again.
4. **Remote-access gateway** (AmneziaWG, branded internally as `gw0`)
   that lets each admin "use the internet as if they were on the
   server," with optional **client-side regional split** so that
   traffic destined to a configured CIDR set bypasses the VPS and exits
   the user's local ISP — only foreign traffic transits the VPS.
   Throughput overhead target ≤20%, ≥50 Mbps per peer.
5. **Recoverable encrypted backups**, pulled from client machines on a
   schedule (the VPS holds no backup credentials; encrypted blobs are
   opaque ciphertext to the backup script).

Small admin set (typically 1–4), shared Docker daemon, single host
kernel. Threat model is **trusted co-admins on a host whose underlying
filesystem is encrypted by the VPS provider** (Layerstack provides
full-disk encryption for the boot volume). Per-user file isolation is
application-layer (Nextcloud ACLs), not block-layer. Operators who need
hard tenant isolation should look elsewhere.

A secondary requirement is **operational discretion** ("camouflage" in
the README): the externally visible footprint (DNS labels, SNI strings,
port banners) and on-disk artefact names do not advertise the
function of the services they belong to. Camouflage is at the names-
and-paths layer; package names visible to a shell user with root are
explicitly not in scope.

## Project Repository

The installer is distributed as a public git repo (target name
`raph-server-installer`). The bootstrap script clones the repo into
`/opt/raph-server-installer` on the VPS during phase 1. Generated
artefacts (peer configs, certs, secrets, rendered templates) are
gitignored — only declarative source lives in version control.

```
<repo-root>/
├── README.md                            # one-pager: what this is, how to install
├── LICENSE                              # MIT
├── bootstrap.sh                         # entry point invoked from cloud-init
├── .env.example                         # documents every supported env var
├── docs/
│   ├── design.md                        # (this document)
│   ├── maintenance.md                   # day-2 ops runbook
│   ├── verification.md                  # 7-check sign-off
│   ├── backups.md                       # restic recovery playbook
│   ├── dns-records.md                   # required DNS layout
│   └── perf-tuning.md                   # gw0 tuning reference
├── stacks/                              # console-managed compose stacks
│   ├── ingress/docker-compose.yml       # NPM (reverse proxy)
│   ├── console/docker-compose.yml       # Portainer (Docker UI)
│   ├── cloud/docker-compose.yml         # Nextcloud + Postgres + Redis + nginx sidecar
│   ├── authelia/docker-compose.yml      # Authelia (SSO / OIDC)
│   ├── enrol/docker-compose.yml         # custom Go service: setup wizard + admin UI
│   └── qedge/docker-compose.yml         # alternate ingress (off by default)
├── host/                                # files that go directly on the host fs
│   ├── sysctl/99-host.conf              # → /etc/sysctl.d/99-host.conf
│   ├── ufw/before.rules.fragment        # NAT MASQUERADE block for /etc/ufw/before.rules
│   ├── wireguard/gw0.conf.template      # gateway server config (peers stripped)
│   └── systemd/                         # gateway / cert-watcher / bootstrap-continue units
├── scripts/
│   ├── bootstrap-host.sh                # base host hardening
│   ├── install-docker.sh                # Docker CE + edge network
│   ├── install-gw0.sh                   # AmneziaWG DKMS install
│   ├── install-mesh.sh                  # optional Tailscale
│   ├── render-templates.sh              # envsubst over .template files
│   ├── provision-peer.sh                # produces peer .conf + QR for a device
│   ├── update-route-tables.sh           # regenerates regional-split CIDR set
│   └── smoke-test.sh                    # end-to-end probes (laptop-side)
├── peers/                               # gitignored: generated peer configs
└── .gitignore
```

The deploy story for an installer-driven host is "the wizard does it."
For an operator developing the installer itself: edit on a laptop,
commit, push, the next bootstrap pulls the new HEAD. `scripts/deploy.sh`
remains as a path for hands-on operators who want to rsync local
working-tree changes to a running VPS without going through the
public-repo round-trip; it is not the primary install mechanism.

## Architecture Overview

```
                       Internet
                          │
                          ▼
                       VPS host
        ┌──────────────────────────────────────────┐
        │ host: Ubuntu LTS, ufw, sysctl tuning     │
        │ ┌──────────────────────────────────────┐ │
        │ │   ingress (Nginx Proxy Manager)      │ │ :80/:443  ── all HTTP(S) ingress
        │ │   wildcard cert via DNS-01           │ │
        │ └──┬──────────┬──────────┬─────────────┘ │
        │    ▼          ▼          ▼               │
        │  console     cloud    user apps          │ Docker network "edge"
        │ (Portainer) (Nextcloud) (compose stacks) │
        │                ▲                         │
        │                │ bind-mounts             │
        │   /srv/store/cloud-{data,config,apps,db} │ Nextcloud + Postgres state on host
        │                                          │
        │ gw0   :443/udp   ── primary gateway, kernel module (QUIC-shape camouflage)
        │ qedge :443/udp   ── alternate ingress (TLS-camouflaged QUIC); MUTUALLY EXCLUSIVE with gw0
        │ mesh                ── admin overlay network (private, no inbound port from internet)
        │ sshd  :22/tcp    ── key-auth only, fail2ban
        └──────────────────────────────────────────┘

                Clients (admins)
                ──────────────────────
   gateway client (default) →  AllowedIPs = world − domestic CIDR set
   mesh client (optional)   →  reach internal services via overlay
   sing-box client (reserve) → alternate ingress + GeoIP-direct rules
   backup workflow          →  rsync /srv/store/cloud-* + pg_dump (ADR-001)
```

Single-host, single-Docker-daemon, ingress (NPM) is the only thing on :80/:443. Every other service is internal to a Docker network and only reachable through ingress (or via the overlay mesh). Per ADR-001, every stack's persistent state lives under `/srv/store/<stack>-*` host bind-mounts so backup is one rsync per box plus a `pg_dump` per Postgres-backed stack — never inside opaque Docker named volumes.

## Component Decisions

| Layer | Choice | Why |
|---|---|---|
| Host OS | Ubuntu 24.04 LTS (not 25.10) | LTS gets 5-yr support; kernel 6.8 is well-supported by every module below. |
| Container runtime | Docker CE + compose v2 | Already familiar, manageable from the web UI, vast image ecosystem. |
| Container UI | **Portainer CE** (referred to internally as `console`) | Mature, supports compose stacks, custom containers, image build, volume/network management. ~150 MB RAM. |
| Reverse proxy | **Nginx Proxy Manager** (referred to internally as `ingress`) | Click-driven UI, auto-Let's Encrypt with DNS-01 wildcard via the operator's chosen DNS provider (Cloudflare / OVH / Route53 / DigitalOcean / Google / Linode / RFC2136), per-host access lists. ~100 MB RAM. |
| File server + collaboration | **Nextcloud** (fpm + Postgres + Redis + nginx sidecar) in Docker (referred to internally as `cloud`) | Drag-and-drop file moves, share-link semantics, integrated Talk for WebRTC calls, groupfolders for shared spaces, OIDC via the Authelia `cloud` client. |
| At-rest encryption | **VPS-provider full-disk encryption** (e.g. Layerstack Layerstack-encrypted host filesystem). Optional Nextcloud Server-side Encryption (`occ app:enable encryption`) if cold-disk-from-host-image protection is needed beyond what the provider gives. | Per-user LUKS was removed in ADR-004 — see that decision for rationale and the SSE opt-in path. |
| Primary gateway | **AmneziaWG** kernel module via DKMS, interface `gw0` | WireGuard variant with anti-DPI junk packets + randomized init headers. Single-digit-% overhead. Survives "reliable for daily work" regional pressure. Interface name `gw0`, not `awg0`/`wg0`, to avoid the obvious banner. |
| Alternate ingress | **Hysteria2** (referred to internally as `qedge`) | QUIC on :443/udp, presents as ordinary TLS handshake. 50–500 Mbps single-stream on 2 vCPU, ~5–10% overhead. Stays stopped by default; switched on only when the primary path is being actively probed/blocked. |
| Admin overlay | **Tailscale** (referred to internally as `mesh`) | Useful for admins not currently behind heavy filtering; gives an out-of-band path to `console`/`ingress` admin UIs that never has to be exposed to the public internet. |
| Regional split routing | **chnroutes2** (or another CIDR set) → `AllowedIPs` complement, refreshed monthly via cron | Client-side: gateway client adds domestic CIDRs as more-specific routes via the local gateway, defaults via VPS. Keeps domestic services fast and direct. Optional — operators outside the original problem domain can skip the split entirely. |
| Backups | **restic over SSH (pulled from client)** | Client cron → `restic -r sftp:vps:… backup`. Server stores only ciphertext; client holds repo password. Off-host, deduped, snapshotted. |
| Firewall | **ufw** + `ufw-docker` for Docker-aware rules | Default-deny inbound; only :22/tcp, :80/tcp, :443/tcp, :443/udp open (the single :443/udp slot serves either `gw0` or `qedge`, not both). |
| Process supervision | systemd units for volume mount + gateway; everything else in Docker | Boundary between host concerns and app concerns stays clean. |

### Naming convention

The installer uses these neutral identifiers consistently:

| Internal name | What it actually is |
|---|---|
| `ingress` | Nginx Proxy Manager (reverse proxy + ACME) |
| `console` | Portainer (Docker management UI) |
| `cloud` | Nextcloud (file server + Talk + groupfolders) |
| `gw0` | AmneziaWG kernel interface |
| `qedge` | Hysteria2 QUIC service (kept stopped by default) |
| `mesh` | Tailscale (admin overlay) |
| `store` | the host bind-mount root for every stack's persistent state (`/srv/store/<stack>-*`, ADR-001) |

Subdomains, paths, container names, systemd units all follow this naming. Public-facing artifacts never use the words "vpn", "wireguard", "tunnel", "stealth", or upstream project names. See ADR-002 for the camouflage policy.

### Things explicitly NOT in scope

- Hard multi-tenant isolation (LXC/VM per user). The threat model assumes
  trusted co-admins, not adversarial users with shell access.
- Client-side end-to-end encryption (Cryptomator etc.). The installer
  delivers application-layer access control on a provider-encrypted
  filesystem; clients see plaintext.
- CDN / DNS-proxy fronting (Cloudflare orange-cloud, OVH AlwaysOn, etc.)
  on the apex or any subdomain. The installer uses the chosen DNS
  provider for DNS-01 ACME only; records stay direct (no proxy in
  front) — proxy-fronting breaks `gw0`/`qedge` ingress and adds an
  unwanted MITM on the SSO/Authelia flow.
- `qedge` as the daily driver. It stays stopped until the primary path
  is actively under pressure.
- Hosting an operator's website. The apex is owned by the wizard and
  Authelia for setup-time and SSO routing; the installer does not
  provide a CMS or static-site stack.

## DNS Layout (`<your-domain>`)

| Record | Type | Target | Purpose |
|---|---|---|---|
| `<your-domain>` | A | `VPS_IP` | apex (serves a landing page during setup, otherwise 404) |
| `*.<your-domain>` | A | `VPS_IP` | wildcard for all subdomains (covers `setup.`, `auth.`, `cloud.`, `enrol.`, `console.`, `gw.`, `cdn.`) |

The wildcard is the only DNS record the wizard strictly requires; `gw.`
and `cdn.` are referenced by `provision-peer.sh` and `qedge` but are
synthesised at runtime from the apex.

Setup-time and steady-state subdomains created/used by the installer:

| Subdomain | Behind | When |
|---|---|---|
| `setup.<your-domain>` | NPM proxy host → `enrol` on `172.17.0.1:8080` | setup phase only — proxy host removed and `enrol` returns 404 on `/setup*` once the wizard is done |
| `auth.<your-domain>` | `authelia` | SSO login portal + OIDC issuer |
| `cloud.<your-domain>` | `cloud-web` (nginx → Nextcloud-fpm) | public, Authelia rule = `bypass`; Nextcloud owns the session via the Authelia OIDC `cloud` client (ADR-003) |
| `enrol.<your-domain>` | `enrol` | admin UI for user / peer / volume management |
| `console.<your-domain>` | `console` (Portainer) | OIDC-gated Docker UI |
| `gw.<your-domain>` | `gw0` | AmneziaWG endpoint (UDP) |
| `cdn.<your-domain>` | `qedge` | alternate ingress SNI (used only when `qedge` is started) |

DNS-01 ACME via the wizard-collected provider credential gives `ingress`
a wildcard cert covering every subdomain — no per-subdomain HTTP-01
dance. SNI for `cdn.<your-domain>` (`qedge`) is presented by Hysteria2
itself with the same cert, so it looks like a perfectly ordinary CDN
edge to a passive observer.

## Filesystem Layout

```
/etc/wireguard/gw0.conf                      # gateway server config (peers + AmneziaWG params)
/etc/wireguard/peers/<device>.conf           # generated peer configs (one per device)
/etc/sysctl.d/99-host.conf                   # ip_forward=1, BBR, fs.inotify limits, etc.
/etc/ufw/before.rules                        # NAT MASQUERADE for gw0
/etc/cron.monthly/update-route-tables        # refresh domestic-CIDR set

/srv/store/cloud-data/                       # Nextcloud user data (uid 33 www-data, ADR-001)
/srv/store/cloud-config/                     # Nextcloud config.php + per-instance state
/srv/store/cloud-apps/                       # Nextcloud custom_apps (operator-installed apps)
/srv/store/cloud-db/                         # Postgres data dir (uid 70)

/opt/stacks/                                 # console-managed compose stacks
  ingress/                                   # NPM
  console/                                   # Portainer (bootstrap-deployed)
  cloud/                                     # Nextcloud + Postgres + Redis + nginx sidecar
  qedge/                                     # alternate ingress (off by default)

/opt/scripts/
  update-route-tables.sh                     # regenerates AllowedIPs complement
  provision-peer.sh                          # produces peer .conf + QR for a device
```

No path on this filesystem contains the words `vpn`, `wireguard`, `amnezia`, `hysteria`, `tailscale`, or `stealth`. Per ADR-001, every stack's persistent state lives under `/srv/store/<stack>-*` so a single `rsync` (plus `pg_dump` for Postgres-backed stacks) covers backup.

## Build Sequence

The installer executes the steps below as two phases: **phase 1**
(unattended, driven by `bootstrap.sh` from cloud-init) and **phase 2**
(interactive, driven by the operator through the `enrol` setup wizard at
`https://setup.<your-domain>`). The split is bookkeeping for which
secrets the operator must supply, not a hard ordering constraint —
phase 2 strictly depends on phase 1 finishing.

### Phase 1 — bootstrap.sh (unattended)

Runs as root from the VPS provider's user-data / cloud-init box. The
operator supplies `DOMAIN` and `ADMIN_EMAIL` at the top of the pasted
command; everything else is computed.

0. **Clone the installer repo** into `/opt/raph-server-installer`.
   Idempotent: re-pasting the bootstrap pulls latest HEAD.
1. **Base host hardening** — fresh Ubuntu 24.04, `fail2ban`, `ufw`
   default-deny inbound (staged, enabled at end of phase 1),
   `unattended-upgrades`, 4 GB swapfile, sysctl block (BBR,
   `ip_forward=1`, fs.inotify limits), SSH set to key-only.
2. **Render templates** — `scripts/render-templates.sh` substitutes
   `${DOMAIN}` and other env vars over every `*.template` file:
   `host/ssh/sshd_config.d/99-hardening.conf.template`,
   `stacks/authelia/configuration.yml.template`,
   `stacks/authelia/snippets/authelia-authrequest.conf.template`,
   `stacks/qedge/config.yaml.template`, etc.
3. **Docker CE + edge network** — `install-docker.sh`. The `edge`
   Docker network is created here; every ingress-fronted stack
   joins it.
4. **AmneziaWG (`gw0`)** — `install-gw0.sh` installs the upstream PPA's
   `amneziawg-dkms` + `amneziawg-tools`. If DKMS reports
   "WAITING FOR REBOOT", `bootstrap.sh` registers
   `bootstrap-continue.service` and reboots; phase 1 resumes
   automatically post-reboot. Falls back to `amneziawg-go` userspace
   if DKMS fails entirely.
5. **`/srv/store` skeleton + cloud bind-mount targets** — create
   `/srv/store/cloud-{data,config,apps,db}` with the right uid/gid
   (33:33 for the three Nextcloud dirs, 70:70 for Postgres). Per
   ADR-001 these host bind-mounts replace the previous LUKS-blob
   layout; the Nextcloud stack refuses to init if perms are wrong.
6. **Bring up the Docker stacks in setup mode** —
   `docker compose up -d` for `ingress`, `authelia`, `cloud`, `console`,
   `enrol`. `enrol` keeps its host-mode listener on `172.17.0.1:8080`;
   `ingress` (NPM) owns `:80` / `:443`. `bootstrap-npm-setup-route.sh`
   then upserts a single proxy host — `setup.<your-domain>` → enrol —
   so the wizard answers at the subdomain root. The wizard's setup-mode
   gate dispatches `/` to the wizard root handler and keeps the internal
   `/setup/<step>` paths for the step pages.
7. **Print the wizard URL** to the cloud-init console:
   `https://setup.<your-domain>/` (or `http://<vps-ip>/` with the
   `Host: setup.<your-domain>` header if DNS is still propagating).

### Phase 2 — wizard at `https://setup.<your-domain>/` (interactive)

Six steps, served by `enrol` against an in-memory state machine. Each
step is idempotent: refreshing or going back does not corrupt state.

1. **Welcome / DNS check** — verify `<your-domain>` and
   `setup.<your-domain>` resolve to the host's public IPv4. The
   operator can override the check if propagation is still in flight.
2. **First admin** — collect username, password, email. Generates the
   argon2id digest, writes the bootstrap `users_database.yml` for
   Authelia. The Nextcloud account is provisioned automatically by
   the `user_oidc` app on first OIDC login (no `occ user:add` here).
3. **DNS provider credentials** — operator chooses Cloudflare, OVH,
   Route53, DigitalOcean, Google, Linode, or RFC2136 from a dropdown
   and pastes the relevant token / API key. The wizard writes the
   provider-specific credentials INI file into NPM's data directory at
   the path `certbot-dns-<provider>` expects.
4. **Cert issuance + ingress wireup** — `enrol` talks to NPM's REST API
   to issue the wildcard cert (DNS-01 via the operator's chosen provider)
   and upsert the four steady-state proxy hosts (`auth`, `cloud`,
   `enrol`, `console`) on HTTPS. The `cloud` proxy host uses the
   Nextcloud advanced_config template (50 GB upload chain, no
   forward-auth — Nextcloud owns its session via OIDC). The bootstrap
   `setup.<your-domain>` proxy host is removed. From here on the
   operator reaches the admin surface at `enrol.<your-domain>` (HTTPS,
   behind Authelia).
5. **Storage** — set the Nextcloud per-user quota default (default
   50 GB; 0 = unlimited). Applied at first OIDC login via
   `occ user:setting <u> files quota`. No passphrases collected; user
   data lives in plaintext on the host's encrypted-at-rest filesystem
   (ADR-004).
6. **Done** — write `/srv/store/.setup-complete`, redirect to the
   Authelia login portal. From this point the `setup.<your-domain>`
   proxy host is gone and `enrol` returns 404 on every `/setup*` route;
   the operator manages users and peers through the regular admin UI at
   `enrol.<your-domain>` (which also surfaces per-user usage via
   `occ user:info`, see ADR-005).

### Optional post-install steps

- **`mesh` (Tailscale)** — manual one-liner per `docs/maintenance.md`.
  The wizard does not install Tailscale; admins who want an out-of-band
  path to `console` and the NPM admin panel install it themselves.
- **`qedge` stack** — deployed but stopped. The wizard does NOT start
  qedge; operators flip the toggle only when the primary `gw0` path is
  actively under pressure.
- **Backups (client-side)** — see `docs/backups.md`. Each admin runs
  `restic init` against `sftp:vps:/srv/store/data` from their own
  laptop; the VPS holds no backup credentials.

The legacy 13-step "deploy from laptop" path is still documented in
`scripts/deploy.sh` and the per-stack READMEs for operators developing
the installer itself.

## Regional Split Routing (optional)

The whole trick is in the **client** `AllowedIPs` line — the server is
unaware. Two ways to realize "default route via VPS, but in-region
traffic stays local":

- **AllowedIPs as complement of a CIDR set** (preferred for the `gw0`
  client): `update-route-tables.sh` pulls a CIDR list (the default
  reference list is <https://github.com/misakaio/chnroutes2>; operators
  can substitute another) and produces the inverted set as ~5000 CIDRs.
  Drop into the client `.conf`. The client installs all of those as
  routes through `gw0`; in-set packets fall back to the system default
  route (local gateway). The routing table grows by ~5k entries — fine
  on Linux/Android/iOS/macOS; Windows handles it but uglier.

- **sing-box / clash-meta with GeoIP rules** (used by the `qedge`
  reserve client): `route.rules` includes a GeoIP-direct entry and a
  default-proxy fallback. No CIDR list management; GeoIP DB is bundled.
  This is why sing-box is the recommended client for the alternate
  path even though the `gw0` path is daily.

The feature is opt-in per-peer. Operators who do not need a regional
split can use the unmodified `AllowedIPs = 0.0.0.0/0, ::/0` from
`provision-peer.sh` and skip the cron entirely.

`update-route-tables.sh` is the load-bearing script when the split is
enabled; it regenerates peer configs (or updates the `AllowedIPs` line
via `wg syncconf`-style update) on a monthly cadence. CIDR drift is
real but slow.

## Verification

End-to-end checks before declaring done. Run from outside the VPS unless noted.

1. **Public HTTPS reachability**
   - `curl -sk https://cloud.<your-domain>/ | grep -qi nextcloud` → matches (Nextcloud owns the session, Authelia rule is `bypass`).
   - `curl -sI https://console.<your-domain>` → 302 to Authelia (forward-auth), or NXDOMAIN if the operator chose to keep it on the mesh only.
2. **Cloud (Nextcloud) round-trip**
   - Open `cloud.<your-domain>` in a private window → bounces to Authelia → land in Nextcloud as the same user, admin role inherited from the `admins` claim (see ADR-003).
   - Drag a file row onto a folder row → file moves.
   - Share a file → "Share link" tab → open URL in incognito → file downloads.
   - Talk → New conversation → start call in two browsers → audio + video work.
   - Upload a 100 MB file → completes (proves the 50 GB upload chain end-to-end).
3. **Gateway throughput (primary path)**
   - From a client outside heavy filtering: `iperf3 -c <vps>` over `gw0`
     → close to the VPS's advertised bandwidth (sanity check).
   - From a client behind heavy filtering (the original use case): the
     ≥50 Mbps sustained target.
4. **Regional split correctness** (only if the regional-split feature
   is enabled on a peer's `.conf` — optional)
   - Client connected via `gw0`, with `AllowedIPs` set to the
     complement of the configured CIDR set:
     - `curl -s ifconfig.me` → returns **VPS public IP** (out-of-set
       traffic exits via VPS).
     - `mtr -rwc 10 <domestic-host>` → first hop is the **local
       gateway**, not the VPS (in-set traffic stays direct).
     - `mtr -rwc 10 <foreign-host>` → first hop is the **VPS**.
5. **Alternate-ingress drill**
   - Stop `gw0` on the server. Start the `qedge` stack from `console`.
   - Switch client to the sing-box profile → all the above checks should still pass.
   - Throughput delta < 20% vs `gw0`.
6. **Backup recoverability**
   - On the backup host: `pg_dump` of `cloud-db` plus `rsync` of `/srv/store/cloud-{data,config,apps}` produces a full snapshot.
   - Restore drill: `psql -U nextcloud < dump.sql` then rsync back, then `docker compose up -d` reproduces the install on a fresh box. See `docs/backups.md` § Cloud (Nextcloud).
7. **Resource budget sanity**
   - `free -m` on the server with everything running (`ingress`, `console`, `cloud`, `gw0`, `mesh`, `qedge` stopped): used RAM ≤ 2.5 GB, leaving ~1.5 GB headroom for user-deployed apps.
   - If headroom is tight, document which container memory-limits to set in `console`.

## Security Model

### Trust boundaries

- **VPS host** — the installer assumes the host kernel and provider
  hypervisor are trusted enough to run the operator's encrypted blobs.
  This is not a hostile-host model. Operators with stronger threat
  models should run the installer on their own iron.
- **Admin SSO (Authelia)** — Authelia is the single authentication
  authority. `cloud` (Nextcloud) and `console` (Portainer) authenticate
  via the Authelia OIDC issuer; `enrol` and the Authelia portal itself
  use NPM forward-auth. ADR-003 documents the one-pattern-per-service
  rule: never mix OIDC and forward-auth on the same hostname.
- **Per-user file isolation** — enforced at the application layer:
  Nextcloud's per-user data dir under `/srv/store/cloud-data/<u>/`
  with the standard ACL/quota model. ADR-004 explains why per-user
  LUKS was removed and how to opt into Nextcloud Server-side
  Encryption (`occ app:enable encryption`) if cold-disk-from-host-image
  protection becomes important again.
- **At-rest encryption** — the VPS provider's full-disk encryption
  (Layerstack-encrypted boot volume) is the cold-disk boundary. Anyone
  with root on the running host can read every user's files; that's the
  intended trust model for a small-operator test setup.
- **Operator's laptop** — peer configs are generated client-side. Peer
  private keys live only on the laptop; the VPS holds public peer keys.

### Authelia policy

The shipped Authelia configuration applies `one_factor` (password)
to every protected route. TOTP is supported but not required by
default — operators who want to enforce it should flip the per-rule
policy to `two_factor` in `stacks/authelia/configuration.yml.template`
and re-render. Authelia's first-login TOTP enrolment flow is enabled
by default, so the operator can opt in per-user without the wizard
having to render a QR code itself.

Each OIDC-using service (`console` Portainer, `cloud` Nextcloud) gets
its own Authelia client with the secret hashed (pbkdf2-sha512) at
config render time; the plaintext secret lives at
`/etc/raph-installer/oidc-<service>-client-secret` (mode 0600) and is
rotated by enrol's finalize via the per-service `rotate*Secret` helper
in `stacks/enrol/oidc.go` (ADR-003).

### Camouflage posture

The installer ships with neutral identifiers (`gw0`, `qedge`, `mesh`,
`cloud`, `console`, `enrol`, `store`). Public-facing artefacts —
hostnames, container names, systemd units, on-disk paths — never use
the words `vpn`, `wireguard`, `tunnel`, `stealth`, or upstream project
names (Nextcloud, Portainer, AmneziaWG, etc.). See ADR-002. The
`cdn.<your-domain>` subdomain backing `qedge` is designed to look like
an ordinary CDN edge under TLS handshake inspection.

This is defense-in-depth against:

- Passive DNS observation
- Casual SNI fingerprinting on the apex
- Port banner scans
- Backup-blob filename leakage

It is **not** invisibility. Anyone with shell access on the box can
`apt list --installed`, `ip -d link show gw0`, `ss -tulpn` and
identify every component. Mesh-only access for the high-value admin
UIs (`console`, NPM admin) is what protects against the only adversary
camouflage cannot stop: a leaked admin credential.

## Risks & Open Trade-offs

- **2–4 GB RAM is tight.** Every app the operator deploys via
  `console` eats into the same pool. Set per-container memory limits
  and treat the swapfile as a safety net, not a pool. RAM-heavy stacks
  (databases, AI inference) warrant a VPS upgrade, not tuning.
- **DKMS module against newer kernels.** Mitigation: pin Ubuntu 24.04
  LTS (kernel 6.8) and accept the userspace fallback (~30% perf hit
  but still hits the 50 Mbps target).
- **`console` is a single point of compromise** for the Docker daemon.
  Keeping it off public DNS and reachable only via `mesh` (or SSH
  tunnel) is doing a lot of the heavy lifting here. Enable Portainer's
  MFA on top.
- **No cold-disk encryption beyond what the VPS provider offers.**
  Per ADR-004, per-user LUKS was removed; user data is plaintext on the
  Layerstack-encrypted host filesystem. Nextcloud Server-side
  Encryption is one `occ app:enable encryption` away if the threat
  model changes.
- **Wildcard cert via DNS-01** means a DNS provider API token lives in
  NPM's encrypted config on the server. Scope it tightly (DNS
  read/write on the single zone), rotate yearly, never share between
  installations.
- **Setup-window exposure.** The wizard binds to :80 with no auth
  before cert issuance. The window is minutes — but operators on
  shared-tenancy VPS providers should treat this as a sensitive
  interval and avoid pasting credentials over a coffee-shop network.
- **CIDR list drift** (regional split feature). Updates monthly-ish.
  If a destination moves to a new prefix outside the list, traffic
  briefly transits the VPS (correct destination, slower path, no
  breakage). Acceptable.
- **Camouflage is defense-in-depth, not invisibility.** See § Security
  Model above.
