# SSO design — `authelia` + `enrol` + cloud/console migration

This is the load-bearing design doc for adding Single Sign-On to the shared
VPS. Implementers (A–E) read this first; everything else flows from here.
Camouflage discipline: nothing user-visible mentions VPN/WG/Amnezia. The
installer creates a single admin via the wizard; additional admins can be
added later through enrol's user-management UI.

## Phase 0 research summary (with citations)

- **Authelia stable tag: `4.39.19`** (Docker Hub authelia/authelia, pushed
  ~2026-04-12). Pin this tag everywhere; do not use `latest`.
  <https://hub.docker.com/r/authelia/authelia/tags>
- **NPM forward-auth integration**: official Authelia recipe uses three
  Nginx snippets (`authelia-location.conf`, `authelia-authrequest.conf`,
  `proxy.conf`) mounted into NPM at `/snippets/`, then referenced from
  the proxy host's "Advanced" tab. Works with NPM 2.14.0 because
  `ngx_http_auth_request_module` ships with NGINX ≥1.9.3.
  <https://www.authelia.com/integration/proxies/nginx-proxy-manager/>
- **copyparty IdP mode**: configured by `[global]` directives
  `idp-h-usr`, `idp-h-grp`, `idp-h-key`, plus `xff-src` for trusted
  upstream subnet. In IdP mode the `[accounts]` block is replaced by
  per-user dynamic volumes using `${u}` (and `${g}`) placeholders;
  passwords are removed entirely (auth comes from the header).
  <https://github.com/9001/copyparty/blob/hovudstraum/docs/idp.md>
- **Portainer OAuth**: custom-OIDC mode requires Authorization URL,
  Access Token URL, Resource URL, Redirect URL, Client ID, Client
  Secret, User Identifier, Scopes; supports automatic team membership
  via OIDC group claim.
  <https://docs.portainer.io/admin/settings/authentication/oauth>
- **Authelia → Portainer integration** (canonical):
  - `redirect_uris: ['https://console.${DOMAIN}']`
    — bare host, no path. **This contradicts the brief's
    `/api/oauth/authorize` guess.** Adjust accordingly.
  - `scopes: openid profile groups email`
  - `token_endpoint_auth_method: client_secret_post`
  - User Identifier (Portainer side): `preferred_username`
  - Endpoints (Authelia side): `/api/oidc/authorization`, `/api/oidc/token`,
    `/api/oidc/userinfo`.
  <https://www.authelia.com/integration/openid-connect/clients/portainer/>
- **wg-easy / awg-easy**: forks exist (w0rng, gennadykataev, YokiToki,
  spcfox), upstream wg-easy v15.2.2 has experimental AmneziaWG support.
  None of them support forward-auth — they all enforce a built-in admin
  password (`PASSWORD_HASH`). Drop-in is therefore **not viable** for
  our header-trust requirement. Build `enrol` from scratch in Go.

## Decisions

### Authelia user storage

- **File-based YAML** (`users_database.yml`). 2 users only, no LDAP.
  Argon2id password hashes (Authelia's default; bcrypt is also
  available but argon2id is recommended).
- **TOTP enabled, second-factor required** — the bootstrap policy is
  `two_factor`. On first login the operator is forced through the TOTP
  enrolment flow inside Authelia's own portal (Authelia generates the
  secret and shows the QR; we do not pre-seed TOTP secrets, because
  doing that requires running Authelia first anyway).

### Storage backend

SQLite at `/var/lib/authelia/db.sqlite3` inside the container, bind-mounted
to `./data/db.sqlite3`. No Redis.

### Access control rules

```yaml
access_control:
  default_policy: 'deny'
  rules:
    # Authelia portal itself — never gate (would loop).
    - domain: 'auth.${DOMAIN}'
      policy: 'bypass'

    # Public file server: 2FA required for everyone.
    - domain: 'cloud.${DOMAIN}'
      policy: 'two_factor'

    # Peer-management UI: 2FA, admins-only group.
    - domain: 'enrol.${DOMAIN}'
      policy: 'two_factor'
      subject: ['group:admins']

    # Portainer: 2FA, admins-only.
    - domain: 'console.${DOMAIN}'
      policy: 'two_factor'
      subject: ['group:admins']
```

The bootstrap admin is a member of group `admins`. No other groups
exist at bootstrap.

### Forward-auth header conventions

Authelia emits these headers when the auth_request succeeds:

- `Remote-User`         → username (e.g. `alice`)
- `Remote-Groups`       → comma-separated groups (`admins`)
- `Remote-Email`        → email
- `Remote-Name`         → display name

