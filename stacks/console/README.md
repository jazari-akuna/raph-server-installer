# console

Docker management UI for the shared VPS. Internally this is Portainer CE;
externally and in all paths/names it is `console`. See `docs/design.md` step 4.

## Hard rules

- **Never bind to `0.0.0.0`.** Loopback only. Reach the UI via SSH tunnel
  or via the `mesh` (Tailscale) overlay once that stack is up.
- **Never publish a public proxy host for `console` in `ingress`.** This
  is the highest-value compromise target on the box.
- **Enable MFA in Portainer immediately after the first admin login.**
- **Set per-stack memory limits via Portainer.** Host has 4 GB RAM total
  and `cloud` + `ingress` + `gw0` + `mesh` already eat into it.

## First-time bootstrap (Option A — one-shot `docker run`)

This is the chicken-and-egg path used the very first time, before this
compose file has been deployed. Run on the VPS as a user in the `docker`
group:

```sh
# create the named volume and the shared edge network up front
docker volume create portainer_data
docker network create edge   # idempotent; harmless if already created

docker run -d \
  --name console \
  --restart unless-stopped \
  --memory 256m \
  --network edge \
  -p 127.0.0.1:9443:9443 \
  -p 127.0.0.1:8000:8000 \
  -v portainer_data:/data \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  portainer/portainer-ce:2.39.1
```

Then immediately tunnel in and create the initial admin account
(see *Access* below). **Portainer wipes the admin-creation form ~5
minutes after first boot for security**; if you miss the window,
`docker restart console` and reconnect within the new window.

After the admin accounts exist, tear down the one-shot container and
adopt the durable compose form:

```sh
docker rm -f console
cd /opt/stacks/console
docker compose up -d
```

The named volume `portainer_data` carries the admin accounts forward,
so you do **not** redo the bootstrap form.

## Steady state (durable form)

From `/opt/stacks/console/` on the VPS:

```sh
docker compose up -d
docker compose ps
docker compose logs -f console
```

To upgrade the pinned image, edit the tag in `docker-compose.yml`,
commit on the laptop, deploy, then:

```sh
docker compose pull && docker compose up -d
```

## Access

The UI is bound to `127.0.0.1:9443` on the VPS. Two supported paths:

### SSH tunnel (always works)

```sh
ssh -L 9443:127.0.0.1:9443 <admin>@<vps>
# then open https://localhost:9443 in a browser
```

Self-signed cert on first boot — accept the warning; the cert never
leaves the loopback path. Replace with the wildcard cert later if
desired (not required, since traffic is loopback-only).

### Mesh (preferred once `mesh` is up)

After step 10 in the build sequence, both admins are on the `mesh`
overlay. Reach the UI directly via Tailscale magic DNS:

```
https://<vps-magicdns-name>:9443
```

No SSH tunnel needed. The `mesh` interface is the only non-loopback
path that ever reaches port 9443; nothing on the public internet can
touch it.

## Initial admin setup

1. Create the admin account within ~5 minutes of first boot. (The
   installer wizard provisions a single admin; add additional
   admins later via Portainer's Users page.)
2. **Enable MFA** for the account (`Settings -> Authentication`).
   This MFA layer is INDEPENDENT of the Authelia TOTP that protects
   the public hostname; even if Authelia is bypassed, Portainer's
   own MFA still applies.
3. Connect the local Docker environment (it is auto-detected via the
   mounted socket; no extra setup needed).
4. Confirm the `edge` network is visible under
   `Environments -> local -> Networks`.

## SSO via Authelia (OIDC) — public hostname

After bootstrap, expose Portainer publicly at
`console.${DOMAIN}` behind Authelia. Two layers run
back-to-back:

- **Forward-auth at NPM**: Authelia's auth_request runs on the proxy
  host. Without an Authelia session, the user is redirected to
  `https://auth.${DOMAIN}` for username + TOTP.
- **OIDC inside Portainer**: once forward-auth lets the request through,
  Portainer's own login screen is set to OAuth-only and walks the user
  through the OIDC dance with Authelia (user already has the session,
  so it's silent).

Two layers because Portainer's own RBAC needs an `oauth_id` on each
user record — the forward-auth headers don't establish that. The
double-prompt is silent in practice (one redirect each, both reusing
the apex SSO cookie).

### Configure Portainer for OIDC

There are two paths. Use whichever matches your appetite for clicking.

#### Path A — script (after admin exists)

```sh
PORTAINER_URL=https://127.0.0.1:9443 \
PORTAINER_USER=<admin> \
PORTAINER_PASS='<your-portainer-admin-password>' \
PORTAINER_CLIENT_SECRET='<plaintext from authelia/README §2>' \
/opt/stacks/console/scripts/configure-oidc.sh
```

The script PUTs `/api/settings` with all the right URLs (see the file
for exact field values). Idempotent.

#### Path B — UI walkthrough

`Settings → Authentication → OAuth → Custom`. Fill in:

| Field | Value |
|---|---|
| Client ID | `console` |
| Client Secret | *plaintext* from `authelia/README.md` §2 |
| Authorization URL | `https://auth.${DOMAIN}/api/oidc/authorization` |
| Access Token URL  | `https://auth.${DOMAIN}/api/oidc/token` |
| Resource URL      | `https://auth.${DOMAIN}/api/oidc/userinfo` |
| Redirect URL      | `https://console.${DOMAIN}` |
| User Identifier   | `preferred_username` |
| Scopes            | `openid profile groups email` |
| Auth Style        | In Params |
| Automatic User Provisioning | enabled |
| Default Team      | (leave blank) |

Save. Browse to `https://console.${DOMAIN}` in an
incognito window. Expected flow:

1. NPM redirects to Authelia portal.
2. Log in (TOTP).
3. NPM lets the request through; Portainer detects the session and
   redirects to its own OAuth endpoint.
4. Authelia shows a one-time consent screen (first time only).
5. Portainer creates a user record for the admin (because
   `OAuthAutoCreateUsers: true`); the user lands on the dashboard.

### Group → team mapping

Currently we have one Authelia group: `admins`. Map it to a Portainer
team:

1. `Users → Teams → Add team`. Name: `admins`.
2. `Settings → Authentication → OAuth → Automatic team membership` →
   On. **Claim name**: `groups`. **Default team**: `admins` (or
   regex-map `admins` → team `admins`).
3. Manually promote each admin to Portainer-admin under `Users`
   (this is a one-time RBAC step; OAuth provisioning creates them
   as standard users by default).

## Operational notes

- The docker socket is mounted **read-only** by default. Many Portainer
  features (deploy stack, pull image, container start/stop) require
  write access. If you hit "permission denied" on those operations,
  drop the `:ro` from the socket mount — and understand that this then
  makes a Portainer compromise equivalent to host root. Keep MFA on.
- The `:8000` edge-agent tunnel is included but only used if remote
  Edge agents are adopted. Safe to leave bound to loopback. Remove the
  port line entirely if you are sure you will never use it.
- Per-stack memory limits: every stack deployed via `console` should
  declare `mem_limit` (compose v2) or the equivalent in the Portainer
  stack form. Budget against ~1.5 GB total headroom on a 4 GB host
  after `ingress` / `cloud` / `gw0` / `mesh` are running.
