# certbot — wildcard cert issuer for the wizard's finalize step.
#
# Built from certbot/certbot upstream + the seven DNS plugins the wizard
# exposes in stacks/enrol/setup.go's supportedDNSProviders list. The image
# is invoked from finalize step 4 via:
#
#   docker compose --profile finalize \
#     -f /opt/stacks/ingress/docker-compose.yml \
#     run --rm certbot certonly \
#       --dns-${provider} \
#       --dns-${provider}-credentials /tmp/dns-creds \
#       -d ${DOMAIN} -d *.${DOMAIN} \
#       --email ${ADMIN_EMAIL} \
#       --non-interactive --agree-tos
#
# /etc/letsencrypt is bind-mounted from the host so the issued cert lands
# in the same directory NPM serves it from. The credentials file lives on
# a tmpfs (RAM-only) — the wizard writes the operator's DNS API token
# there with mode 0600 just before invoking certbot, defers a delete, and
# the file never touches persistent storage.
#
# Plugin name reference (PyPI):
#   certbot-dns-cloudflare      — Cloudflare API token
#   certbot-dns-ovh             — OVH application key + consumer key
#   certbot-dns-route53         — AWS IAM credentials
#   certbot-dns-digitalocean    — DigitalOcean PAT
#   certbot-dns-google          — GCP service-account JSON
#   certbot-dns-linode          — Linode PAT
#   certbot-dns-rfc2136         — generic BIND-style dynamic DNS
#
# These names match the --dns-${provider} flags certbot exposes; the
# wizard's setup.go already encodes the same identifiers in
# supportedDNSProviders so the flag construction in finalizeIssueCert is
# a direct concatenation.

FROM certbot/certbot:latest

# --no-cache-dir keeps the image small; --break-system-packages is needed
# on the underlying Alpine + system Python because certbot/certbot sets
# PYTHON3_VERSION to a system interpreter (as of 2.x) and modern pip
# refuses to install into it without the override. The plugins are pure
# Python and the upstream image documents this install pattern.
RUN pip install --no-cache-dir --break-system-packages \
        certbot-dns-cloudflare \
        certbot-dns-ovh \
        certbot-dns-route53 \
        certbot-dns-digitalocean \
        certbot-dns-google \
        certbot-dns-linode \
        certbot-dns-rfc2136

# Default ENTRYPOINT is inherited from certbot/certbot (it's the certbot
# CLI). The wizard always passes explicit subcommand + flags so we don't
# customize CMD here — `docker compose run --rm certbot certonly ...`
# flows through unchanged.