All four are passed through to the upstream by the
`authelia-authrequest.conf` snippet. **enrol** and **cloud** trust
`Remote-User` + `Remote-Groups`. Implementers must use these exact
header names — not `X-Remote-*`, not `X-Forwarded-User`.

### enrol tech stack

- **Go** (single static binary; net/http stdlib + html/template).
  ~600–900 LOC total.
- Multi-stage Docker build: `golang:1.23-alpine` → `gcr.io/distroless/static`.
- Listens on `:8080` inside container; no public port; reverse-proxied
  by `ingress`.

### enrol API + UI

- `GET  /`              dashboard / peer list (HTML)
- `GET  /peers`         peer list (HTML)
- `POST /peers`         add peer (form: name, device-tag); returns config + QR
- `GET  /peers/{name}`  peer detail (HTML; shows config text, QR, audit)
- `POST /peers/{name}/delete`   remove peer
- `GET  /peers/{name}/config`   download .conf
- `GET  /peers/{name}/qr.png`   QR PNG
- `GET  /audit`         JSON audit log (last 200 entries)
- `GET  /healthz`       health probe (200 OK if gw0.conf readable)

All routes require `Remote-User` header; `/peers/*` mutating routes
additionally require `Remote-Groups` containing `admins`. Missing
headers → 401 with a one-line "go to auth portal" page (we don't
trigger redirects from inside enrol; Authelia handles the redirect at
the NPM layer).

### enrol storage model

- **Source of truth**: `/etc/amnezia/amneziawg/gw0.conf` on the host,
  bind-mounted into the container at the same path (rw, root-owned).
- **Sidecar metadata**: `/etc/amnezia/amneziawg/peers-meta.json`
  — JSON map of `{public_key: {name, device_tag, added_by, added_at}}`.
  Created on first add; missing entries fall back to "(unmanaged)".
- **Peer IP allocation**: re-implement the same algorithm as
  `scripts/provision-peer.sh` (parse [Peer] AllowedIPs in the 10.99.0.0/24
  subnet, pick lowest unused octet ≥10).
- **Audit log**: append-only `/etc/amnezia/amneziawg/peers-audit.log`,
  one JSON line per change. Last 200 served via `/audit`.

### enrol privilege model

- Compose service runs with `cap_add: [NET_ADMIN]` and bind-mounts
  `/etc/amnezia/amneziawg:/etc/amnezia/amneziawg:rw`. **No
  `network_mode: host`** — that would defeat the edge-network
  isolation.
- After mutating gw0.conf, enrol attempts to live-reload via:
  `awg syncconf gw0 <(awg-quick strip /etc/amnezia/amneziawg/gw0.conf)`
  using `nsenter --target 1 --net --mount` so the command runs in the
  host network namespace where `gw0` exists.
- If `nsenter`/`awg` unavailable in container, log a WARNING and
  return success to the user with a banner: "config saved; reload
  required: `sudo systemctl restart awg-quick@gw0` on host".
- enrol's container ships `awg`/`awg-quick` binaries (alpine
  `amneziawg-tools` not in upstream repos; build a tiny stage that
  copies them from a Debian container, or use util-linux `nsenter`
  to call the host's binaries via PID 1 namespace). Simplest:
  install `wireguard-tools` in the runtime stage and let `nsenter`
  reach the host's `awg` binary.

### copyparty IdP migration

- Drop `[accounts]` block entirely.
- Drop `SAGAN_PW_HASH` / `MARCUS_PW_HASH` from compose env and
  `.env.example`.
- Add to `[global]`:
  ```
  idp-h-usr: Remote-User
  idp-h-grp: Remote-Groups
  xff-src: 172.16.0.0/12   # docker default networks live here
  ```
  (We do NOT use `idp-h-key` — defence in depth would require sharing
  a secret between NPM and copyparty. The `xff-src` constraint plus
  the fact that NPM is the only thing on `edge` → cloud:3923 path is
  sufficient. Document this trade-off in the README.)
- Replace per-user static volume blocks with one templated block:
  ```
  [/u/${u}]
    /w/${u}
    accs:
      rwmda: ${u}
  ```
  Each user only sees their own URL path, mounted from a per-user host
  bind-mount. The bind-mount list in compose maps `/srv/store/mnt/<u>`
  onto `/w/<u>`, and copyparty maps URL `/u/<u>/` onto `/w/<u>` only
  when the request authenticates as `<u>`.
- Admin (group:admins) override: any volume the user owns gets `rwmda`.
  No global admin volume — group-membership only matters for enrol/console.

### Portainer OIDC

- Authelia client config:
  ```yaml
  - client_id: 'console'
    client_name: 'Console'
    client_secret: '<pbkdf2-sha512 hash>'   # generated at deploy
    public: false
    authorization_policy: 'two_factor'
    require_pkce: false
    redirect_uris: ['https://console.${DOMAIN}']
    scopes: ['openid', 'profile', 'groups', 'email']
    token_endpoint_auth_method: 'client_secret_post'
  ```
