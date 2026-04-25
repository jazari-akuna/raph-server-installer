# enrol — peer-management web UI for `gw0`

Public hostname: `enrol.antarctica-engineering.com`. Auth delegated to
Authelia via NPM forward-auth: enrol trusts `Remote-User` and
`Remote-Groups` headers. Membership of group `admins` is required for
all mutating routes.

This stack co-exists with `scripts/provision-peer.sh` (CLI). Both write
to the same source of truth (`/etc/amnezia/amneziawg/gw0.conf`); they
do not conflict, but operators should pick one workflow per peer.

## Layout

```
stacks/enrol/
├── docker-compose.yml
├── Dockerfile           # multi-stage: golang:1.23 → debian:bookworm-slim
├── go.mod               # stdlib-only (no external deps)
├── main.go              # ~900 LOC; entire app
├── web/
│   ├── templates/*.html
│   └── static/style.css
├── .env.example
└── README.md
```

## Auth flow

1. User browses `https://enrol.antarctica-engineering.com/peers`.
2. NPM's auth_request subrequest hits `http://authelia:9091/api/verify`.
3. If no session → 401 → NPM's `error_page 401 = ...` redirects to
   `https://auth.antarctica-engineering.com/?rd=…`.
4. User logs in (TOTP-required), Authelia sets the SSO cookie on the
   apex domain, redirects back.
5. Now the auth_request returns 200; Authelia writes
   `Remote-User: sagan` and `Remote-Groups: admins` into the response.
6. NPM forwards them as request headers to `enrol`.
7. enrol's middleware reads those headers and gates accordingly.

If `Remote-User` is absent, enrol returns 401 with a one-line message
pointing at the SSO portal — that condition only happens if NPM is
misconfigured.

## Privilege & host integration

- Container runs with `cap_add: [NET_ADMIN, SYS_ADMIN]` and
  `pid: host` so it can `nsenter --target 1 --net --mount` into init's
  net+mount namespaces and call the host's `awg` / `awg-quick` binaries.
- `/etc/amnezia/amneziawg` is bind-mounted **rw**: gw0.conf,
  peers-meta.json, peers-audit.log, and gw0_public.key all live there.
- After mutating gw0.conf the app calls
  `nsenter --target 1 --net --mount -- bash -c 'awg syncconf gw0 <(awg-quick strip /etc/amnezia/amneziawg/gw0.conf)'`.
  If that fails (e.g. awg-quick not on the host's PATH) it falls back to
  vanilla `wg syncconf` / `wg-quick strip`. If both fail, the user sees
  a banner: "config saved; reload required: `sudo systemctl restart awg-quick@gw0`".

## Storage model

| File | Role | Owner |
|---|---|---|
| `/etc/amnezia/amneziawg/gw0.conf` | Source of truth (peers + interface params). | install-gw0.sh + enrol + provision-peer.sh |
| `peers-meta.json` | Sidecar map: pubkey → {name, device_tag, added_by, added_at}. | enrol |
| `peers-audit.log` | Append-only JSON-line log of add/remove events. | enrol |
| `gw0_public.key` | Cached server pubkey. | install-gw0.sh |

Peers added via `provision-peer.sh` show up in enrol as "(unmanaged)"
because they have no sidecar metadata. They're still listed and can
still be removed via the UI.

The peer IP allocation algorithm matches `provision-peer.sh`: parse all
`AllowedIPs` host octets in `10.99.0.0/24`, pick the lowest unused
≥10.

## Deploy

On the VPS, after authelia is up and after NPM has been wired:

```sh
cd /opt/stacks/enrol
docker compose build
docker compose up -d
docker compose logs -f enrol
```

Then create the NPM proxy host:

```sh
NPM_URL=http://127.0.0.1:81 \
NPM_EMAIL=raphaelcasimir.inge@gmail.com \
NPM_PASS=changeme \
/opt/stacks/authelia/scripts/wire-npm-routes.sh
```

(The script idempotently creates / updates all four hosts — see the
authelia README.)

## Environment

| Var | Default | Meaning |
|---|---|---|
| `ENROL_LISTEN` | `:8080` | bind address |
| `ENROL_AWG_DIR` | `/etc/amnezia/amneziawg` | where gw0.conf lives |
| `ENROL_AWG_IFACE` | `gw0` | interface name |
| `ENROL_AWG_ENDPOINT` | `gw.antarctica-engineering.com:51820` | written into client configs |
| `ENROL_PEER_SUBNET` | `10.99.0.0/24` | peer subnet |
| `ENROL_PEER_START` | `10` | first host octet to assign |
| `ENROL_HEADER_USER` | `Remote-User` | header carrying authenticated username |
| `ENROL_HEADER_GROUPS` | `Remote-Groups` | header carrying comma-separated groups |
| `ENROL_REQUIRED_GROUP` | `admins` | group required for mutating routes |

## Security notes

- The private key is generated **inside** the container (Curve25519,
  crypto/rand). It is shown to the user **once** on the post-add page
  and never persisted. If the user navigates away without saving the
  config, the peer survives but the key is unrecoverable — they must
  delete and re-add.
- enrol does not call out to the public internet at any point.
- enrol does not run a database. State is files on disk.
- Container memory limit: 128 MB.

## Endpoints

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET  | `/peers`             | admins | dashboard |
| POST | `/peers`             | admins | add peer (form: name, device_tag) |
| GET  | `/peers/{name}`      | admins | detail |
| POST | `/peers/{name}/delete` | admins | remove |
| GET  | `/peers/{name}/config` | admins | download .conf (without privkey) |
| GET  | `/peers/{name}/qr.png` | admins | QR PNG |
| GET  | `/audit`             | admins | last 200 audit entries (JSON) |
| GET  | `/healthz`           | none | liveness probe |
