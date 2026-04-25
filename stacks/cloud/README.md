# cloud

File server for the shared VPS. Internally this is
[copyparty](https://github.com/9001/copyparty); externally and in all
paths/names it is `cloud`. See `docs/plan.md` step 7.

Reachable only via `ingress` at `cloud.antarctica-engineering.com`.
There are no public port bindings on this service.

## Hard rules

- **No public ports.** This stack never publishes to the host. The only
  way in is through `ingress` over the `edge` Docker network.
- **ACLs are strictly per-user.** `sagan` has no access to `/marcus`
  and vice versa. Do not add a shared volume without re-opening that
  decision.
- **No anonymous access.** Every request must authenticate.
- **Per-user data lives behind LUKS.** `/srv/store/mnt/sagan` and
  `/srv/store/mnt/marcus` on the host are LUKS unlock points (see
  build-sequence step 6). When the blob is unmounted, the path is an
  empty directory — copyparty then sees an empty volume, which is the
  desired fail-closed behaviour.

## Layout

```
stacks/cloud/
├── docker-compose.yml      # service definition (image pinned)
├── conf/
│   └── copyparty.conf      # accounts + volumes + ACLs (mounted /cfg:ro)
├── data/                   # persistent state (hash DB, thumbs); gitignored
├── .env.example            # placeholders for argon2 password hashes
└── README.md               # this file
```

The `data/` directory is created on first run and holds copyparty's
internal state: per-volume upload index, thumbnail cache, the salt file
under `ah-salt.txt`, etc. It is gitignored and treated as cache (safe
to delete; will be rebuilt; thumbnails will regenerate on access).

## Generating password hashes

copyparty stores passwords as argon2 hashes (`ah-alg: argon2` in the
config). Generate a hash by running the bundled CLI inside a throwaway
copyparty container so the version and algorithm match the running
service exactly:

```sh
docker run --rm -it copyparty/ac:1.20.14 \
    --ah-alg argon2 --ah-cli
```

It prompts for a password twice and prints one line beginning with `+`
(e.g. `+argon2id$v=19$m=...$<salt>$<hash>`). Paste that entire line —
**including the leading `+`** — into the `.env` on the VPS:

```
# /opt/stacks/cloud/.env  (chmod 0600)
SAGAN_PW_HASH=+argon2id$v=19$m=...$...$...
MARCUS_PW_HASH=+argon2id$v=19$m=...$...$...
```

Then `docker compose up -d` from `/opt/stacks/cloud/`. Confirm the
hashes were applied by trying to log in over the `ingress` proxy.

If you change the password for an account, regenerate the hash and
restart the container; copyparty does not re-read its config without
a restart (`docker compose restart cloud`).

If you ever change `ah-alg`, every existing hash becomes invalid —
regenerate all of them in one pass.

## Bind-mount semantics (LUKS interaction)

`/srv/store/mnt/sagan` and `/srv/store/mnt/marcus` on the host are the
mount points for each user's encrypted blob (see plan step 6). The
compose file bind-mounts them into the container at `/w/sagan` and
`/w/marcus`. Three states matter:

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

## Adding the proxy host in `ingress` (NPM)

Once `ingress` is up with the wildcard cert provisioned for
`*.antarctica-engineering.com`, add `cloud` as a proxy host:

1. Open the NPM admin UI (loopback / SSH-tunnel /
   `mesh` — never public). `Hosts -> Proxy Hosts -> Add Proxy Host`.
2. **Details tab**
   - Domain Names: `cloud.antarctica-engineering.com`
   - Scheme: `http`
   - Forward Hostname / IP: `cloud`  (the Docker DNS name of this
     service on the `edge` network; resolves because `ingress` shares
     that network)
   - Forward Port: `3923`  (copyparty's default)
   - Cache Assets: off
   - Block Common Exploits: on
   - Websockets Support: **on**  (copyparty's UI uses websockets for
     uploads and live progress)
3. **SSL tab**
   - SSL Certificate: pick the existing `*.antarctica-engineering.com`
     wildcard cert.
   - Force SSL: on
   - HTTP/2 Support: on
   - HSTS Enabled: on (only after you've confirmed the host works end
     to end — HSTS is sticky)
4. Save. Test from outside the VPS:
   ```sh
   curl -sI https://cloud.antarctica-engineering.com
   ```
   Expect HTTP 200 (or 401 challenge), valid cert covering the
   wildcard, no `Server: copyparty` banner leaking through (copyparty
   doesn't advertise itself in the default banner — fine).

The `ingress` container must be on the `edge` network for the
`cloud:3923` Docker-DNS lookup to work; it is, by virtue of its own
compose file. If you see `host not found`, check that both stacks are
attached to the `edge` external network.

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
before bumping — copyparty's config syntax is occasionally extended.

## Operational notes

- **Memory limit is 384 MB.** copyparty is small but the `/ac` image
  bundles ffmpeg and Pillow for thumbnails; transcoding a large media
  library can spike. If you see OOM-kills in `dmesg`, raise the limit
  in `docker-compose.yml` rather than removing it.
- **Restart on config change.** Edits to `conf/copyparty.conf` only
  apply after `docker compose restart cloud`. The config volume is
  mounted read-only; the container cannot mutate it from inside.
- **Hashes regenerate on first run.** The first request after a fresh
  `data/` directory triggers a hash sweep across mounted volumes.
  Expect elevated CPU for a few minutes per user with a non-empty
  blob; subsequent restarts are quick.
- **Don't add `r: *` anywhere.** That would make the volume world-
  readable and break the no-anonymous-access invariant.
