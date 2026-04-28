# task — runbook

Project tracker for the shared VPS. Externally and in all
paths/stack-names it is `task`. Internally this is
[Vikunja](https://vikunja.io) (`vikunja/vikunja:2.3.0`) — `task` is
the camouflage name (ADR-002). Reachable at `https://task.${DOMAIN}`.
Auth is delegated to Authelia via OIDC (no local accounts).

## Why Vikunja (and not Plane)

We tried Plane v0.27.1 first. Source-code probe of the running
containers showed Plane only ships hardcoded OAuth providers —
Google / GitHub / GitLab — and has zero generic OIDC support. Plane
v1.3.0 adds Gitea but still no OIDC. With Authelia as our IdP, no
generic OIDC = no SSO. Vikunja's OIDC config (`auth.openid.providers.*`)
is exactly what we need: generic, doc'd, callback at
`/auth/openid/<provider_key>`. See ADR-009.

## Stack composition

| Service | Image | Listens on | Bind mount |
|---|---|---|---|
| `task-app` | `vikunja/vikunja:2.3.0` | `:3456` | `/srv/store/task-files` → `/app/vikunja/files` (uid 1000) |
| `task-db` | `postgres:16.6-alpine` | `:5432` (internal) | `/srv/store/task-db` → `/var/lib/postgresql/data` (uid 70) |

NPM proxies `task.${DOMAIN}` → `task-app:3456` over the `edge` network.
Authelia rule for `task.${DOMAIN}` is **bypass** — Vikunja owns its
session.

## Deploy runbook

### 1. Generate secrets

```sh
sudo install -d -m 0755 -o 1000 -g 1000 /srv/store/task-files
sudo install -d -m 0700 /srv/store/task-db
sudo chown 70:70 /srv/store/task-db

# DB password, JWT signing key (each used once at deploy time).
PG_PWD="$(openssl rand -base64 32 | tr -d '\n=+/')"
JWT_SECRET="$(openssl rand -base64 64 | tr -d '\n')"

# OIDC client secret: matched by hash inside Authelia.
OIDC_SECRET="$(cat /etc/raph-installer/oidc-task-client-secret)"

sudo tee /opt/stacks/task/.env >/dev/null <<EOF
DOMAIN=${DOMAIN}
POSTGRES_PASSWORD=$PG_PWD
VIKUNJA_JWT_SECRET=$JWT_SECRET
VIKUNJA_OIDC_CLIENT_SECRET=$OIDC_SECRET
EOF
sudo chmod 0600 /opt/stacks/task/.env
```

### 2. Bring up

```sh
cd /opt/stacks/task
docker compose up -d
docker compose logs -f task-app   # wait for "ready to handle requests"
```

### 3. First login (creates the operator's Vikunja account from OIDC)

Browse `https://task.${DOMAIN}/`. Vikunja shows a single button:
**Single Sign-On**. Click → Authelia portal → log in / 2FA → bounced
back to Vikunja, automatically logged in as your OIDC user.

The first OIDC user that logs in is **NOT** automatically an admin —
Vikunja has no built-in admin role beyond creator-of-thing. To grant
admin powers (delete-anyone-account, change site settings):

```sh
docker exec -it task-db psql -U vikunja -d vikunja \
  -c "UPDATE users SET status = 1, is_active = true WHERE username = '<your-username>';"
```

Vikunja's permission model is per-project / per-team — there is no
global admin in the Nextcloud sense. Site-level settings are
controlled by env vars in `docker-compose.yml`, not a UI.

## Backups

Two artefacts:

1. **Postgres dump** — authoritative.
   ```sh
   docker exec task-db pg_dump -U vikunja -d vikunja \
     > /srv/store/task-db-backup/task-$(date -u +%Y%m%dT%H%M%SZ).sql
   ```
2. **Files bind** — `/srv/store/task-files` (attachments + avatars).
   Single rsync target.

Restic (or whatever wraps the backups) should run the `pg_dump` first,
then take the rsync of `/srv/store/task-{files,db-backup}`. The raw
`/srv/store/task-db` directory is also captured but treated as a
safety-net only — restoring from a hot data dir is fragile.

See `docs/backups.md` for the full host-level backup pipeline.

## Upgrade procedure

Vikunja follows semver. Upgrade by image-tag bump only:

1. Snapshot Postgres: `docker exec task-db pg_dump ... > snapshot.sql`.
2. Edit `image: vikunja/vikunja:2.3.0` → newer pinned tag.
3. `docker compose pull task-app && docker compose up -d task-app`.
4. Vikunja runs DB migrations on startup; tail `docker compose logs -f
   task-app` until "ready to handle requests" appears again.
5. Roll back: `docker compose down task-app; <restore image tag>;
   restore Postgres dump if migrations were destructive`.

Stay on a pinned tag — never `latest` or `unstable`. ADR-006.

## Resource budget

Vikunja is a small Go binary. Steady state:
- `task-app` ~80 MB
- `task-db` ~120 MB

Total ~200 MB, well within the per-stack envelope. ADR-008.

## History

Originally this slot was named `plane` and ran `makeplane/plane:v0.27.1`
(then briefly `v1.3.0`). Plane lacked generic OIDC at every version we
checked, so we swapped to Vikunja and renamed the slot to `task` to
match the new product's character.

## Things deliberately NOT enabled

- **Local username/password accounts** (`VIKUNJA_AUTH_LOCAL_ENABLED=false`). Every account is OIDC, so users live in Authelia's `users_database.yml` and join Vikunja on first login.
- **Public registration**. Even with OIDC enabled, this is a defense-in-depth toggle.
- **Vikunja TOTP**. Authelia handles 2FA at the IdP layer.
- **Mailer**. No SMTP wired in; password resets / email reminders are off. (OIDC-only auth means there's no password to reset anyway.)
- **Typesense / search backend**. Vikunja falls back to Postgres full-text. Add Typesense later only if search latency bites.
