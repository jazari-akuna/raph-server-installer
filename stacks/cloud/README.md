# cloud

File server + collaboration for the shared VPS. Internally this is
[Nextcloud](https://nextcloud.com/) 30 (fpm flavour); externally and in
all paths/names it is `cloud`. Reachable only via `ingress` at
`cloud.${DOMAIN}`. There are no public port bindings on this stack.

This replaces the previous copyparty-based implementation. The external
surface (URL, stack name, NPM proxy host) is preserved; only the
implementation underneath changed.

## What this is

Four containers on the existing `edge` Docker network:

| Service       | Image                          | Role                                              |
| ---           | ---                            | ---                                               |
| `cloud`       | `nextcloud:30.0.6-fpm-alpine`  | Nextcloud, php-fpm on :9000                       |
| `cloud-db`    | `postgres:16.6-alpine`         | Postgres 16, the canonical Nextcloud DB           |
| `cloud-redis` | `redis:7.4-alpine`             | memcache + distributed file lock                  |
| `cloud-web`   | `nginx:1.27-alpine`            | nginx sidecar on :80 — what NPM proxies to        |

NPM terminates TLS upstream and proxies `cloud.${DOMAIN}` →
`cloud-web:80`, which fastcgi_passes to `cloud:9000`. Nothing is
published to the host.

## Auth — Authelia OIDC

Auth is delegated to Authelia using **OIDC** (the `user_oidc` Nextcloud
app), NOT the old forward-auth header model used by copyparty. NPM does
not inject `Remote-User` or `X-Forward-Auth-Secret` for this stack.

`user_oidc` auto-provisions a Nextcloud account on the first successful
login. The Authelia client ID + secret pair is configured in
`stacks/authelia/configuration.yml` and is rotated by the bootstrap
finalize pipeline (see `stacks/enrol/oidc.go`).

## Apps installed

The bootstrap pipeline enables the following Nextcloud apps after the
first `compose up`:

- **user_oidc** — SSO via Authelia.
- **spreed** — Talk (audio/video calls + chat). Basic mode only;
  see "Talk basic mode" below.
- **groupfolders** — admin-managed shared spaces with their own ACLs.

To enable manually after a fresh install:

```sh
docker exec -u www-data cloud php occ app:install user_oidc
docker exec -u www-data cloud php occ app:install spreed
docker exec -u www-data cloud php occ app:install groupfolders
```

## Where data lives

All persistent state is on **external** bind mounts under
`/srv/store/cloud-*` on the host (NOT docker named volumes), so backup
is a single rsync of the parent directory plus a `pg_dump` snapshot.

| Host path                | Container mount                  | Service(s)             | uid:gid |
| ---                      | ---                              | ---                    | ---     |
| `/srv/store/cloud-data`  | `/var/www/html/data`             | `cloud` and `cloud-web`| 33:33   |
| `/srv/store/cloud-config`| `/var/www/html/config`           | `cloud`                | 33:33   |
| `/srv/store/cloud-apps`  | `/var/www/html/custom_apps`      | `cloud`                | 33:33   |
| `/srv/store/cloud-db`    | `/var/lib/postgresql/data`       | `cloud-db`             | 70:70   |

`bootstrap-phase2.sh` creates these directories with the correct
ownership before this stack comes up. Redis state is in-container only
(cache + ephemeral lock set; safe to lose on restart).

Backup recipe (laptop-side, over SSH):

```sh
ssh vps "docker exec cloud-db pg_dump -U nextcloud nextcloud" \
  > snapshots/$(date +%F)-cloud-db.sql
rsync -aAXH --delete vps:/srv/store/cloud-data/ snapshots/cloud-data/
rsync -aAXH --delete vps:/srv/store/cloud-config/ snapshots/cloud-config/
rsync -aAXH --delete vps:/srv/store/cloud-apps/ snapshots/cloud-apps/
```

Restore is the reverse, plus `psql -U nextcloud < snapshot.sql`.

## How to run `occ`

Nextcloud's `occ` CLI is the canonical admin tool. Run it as `www-data`
inside the `cloud` container:

```sh
docker exec -u www-data cloud php occ <command>
# e.g.
docker exec -u www-data cloud php occ status
docker exec -u www-data cloud php occ user:list
docker exec -u www-data cloud php occ files:scan --all
```

The `cloud-web` sidecar does NOT have php-cli — `occ` only works in
the `cloud` container.

## Server-side encryption (SSE)

Not enabled by default. SSE only protects against host-disk theft; it
does NOT protect against a compromised running server, because the
server holds the master key and the admin can decrypt anything.
Performance also drops noticeably on large directory walks.

To opt in:

```sh
docker exec -u www-data cloud php occ app:enable encryption
docker exec -u www-data cloud php occ encryption:enable
docker exec -u www-data cloud php occ encryption:enable-master-key
```

This is irreversible without a recovery-key migration ceremony — be
sure before flipping it on. See
<https://docs.nextcloud.com/server/latest/admin_manual/configuration_files/encryption_configuration.html>.

## Talk basic mode

Spreed is installed in **basic mode** — peer-to-peer browser WebRTC
only, no High Performance Backend (HPB), no Coturn TURN server.
Concretely:

- 1:1 calls and small (≤4) group calls work directly browser-to-browser.
- Signaling rides Nextcloud's built-in long-poll, not WebSockets.
- Clients behind symmetric NAT / CGNAT (~5% of consumer ISPs in
  practice) may fail to connect because there is no TURN relay.

Consider deploying the HPB + Coturn pair when **any** of the following
becomes the steady-state pain:

- More than ~4 concurrent participants per call (P2P mesh complexity
  goes O(n²); CPU and bandwidth on the originator suffocate).
- Audio drops or one-way audio in 3-way calls.
- Connection failures from clients on CGNAT or restrictive corporate
  NAT, traceable to ICE candidate-pair selection failing.

Reference docs:
<https://nextcloud-talk.readthedocs.io/en/latest/PRODUCTION/> and the
HPB project at <https://github.com/strukturag/nextcloud-spreed-signaling>.

## 50 GB upload chain

Large uploads have to be permitted at every layer of the proxy chain.
Each layer enforces its own ceiling and the **most restrictive one
wins** — if even one layer is set to (say) 100 MB, all uploads above
100 MB fail at that hop with no useful error message. All four must
agree.

| Layer            | Where it's configured                                        | Knob(s)                                                         |
| ---              | ---                                                          | ---                                                             |
| php-fpm          | `stacks/cloud/conf/zz-uploadlimits.ini`                      | `upload_max_filesize`, `post_max_size`, `max_execution_time`    |
| nginx-sidecar    | `stacks/cloud/conf/nginx.conf`                               | `client_max_body_size`, `fastcgi_request_buffering off`         |
| NPM (TLS edge)   | `stacks/enrol/setup.go` → `npmAdvNextcloudTmpl`              | `client_max_body_size`, `proxy_request_buffering off`           |
| Nextcloud quota  | `occ config:system:set default_quota --value="50 GB"`        | per-user upload + storage quota                                 |

`fastcgi_request_buffering off` and the matching NPM
`proxy_request_buffering off` are load-bearing: without them, nginx
spools the entire request body to its local tmpfs before forwarding,
which both fills the container fs and stalls the upload long before
50 GB. With buffering off, body bytes stream straight through.

If you raise these, raise them everywhere in agreement.

## Resource budget

Typical 2-user steady-state RSS on a 2 GB VPS:

| Service     | Typical RSS | Notes                                              |
| ---         | ---         | ---                                                |
| cloud       | ~280 MB     | fpm master + 2-3 idle workers + opcache            |
| cloud-db    | ~80 MB      | small DB, plenty of shared_buffers headroom        |
| cloud-redis | ~20 MB      | cache + lock set are tiny                          |
| cloud-web   | ~60 MB      | nginx workers                                      |
| **Total**   | **~440 MB** |                                                    |

Adding the Talk HPB + Coturn pair pushes total cloud-stack footprint to
roughly **1.4 GB** (signaling server + Janus media server + Coturn TURN
relay + slightly fatter nginx config). On a 2 GB VPS that displaces
budget for Authelia / NPM / enrol; size accordingly before enabling.

## Maintenance

### Upgrading Nextcloud

Bump the image tag in `docker-compose.yml`, pull, recreate, then run
the schema migrations:

```sh
cd /opt/stacks/cloud
# edit docker-compose.yml: nextcloud:30.0.6-fpm-alpine → 30.0.7 or 31.x
docker compose pull
docker compose up -d
docker exec -u www-data cloud php occ upgrade
docker exec -u www-data cloud php occ db:add-missing-indices
docker exec -u www-data cloud php occ maintenance:mode --off
```

Major-version jumps (30 → 31) require checking the
[upgrade matrix](https://docs.nextcloud.com/server/latest/admin_manual/maintenance/upgrade.html)
first — Nextcloud only supports skipping at most one major version per
upgrade run. Always snapshot `/srv/store/cloud-*` and dump the DB
before a major-version bump.

### Editing config

Edits to `conf/nginx.conf` or `conf/zz-uploadlimits.ini` only take
effect after recreating the relevant container:

```sh
docker compose restart cloud-web    # nginx changes
docker compose restart cloud        # php / fpm changes
```

The conf files are bind-mounted read-only; the container cannot mutate
them from inside.

### Required env vars

The stack will refuse to start without these set in
`/opt/stacks/cloud/.env`:

- `POSTGRES_PASSWORD` — random, generated by bootstrap-phase2.sh.
- `NEXTCLOUD_ADMIN_PASSWORD` — random, generated by bootstrap-phase2.sh.
- `NEXTCLOUD_TRUSTED_DOMAINS` — must include `cloud.${DOMAIN}`.

Optional with sensible defaults: `POSTGRES_USER`, `POSTGRES_DB`,
`NEXTCLOUD_ADMIN_USER`, `REDIS_HOST`. See `.env.example`.

## Layout

```
stacks/cloud/
├── docker-compose.yml         # 4 services, all on `edge`
├── conf/
│   ├── nginx.conf             # sidecar config; 50 GB streaming upload knobs
│   └── zz-uploadlimits.ini    # php-fpm 50 GB upload override
├── data/                      # legacy copyparty state — Wave 2 nukes this
├── .env.example               # documents POSTGRES_PASSWORD etc.
└── README.md                  # this file
```
