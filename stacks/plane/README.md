# plane

Project tracker for the shared VPS — issues, projects, workspaces,
attachments. Internally this is [Plane](https://github.com/makeplane/plane)
v0.27.1; externally and in all paths/names it is `plane`. Reachable
only via `ingress` at `plane.${DOMAIN}`. There are no public port
bindings on this stack.

This replaces Jira / Linear in the operator's workflow. Like the
`cloud` stack, auth is delegated to Authelia via OIDC (Plane owns its
own session); like all other stacks, persistent state lives on
`/srv/store/plane-*` bind-mounts so backup is one rsync + one pg_dump.

> **Release pinning.** This file was authored against Plane **v0.27.1**
> (released 2025-07-04). The latest stable release at any moment can
> be checked at <https://github.com/makeplane/plane/releases>; if you
> bump the tag here, also check the upstream `deployments/cli/community/
> docker-compose.yml` for service-composition drift, and snapshot the
> DB + bind-mounts before pulling. See "Upgrading" below.

## What this is

Fourteen containers on the existing `edge` Docker network — 13 Plane
services + 1 (`plane-mc`) that we add to seed the MinIO bucket on
first up:

| Service          | Image                                          | Role                                                    |
| ---              | ---                                            | ---                                                     |
| `plane-proxy`    | `makeplane/plane-proxy:v0.27.1`                | Caddy fan-out (web/admin/space/live/api) — what NPM hits|
| `plane-web`      | `makeplane/plane-frontend:v0.27.1`             | Next.js end-user UI (workspaces, projects, issues)      |
| `plane-admin`    | `makeplane/plane-admin:v0.27.1`                | Next.js god-mode panel (instance admin, OIDC config)    |
| `plane-space`    | `makeplane/plane-space:v0.27.1`                | Next.js public-share UI (read-only project links)       |
| `plane-live`     | `makeplane/plane-live:v0.27.1`                 | WebSocket / live-collaboration server                   |
| `plane-api`      | `makeplane/plane-backend:v0.27.1`              | Django REST API + gunicorn (the big one, ~300 MB RSS)   |
| `plane-worker`   | `makeplane/plane-backend:v0.27.1`              | Celery default queue (concurrency 1)                    |
| `plane-beat`     | `makeplane/plane-backend:v0.27.1`              | Celery beat scheduler (periodic tasks)                  |
| `plane-migrator` | `makeplane/plane-backend:v0.27.1`              | One-shot Django schema migrator (`restart: no`)         |
| `plane-db`       | `postgres:15.7-alpine`                         | Postgres 15 (Plane requires PG 14-16)                   |
| `plane-redis`    | `valkey/valkey:7.2.11-alpine`                  | Cache + Celery result backend (broker is RabbitMQ)      |
| `plane-mq`       | `rabbitmq:3.13.6-management-alpine`            | Celery broker (durable task queue)                      |
| `plane-minio`    | `minio/minio:RELEASE.2024-12-18T13-15-44Z`     | S3-compatible attachment store                          |
| `plane-mc`       | `minio/mc:RELEASE.2024-12-18T13-15-44Z`        | One-shot bucket-init job (ours, NOT upstream)           |

NPM terminates TLS upstream and proxies `plane.${DOMAIN}` →
`plane-proxy:80`, which fans out internally. Nothing is published to
the host (we strip upstream's `ports: 80/443` block — NPM is the
edge).

## Auth — Authelia OIDC (manual one-time bootstrap)

Auth is delegated to Authelia using **OIDC**. NPM does not inject
`Remote-User` or `X-Forward-Auth-Secret` for this stack; Plane owns
its own session.

Plane has **no env-var path** to bootstrap OIDC — by deliberate
upstream design, the operator visits `/god-mode/` first and pastes
the OIDC config into the UI. See ADR-009 in
`/opt/raph-server-installer/docs/architecture-decisions.md` for the
rationale.

### First-time setup (manual, after `docker compose up -d`)

1. **Visit `https://plane.${DOMAIN}/god-mode/` in a browser.**
   Sign up with the operator's email + a temporary password. **The
   first user to sign up at this URL becomes instance admin** —
   first-user-wins. Do this **immediately** after the first compose-up;
   see "First-user-wins footgun" below.

2. **Paste OIDC config** at
   `https://plane.${DOMAIN}/god-mode/authentication/oidc/`:

   | Field                | Value                                               |
   | ---                  | ---                                                 |
   | IdP Display Name     | `Authelia`                                          |
   | Client ID            | `plane`                                             |
   | Client Secret        | contents of `/etc/raph-installer/oidc-plane-client-secret` (mode 0600 root) |
   | Authorize URL        | `https://auth.${DOMAIN}/api/oidc/authorization`     |
   | Token URL            | `https://auth.${DOMAIN}/api/oidc/token`             |
   | User Info URL        | `https://auth.${DOMAIN}/api/oidc/userinfo`          |
   | Auto user signup     | **ON** (so first-time Authelia logins auto-provision a Plane account) |

   Save. The server-side check happens immediately; if any field is
   wrong (especially Client Secret or one of the URLs) the save itself
   fails with an error toast.

3. **Verify in a private window.** `https://plane.${DOMAIN}/` →
   "Sign in with Authelia" → bounces to Authelia → log in → land in
   Plane as the same Authelia user.

4. **Generate the admin API token** for enrol's admin page (Wave B):
   `/god-mode/workspace/api-tokens/` → "Create personal access token"
   for a service-account user (NOT the operator's own account — the
   token grants full Plane API access). Write the token to
   `/etc/raph-installer/plane-admin-token` (mode 0600 root).

### First-user-wins footgun — read this

If `plane.${DOMAIN}` is reachable publicly **before** the operator
claims `/god-mode/`, an attacker who finds the URL gets instance
admin instead. Concretely: any random visitor to `/god-mode/` between
"first compose-up" and "operator finishes signup" wins.

Mitigation, in order of paranoia:

- **Default mitigation (do this every time).** Complete steps 1-3
  above **in the same deploy session** as the first
  `docker compose up`. The window of exposure is then ~minutes, not
  hours.

- **Belt-and-braces (do this if the operator might not be at the
  keyboard within minutes).** Temporarily gate `plane.${DOMAIN}` at
  the Authelia layer with `policy: one_factor` instead of `bypass` for
  the first 24 h, then flip to `bypass` after god-mode is claimed and
  OIDC is configured. Edit
  `stacks/authelia/configuration.yml.template`, find the
  `plane.${DOMAIN}` access rule, change `policy: bypass` →
  `policy: one_factor`, re-render templates, restart Authelia. After
  step 3 verifies, change it back. (This breaks the OIDC dance during
  the gated window — that's intentional; the operator is the only one
  who should be there.)

### No-OIDC fallback

If Authelia is down, the bootstrap admin can still sign in at
`/god-mode/instance-admin/sign-in` with the email + password set in
step 1. This is Plane's documented escape hatch — DO NOT delete the
bootstrap admin account.

## Where data lives

All persistent state is on **external** bind mounts under
`/srv/store/plane-*` on the host (NOT docker named volumes), so backup
is a single rsync of the parent directory plus a `pg_dump` snapshot,
per ADR-001.

| Host path                | Container mount                  | Service(s)                                    | uid:gid     |
| ---                      | ---                              | ---                                           | ---         |
| `/srv/store/plane-db`    | `/var/lib/postgresql/data`       | `plane-db`                                    | 70:70       |
| `/srv/store/plane-minio` | `/export`                        | `plane-minio` and `plane-mc`                  | 1000:1000   |
| `/srv/store/plane-mq`    | `/var/lib/rabbitmq`              | `plane-mq`                                    | 100:101     |
| `/srv/store/plane-logs`  | `/code/plane/logs`               | `plane-api` `plane-worker` `plane-beat` `plane-migrator` | 1001:1001 |

`bootstrap-phase2.sh` creates these directories with the correct
ownership before this stack comes up. **Verify the minio uid/gid on
the actual image before chowning** — `docker run --rm
minio/minio:RELEASE.2024-12-18T13-15-44Z id` to check; the default is
1000:1000 but minio has occasionally bumped it across releases.

`plane-redis` (Valkey) state is **intentionally not persisted** — it's
cache + Celery result backend, both safe to lose on restart. The
durable task queue lives on `plane-mq` (RabbitMQ).

### Backup recipe (laptop-side, over SSH)

Mirror of the cloud stack's recipe, with three rsync targets instead
of three:

```sh
ssh vps "docker exec plane-db pg_dump -U plane plane" \
  > snapshots/$(date +%F)-plane-db.sql
rsync -aAXH --delete vps:/srv/store/plane-minio/ snapshots/plane-minio/
rsync -aAXH --delete vps:/srv/store/plane-mq/    snapshots/plane-mq/
rsync -aAXH --delete vps:/srv/store/plane-logs/  snapshots/plane-logs/
```

Restore is the reverse, plus `psql -U plane < snapshot.sql`. Order:

```sh
# On the new VPS, with the plane stack down:
rsync -aAXH --delete snapshots/plane-minio/ vps:/srv/store/plane-minio/
rsync -aAXH --delete snapshots/plane-mq/    vps:/srv/store/plane-mq/
ssh vps "docker compose -f /opt/stacks/plane/docker-compose.yml up -d plane-db"
ssh vps "docker exec -i plane-db psql -U plane -d plane" < snapshots/<date>-plane-db.sql
ssh vps "docker compose -f /opt/stacks/plane/docker-compose.yml up -d"
```

The RabbitMQ rsync is technically optional — Plane will rebuild empty
queues on boot and Celery's idempotent task design tolerates losing
in-flight jobs. Including it preserves any in-flight Celery jobs
across restore, which matters for long-running attachment uploads
that haven't finished post-processing.

## Resource budget — the constraint that drives everything

Target per ADR-008: ≤ 2.0 GB total RSS across all containers under
steady load on a 2 GB VPS, ≤ 2.2 GB under burst, with 4 GB swap to
absorb spikes.

This stack is the biggest single contributor on the box. Tuned
budget:

| Service        | mem_limit | Typical RSS | Knob                                                      |
| ---            | ---       | ---         | ---                                                       |
| `plane-api`    | 512m      | ~300 MB     | `GUNICORN_WORKERS=1`                                      |
| `plane-worker` | 384m      | ~200 MB     | `CELERY_WORKER_CONCURRENCY=1`                             |
| `plane-beat`   | 192m      | ~120 MB     | (no knob — beat scheduler RSS is what it is)              |
| `plane-web`    | 192m      | ~120 MB     | (Next.js prod build; not tunable)                         |
| `plane-admin`  | 192m      | ~120 MB     | "                                                         |
| `plane-space`  | 192m      | ~120 MB     | "                                                         |
| `plane-live`   | 192m      | ~120 MB     | "                                                         |
| `plane-db`     | 256m      | ~80 MB      | `shared_buffers=64MB max_connections=50` via `command:`   |
| `plane-mq`     | 192m      | ~120 MB     | `RABBITMQ_VM_MEMORY_HIGH_WATERMARK=0.4`                   |
| `plane-minio`  | 192m      | ~80 MB      | (default is fine on a single-bucket workload)             |
| `plane-redis`  | 96m       | ~30 MB      | `--maxmemory 64mb --maxmemory-policy allkeys-lru`         |
| `plane-proxy`  | 64m       | ~30 MB      | (Caddy at idle; not tunable)                              |
| `plane-mc`     | 128m      | (one-shot)  | (exits within seconds of plane-minio becoming healthy)    |
| `plane-migrator` | 128m    | (one-shot)  | (exits within seconds of migrations completing)           |
| **Total tuned** | -        | **~1.45 GB**| Steady state under light load                             |

Combined with the cloud stack (~440 MB) + system baseline (~320 MB)
that's **~2.21 GB / 2 GB** — over by ~200 MB. The 4 GB swap
configured by the bootstrap absorbs the overage; on a Layerstack VPS
swap-on-disk is fine for the brief peaks (issue-creation,
attachment-upload).

### Documented fallback if Plane OOMs in steady state

Drop the `plane-mq` service entirely and route Celery through
`plane-redis`. Saves ~120 MB at the cost of broker durability for
long workflows (long uploads, multi-step webhook fan-out can be lost
on a redis restart). To switch:

1. Comment out the `plane-mq` service block in `docker-compose.yml`.
2. Remove the `plane-mq` healthy depends_on entries from `plane-api`,
   `plane-worker`, `plane-beat`, `plane-migrator`.
3. Set `CELERY_BROKER_URL=redis://plane-redis:6379/1` in `.env` (note
   `/1` = a different DB number than the result-backend, which uses
   `/0`).
4. Override `AMQP_URL` to the same Redis URL so the api code path that
   peeks broker state doesn't try to dial RabbitMQ:
   `AMQP_URL=redis://plane-redis:6379/1`.
5. `docker compose up -d` — workers will reconnect to redis.

Reverse the steps to switch back.

## Maintenance

### Required env vars

The stack will refuse to start without these set in
`/opt/stacks/plane/.env` (each has a `:?` validator in
`docker-compose.yml`):

- `POSTGRES_PASSWORD` — random, generated by bootstrap-phase2.sh
- `RABBITMQ_DEFAULT_PASS` — random, generated by bootstrap-phase2.sh
- `AWS_ACCESS_KEY_ID` — defaults to `plane`; override only if sharing
- `AWS_SECRET_ACCESS_KEY` — random, generated by bootstrap-phase2.sh
- `SECRET_KEY` — random Django secret, generated by bootstrap-phase2.sh
- `LIVE_SERVER_SECRET_KEY` — random, generated by bootstrap-phase2.sh

Optional with sensible defaults: `APP_DOMAIN`, `WEB_URL`,
`CORS_ALLOWED_ORIGINS`, `POSTGRES_USER`, `POSTGRES_DB`,
`RABBITMQ_DEFAULT_USER`, `RABBITMQ_DEFAULT_VHOST`, `AWS_S3_BUCKET_NAME`,
`AWS_REGION`, `API_KEY_RATE_LIMIT`, `CELERY_WORKER_CONCURRENCY`,
`GUNICORN_WORKERS`, `FILE_SIZE_LIMIT`. See `.env.example`.

### Plane admin API token

The `/etc/raph-installer/plane-admin-token` file (mode 0600 root)
holds a personal access token generated in god-mode for a dedicated
service-account Plane user. enrol reads it on startup and uses it to
query Plane usage stats for the admin page (workspaces, projects,
issue counts per user, attachment bytes per user).

To generate (one-time, after first OIDC-login is working):

1. In the Plane web UI, create a service-account workspace member
   (e.g. `enrol-bot@${DOMAIN}`) — admin role on every workspace whose
   stats you want surfaced in the admin page.
2. Sign into Plane as that user.
3. Workspace settings → "API tokens" → "Add API token".
4. Copy the token (shown ONCE; Plane does not store it server-side
   in retrievable form).
5. On the VPS:
   ```sh
   echo -n '<TOKEN>' > /etc/raph-installer/plane-admin-token
   chmod 0600     /etc/raph-installer/plane-admin-token
   chown root:root /etc/raph-installer/plane-admin-token
   ```
6. `docker compose restart enrol` so it picks up the new token.

To rotate: same procedure, then revoke the old token in the same UI.

### Upgrading Plane

Pinned to a specific tag per ADR-006 (NEVER `latest` / `stable`).
Upstream Plane has historically shipped breaking changes under
`stable`, including env-var renames and god-mode flow changes
(v0.27 → v0.28 was one such break) — the pinned tag turns surprise
3am pages into planned upgrade windows.

Quarterly cadence (see `docs/maintenance.md`):

```sh
# 1. Snapshot first — non-negotiable.
ssh vps "docker exec plane-db pg_dump -U plane plane" > snapshots/pre-upgrade-plane-db.sql
rsync -aAXH --delete vps:/srv/store/plane-minio/ snapshots/pre-upgrade-plane-minio/

# 2. Check the changelog at https://github.com/makeplane/plane/releases
#    for the new tag, especially BREAKING-CHANGE markers.

# 3. Edit stacks/plane/docker-compose.yml — bump every makeplane/plane-*
#    image from v0.27.1 to the new tag. Keep them in lockstep — Plane
#    requires the frontends, api, and proxy be on the same release.
#    Re-check upstream's deployments/cli/community/docker-compose.yml
#    for service-composition drift (new services added, env-var
#    renames).

# 4. Pull + recreate.
ssh vps "cd /opt/stacks/plane && docker compose pull && docker compose up -d"

# 5. plane-migrator runs automatically (depends_on
#    service_completed_successfully gates api/worker/beat on it).
#    Watch its logs:
ssh vps "docker logs -f plane-migrator"

# 6. Smoke: open plane.${DOMAIN}, verify SSO + create-issue +
#    upload-attachment all work. Run the enrol admin page and confirm
#    the Plane usage columns still populate.
```

If the migrator fails or Plane refuses to come up clean, the rollback
is: revert the `docker-compose.yml` tag bump, restore the pre-upgrade
DB dump (`psql < snapshots/pre-upgrade-plane-db.sql`), restore the
minio bind-mount, `docker compose up -d`. The bind-mount-based backup
model makes this fast — no `docker volume` recovery dance.

### MinIO bucket integrity check

```sh
docker exec plane-mc mc alias set local http://plane-minio:9000 \
  "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY"
docker exec plane-mc mc ls local/uploads
docker exec plane-mc mc admin info local
```

(The `plane-mc` container exits after first-up's bucket-create; you
need to `docker compose run --rm plane-mc sh` to get an interactive
shell, or just run the same commands inside `plane-minio` itself with
`mc` installed there.)

## Layout

```
stacks/plane/
├── docker-compose.yml         # 14 services, all on `edge`
├── .env.example               # documents every env var
└── README.md                  # this file
```

No `conf/` subdirectory — Plane's bundled `plane-proxy` (Caddy) does
not need any config-file overrides for our deployment (we don't use
its built-in TLS / ACME paths because NPM is the TLS terminator).
