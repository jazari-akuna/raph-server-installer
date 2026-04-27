# raph-server-installer

A turnkey installer that recreates a self-hosted private-cloud stack on
a fresh Ubuntu 24.04 VPS. One pasted command into the cloud-init / user-
data box of any major VPS provider, and the server brings itself up.
Final configuration happens through a web wizard at
`https://setup.<your-domain>/` — no SSH required.

The stack delivers:

- **SSO with optional TOTP** (Authelia) — single login for every service.
- **Encrypted file storage** (copyparty + LUKS2) — per-user encrypted
  volumes, plus a shared encrypted volume for collaboration.
- **AmneziaWG gateway** (`gw0`) — anti-DPI WireGuard variant for remote
  access, with optional client-side regional split routing.
- **Reverse proxy with auto-renewing wildcard TLS** (Nginx Proxy
  Manager) — DNS-01 ACME against your DNS provider's API.
- **Docker management UI** (Portainer) — gated by SSO, not on public DNS.
- **Setup-and-admin web app** (`enrol`, custom Go service) — runs the
  install wizard and then becomes the steady-state user/peer/volume
  admin UI.

Operational discretion ("camouflage") is built in: hostnames, paths,
container names, and systemd units never use the words `vpn`,
`wireguard`, `vault`, `luks`, etc. The public-facing footprint is
designed to look like an ordinary application server under passive
observation.

---

## Status

Pre-1.0. The repo is being rebuilt from a hand-deployed reference
installation into a publishable installer. Bootstrap automation, setup
wizard, and CI are landing in waves; `docs/design.md` is the canonical
description of the target system.

---

## Requirements

- A VPS running **Ubuntu 24.04 LTS** with at least **2 GB RAM** and
  **40 GB disk**. 4 GB / 100 GB is more comfortable for production.
- A **public IPv4** address.
- A **registered domain** whose DNS the operator controls. The
  installer needs a wildcard A record pointing at the VPS:
  - `*.<your-domain>` → VPS public IPv4
  - (optional) `<your-domain>` → VPS public IPv4 for the apex
