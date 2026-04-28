# ingress (NPM) — runbook

`ingress` is the only service on the VPS that listens on public :80 and :443.
It's Nginx Proxy Manager under the hood; the camouflage name is `ingress`
everywhere outside this file. Its admin UI on :81 is bound to 127.0.0.1 and
is reachable only via the `mesh` overlay or an SSH tunnel — there is no
public hostname for it.

This runbook covers the first deploy: DNS prerequisites, OVH API
credentials, initial admin account, wildcard cert via DNS-01, and the first
proxy host (`cloud.${DOMAIN}`).

DNS for `${DOMAIN}` is hosted at the `${DNS_PROVIDER}` registrar (default: OVH).
We use the DNS-01 challenge with the bundled `dns-ovh` certbot plugin against the
OVH API. No CDN / DNS proxy sits in front of the records (see §5).

---

## 1. DNS prerequisites (do this BEFORE requesting the cert)

The Let's Encrypt DNS-01 challenge writes a `_acme-challenge` TXT record
into the zone via the OVH API. The base records below must already exist
so the cert covers the names we actually serve. All records are plain A
records, no CDN proxy in front.

| Record                                | Type | Target   | Notes  |
|---------------------------------------|------|----------|--------|
| `${DOMAIN}`          | A    | `VPS_IP` | direct |
| `*.${DOMAIN}`        | A    | `VPS_IP` | direct |
| `gw.${DOMAIN}`       | A    | `VPS_IP` | direct |
| `cdn.${DOMAIN}`      | A    | `VPS_IP` | direct |

The wildcard plus the apex are what the cert request below will cover.
`gw.` and `cdn.` are required by other stacks (see `docs/design.md` DNS
Layout section) and should be in place at the same time.

Verify before continuing:

```sh
dig +short ${DOMAIN}
dig +short anything.${DOMAIN}   # wildcard
dig +short gw.${DOMAIN}
dig +short cdn.${DOMAIN}
```

All four must return the VPS public IP.

---

## 2. OVH API credentials

Create an OVH application token scoped to **only** this single zone.

1. Visit the OVH token creator for your region:
   - **Europe / .com domains:** <https://auth.eu.ovhcloud.com/api/createToken>
   - **Canada / .ca domains:** <https://auth.ca.ovhcloud.com/api/createToken>
   - **US domains:** <https://api.us.ovhcloud.com/createToken/>
2. Fill in:
   - **Application name:** `<your-domain> DNS-01` (anything
     descriptive — visible only in your OVH manager).
   - **Validity:** unlimited (we rotate manually).
   - **Rights:** add five rows, scoped tightly to the single zone.
     The leading `GET /domain/zone/` (note: no wildcard, just that one
     literal path) is required by `certbot-dns-ovh` to enumerate zones
     when the rest of the grants are zone-scoped — without it, certbot
     fails with `403 Forbidden for url: https://eu.api.ovh.com/1.0/domain/zone/`:
     - `GET    /domain/zone/`
     - `GET    /domain/zone/${DOMAIN}/*`
     - `PUT    /domain/zone/${DOMAIN}/*`
     - `POST   /domain/zone/${DOMAIN}/*`
     - `DELETE /domain/zone/${DOMAIN}/*`
     Do **not** grant `/domain/zone/*` (all zones).
3. Submit. OVH shows three values:
   - `application_key`
   - `application_secret`
   - `consumer_key`
   Save all three immediately — `consumer_key` is shown only once.
4. Note your endpoint identifier: `ovh-eu`, `ovh-ca`, `ovh-us`, etc. —
   must match the region of the OVH account that owns the domain.

Operational rules:

- Scope is **DNS read/write on `${DOMAIN}` only**. Nothing else.
- **Rotate yearly.** Calendar reminder lives with the admins.
- **Never commit a real token.** `.env` is gitignored; `.env.example` is
  the only token-shaped file in git, and it holds placeholders.
- The four values are pasted into NPM's "Credentials File Content" field
  during cert creation (step 3d below) in INI form. NPM stores it inside
  `./data` / `./letsencrypt` (bind-mounted in the compose file). Treat
  those directories as secret material.

### Manual-TXT fallback (no API)

If you'd rather not create an OVH application token, the manual mode is:

- Issue the cert via `certbot certonly --manual --preferred-challenges dns`
  on the VPS (NPM's UI does not natively support manual mode — drive
  certbot directly, then import the resulting cert into NPM under
  *SSL Certificates → Add → Custom*).
- Certbot prints a TXT value; you paste it into OVH's DNS panel as
  `_acme-challenge` and `_acme-challenge.*` for the wildcard.
- Renewal repeats this every ~60 days. **Recurring chore — fine for a
  one-off issuance or disaster recovery, not for daily operation.**

---

## 3. First-deploy workflow

### 3a. Bring the stack up

The `edge` Docker network must already exist (created in Step 3 of
`docs/design.md`). Then, on the VPS, either via `console` (Portainer) or
directly:

```sh
cd /opt/stacks/ingress
docker compose up -d
```

NPM listens on :80, :443, and :81. :80/:443 are public; :81 is bound
to 127.0.0.1.

### 3b. Reach the admin UI via SSH tunnel

The admin UI is loopback-only. From an admin laptop:

```sh
ssh -L 81:127.0.0.1:81 <admin>@<vps>
```

Then open `http://127.0.0.1:81` in a local browser. (Once `mesh` is up,
admins can also reach :81 over the overlay without the tunnel.)

### 3c. Initial admin account

NPM ships with default credentials on first run:

- Email: `admin@example.com`
- Password: `changeme`

Log in with these once, then **immediately** change both the email and
the password to a real admin identity. Do this before doing anything
else. Enable any available 2FA / per-user accounts for the admins.

### 3d. Request the wildcard cert (DNS-01, OVH provider)

In the admin UI:

1. **SSL Certificates** -> **Add SSL Certificate** -> **Let's Encrypt**.
2. Domain Names — add **both**:
   - `*.${DOMAIN}`
   - `${DOMAIN}`
3. Email Address — an admin email at least one operator can read.
4. **Use a DNS Challenge** -> ON.
5. DNS Provider -> **OVH**.
6. **Credentials File Content** — paste the four values from §2 in INI
   form, exactly:

   ```ini
   dns_ovh_endpoint = ovh-eu
   dns_ovh_application_key = REPLACE_ME
   dns_ovh_application_secret = REPLACE_ME
   dns_ovh_consumer_key = REPLACE_ME
   ```

   Substitute your actual region for the endpoint. NPM templates this
   into a credentials file and calls `certbot` with the `dns-ovh` plugin.
7. Agree to the LE TOS, Save.

If the DNS records from §1 are correct and the application token has the
three grants on the zone, issuance takes ~30-90s. The cert covers the
apex plus every subdomain we'll ever serve via `ingress`, so this is a
one-time step (plus auto-renewal, which NPM handles).

### 3e. First proxy host: `cloud.${DOMAIN}`

This proves the stack works end-to-end and gives `cloud` (Nextcloud) a
public HTTPS front. (Note: the wizard's `finalizeWireNPM` does this for
you automatically; the manual recipe below is for diagnostics / reference.)

In the admin UI -> **Hosts** -> **Proxy Hosts** -> **Add Proxy Host**:

- **Domain Names**: `cloud.${DOMAIN}`
- **Scheme**: `http`
- **Forward Hostname / IP**: `cloud-web`  (the nginx-sidecar container
  on the `edge` network — Docker DNS resolves it)
- **Forward Port**: `80`  (cloud-web nginx-sidecar listens on :80; NPM
  terminates TLS upstream)
- **Cache Assets**: off (Nextcloud's own cache headers are preferred)
- **Block Common Exploits**: on
- **Websockets Support**: on (required for Nextcloud Talk signaling)
- **SSL** tab:
  - SSL Certificate: the wildcard issued in step 3d.
  - **Force SSL**: on
  - **HTTP/2 Support**: on
  - **HSTS Enabled**: on
  - **HSTS Subdomains**: on (only after the wildcard is in place and
    you're sure every future subdomain will also be HTTPS)
- Save.

Both NPM and `cloud` must be on the Docker network named `edge`.
`edge` is declared external in this stack's `docker-compose.yml`; the
`cloud` stack joins the same network.

Verify from outside the VPS:

```sh
curl -sI https://cloud.${DOMAIN}
# HTTP/2 200, Strict-Transport-Security header present, valid LE cert.
```

---

## 4. Things deliberately NOT exposed via `ingress`

Do not create public proxy hosts for:

- **`console`** (Portainer admin UI). Mesh-only.
- **`ingress`'s own admin UI** (this NPM instance, port 81). Mesh-only /
  SSH-tunnel-only. The compose file binds it to 127.0.0.1 to make this
  hard to undo by accident.

These are the highest-value compromise targets in the stack. Public DNS
for them stays absent. If you need to reach them remotely, use `mesh`
or the SSH tunnel from step 3b.

---

## 5. Note on CDN / DNS-proxy layers (out of scope)

We use OVH for **DNS only**. No reverse-proxy / CDN sits in front of the
records (Cloudflare orange-cloud, OVH's "AlwaysOn", anything similar).
Putting one in front would:

- Break `gw0` (UDP/443 with AmneziaWG framing — HTTP-only proxies don't carry it).
- Break `qedge` (Hysteria2/QUIC on UDP/443 — same reason; also collides with `gw0` if both run).
- Insert a third-party-terminated TLS hop in front of `ingress`, which
  defeats the wildcard cert chain we just issued and adds an unwanted
  MITM.

If proxy-fronting gets re-opened, that's a plan-level decision — go
back to `docs/design.md`. Do not flip the toggle quietly.
