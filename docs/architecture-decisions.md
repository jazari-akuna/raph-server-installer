# Architecture decisions

Living record of the load-bearing design choices in this repo. Each decision is here because future-us (or future-Claude) would otherwise have to re-derive it from scratch — and the answer isn't obvious from the code alone.

Add a new entry when you make a choice that future contributors should not silently overturn. Keep entries terse: rationale + the alternatives we rejected and why.

---

## ADR-001 — Backup model: every stack's state on `/srv/store/<stack>-*` bind-mounts

**Decision.** All persistent state for every stack lives on host bind-mounts under `/srv/store/<stack>-*`, never in docker named volumes. Postgres and other DB data dirs use the same convention.

Backup recipe across the box: one `rsync /srv/store/` + a `pg_dump` per Postgres-backed stack. That's it. Restore: `rsync` back, `psql < dump.sql`, `docker compose up -d`.

**Why.** Two reasons. First: bind-mounts are immediately backupable from the host without `docker cp` gymnastics or per-volume `docker run --rm -v` recipes. Second: when a service breaks, you can `ls /srv/store/<stack>-*` from a recovery shell and see exactly what's there — named volumes are opaque (`/var/lib/docker/volumes/<hash>/_data`).

**Exception.** Image-managed code that has no backup-relevant state may use a named volume. Example: `cloud-html` in `stacks/cloud/docker-compose.yml` holds the Nextcloud webroot copied from the image at `/usr/src/nextcloud/`; backup-by-image-tag is correct, rsync of those files is wasteful. The rule is "if losing this directory means data loss, it's a bind-mount." If "losing this directory means re-running the entrypoint", it's a named volume.

**Rejected alternatives.**
- Named volumes everywhere with `docker run --rm -v vol:/data alpine tar czf -` for backup. Operationally noisy, easy to forget a volume.
- Postgres-data on a named volume (image default). Same problem; the bind-mount + `pg_dump` belt-and-braces pair is cleaner.

---

## ADR-002 — Camouflage naming: external URLs, internal stack names, image identity

**Decision.** External URL pattern: `<service>.${DOMAIN}` (e.g. `cloud.orgabots.com`, `plane.orgabots.com`). Internal stack name, service name, container name, network name, and bind-mount prefix all use the same `<service>` prefix. The actual upstream image identity (Nextcloud, Plane, etc) is NOT surfaced anywhere a user-facing URL or container name appears.

**Why.** Two reasons. First: cosmetic — operator-facing names match the brand domain rather than the implementation. Second: makes implementation swaps low-friction (we just swapped copyparty → Nextcloud under the `cloud` name with zero URL change and the launcher tile reused).

**How to apply.** When adding a new stack, pick the URL prefix first. Everything else (container names, bind-mount paths, env-var prefixes) inherits. Don't name a service "nextcloud" or "plane-backend" at the outer layer — name it `cloud` or `plane-api`.

---

## ADR-003 — Auth: OIDC vs forward-auth, two patterns, never mixed per service

**Decision.** Each service uses exactly one of two auth patterns:

| Pattern | Used by | NPM advanced_config | Authelia rule |
|---|---|---|---|
| **OIDC (the app owns its session)** | cloud (Nextcloud), plane | `npmAdvNextcloudTmpl` / `npmAdvPlaneTmpl` — no forward-auth | `bypass` |
| **forward-auth (NPM gates with Authelia, then header-injects identity)** | enrol, console (Portainer) | `npmAdvFwdAuthTmpl` — auth_request to Authelia, sets `Remote-*` and `X-Forward-Auth-Secret` | `one_factor` (or per-rule) |