- Portainer side:
  - Authorization URL: `https://auth.${DOMAIN}/api/oidc/authorization`
  - Access Token URL: `https://auth.${DOMAIN}/api/oidc/token`
  - Resource URL: `https://auth.${DOMAIN}/api/oidc/userinfo`
  - Redirect URL: `https://console.${DOMAIN}`
  - Client ID: `console`
  - User Identifier: `preferred_username`
  - Scopes: `openid profile groups email`
  - Auth Style: `In Params`
  - Default Team: leave blank
- Portainer's admin must already exist (manual bootstrap) before
  enabling OIDC, otherwise OIDC users have no team mapping. Admin
  enrolment is a manual operator step we can't automate from this
  project.

### NPM proxy hosts

Four hosts. `auth.` is bypass; the others get forward-auth Advanced
config that calls `auth-request` against `http://authelia:9091/api/verify`.

| Hostname                                | Forward         | Forward-auth | Notes |
|-----------------------------------------|-----------------|--------------|-------|
| auth.${DOMAIN}         | authelia:9091   | NO (loop)    | bypass policy |
| enrol.${DOMAIN}        | enrol:8080      | YES          | admins group |
| cloud.${DOMAIN}        | cloud:3923      | YES          | UPDATE existing host id 1 |
| console.${DOMAIN}      | console:9443    | YES          | scheme=https + skip cert verify |

NPM container must be on the `edge` network (already is). Authelia
joins `edge`. enrol joins `edge`.

### Container/network names (final, do not change)

- `authelia` — service + container name
- `enrol`    — service + container name
- `auth.`    — public hostname for Authelia portal
- `enrol.`   — public hostname for peer manager
- All other names unchanged from existing stacks.

### Secret inventory (Authelia)

Generated at deploy time, mode 0600, never committed:

1. `AUTHELIA_JWT_SECRET`            — 64-byte random string (forgot-pw etc.)
2. `AUTHELIA_SESSION_SECRET`        — 64-byte random string (cookie sealer)
3. `AUTHELIA_STORAGE_ENCRYPTION_KEY` — 64-byte random string (sqlite blob enc)
4. `AUTHELIA_IDENTITY_PROVIDERS_OIDC_HMAC_SECRET` — 64-byte random string
5. `AUTHELIA_IDENTITY_PROVIDERS_OIDC_ISSUER_PRIVATE_KEY` — RSA-2048 PEM PKCS#8
6. `PORTAINER_OIDC_CLIENT_SECRET`   — random alphanumeric, hashed with
                                      `authelia crypto hash generate pbkdf2`
                                      for the configuration.yml; plaintext
                                      goes into Portainer's UI.

The OIDC issuer key is a multi-line PEM. We pass it through the env via
`AUTHELIA_IDENTITY_PROVIDERS_OIDC_JWKS_0_KEY_FILE=/run/secrets/oidc-key.pem`
(file in compose secrets) rather than escaping the newlines into .env.
Bind-mount `./secrets/oidc-key.pem` (chmod 0600) into the container.

### Bootstrap user passwords

Both `changeme`. Argon2id hashed. Operator rotates via Authelia portal
post-deploy (no Authelia CLI subcommand for this — they edit
`users_database.yml` and `docker compose restart authelia`, or change
their own password via the portal once configured for it).

## Implementer mapping (file ownership)

- **A — Authelia stack**: `stacks/authelia/{docker-compose.yml,
  configuration.yml, users_database.yml.example, .env.example,
  README.md, secrets/.gitkeep, data/.gitkeep, scripts/wire-npm-routes.sh
  (placeholder, E will fill)}`
- **B — enrol app**: `stacks/enrol/{Dockerfile, docker-compose.yml,
  .env.example, README.md, main.go, go.mod, web/templates/*.html,
  web/static/style.css}`
- **C — cloud migration**: edits to `stacks/cloud/{conf/copyparty.conf,
  docker-compose.yml, .env.example (new), README.md}`
- **D — Portainer OIDC**: edits to `stacks/console/README.md` only;
  optionally `stacks/console/scripts/configure-oidc.sh`
- **E — NPM wire-up**: `stacks/authelia/snippets/{authelia-location.conf,
  authelia-authrequest.conf, proxy.conf}` and the populated
  `stacks/authelia/scripts/wire-npm-routes.sh`. Cross-cuts: ingress
  compose only gets a `./snippets:/snippets:ro` bind-mount added if
  necessary; document that delta but don't commit it from E
  (manager handles inter-stack edits).
