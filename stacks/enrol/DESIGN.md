# enrol — server admin UI design

This is the design doc for the **expanded** `enrol` service. Prior scope
was peer (gw0) management only; current scope is full server admin —
Authelia users, LUKS storage volumes, TOTP enrolment, peer (device)
management — driven from a single browser-only operator UI so the human
operator never has to SSH for routine create/edit/delete.

The first version of this doc lived inside `../authelia/DESIGN.md`
(implementer mapping → "B — enrol app"). That doc is preserved as the
SSO-layer source of truth; this file extends it and is the canonical
reference for everything inside the `enrol` service from this revision
forward.

> **Camouflage.** "enrol" is the operator-facing service name. The
> public hostname is `enrol.${DOMAIN}` (e.g. `enrol.example.com`). Nothing in
> URL paths, container names, audit-log paths, or the UI mentions
> luks/wireguard/amnezia/vault. See `claude-readme.md` § "Camouflage
> naming."

---

## 0. Phase 0 research findings (citations + decisions)

These were verified against the live 4.39.19 deployment on the VPS and
upstream docs at the dates indicated.

### 0.1 Authelia 4.39.19 user schema and CLI

- `users_database.yml` schema (per the [Authelia password reference
  guide](https://www.authelia.com/reference/guides/passwords/),
  fetched 2026-04-25):

  ```yaml
  users:
    <name>:
      disabled: false                   # bool
      displayname: 'Sagan'              # str
      password: '$argon2id$v=19$m=...'  # argon2id PHC string
      email: 'alice@…'                  # str
      groups: ['admins']                # []str
      # OIDC claims also accepted:
      #   given_name, family_name, middle_name, nickname,
      #   profile, picture, website, gender, birthdate, zoneinfo,
      #   locale, phone_number, phone_extension
      # Address object: street_address, locality, region, postal_code, country
      # Custom: extra
  ```

  We use the core five fields only (`disabled`, `displayname`,
  `password`, `email`, `groups`). OIDC-extended claims (e.g.
  `given_name`) are not exposed in the UI in this version.

- **Argon2id PHC format**: `$argon2id$v=19$m=<KiB>,t=<iter>,p=<par>$<salt>$<hash>`
  where salt and hash are RFC 4648 **base64-no-padding** encoded.
  Authelia's configured params (`stacks/authelia/configuration.yml`):
  variant `argon2id`, iterations `3`, memory `65536` KiB (= 64 MiB),
  parallelism `4`, key length `32`, salt length `16`. enrol writes
  hashes with these exact parameters so authelia accepts them without
  a rehash on next-login.

- **CLI for TOTP** (verified `docker exec authelia authelia storage user
  totp generate --help`):
  - `authelia storage user totp generate <user> --force` — writes secret
    to sqlite, returns the otpauth URI on stdout. Optional `--path
    <png>` writes a QR PNG to disk (we do not use this — we generate
    the QR from the URI inside enrol via `qrencode`, so the secret
    never lands on disk except inside Authelia's encrypted sqlite).
  - `authelia storage user totp delete <user>` — used by user-delete.

- **File watcher**: `authentication_backend.file.watch: true` is set in
  the live config. Authelia uses fsnotify on the parent directory;
  atomic-rename writes (tmpfile + rename) trigger a reload. Verified
  on the VPS by manual edit during deploy. If reload misfires, the
  fallback is `docker exec authelia kill -HUP 1` (see Operator
  Runbook below). enrol does not use this fallback in normal flow.

### 0.2 copyparty IdP single-mount semantics

- Per the [copyparty IdP docs](https://github.com/9001/copyparty/blob/hovudstraum/docs/idp.md)
  and the upstream [example config](https://github.com/9001/copyparty/blob/hovudstraum/docs/examples/docker/idp/copyparty.conf),
  the `${u}` placeholder in a volume rule (e.g. `[/u/${u}] /w/${u}`)
  is substituted at request time with the authenticated username.
  copyparty does **not** auto-create on-disk paths; the destination
  must exist or the volume serves empty / 404.

- A naive layout uses one static per-user bind-mount in compose
  (`/srv/store/mnt/<u>:/w/<u>`). Adding a new user requires editing
  compose + recreating the container.

- **Decision**: replace with a single bind-mount
  `/srv/store/mnt:/w` so any new on-disk subdirectory under
  `/srv/store/mnt` is immediately visible to copyparty as `/w/<u>` —
  no compose edit per user. See § 0.3 for the propagation caveat.

### 0.3 Mount propagation under a single bind-mount (the gotcha)

- Docker bind-mounts default to `rprivate` propagation. With a single
  `/srv/store/mnt:/w` mount established at container start, **subsequent**
  mounts on the host inside `/srv/store/mnt/<u>` (e.g. on LUKS unlock)
  are *not* visible inside the container.

- Verified empirically on the VPS (`bind-propagation=rslave`):

  ```
  bind-propagation=rprivate (default) → tmpfs at /srv/store/mnt/foo  → invisible inside container
  bind-propagation=rslave             → tmpfs at /srv/store/mnt/foo  → visible at /w/foo
  ```

- **Decision**: cloud's bind-mount uses `bind-propagation=rslave`.
  `rslave` (not `rshared`) is the right choice — the container
  receives mount events from the host but cannot push them upward.
  Mounts done by enrol on the host (via `cryptsetup open` + `mount`)
  appear in cloud automatically.

  Requires the host's `/` to be a `shared` mount, which it is on the
  VPS (verified via `/proc/self/mountinfo`: `/ ... shared:1`).

### 0.4 Privilege model trade-off

- Container needs cryptsetup (loopback + dm-mapper), mkfs.ext4, mount,
  umount, shred. The minimal capability set is `CAP_SYS_ADMIN` (mount
  syscalls), `CAP_DAC_OVERRIDE` (read/write across owners), plus access
  to `/dev/loop-control`, every `/dev/loop*` instance, and
  `/dev/mapper/control` + `/dev/mapper/*`.

- Two options were considered:

  | Option | Effect | Trade-off |
  |---|---|---|
  | A. `privileged: true`         | Container has all caps, all devices, no AppArmor profile. | Coarse but reliable. |
  | B. Targeted: `cap_add: [SYS_ADMIN, MKNOD, NET_ADMIN]` + `--device-cgroup-rule='b 7:* rwm'` (loopN) + `--device=/dev/loop-control` + `/dev/mapper:/dev/mapper:rshared` + AppArmor=unconfined | Same effective power for our use case, narrower default profile. | More moving parts; AppArmor profile sometimes blocks dm setup. |

  **Decision**: Option A — `privileged: true`. enrol is already a high-
  value target by virtue of being the user-creator + ssh-bypass admin
  UI; trying to micromanage caps when the practical access set
  encompasses all of dm-crypt, loop devices, and shred-on-block is
  cosmetic. Document this decision prominently in `README.md` and call
  out the threat-model implication: **anyone with admin access to enrol
  is effectively root on the host.**

- Mitigations (defence in depth, all already deployed):
  - NPM proxy host with HTTPS + Authelia forward-auth (`two_factor`,
    `subject: group:admins`).
  - Authelia regulation (5 attempts / 15 min ban).
  - CSRF tokens on every mutating form (new in this revision).
  - Audit log captures actor + action + target + result for every
    state-changing operation.

### 0.5 cryptsetup parameters

Match `scripts/create-store-volume.sh` exactly so enrol-created volumes
look identical to operator-created ones:

```
cryptsetup luksFormat
    --type luks2
    --cipher aes-xts-plain64
    --key-size 512
    --hash sha512
    --pbkdf argon2id
    --use-urandom
    /srv/store/data/<u>.img
```

Image size: 50 GB sparse (`dd if=/dev/zero of=… bs=1 count=0 seek=50G`).
Filesystem: ext4 with label `store_<u>`. Mountpoint: `/srv/store/mnt/<u>`,
0700, owned by host uid for `<u>`.

---

## 1. Mission and responsibilities

`enrol` is the single browser-driven operator interface for:

1. **Authelia users** — list / create / edit / delete (group membership,
   email, displayname, password rotation). TOTP secret generation +
   one-shot QR display. Atomic write of `users_database.yml` with
   tmpfile+rename so authelia's file watcher reloads cleanly.

2. **LUKS storage** per user — create / change-passphrase / unlock
   (mount) / lock (unmount) / delete. Volumes follow the layout in
   `docs/design.md` § Filesystem Layout: 50 GB sparse blob at
   `/srv/store/data/<u>.img`, mountpoint `/srv/store/mnt/<u>`, mapper
   `store_<u>`. Volume creation is one of the steps in user-create;
   volume deletion is part of user-delete (`shred -uvz` then `rm`).

3. **Devices (peers)** — same model as the original `enrol` (gw0 peer
   list, AmneziaWG keys, AllowedIPs management). Device naming is
   constrained to `<user>-<tag>` (e.g. `alice-laptop`, `alice-phone`)
   so the UI can group devices by their owning user.

4. **Audit log** — append-only JSON-line log on host disk
   (`/etc/amnezia/amneziawg/peers-audit.log`, kept under that path
   for backwards compatibility — peers, users, and LUKS events all
   land in the same file). Action namespace expanded:

   ```
   peer.add        peer.remove
   user.create     user.update     user.delete     user.password
   luks.create     luks.passphrase luks.unlock     luks.lock     luks.delete
   totp.generate   totp.delete
   csrf.fail
   ```

---

## 2. Architecture

```
                                                                    ┌──────────────────────────┐
                              ┌──────────────────────────────────►  │ users_database.yml       │
                              │  YAML write (tmpfile+rename)        │  /opt/stacks/authelia/   │
                              │                                     │  users_database.yml      │
                              │                                     │  (rw bind-mount)         │
                              │                                     └──────────────────────────┘
 browser ─→ NPM ─→ enrol  ────┤                                     ┌──────────────────────────┐
              forward-auth    │  docker exec authelia               │  authelia container      │
              Remote-User     ├─────────────────────────────────►   │  → CLI: storage user totp│
              Remote-Groups   │  via bind-mounted /var/run/docker.sock          generate / delete │
                              │                                     └──────────────────────────┘
                              │                                     ┌──────────────────────────┐
                              │  cryptsetup / mkfs / mount /        │ /srv/store/data/<u>.img  │
                              ├─────────────────────────────────►   │ /srv/store/mnt/<u>       │
                              │  shred (privileged container)       │ (rw bind-mount)          │
                              │                                     └──────────────────────────┘
                              │                                     ┌──────────────────────────┐
                              │  awg syncconf (network_mode: host)  │ /etc/amnezia/amneziawg/  │
                              └─────────────────────────────────►   │ gw0.conf, peers-audit.log│
                                                                    └──────────────────────────┘
```

- enrol shares the host's network namespace (`network_mode: host`)
  so `awg syncconf gw0` still works without nsenter (carried over from
  the prior plan-B compose).
- `privileged: true` is added so cryptsetup can drive loop devices and
  dm-mapper. AppArmor profile is the docker default (overridden only
  if dm setup is blocked, which it isn't on this host).
- No new docker network — the only inbound is from NPM via the docker
  bridge gateway IP `172.17.0.1:8080`, kept from the prior compose.

### Bind-mounts (final)

| Host path | Container path | Mode | Purpose |
|---|---|---|---|
| `/etc/amnezia/amneziawg` | same | rw | gw0.conf, peers-meta.json, peers-audit.log |
| `/srv/store` | same | rw + `:rshared` | LUKS blobs + mountpoints (rshared so cryptsetup-driven mounts inside the container are visible on the host and vice versa) |
| `/opt/stacks/authelia/users_database.yml` | `/etc/authelia/users_database.yml` | rw | YAML edits |
| `/var/run/docker.sock` | same | rw | for `docker exec authelia …` |
| `/usr/bin/awg`, `/usr/bin/awg-quick` | same | ro | host's AmneziaWG userspace (kept from prior compose) |

---

## 3. Privilege & threat model

- **enrol effectively has root on the host.** Everyone with admin
  access to the UI can create LUKS volumes, mount/unmount them, edit
  Authelia users (incl. their own group membership), regenerate TOTP
  secrets, and shred data. This is documented in big bold letters in
  `README.md`.

- Mitigations:
  - NPM access list (TLS only, no plain http) + Authelia forward-auth.
  - Authelia policy `two_factor`, `subject: group:admins`.
  - CSRF tokens on every POST. Token bound to the session cookie.
  - Audit log in append-only JSONL.
  - The container exposes only `172.17.0.1:8080`, NOT the public WAN.
  - No SSH path; the docker socket and `/srv/store` are the actual
    reachability vectors and they require either container shell
    access or compromise of the enrol Go binary itself.

- **What this does NOT defend against:**
  - A co-admin going rogue (out of scope per `docs/design.md` threat
    model — co-admins can already read each other's data).
  - Compromise of the Authelia portal (would let an attacker into
    enrol with admin role).
  - Compromise of the docker daemon / host root via any means
    other than enrol — once host root is achieved, the LUKS keys
    in memory of unlocked volumes are exfiltratable.

---

## 4. Data model

### 4.1 User

```go
type User struct {
    Name        string    // [a-z0-9]{1,32}, lowercase
    DisplayName string
    Email       string
    Groups      []string  // default ["admins"]
    Disabled    bool
    PWHash      string    // argon2id PHC, never displayed
}
```

- The `Name` is the YAML map key in `users_database.yml` and is the
  unix username on the host. enrol creates a host user (`useradd -M
  -s /usr/sbin/nologin <u>`) on user-create so `chown` on the
  mountpoint resolves to a real uid. `useradd -M` skips home dir
  creation; the LUKS mountpoint is the user's data area.

### 4.2 LUKS volume

```go
type Volume struct {
    User       string
    ImagePath  string  // /srv/store/data/<u>.img
    Mountpoint string  // /srv/store/mnt/<u>
    Mapper     string  // store_<u>
    Mounted    bool    // queried at render time
    SizeBytes  int64   // stat of .img
    KeyslotsUsed int   // 0..7 from `cryptsetup luksDump`
}
```

- Existence is determined by file stat (`<.img>` exists ⇒ volume
  exists). Adoption of pre-existing volumes is automatic — enrol does
  not have a "register existing volume" step.

### 4.3 Device (peer)

Carried over from prior version. Sidecar JSON map keyed by public key:

```go
type Peer struct {
    Name      string  // "<user>-<tag>"
    DeviceTag string  // laptop | phone | tablet | other
    User      string  // derived from Name prefix; "" if unmanaged
    PublicKey string
    IP        string
    AddedBy   string
    AddedAt   time.Time
}
```

- Group-by-user in the UI is computed at render time: split on `-`,
  take the first segment, look up against the known user list.
  Peers whose prefix doesn't match a known user fall under
  "Unassigned".

---

## 5. HTTP API & HTML routes

Public, all behind Authelia forward-auth:

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET  | `/`                          | required        | redirect → `/users` |
| GET  | `/users`                     | admins          | list users (HTML) |
| POST | `/users`                     | admins + CSRF   | create user (form) |
| GET  | `/users/<u>`                 | admins          | user detail (HTML; LUKS state, devices, edit forms) |
| POST | `/users/<u>/edit`            | admins + CSRF   | update displayname / email / groups |
| POST | `/users/<u>/password`        | admins + CSRF   | rotate Authelia password |
| POST | `/users/<u>/totp`            | admins + CSRF   | regenerate TOTP secret; render QR once |
| POST | `/users/<u>/luks/passphrase` | admins + CSRF   | luksAddKey + luksRemoveKey |
| POST | `/users/<u>/luks/unlock`     | admins + CSRF   | open + mount |
| POST | `/users/<u>/luks/lock`       | admins + CSRF   | umount + close |
| POST | `/users/<u>/delete`          | admins + CSRF   | full teardown |
| GET  | `/peers`                     | admins          | grouped-by-user device list (HTML) |
| POST | `/peers`                     | admins + CSRF   | add peer (form: user + tag) |
| GET  | `/peers/<name>`              | admins          | peer detail (HTML) |
| POST | `/peers/<name>/delete`       | admins + CSRF   | remove peer |
| GET  | `/peers/<name>/config`       | admins          | download .conf |
| GET  | `/peers/<name>/qr.png`       | admins          | QR PNG |
| GET  | `/audit`                     | admins          | last 200 audit JSONL |
| GET  | `/healthz`                   | none            | liveness |

CSRF token is minted on first GET of any HTML page, stored in an
`Enrol-CSRF` cookie (`SameSite=Strict; HttpOnly=false; Secure;
Path=/`), and embedded in every form as `<input type="hidden"
name="csrf" value="…">`. The middleware compares form value vs cookie
on every POST and rejects mismatches with 403 + `csrf.fail` audit
entry. Token is 32 bytes from `crypto/rand`, base64url-no-padding.
Tokens are session-scoped — re-issued on next GET if missing/invalid.

---

## 6. Form flows

### 6.1 Create user (`POST /users`)

Form fields:

- `name` — lowercase, `[a-z0-9]{1,32}`, must not exist yet.
- `displayname`
- `email`
- `password` — at least 12 chars; not displayed back; salted+hashed
  via argon2id with the `configuration.yml` parameters (memory 65536,
  iter 3, parallelism 4, key 32, salt 16).
- `luks_passphrase` — independent of `password`. We deliberately
  separate them so a leaked Authelia password doesn't unlock the data
  blob.

Server steps (atomic — earlier steps undo on later failure):

1. validate form, CSRF
2. `useradd -M -s /usr/sbin/nologin -U <u>` on host (idempotent)
3. argon2id hash the password
4. read `users_database.yml`, splice in the new user, atomic-rename
   write
5. wait briefly for authelia to reload (best-effort: fs-notify gives
   ≤2 s; we don't block)
6. `cryptsetup luksFormat` on a new sparse 50 GB image (writing the
   passphrase to stdin via `--key-file=-`)
7. `cryptsetup open` + `mkfs.ext4 -L store_<u>` + `mount` + `chown`
   + `umount` + `cryptsetup close` (matches
   `scripts/create-store-volume.sh`)
8. `mkdir -p /srv/store/mnt/<u>`, `chown <u>:<u>`, `chmod 0700`
9. `docker exec authelia authelia storage user totp generate <u> --force`
10. parse the otpauth URI from stdout
11. render the response page: TOTP QR (PNG via `qrencode`), otpauth
    URI as plaintext under the QR, password-handoff banner ("show
    once").
12. audit `user.create`, `luks.create`, `totp.generate`

If step 4 fails (authelia file write), nothing is rolled back yet —
we error early. If step 6 or 7 fails after the YAML write, the user
exists in Authelia but has no volume; the user-detail page surfaces
this state and offers a "Create LUKS volume" button.

### 6.2 Delete user (`POST /users/<u>/delete`)

Form: hidden `confirm_name` text field; the JS-less confirm flow is
"type the username to enable the delete button," validated server-side
(reject if `confirm_name != name`).

Server steps (best-effort, log + continue on per-step failure):

1. validate form + CSRF + `confirm_name == name`
2. lock check: if mounted, `umount + cryptsetup close`
3. `shred -uvz /srv/store/data/<u>.img && rm -f /srv/store/data/<u>.img`
4. `rm -rf /srv/store/mnt/<u>` (it should be empty — 0700 means
   nothing to lose)
5. enumerate peers whose name has prefix `<u>-` and remove each from
   gw0.conf + sidecar metadata
6. `docker exec authelia authelia storage user totp delete <u>`
7. read users_database.yml, drop the entry, atomic-rename write
8. `userdel <u>` on host (best-effort; ignore if user has live processes)
9. audit each step (`luks.lock`, `luks.delete`, `peer.remove` × N,
   `totp.delete`, `user.delete`)

The redirect target after delete is `/users` with a flash message.

### 6.3 LUKS unlock / lock

- **Unlock**: form takes the passphrase. enrol pipes it to
  `cryptsetup open --key-file=-` then mounts. Does **not** call the
  systemd template unit — the unit was designed for a tty operator;
  enrol calls cryptsetup directly. The systemd unit
  (`store-mount@.service`) remains valid for SSH operators. Both
  paths converge on the same mapper name + mountpoint.

- **Lock**: `umount + cryptsetup close`. Idempotent.

### 6.4 Change LUKS passphrase

Form: `old`, `new`, `new_confirm`.

Server: `cryptsetup luksAddKey --key-file=<old> <img> <new>` then
`cryptsetup luksRemoveKey --key-file=<new> <img> <old>`. The
add-then-remove order means the user is never left without a working
passphrase even on partial failure. Both passphrases are passed via
stdin file descriptors (no command-line / no temp files).

---

## 7. Migration / adoption of existing state

On first run after deploy:

1. enrol enumerates `users_database.yml` and treats every entry as a
   managed user.
2. For each, it stat()s `/srv/store/data/<u>.img` and reports the
   volume's existence + mounted state. It does **not** mutate
   anything.
3. It enumerates peers from gw0.conf + sidecar JSON, groups them by
   user prefix as described in § 4.3, and renders.

There is no separate "import" step. Any users, peers, and LUKS blobs
that pre-date the wizard appear exactly as if enrol had created them.

---

## 8. CSRF strategy (operational)

- Cookie name: `enrol_csrf`. Path `/`, `Secure`, `SameSite=Strict`,
  not `HttpOnly` (templates need to read it server-side via the
  request, not client-side, but we still avoid HttpOnly=true for
  symmetry — there is no JS in the app).
- Token: 32 random bytes, base64url-no-padding (43 chars).
- Mint flow: any GET handler that renders a form calls
  `ensureCSRF(w, r)` which reads or sets the cookie and stashes the
  token on the request context.
- Verify flow: every POST handler calls `requireCSRF(r)`. The form
  must POST a `csrf` field whose value matches the cookie. Mismatch
  → 403, `csrf.fail` audit entry.
- Renewal: token rotates every 24 h (cookie max-age) and on
  successful logout (we don't have an explicit logout — Authelia
  owns sessions — so this is a TODO if-and-when Authelia signals
  logout to the upstream).

---

## 9. Error handling and idempotency

- Every mutating handler holds `s.muMaster` (a single mutex) for the
  duration of state changes. This serialises user-create vs. peer-add
  vs. luks-unlock at minimal cost — the UI is operator-only, two
  admins.
- Steps that touch disk use atomic-rename writes wherever possible.
  Anything that touches the kernel (cryptsetup, mount, useradd) is
  retry-once-then-fail.
- The audit log is the post-mortem trail. Every handler records
  result `ok` or `fail` with a brief detail string.
- Authelia hot-reload latency is 1–3 s typical. UI redirects
  post-create do not wait — the response page just renders; if the
  user clicks "log in as new account" within 3 s they may hit a 401
  briefly. Acceptable.

---

## 10. Operator runbook (in-UI guidance)

Every page that triggers an irreversible operation includes:

- A short "what this does" note above the button.
- For destructive ops, a "type the name to confirm" gate.
- For one-shot disclosures (TOTP QR, generated password), a banner
  saying "shown once; will not be retrievable" and a copy-to-clipboard
  button (no JS — uses a plain textarea + browser select).

If the operator escapes mid-flow (e.g. closes the tab right after
user-create but before saving the TOTP QR), the recovery is:
**`POST /users/<u>/totp`** to regenerate. The previous QR is
invalidated.

If Authelia file-watch reload misfires (rare), the recovery is
`docker exec authelia kill -HUP 1` from a host shell. enrol does
not currently expose this; planned future work if it becomes
relevant.

---

## 11. What this version does NOT do

- WebAuthn / hardware-key registration for users (Authelia config
  has webauthn disabled).
- SMTP-based password reset (Authelia config has it disabled).
- Multi-tenant role assignment beyond `admins` group.
- Per-volume size customisation — every new volume is 50 GB sparse.
  Resizing is a manual op via `dd if=/dev/zero seek=…` + `resize2fs`,
  not exposed in the UI.
- Backup integration (restic is client-pull; nothing to do server-side).
- Quotas / accounting per user.

These are intentional out-of-scope items for this revision.

---

## 12. File ownership inside `stacks/enrol/`

```
stacks/enrol/
├── DESIGN.md            (this file)
├── README.md            operator-facing summary + privilege warning
├── Dockerfile           multi-stage: golang:1.23 → debian:trixie-slim
├── docker-compose.yml   privileged: true + new bind-mounts
├── go.mod               stdlib + gopkg.in/yaml.v3
├── go.sum               (committed; vendored deps deterministic)
├── main.go              entry point; wires routes + middleware
├── auth.go              Remote-User / Remote-Groups middleware (carry-over)
├── csrf.go              CSRF mint+verify
├── audit.go             JSONL audit (carry-over, expanded actions)
├── users.go             users_database.yml read/write + argon2id
├── luks.go              cryptsetup / mkfs / mount / shred
├── totp.go              docker exec → authelia storage user totp
├── peers.go             gw0.conf parser/writer (carry-over)
├── server.go            handlers + template wiring
└── web/
    ├── static/style.css
    └── templates/
        ├── _layout.html        nav: users / devices / audit
        ├── users.html
        ├── user-detail.html
        ├── user-created.html   one-shot TOTP QR + password handoff
        ├── peers.html          grouped-by-user
        ├── peer-created.html
        └── peer-detail.html
```

The original single-file `main.go` (~1000 LOC) is split into the
files above for readability.
