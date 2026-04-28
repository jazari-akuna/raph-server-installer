# authelia — SSO portal

Authelia is the SSO gate for the shared VPS. Internally and externally
the host is `auth.${DOMAIN}`. It serves two roles:

1. **Forward-auth** for `enrol`, `cloud`, and `console` (NPM does
   `auth_request` against `http://authelia:9091/api/verify`).
2. **OIDC IdP** for Portainer (`console`).

See `DESIGN.md` for the full design rationale.

## Hard rules

- Three core secrets (`jwt-secret`, `session-secret`,
  `storage-encryption-key`, `oidc-hmac-secret`) plus the OIDC issuer
  private key live under `./secrets/`, mode 0600 root:root. Never
  committed.
- The `users_database.yml` (with argon2id password digests) is
  gitignored. Only `.example` lives in the repo.
- TOTP is **required** for every protected service (policy
  `two_factor`). On first login each user enrols their TOTP via the
  Authelia portal — the deploy report includes the enrolment URL /
  ASCII QR for scanning.
- Memory limit 256 MB. Authelia is small.
- No public port binding. NPM proxies `auth.${DOMAIN}`
  → `authelia:9091` over the `edge` Docker network.

## Layout

```
stacks/authelia/
├── docker-compose.yml
├── configuration.yml          # main config (committed; no secrets inside)
├── users_database.yml.example # bootstrap shell (no real hash committed)
├── .env.example               # placeholders for secret env vars
├── DESIGN.md                  # design doc (committed)
├── README.md                  # this file
├── secrets/                   # gitignored: real secret material
│   ├── jwt-secret
│   ├── session-secret
│   ├── storage-encryption-key
│   ├── oidc-hmac-secret
│   └── oidc-key.pem           # RSA-2048 PKCS#8 OIDC issuer key
├── data/                      # gitignored: sqlite DB + notifier state
├── snippets/                  # NPM forward-auth snippets (committed)
│   ├── authelia-location.conf
│   ├── authelia-authrequest.conf
│   └── proxy.conf
└── scripts/                  # (formerly held wire-npm-routes.sh — removed;
                              #  proxy-host wiring now lives in
                              #  stacks/enrol/setup.go via finalizeWireNPM)
```

## Deploy runbook

These steps run on the VPS as the bootstrap admin (NOPASSWD sudo). The
repo path on the VPS is `/opt/stacks/authelia/` (rsynced from the laptop).

### 1. Generate secrets

```sh
sudo install -d -m 0700 -o root -g root /opt/stacks/authelia/secrets
sudo install -d -m 0700 -o root -g root /opt/stacks/authelia/data

# 64-byte secrets, written via tee so plaintext never enters argv.
for name in jwt-secret session-secret storage-encryption-key oidc-hmac-secret; do
    openssl rand -base64 64 \
        | tr -d '\n' \
        | sudo tee "/opt/stacks/authelia/secrets/$name" >/dev/null
    sudo chmod 0600 "/opt/stacks/authelia/secrets/$name"
done

# RSA-2048 OIDC issuer key (PKCS#8 PEM).
TMP="$(mktemp)"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$TMP"
openssl pkcs8 -topk8 -inform PEM -outform PEM -nocrypt -in "$TMP" \
    | sudo tee /opt/stacks/authelia/secrets/oidc-key.pem >/dev/null
shred -u "$TMP"
sudo chmod 0600 /opt/stacks/authelia/secrets/oidc-key.pem
```

### 2. Generate Portainer OIDC client secret + hash, render config

