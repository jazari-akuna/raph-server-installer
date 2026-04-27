# cloud

File server for the shared VPS. Internally this is
[copyparty](https://github.com/9001/copyparty); externally and in all
paths/names it is `cloud`. See `docs/design.md` step 7.

Reachable only via `ingress` at `cloud.${DOMAIN}`.
There are no public port bindings on this service.

**As of the SSO migration, identity is delegated to Authelia.** copyparty
runs in IdP / forward-auth mode and trusts the `Remote-User` and
`Remote-Groups` headers that NPM injects after a successful auth_request
against Authelia. Per-user passwords are no longer used.

## Hard rules

- **No public ports.** This stack never publishes to the host. The only
  way in is through `ingress` over the `edge` Docker network.
- **ACLs are strictly per-user.** Each user only has access to their
  own `/u/<u>` path; no cross-user reach. The `[/u/${u}]` volume block
  grants permissions only to `${u}` itself.
- **No anonymous access.** Authelia gates the proxy host with policy
  `two_factor`; copyparty itself rejects requests where
  `Remote-User` is absent (defence in depth).
- **Per-user data lives behind LUKS.** `/srv/store/mnt/<u>` on the
  host are LUKS unlock points, one per user (see build-sequence
  step 6). When the blob is unmounted, the path is an empty directory
  — copyparty then sees an empty volume, which is the desired
  fail-closed behaviour.
- **Trust boundary is the docker network.** `xff-src: 172.16.0.0/12`
  in `conf/copyparty.conf` restricts header trust to docker-internal
  upstreams. Anyone reaching `cloud:3923` from outside the docker
  network — which can't happen because there is no public port —
  would have their forwarded headers ignored.

## Layout

```
stacks/cloud/
├── docker-compose.yml      # service definition (image pinned)
├── conf/
│   └── copyparty.conf      # IdP-mode config: idp-h-usr, idp-h-grp, ${u} volume
├── data/                   # persistent state (hash DB, thumbs); gitignored
├── .env.example            # placeholder; no secrets in IdP mode
└── README.md               # this file
```

The `data/` directory is created on first run and holds copyparty's
internal state: per-volume upload index, thumbnail cache, the salt file
under `ah-salt.txt`, and the IdP user cache (idp-store=3 — remembers
users + groups across restarts so volumes don't have to be re-walked).
It is gitignored and treated as cache (safe to delete; will be rebuilt;
thumbnails will regenerate on access).

## SSO flow (end to end)

```
browser → ingress (NPM, :443) ──auth_request──→ authelia:9091/api/verify
                                                        │
                                            ↓ 200 + Remote-* headers
                                                        │
                                       → cloud:3923 (with Remote-User: <u>)
                                                        │
                                       → copyparty serves /u/<u>
```

If the user has no Authelia session, the auth_request returns 401, NPM
issues a 302 to `https://auth.${DOMAIN}/?rd=…`, the
user logs in (TOTP-required), Authelia redirects them back, the auth
cookie is now set on the apex `${DOMAIN}` so
`cloud.${DOMAIN}` shares it.

## Bind-mount semantics (LUKS interaction)

The host's `/srv/store/mnt` directory is bind-mounted into the
container as `/w` (a **single** mount, not per-user). copyparty's
IdP rule resolves the `${u}` placeholder at request time:

```
[/u/${u}]
  /w/${u}
```

So an authenticated request from `<u>` is served from `/w/<u>`
which is `/srv/store/mnt/<u>` on the host. Adding a new user is
purely a host-side operation — `enrol` creates the LUKS blob, the
mountpoint, and the Authelia user, and copyparty picks it up on
the next request without any cloud-side change. (Earlier versions
used static per-user bind-mounts and required editing this
compose every time a user was added; we removed that.)

The bind-mount uses `propagation: rslave` so that subsequent host
mounts under `/srv/store/mnt/<u>` (when enrol or `mount-stores.sh`
unlocks a LUKS blob) become visible inside the container at
`/w/<u>`. With the default `rprivate` propagation, the container
keeps seeing the underlying empty directory even after the host
unlocks the volume — that was a real bug we hit and fixed. See
`../enrol/DESIGN.md` § 0.3.

Three states matter:

| Host state                                  | What `cloud` sees                  |
|---|---|
| Blob unmounted (default after reboot)       | empty directory; volume looks empty in the UI |
| Blob mounted but you have no permission     | n/a — same UID maps; permissions handled by LUKS+ext4 owner |
| Blob mounted and the owning user logs in    | their files |

This is fail-closed by design: if the host reboots, nobody's files
appear in `cloud` until an admin manually unlocks the relevant blob
with `mount-stores.sh`. Passphrases are never on disk. Empty volumes
in the web UI after reboot are *expected*, not a bug.

If a user reports "my files are gone": check `mountpoint -q
/srv/store/mnt/$user` on the host before assuming any data loss.

## Migration from password-mode (one-time)

If `data/` already exists from a pre-SSO deploy, copyparty's user index
remembers the old `[accounts]` users — but those accounts are now
unreachable (no password is ever sent through Authelia). They're
harmless. New per-user volumes are created via the IdP path on first
hit. Optional: clean up by deleting `data/up2k.snap` and letting
copyparty rebuild.

The `.env` file (which used to hold per-user `<USER>_PW_HASH` values)
is now obsolete on the VPS; delete it post-migration:

```sh
sudo rm /opt/stacks/cloud/.env
```

## Adding the proxy host in `ingress` (NPM)

Use the wire-up script in the authelia stack:

```sh
NPM_URL=http://127.0.0.1:81 \
NPM_EMAIL=admin@example.com \
NPM_PASS=changeme \
/opt/stacks/authelia/scripts/wire-npm-routes.sh
```

The script idempotently UPDATES the existing `cloud` proxy host (host
id 1 in the current NPM state) with the forward-auth Advanced config
that includes the snippets from `/opt/stacks/authelia/snippets/`.

Verify from outside the VPS:

```sh
# Without a session: redirect to Authelia portal.
curl -sIL https://cloud.${DOMAIN} | head -20
# Expect: 302 → https://auth.${DOMAIN}/?rd=...
```

## Steady state

From `/opt/stacks/cloud/` on the VPS:

```sh
docker compose up -d
docker compose ps
docker compose logs -f cloud
```

To upgrade the pinned image, edit the tag in `docker-compose.yml`
(currently `copyparty/ac:1.20.14`), commit on the laptop, deploy, then:

```sh
docker compose pull && docker compose up -d
```

Check the [release notes](https://github.com/9001/copyparty/releases)
before bumping — copyparty's config syntax is occasionally extended,
and IdP-mode directives in particular have evolved.

## Operational notes

- **Memory limit is 2 GB** (per-app cap from the plan). copyparty is
  small but the `/ac` image bundles ffmpeg and Pillow for thumbnails;
  transcoding a large media library can spike. The 2 GB ceiling is a
  cap, not a guarantee — typical RSS stays well below. If you see
  OOM-kills in `dmesg`, the limit is the wrong knob to turn first;
  investigate which workload is eating the budget.
- **Restart on config change.** Edits to `conf/copyparty.conf` only
  apply after `docker compose restart cloud`. The config volume is
  mounted read-only; the container cannot mutate it from inside.
- **Hashes regenerate on first run.** The first request after a fresh
  `data/` directory triggers a hash sweep across mounted volumes.
  Expect elevated CPU for a few minutes per user with a non-empty
  blob; subsequent restarts are quick.
- **Don't add `r: *` anywhere.** That would make a volume world-
  readable and break the no-anonymous-access invariant — even though
  Authelia would gate it at the front, defence in depth matters.
