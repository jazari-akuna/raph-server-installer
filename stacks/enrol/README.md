# enrol — server admin UI

Public hostname: `enrol.antarctica-engineering.com`. Auth delegated to
Authelia via NPM forward-auth: enrol trusts `Remote-User` and
`Remote-Groups` headers. Membership of group `admins` is required for
all routes besides the static healthcheck.

This is the operator-facing UI for the shared VPS. From the browser
you can: create / edit / delete Authelia users; provision LUKS volumes;
unlock + lock data blobs; rotate LUKS passphrases; regenerate TOTP
secrets; add + remove peer (gateway) devices grouped by user; review an
append-only audit log.

The full design lives in [`DESIGN.md`](DESIGN.md). This README is the
day-to-day operator reference.

## ⚠ Privilege & Threat Model — read this first

**enrol effectively has root on the host.** Anyone with admin access
to the UI can:

- create or destroy LUKS volumes (and shred the underlying blob),
- mount / unmount filesystems on the host,
- edit Authelia's `users_database.yml` (create/delete users, change
  passwords, change group membership including their own),
- exec into any container via the bind-mounted docker socket (the
  TOTP-generation path uses this).

The container therefore runs `privileged: true` with bind-mounts of
`/srv/store`, the Authelia config, and `/var/run/docker.sock`. We
considered a finer-grained capability set
(`SYS_ADMIN`+`MKNOD`+`NET_ADMIN` plus device cgroup rules); the
practical access surface is the same and the surgical version is
fragile across kernel updates. See `DESIGN.md` § 0.4 for the full
trade-off discussion.

Mitigations (defence in depth, all present):

- NPM HTTPS with the wildcard cert.
- Authelia forward-auth, policy `two_factor`, `subject: group:admins`.
- Authelia regulation (5 attempts / 15 min ban).
- CSRF tokens on every mutating form.
- JSONL audit log of every state-changing operation.
- Container exposes `172.17.0.1:8080` only — never the public WAN.

What this does NOT defend against:

- A co-admin going rogue (out of scope per the project threat model).
- Compromise of the Authelia portal.
- An attacker who already has shell on the host.

## Auth flow

1. Browser → `https://enrol.antarctica-engineering.com/users`.
2. NPM auth_request → `http://authelia:9091/api/verify`.
3. No session → 401 → NPM 302s to the Authelia portal.
4. Login + TOTP → Authelia sets the SSO cookie on the apex domain.
5. Auth_request now returns 200 + `Remote-User` / `Remote-Groups`.
6. enrol's middleware reads the headers and gates accordingly.

If `Remote-User` is missing, enrol returns 401 — that condition only
happens if NPM is misconfigured.

## Endpoints

| Method | Path                              | Auth          | Purpose |
|---|---|---|---|
| GET    | `/users`                          | admins        | list users |
| POST   | `/users`                          | admins + CSRF | create user (form) |
| GET    | `/users/<u>`                      | admins        | user detail |
| POST   | `/users/<u>/edit`                 | admins + CSRF | display name / email / groups |
| POST   | `/users/<u>/password`             | admins + CSRF | rotate Authelia password |
| POST   | `/users/<u>/totp`                 | admins + CSRF | regenerate TOTP, render QR once |
| POST   | `/users/<u>/luks/passphrase`      | admins + CSRF | luksAddKey + luksRemoveKey |
| POST   | `/users/<u>/luks/unlock`          | admins + CSRF | open + mount |
| POST   | `/users/<u>/luks/lock`            | admins + CSRF | umount + close |
| POST   | `/users/<u>/delete`               | admins + CSRF | shred volume + drop user |
| GET    | `/peers`                          | admins        | devices, grouped by user |
| POST   | `/peers`                          | admins + CSRF | add peer (form: user + tag) |
| GET    | `/peers/<name>`                   | admins        | peer detail |
| POST   | `/peers/<name>/delete`            | admins + CSRF | remove peer |
| GET    | `/peers/<name>/config`            | admins        | download .conf (no privkey) |
| GET    | `/peers/<name>/qr.png`            | admins        | QR PNG |
| GET    | `/audit`                          | admins        | last 200 audit entries (JSON) |
| GET    | `/healthz`                        | none          | liveness probe |

## Storage / state on disk

| File | Role |
|---|---|
| `/etc/amnezia/amneziawg/gw0.conf` | Source of truth: peers + interface params. |
| `/etc/amnezia/amneziawg/peers-meta.json` | Sidecar: pubkey → {name, tag, added_by, added_at}. |
| `/etc/amnezia/amneziawg/peers-audit.log` | Append-only JSONL audit (peer + user + LUKS lifecycle). |
| `/srv/store/data/<u>.img` | LUKS2 sparse blob, 50 GB, argon2id KDF. |
| `/srv/store/mnt/<u>` | Mountpoint, 0700, owned by host user `<u>`. |
| `/opt/stacks/authelia/users_database.yml` | Authelia file backend; argon2id hashes. |