```sh
PORTAINER_CLIENT_SECRET="$(openssl rand -hex 32)"
echo "PORTAINER_CLIENT_SECRET=$PORTAINER_CLIENT_SECRET"   # save for Portainer UI step

PORTAINER_CLIENT_SECRET_HASH="$(docker run --rm authelia/authelia:4.39.19 \
    authelia crypto hash generate pbkdf2 --variant sha512 \
    --password "$PORTAINER_CLIENT_SECRET" \
    | awk '/Digest:/ {print $2}')"

# Render configuration.yml from the .template, substituting only the one
# placeholder. envsubst with an explicit allow-list keeps Authelia's own
# `${...}` template syntax (used by Authelia's runtime template engine)
# untouched.
AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH="$PORTAINER_CLIENT_SECRET_HASH" \
    envsubst '$AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH' \
    < /opt/stacks/authelia/configuration.yml.template \
    | sudo tee /opt/stacks/authelia/configuration.yml >/dev/null
sudo chmod 0600 /opt/stacks/authelia/configuration.yml

# Stash the hash in .env too in case other tooling wants to re-render
# without regenerating.
sudo tee /opt/stacks/authelia/.env >/dev/null <<EOF
AUTHELIA_OIDC_CONSOLE_CLIENT_SECRET_HASH=$PORTAINER_CLIENT_SECRET_HASH
EOF
sudo chmod 0600 /opt/stacks/authelia/.env
```

**Save `$PORTAINER_CLIENT_SECRET` (plaintext) — it's needed for
the Portainer UI step in `stacks/console/README.md`.**

### 3. Generate user password hashes

```sh
HASH="$(docker run --rm authelia/authelia:4.39.19 \
    authelia crypto hash generate argon2 \
    --variant argon2id --iterations 3 --memory 65536 --parallelism 4 \
    --key-length 32 --salt-length 16 --password 'changeme' \
    | awk '/Digest:/ {print $2}')"

sudo cp /opt/stacks/authelia/users_database.yml.example \
        /opt/stacks/authelia/users_database.yml
sudo sed -i "s|\\\$argon2id\\\$v=19\\\$m=65536,t=3,p=4\\\$REPLACE_ME\\\$REPLACE_ME|$HASH|g" \
        /opt/stacks/authelia/users_database.yml
sudo chmod 0600 /opt/stacks/authelia/users_database.yml
```

(Both users get the same `changeme` digest at bootstrap. Operator rotates
each independently after first login.)

### 4. Bring Authelia up

```sh
cd /opt/stacks/authelia
docker compose up -d
docker compose logs -f authelia      # wait for "Server listening on..." line
```

### 5. Wire NPM

Proxy hosts (auth, enrol, cloud, console, plane) are upserted by enrol's
finalize step — see `finalizeWireNPM` in `stacks/enrol/setup.go`. The
wizard runs it once during install; re-running enrol's finalize is
idempotent and updates any host whose `forward_host` / `advanced_config`
has drifted.

## First-login flow (operator)

1. Browse `https://auth.${DOMAIN}/`. Authelia portal
   appears (Authelia handles its own TLS termination via NPM in front).
2. Log in as the bootstrap admin / `changeme`.
3. The portal demands second-factor enrolment → click "Register device"
   → scan the displayed QR code into Aegis / Google Authenticator /
   1Password / etc., enter the 6-digit code to confirm.
4. Repeat for any additional admins.
5. **Rotate `changeme`** for each user. There are two paths:
   - **Portal-driven**: enable `password_reset` (set `disable: false`
     under `authentication_backend.password_reset` in
     `configuration.yml`), add SMTP creds to the notifier, restart.
     The "Forgot password" flow then works.
   - **Direct edit**: regenerate argon2id digest with the docker
     `authelia crypto hash generate` invocation from §3 (substituting a
     real password), edit `/opt/stacks/authelia/users_database.yml` in
     place (`watch: true` reloads without restart).

## Secret rotation runbook

```sh
# 1. Generate replacement secrets the same way as deploy §1.
# 2. Stop authelia: docker compose stop authelia
# 3. Overwrite the targeted secret file under ./secrets/
# 4. docker compose start authelia
# 5. Sessions invalidate on session-secret rotation (everybody logs in
#    again). storage-encryption-key MUST NOT be rotated without first
#    re-encrypting the sqlite blob — see Authelia upstream docs for the
#    procedure. If unsure, keep storage-encryption-key.
```

The OIDC issuer key is stable; rotating it invalidates all in-flight
OIDC sessions (Portainer logins). Rotate yearly with operator coordination.

## Health checks

```sh
# Container health (compose):
docker compose ps                                      # → "healthy"

# From outside the VPS:
curl -sI https://auth.${DOMAIN}/api/health
# Expect: HTTP/2 200, body {"status":"OK"}.
```