- A **DNS provider that supports DNS-01 ACME** through one of the
  shipped certbot plugins (see [Supported DNS providers](#supported-dns-providers)
  below).
- About **15 minutes** for the full bootstrap + wizard run on a 2 vCPU box.

That's it. Operators do not need DevOps experience; the wizard handles
every config decision interactively and explains what each step does.

---

## Quick install

1. **Set up DNS** at your registrar / DNS provider:
   - Wildcard A record: `*.<your-domain>` → VPS IPv4
   - (optional) Apex A record: `<your-domain>` → VPS IPv4
   - Generate an API token / credential for your DNS provider, scoped
     to read+write on the single zone. You will paste this into the
     setup wizard, not into the bootstrap command.

2. **Provision an Ubuntu 24.04 VPS** at any provider that supports
   cloud-init / user-data (LayerStack, Hetzner, Vultr, DigitalOcean,
   Linode, etc.).

3. **Paste this into the user-data / first-boot script box**, replacing
   the two values at the top:

   ```bash
   #!/usr/bin/env bash
   export DOMAIN="example.com"
   export ADMIN_EMAIL="admin@example.com"
   curl -fsSL https://raw.githubusercontent.com/jazari-akuna/raph-server-installer/main/bootstrap.sh | bash
   ```

   Boot the VPS. The bootstrap runs unattended — typically 5–10 minutes
   on a 2 vCPU host (longer if a kernel reboot is needed for the
   AmneziaWG DKMS module).

4. **Open the wizard** at `https://setup.<your-domain>/` (or
   `https://<vps-ip>/` with the `Host: setup.<your-domain>` header if DNS
   is still propagating). The wizard ships behind a self-signed cert
   until you finish the finalize step, so your browser will show a
   "not secure" warning on first visit — click "Advanced → Proceed".
   The wizard walks through six steps:

   1. Welcome / DNS propagation check
   2. First admin (username, password, email)
   3. DNS provider credentials (cert issuance)
   4. Wildcard certificate issuance + reverse-proxy wireup
   5. Encrypted storage (admin's per-user volume + shared volume)
   6. Done — redirect to login

   At completion the server is fully configured. The wizard closes
   itself (the `setup.<your-domain>` host is removed and any residual
   `/setup` request returns 404) and the operator is dropped into the
   Authelia login portal.

---

## Supported DNS providers

The installer hands off cert issuance to Nginx Proxy Manager's bundled
certbot. The wizard collects the provider-specific credential at step 3
and writes it where certbot expects it. As of this revision the
following plugins are wired:

| Provider | Plugin | Credential format |
|---|---|---|
| Cloudflare | `certbot-dns-cloudflare` | API token (zone-scoped) |
| OVH | `certbot-dns-ovh` | application_key / application_secret / consumer_key |
| AWS Route 53 | `certbot-dns-route53` | access key + secret |
| DigitalOcean | `certbot-dns-digitalocean` | personal access token |
| Google Cloud DNS | `certbot-dns-google` | service account JSON |
| Linode | `certbot-dns-linode` | personal access token |
| RFC2136 (BIND, PowerDNS, etc.) | `certbot-dns-rfc2136` | TSIG key |

Adding another provider is a contribution-sized change: see
[Adding a DNS provider](#adding-a-dns-provider) below.

---

## What gets deployed

| Internal name | What it actually is | Public URL | Auth |
|---|---|---|---|
| `ingress` | Nginx Proxy Manager | (admin: loopback only) | NPM admin user |
| `auth` | Authelia | `https://auth.<your-domain>/` | password / TOTP |
| `cloud` | copyparty file server | `https://cloud.<your-domain>/` | SSO via Authelia |
| `console` | Portainer (Docker management UI) | `https://console.<your-domain>/` | OIDC via Authelia |
| `enrol` | Setup wizard + admin UI (Go) | `https://enrol.<your-domain>/` | SSO via Authelia |
| `gw0` | AmneziaWG gateway (UDP :51820) | `gw.<your-domain>` | per-peer keys |
| `qedge` | Hysteria 2 alternate ingress | `cdn.<your-domain>` (UDP :443) | per-client password |
| `mesh` | Tailscale (optional, manual) | private overlay | tailnet ACL |

The split between "what it actually is" and the neutral name is the
camouflage layer. Operators who do not care about that can grep the repo
for the upstream project names and rebrand freely; nothing in the stack
depends on the names being neutral.

---

## Security model

**Trust boundary.** The installer assumes the VPS host kernel and the
hosting provider hypervisor are in the trust boundary. This is **not**
a hostile-host model. Operators with stronger threat models should run
the installer on their own iron.

**SSO is the trust authority.** Authelia is the single source of
identity. `cloud` (copyparty) trusts an `Remote-User` header injected
by NPM's forward-auth snippet; `console` (Portainer) trusts Authelia's
OIDC issuer. The default Authelia policy is `one_factor` (password) on
every protected route — operators who want enforced TOTP flip
the per-rule policy to `two_factor` in
`stacks/authelia/configuration.yml.template` and re-render. Authelia's
first-login TOTP enrolment flow is enabled by default, so users can
opt in per-account without the wizard rendering QR codes itself.

**Encryption is at-rest, not end-to-end.** Each user gets a LUKS2
sparse blob at `/srv/store/data/<user>.img` (default size: chosen by
the operator at user-create time). The user controls the passphrase
and types it at every reboot — passphrases are never stored on disk
and the installer does not see them after the wizard step. A
**shared volume** at `/srv/store/data/_shared.img` mounts unattended
via a root-owned keyfile; this is a deliberate trade-off that lets
collaboration paths inside `cloud` survive a reboot at the cost of
"anyone with root on the host can read /shared at rest."

**Admin UIs are not on public DNS.** Portainer's admin UI and NPM's own
admin panel bind to loopback. Operators reach them via SSH tunnel from
a laptop or via a manual Tailscale install (see
`docs/maintenance.md` § Tailscale).

**Camouflage posture.** Hostnames, container names, paths, and systemd
units use neutral identifiers. Public-facing artefacts never reveal the
underlying upstream project names. This is defense-in-depth against
passive DNS observation, casual SNI fingerprinting, port banner scans,
and backup-blob filename leakage. It is **not** invisibility — anyone
with shell access on the box can list installed packages and identify
every component. See `docs/design.md` § Security Model for the full
explanation.

**What is explicitly NOT in scope:**

- Hard multi-tenant isolation (LXC / VM / rootless Docker per user).
- Client-side end-to-end encryption (Cryptomator, age, etc.).
- CDN / DNS-proxy fronting on any record (Cloudflare orange-cloud, OVH
  AlwaysOn) — this breaks `gw0` / `qedge` ingress and adds an unwanted
  MITM on the SSO flow.
- A push-based backup tool running on the server. Backups are pulled
  from the operator's laptop (restic over SSH); the VPS holds no
  backup credentials.
- Hosting an operator's website. The apex is owned by the wizard /
  Authelia for setup-time and SSO routing.

---

## Documentation

- [`docs/design.md`](docs/design.md) — canonical architecture document.
  Component decisions, build sequence, DNS layout, filesystem layout,
  security model, risks. Read this first if you intend to modify the
  installer.
- [`docs/maintenance.md`](docs/maintenance.md) — day-2 operations
  runbook. Cadence at a glance, image bumps, cert renewal, kernel
  updates, LUKS rotation, user lifecycle CLI fallbacks, triage table.
- [`docs/verification.md`](docs/verification.md) — seven-check sign-off
  procedure. Run before declaring an install "in service."
- [`docs/backups.md`](docs/backups.md) — restic recovery procedures.
  Pull-from-client backup model.
- [`docs/dns-records.md`](docs/dns-records.md) — required DNS layout +
  per-subdomain notes.
- [`docs/perf-tuning.md`](docs/perf-tuning.md) — `gw0` AmneziaWG
  throughput tuning and DKMS troubleshooting.
- Per-stack READMEs under `stacks/<name>/README.md` cover stack-local
  details (NPM cert configuration, Authelia secret rotation, copyparty
  ACL syntax, `qedge` switchover procedure).

---

## Adding a DNS provider

Each shipped DNS provider is wired through three touchpoints:

1. The wizard's step 3 form — `stacks/enrol/web/templates/setup-dns.html`
   — adds an option to the dropdown and the relevant credential fields.
2. The wizard's submit handler — `stacks/enrol/setup.go` — writes the
   credential file in the format `certbot-dns-<provider>` expects, into
   NPM's `data/letsencrypt/` mount.
3. NPM's bundled certbot plugins — `stacks/ingress/docker-compose.yml`
   already builds in `certbot-dns-cloudflare`, `certbot-dns-ovh`,
   `certbot-dns-route53`, `certbot-dns-digitalocean`, `certbot-dns-google`,
   `certbot-dns-linode`, and `certbot-dns-rfc2136`. Adding a new
   provider means rebuilding the NPM image with the new plugin and
   bumping the pinned tag in the compose file.

A worked example for one of the shipped providers is the cleanest place
to start. PRs welcome.

---

## Contributing

Issues and PRs are welcome at
<https://github.com/jazari-akuna/raph-server-installer>.

Conventions:

- **Domain placeholders.** In prose, use angle-bracket placeholders:
  `<your-domain>`, `<admin>`, `<user>`. In configs and templates, use
  shell-style env vars: `${DOMAIN}`, `${ADMIN_USERS}`, etc.
- **No personal data.** This is a public installer; do not commit
  personal email addresses, hostnames, IPs, or peer configs. The
  `.gitignore` lists every output the runtime generates.
- **Camouflage naming.** Public-facing artefacts (DNS labels, container
  names, systemd units, paths, scripts) never use the words `vpn`,
  `wireguard`, `amnezia`, `tunnel`, `stealth`, `vault`, `luks`,
  `crypt`, `tailscale`, or `hysteria`. The neutral vocabulary is in
  `docs/design.md` § Naming convention.
- **Small, reviewable PRs.** Prefer single-purpose changes. Touching
  the bootstrap script or the wizard state machine should come with a
  smoke-test update under `tests/`.
- **CI must pass.** Shellcheck on every script, `go vet ./...` and
  `go build ./...` on the `enrol` service, `docker compose config` on
  every stack, render-templates dry-run against `.env.example`. See
  `.github/workflows/ci.yml`.

For non-trivial changes, open an issue first to discuss the approach.

---

## License

[MIT](LICENSE). Copyright 2026 raph-server-installer contributors.