## Layout

```
stacks/enrol/
├── DESIGN.md            full design + research findings
├── README.md            (this file)
├── Dockerfile           multi-stage: golang:1.23 → debian:trixie-slim
├── docker-compose.yml   privileged: true + the new bind-mounts
├── go.mod, go.sum
├── main.go              entry point
├── server.go            routes + handlers
├── auth.go              Remote-* middleware
├── csrf.go              CSRF mint/verify
├── audit.go             JSONL audit log
├── peers.go             gw0.conf parser/writer + keypair gen
├── users.go             users_database.yml + argon2id
├── luks.go              cryptsetup / mkfs / mount / shred
├── totp.go              docker-exec into authelia for TOTP
├── web/static/style.css
└── web/templates/
    ├── _layout.html
    ├── users.html
    ├── user-detail.html
    ├── user-created.html
    ├── peers.html       (grouped-by-user)
    ├── peer-created.html
    └── peer-detail.html
```

## Deploy

On the laptop:

```sh
./scripts/deploy.sh
```

On the VPS:

```sh
cd /opt/stacks/enrol
docker compose build
docker compose up -d --force-recreate
docker compose logs -f enrol
```

The cloud stack must also be redeployed once after the single-mount
refactor:

```sh
cd /opt/stacks/cloud
docker compose up -d --force-recreate
```

Authelia, NPM, console (Portainer) do not need to be touched.

## Migration: adopting existing state

On first run after upgrade, enrol enumerates `users_database.yml` and
treats every entry as a managed user — including the existing `sagan`
and `marcus` who were created out-of-band. Their LUKS blobs and peers
are similarly adopted; nothing is rewritten until an operator action
modifies it. There is no "import" step.

## Environment

| Var | Default | Meaning |
|---|---|---|
| `ENROL_LISTEN`              | `172.17.0.1:8080` | bind address |
| `ENROL_AWG_DIR`             | `/etc/amnezia/amneziawg` | gw0.conf + sidecars |
| `ENROL_AWG_IFACE`           | `gw0` | interface |
| `ENROL_AWG_ENDPOINT`        | `gw.antarctica-engineering.com:51820` | client endpoint |
| `ENROL_PEER_SUBNET`         | `10.99.0.0/24` | peer subnet |
| `ENROL_PEER_START`          | `10` | first host octet |
| `ENROL_HEADER_USER`         | `Remote-User` | auth header |
| `ENROL_HEADER_GROUPS`       | `Remote-Groups` | groups header |
| `ENROL_REQUIRED_GROUP`      | `admins` | gating group |
| `ENROL_RELOAD_NSENTER`      | `false` | network_mode: host = no nsenter |
| `ENROL_USERS_DB`            | `/etc/authelia/users_database.yml` | file inside container (bind from host) |
| `ENROL_AUTHELIA_CONTAINER`  | `authelia` | docker exec target for TOTP |
| `ENROL_STORE_DATA_DIR`      | `/srv/store/data` | LUKS blobs |
| `ENROL_STORE_MNT_DIR`       | `/srv/store/mnt` | mountpoints |
| `ENROL_LUKS_SIZE_GB`        | `50` | new-volume size |

## Operator runbook

### "the TOTP QR is gone before I scanned it"
`POST /users/<u>/totp` regenerates and re-displays. Invalidates the
previous secret.

### "Authelia didn't pick up my YAML edit"
The file watcher is fsnotify-driven and reloads on rename. If a reload
misses (rare), `docker exec authelia kill -HUP 1`.

### "the cloud UI shows empty after I created a user"
The LUKS volume must be unlocked (mounted) for files to appear. Use
`POST /users/<u>/luks/unlock` from the user detail page. Cloud's bind-
mount is `propagation: rslave` so mounts done by enrol propagate
in automatically; if not, restart the cloud container.

### "a peer doesn't show up under any user"
Peers without a `<user>-` prefix matching a known username land in the
"(unassigned)" group. Either rename the peer (delete + re-add with the
correct user) or accept the unassigned status.

### "I want to scrub all evidence of a test user"
`POST /users/<u>/delete` does the full teardown: LUKS shred, peer
removal, TOTP delete, YAML drop, host userdel. The audit log retains
records of the operations (intentional).

## Security notes

- Peer private keys are generated **inside the container** (X25519,
  `crypto/rand`) and shown to the operator **once** on the post-add
  page. They are never persisted. If the operator loses the page, the
  peer must be deleted and re-added.
- TOTP secrets live only inside Authelia's encrypted sqlite. enrol
  fetches the otpauth URI from the CLI's stdout and renders a QR;
  neither is logged.
- Argon2id parameters match
  `stacks/authelia/configuration.yml` exactly so authelia accepts
  enrol-issued hashes without rehashing on next-login.
- LUKS passphrases are independent of Authelia passwords by design —
  see DESIGN.md § 6.1.
- enrol does not call out to the public internet at any point.
