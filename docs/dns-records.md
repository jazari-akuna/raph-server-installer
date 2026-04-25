# DNS Records — `antarctica-engineering.com` (OVH)

Operator reference. DNS is hosted at **OVH**. Records are direct-A only —
no CDN / reverse-proxy / "AlwaysOn" fronting (see Out of scope).

For design rationale see `docs/plan.md` § DNS Layout. For the OVH
application token (DNS-01 wildcard cert), see
`stacks/ingress/README.md` § 2 — do not duplicate the steps here.

---

## 1. Required A records

All point at the VPS public IPv4. No proxy in front. Same TTL for all four.

| Name                              | Type | Target   | Notes                |
|-----------------------------------|------|----------|----------------------|
| `antarctica-engineering.com`      | A    | `VPS_IP` | apex                 |
| `*.antarctica-engineering.com`    | A    | `VPS_IP` | wildcard             |
| `gw.antarctica-engineering.com`   | A    | `VPS_IP` | gateway endpoint     |
| `cdn.antarctica-engineering.com`  | A    | `VPS_IP` | alternate-ingress SNI|

Apex + wildcard are what the wildcard LE cert covers (issued via DNS-01
by `ingress`). `gw.` and `cdn.` exist as explicit names so peers connect
to a stable hostname even though the wildcard would technically resolve
them — clearer intent for ops.

---

## 2. What each subdomain serves

| Subdomain                              | Service        | Public DNS? | Notes                                              |
|----------------------------------------|----------------|-------------|----------------------------------------------------|
| `cloud.antarctica-engineering.com`     | `cloud` (copyparty) | yes    | HTTPS via `ingress`; password-protected           |
| `gw.antarctica-engineering.com`        | `gw0` endpoint | yes         | UDP/51820 — peers dial this hostname              |
| `cdn.antarctica-engineering.com`       | `qedge` SNI    | yes         | UDP/443 (QUIC); presented as TLS handshake        |
| `console.antarctica-engineering.com`   | `console` (Portainer) | **NO** | mesh-only; no public record by design             |
| `ingress` admin (port 81)              | NPM admin UI   | **NO**      | mesh-only / SSH-tunnel; no public hostname        |
| user-app subdomains                    | user stacks    | yes (covered by wildcard) | created on demand in `ingress`        |

Admin UIs (`console`, `ingress` admin) MUST stay off public DNS. This is
load-bearing — see `claude-readme.md` § "Admin UIs are not on public DNS".

---

## 3. TTL recommendations

| Phase                      | TTL    |
|----------------------------|--------|
| Active development / churn | 300    |
| Steady state               | 3600   |

Drop to 300 before changing the VPS IP; bump back up after the change
has propagated and you've verified resolution.

---

## 4. Verification

Always query an off-OVH resolver so you don't see cache. Replace
`<VPS_IP>` with the actual IP.

```sh
dig +short @1.1.1.1 antarctica-engineering.com
dig +short @1.1.1.1 wildcard-test-anything.antarctica-engineering.com
dig +short @1.1.1.1 gw.antarctica-engineering.com
dig +short @1.1.1.1 cdn.antarctica-engineering.com
dig +short @8.8.8.8 antarctica-engineering.com    # cross-check second resolver
```

All four `dig` calls must return exactly `<VPS_IP>` (single A line, no
`CNAME` chain, no proxy IP).

Negative checks (these should NOT resolve publicly):

```sh
dig +short @1.1.1.1 console.antarctica-engineering.com    # expect: empty
```

If `console` resolves publicly, somebody added a record they shouldn't
have — pull it.

---

## 5. OVH application token

Required only for `ingress` to issue the wildcard cert via DNS-01 against
the OVH API. **Creation steps live in `stacks/ingress/README.md` § 2** —
including the exact GET/POST/DELETE grant list scoped to the single
zone, and the rotation policy. Do not duplicate them here; if the
process changes, change them there.

Operational rules carried over for visibility:
- Scope to `/domain/zone/antarctica-engineering.com/*` only. Never the
  `/domain/zone/*` wildcard.
- Rotate yearly. Both admins (sagan, marcus) hold the rotation
  reminder.
- Tokens never go in git. `.env` is gitignored; only `.env.example`
  with placeholders is tracked.

---

## 6. Out of scope (do not enable)

The following layers MUST NOT sit in front of any record on this zone:

- Cloudflare orange-cloud / proxy.
- OVH AlwaysOn / OVH Web Hosting redirect.
- Any third-party CDN, WAF, or reverse-proxy fronting.

Why: `gw0` (UDP/51820) and `qedge` (UDP/443 QUIC) ride raw IP — an
HTTP-only proxy drops them. A TLS-terminating proxy in front of
`ingress` also breaks the wildcard cert chain we issued and inserts an
unwanted MITM. Re-opening this is a plan-level decision; see
`docs/plan.md` § "Things explicitly NOT in scope".