**Why.** OIDC services have their own user db / session / RBAC and need to own login (Nextcloud's OIDC drops you into Nextcloud-as-the-user; forward-auth would just inject a header Nextcloud would have to re-resolve). Forward-auth services are thin and benefit from Authelia's session being authoritative — no duplicate user db.

**The trap we already fell into.** Mixing the two on one service (we tried forward-auth + Nextcloud headers in the copyparty era) means two layers each think they're authoritative, and the failure modes are silent (header missing → wrong user, header present + session missing → infinite redirect). One pattern per service. No exceptions.

**Authelia client config.** Each OIDC-using service gets its own client in `stacks/authelia/configuration.yml.template`: `console` (Portainer), `cloud` (Nextcloud), `plane`. Plaintext secret at `/etc/raph-installer/oidc-<service>-client-secret` (mode 0600), hash in `.env` as `AUTHELIA_OIDC_<SERVICE>_CLIENT_SECRET_HASH`. Rotation function in `stacks/enrol/oidc.go` per service, mirror `rotateClientSecret(envPath, plaintextPath, envVar)` helper.

**Admin-only pages** (in enrol) gated by `requireAdmin` middleware which reads `Remote-Groups` for `admins` (Authelia injects this via forward-auth on enrol's NPM proxy host).

---

## ADR-004 — LUKS removed; Nextcloud SSE available as opt-in

**Decision.** No per-user LUKS volumes. No `_shared` LUKS. No `shared-store.service`. No `/login-intercept` HTTP handler in enrol that auto-unlocks volumes on Authelia login.

User data lives in plaintext on the host's encrypted-at-rest filesystem (Layerstack VPS provides full-disk encryption for the boot volume). Per-user file isolation is enforced at the application layer (Nextcloud's per-user data dir + ACLs).

**Why we ripped it.** Two reasons:
1. **Per-user LUKS** had real cold-disk protection (an attacker with disk image but no per-user passphrase couldn't read user files), but operationally cost us: a near-incident where deleting a user with `shred -u` grew a sparse 50 GB image to fully allocated, the rslave-mount-propagation complexity, the lock-button UX, the auto-unlock middleware. Two-admin test setup → cost > benefit.
2. **`_shared` single-key LUKS with the keyfile on disk** was theater — anyone with root on the host could trivially read both the keyfile and the volume. Zero benefit against either threat (cold disk, runtime compromise).

**Mitigation if cold-disk protection becomes important again.** Nextcloud's Server-side Encryption (SSE) app is a one-toggle replacement: `occ app:enable encryption && occ encryption:enable`. SSE protects against host-disk theft (admin who has only the disk image cannot decrypt user files) but not runtime compromise (a running Nextcloud admin can still decrypt anything). Acceptable tradeoff for a small operator.

**Don't suggest re-adding LUKS** unless the threat model actually changes (multi-tenant operator boundary appears, regulatory ask, etc).

---

## ADR-005 — Storage / usage tracking: unified admin page, 60s in-memory cache, no external DB

**Decision.** The `/users` page in enrol (renamed h1/title to "Admin", route preserved for bookmarks, gated by existing `requireAdmin`) shows a per-user table aggregating usage from every stack that holds user data:

| Source | Data path | Field |
|---|---|---|
| Filesystem (`du -sb`) | `/srv/store/cloud-data/<u>/files` | OnDiskBytes |
| Nextcloud `occ user:info <u> --output=json` | shells via `docker exec -u www-data cloud php occ ...` | NCQuotaUsed, NCFileCount, last login |
| Plane REST API | `https://plane.${DOMAIN}/api/v1/...` with bearer token from `/etc/raph-installer/plane-admin-token` | PlaneIssues, PlaneAttBytes, last activity |

**Cache.** 60 s in-memory TTL'd cache per user. Single mutex. Mirrors the existing `duCache` pattern in `stacks/enrol/storage.go`. Shell-out cost is otherwise ~50 ms per user × N users × every page hit.

**Why no external storage.** Three reasons. First: usage is derivable from authoritative sources at any time, so there's nothing to back up. Second: cache invalidation across processes is the wrong problem to take on for a 2-admin test setup. Third: keeps enrol's deployment story (single Go binary + state.json) intact.

**Why same page, not a new route.** Bookmarks. Audit trail. The enrol nav already has `/users` as the admin landing.

**When to add a new column.** When a new stack starts holding per-user data that the operator needs to monitor (e.g. quota approaching, abuse detection). The pattern: extend `UserStorage` struct in `storage.go`, add a `<stack>Client` if the source is HTTP, slot it into `storageSnapshot` under the same cache.

**Don't.** Don't add a "current sessions" or "recent activity" view here — that's a different concern (live monitoring) and belongs in Grafana / Authelia logs, not enrol's admin page.

---

## ADR-006 — Image-pin policy: never `latest`, never `stable`, audit quarterly

**Decision.** Every `image:` in every `docker-compose.yml` MUST be pinned to a specific version tag (e.g. `nextcloud:30.0.6-fpm-alpine`, `plane-backend:v0.27.1`, `postgres:16.6-alpine`). Never `latest`, `stable`, `lts`, or any floating tag.

**Why.** Two real incidents prevented:
1. Plane has historically shipped breaking-change releases under `stable` (env vars renamed, god-mode flow changed). Pinned tag → planned upgrade window. Floating tag → 3am page.
2. Postgres major bumps (15→16) require `pg_upgrade` runs. Floating tag turns this into "everything is broken on next compose pull" instead of "we deliberately chose to upgrade today."

**Bump cadence.** Quarterly audit listed in `docs/maintenance.md`. Each bump is its own PR with its own smoke test. Pre-bump: `pg_dump` snapshot + `rsync /srv/store`.

**Exception.** One-shot init/migration containers (Plane's `plane-mc` bucket-creator, `plane-migrator`) follow the same rule — they can break too.

---

## ADR-007 — One commit per multi-feature delivery (operator preference)

**Decision.** Multi-stack / multi-wave deliveries land as ONE commit at the end, not one-commit-per-agent or one-commit-per-wave.

Author `mail@raph.io`, standard `Co-Authored-By: Claude Opus 4.7 (1M context)` footer. Commit message body summarises the whole delivery: which stacks changed, which were torn out, which user-facing surface changed.

**Why.** Operator preference. Bisect-friendliness traded for a clean PR/release-notes story. The wave-by-wave detail is in the plan files at `/root/.claude/plans/`.

**When to break the rule.** A genuinely separable bug fix that ships ahead of the multi-stack delivery is its own commit. Don't bundle bug fixes into the feature commit just because they're in the working tree.

---

## ADR-008 — Resource budget on a 2 GB VPS

**Decision.** Target `≤ 2.0 GB` total RSS across all containers under steady load, `≤ 2.2 GB` under burst (upload + concurrent activity). Swap configured at 4 GB to absorb bursts without OOM.

**Per-stack tuning knobs that actually move the needle:**
- Postgres: `shared_buffers=64MB max_connections=50` via `command:` override. Default `max_connections=1000` blows the budget on its own.
- RabbitMQ: `RABBITMQ_VM_MEMORY_HIGH_WATERMARK=0.4` — caps RMQ to 40% of its container's mem_limit.
- Celery workers: `CELERY_WORKER_CONCURRENCY=1`. Default `auto` spins one worker per CPU core.
- php-fpm: `PHP_FPM_PM_MAX_CHILDREN=5` (not the default 50). Each child is ~80 MB.
- Per-container `mem_limit:` to fail fast (OOM-kill the offender) rather than slow down the whole box.

**Documented fallback** if Plane OOMs in steady state: drop the `plane-mq` (RabbitMQ) service, route Celery through `plane-redis` via `CELERY_BROKER_URL=redis://plane-redis:6379/1`. Saves ~120 MB at the cost of broker durability for long workflows.

**Don't.** Don't share Postgres or Redis across stacks. The ~80 MB / ~20 MB savings are not worth coupling outage scopes — one DB hiccup taking down both cloud and plane is a 10× worse outcome than running two separate Postgres instances.

---

## ADR-009 — Plane: god-mode bootstrap is manual, by design

**Decision.** Plane's instance-admin claim (`/god-mode/`) and OIDC configuration paste are done manually in the browser by the operator on first deploy. No env-var skip, no automation.

**Why.** Plane has no env-var path to bootstrap god-mode credentials — it's deliberate (first-user-wins on `/god-mode/sign-up`). Trying to script it via the API would require god-mode auth we don't have yet. Manual is the supported path.

**Footgun.** First-user-wins means if `plane.${DOMAIN}` is reachable publicly before the operator claims god-mode, an attacker who finds the URL claims it instead. Mitigation: complete claim + OIDC paste **immediately** in the same deploy session as the first `docker compose up`. Document the URL gate (one-shot Authelia `one_factor` policy on `plane.${DOMAIN}` for the first 24 h, then flip to `bypass`) as the defensive fallback.
