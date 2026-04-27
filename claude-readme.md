# claude-readme

Entry point for any Claude session that opens this repo. Read `docs/design.md` first — it is the canonical design and the source of truth.

## What this repo is

The editable, git-tracked source of truth for a turnkey VPS installer (raph-server-installer). The VPS is a small box (2 vCPU, 4 GB RAM, 150 GB disk, public IPv4, CN2-GIA backbone, domain configured per install) used as a Docker host, reverse-proxied app platform, encrypted file server, and remote-access gateway with regional split routing for users behind heavy filtering.

## Current state

Just bootstrapped. The repo scaffold (`docs/`, `stacks/`, `host/`, `scripts/`) exists. Server-side work has not started yet.

## How to work in this repo

Edit on this laptop. Commit. Deploy to the VPS via `scripts/deploy.sh` (rsync of `stacks/` + `host/` + reload systemd). Do not edit files directly on the VPS — the server filesystem must remain reproducible from this repo.

The repo layout, build sequence, and verification steps are all in `docs/design.md`. Start at **Step 0** of the build sequence and work down. Each step is intended to be runnable and verifiable on its own.

## Load-bearing conventions a fresh agent must respect

These are decisions made in the brainstorming session that produced `docs/design.md`. They are easy to violate accidentally if you skip the plan.

- **Camouflage naming.** Public DNS, file paths, container names, systemd units, and script names never use the words `vpn`, `wireguard`, `amnezia`, `tunnel`, `stealth`, `vault`, `luks`, `crypt`, `tailscale`, or `hysteria`. Use the neutral vocabulary defined in the plan: `ingress`, `console`, `cloud`, `gw0`, `qedge`, `mesh`, `store`. This is defense-in-depth against passive observation, not against a shell-level attacker — package install names are out of scope.
- **Admin UIs are not on public DNS.** Portainer (`console`) and the NPM admin panel (`ingress` admin) are reachable only via the `mesh` overlay or SSH tunnel. Do not create public proxy hosts for them.
- **Encryption is at-rest, not E2EE.** Each user has a LUKS2 sparse file at `/srv/store/data/$user.img` mounted at `/srv/store/mnt/$user/` after manual passphrase unlock. Threat model is disk theft / hosting snapshots / backup leaks — co-admins can read each other's data while the server is running. Do not introduce Cryptomator / client-side E2EE without re-opening that decision.
- **Manual unlock on every reboot.** Passphrases are never stored on disk. `cloud` (copyparty) returns empty volumes until an admin runs `mount-stores.sh`. This is intentional — keep it that way.
- **Regional split routing is client-side.** Server is unaware. The split lives in the peer config's `AllowedIPs` line (complement of the chnroutes2 CIDR set). Refresh monthly via `update-route-tables.sh`.
- **Backups are pulled from clients, not pushed from the server.** The server holds no backup credentials. `restic` runs on the laptop on a cron / systemd-timer, pulling each `$user.img` over SSH. Do not put a backup tool on the server.
- **Single Docker daemon, shared.** Both admins are in the `docker` group. Not multi-tenant isolation. Do not introduce rootless Docker, LXC, or per-user VMs without re-opening that decision.
- **4 GB RAM is tight.** Set per-container memory limits in `console`. Treat the swapfile as a safety net, not a pool.

## What lives where

- `docs/design.md` — the design doc. Read it before doing anything.
- `docs/` — additional docs as the project grows (operational runbooks, recovery procedures, etc.).
- `stacks/` — Docker Compose stacks deployed to the VPS via `console`. One subfolder per stack.
- `host/` — files that go directly onto the VPS host filesystem (sysctl, ufw fragments, systemd units, gateway config templates).
- `scripts/` — deploy + operational scripts (`deploy.sh`, `mount-stores.sh`, `update-route-tables.sh`, `provision-peer.sh`).
- `peers/` — gitignored output of `provision-peer.sh` (per-device peer configs + QR codes).

## Out of scope

Do not propose, do not scaffold:

- Hard multi-tenant isolation (LXC / VM / rootless Docker per user).
- Client-side end-to-end encryption (Cryptomator, age, etc.).
- CDN / DNS-proxy fronting on any record (Cloudflare orange-cloud, OVH AlwaysOn, etc.) — breaks `gw0` / `qedge` ingress.
- Any admin UI on public DNS.
- A push-based backup tool running on the server.
- `qedge` (alternate ingress) running by default. It stays stopped until needed.
