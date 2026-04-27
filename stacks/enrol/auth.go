// auth.go — Remote-User / Remote-Groups middleware.

package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// forwardAuthHeader is the request header NPM injects on every protected
// proxy host (auth/enrol/cloud/console plus the wizard's setup host). The
// value matches cfg.forwardAuthSecret. enrol fails closed: empty server
// secret OR missing/wrong client header → 401, BEFORE any Remote-User
// lookup, regardless of how the request reached us.
const forwardAuthHeader = "X-Forward-Auth-Secret"

// checkForwardAuthSecret returns nil iff the request carries the right
// X-Forward-Auth-Secret header AND the server has a non-empty secret to
// compare against. Constant-time compare via crypto/subtle so a timing
// oracle on the secret's length / prefix isn't trivially observable.
//
// Fail-closed when cfg.forwardAuthSecret is empty: that's the bootstrap-
// script-failed-but-container-came-up case, and silently accepting any
// header in that case would re-open exactly the bug this gate exists to
// close. Operators get loud 401s + the startup log warning instead.
func checkForwardAuthSecret(r *http.Request, cfg config) error {
	if cfg.forwardAuthSecret == "" {
		return errors.New("server forward-auth secret unset (fail-closed)")
	}
	got := r.Header.Get(forwardAuthHeader)
	if got == "" {
		return errors.New("missing " + forwardAuthHeader)
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.forwardAuthSecret)) != 1 {
		return errors.New("forward-auth secret mismatch")
	}
	return nil
}

// requireForwardAuth gates a handler on JUST the X-Forward-Auth-Secret
// check — it does NOT call authFromRequest, so Remote-User / Remote-Groups
// may be absent. Use this on endpoints that are reachable over the public
// internet via NPM but cannot use requireAuth because Authelia hasn't
// issued a session cookie yet at that point in the flow.
//
// Concretely: /login-intercept (the SSO POST→Authelia proxy that triggers
// auto-LUKS-unlock on success) MUST be unauthenticated from Authelia's
// perspective, but MUST still refuse forged calls from any container on
// the docker bridge — otherwise an attacker on docker0 can POST forged
// credentials directly to 172.17.0.1:8080/login-intercept and (a) drive
// Authelia rate limits / log noise, (b) probe valid usernames via timing,
// (c) on a correct guess, trigger an unsolicited LUKS unlock side-effect.
// The forward-auth secret is the right gate: NPM injects it on the
// rewrite, bridge attackers don't have it.
func requireForwardAuth(cfg config, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := checkForwardAuthSecret(r, cfg); err != nil {
			http.Error(w,
				"401 — forward-auth header required",
				http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

type authedUser struct {
	Name   string
	Groups []string
}

func (u authedUser) inGroup(g string) bool {
	for _, x := range u.Groups {
		if x == g {
			return true
		}
	}
	return false
}

func authFromRequest(r *http.Request, cfg config) (authedUser, error) {
	user := r.Header.Get(cfg.headerUser)
	if user == "" {
		return authedUser{}, errors.New("missing " + cfg.headerUser)
	}
	groupsRaw := r.Header.Get(cfg.headerGroups)
	var groups []string
	for _, g := range strings.Split(groupsRaw, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			groups = append(groups, g)
		}
	}
	return authedUser{Name: user, Groups: groups}, nil
}

// requireAuth gates a handler on the presence of valid Authelia
// Remote-User / Remote-Groups headers. The `requireGroup` flag is the
// LEGACY all-or-nothing admin gate — when true, the user must be in
// `cfg.requiredGroup` (default `admins`). For the modernized two-tier
// model (admin vs regular user), prefer requireAdmin (admin-only) for
// the day-2 management surface and requireAuth(cfg, false, ...) for
// surfaces any authenticated user may reach (the launcher, the icon
// fileserver, audit-on-self if we ever add it).
//
// The Authelia ACL on enrol.${DOMAIN} should be `one_factor` with NO
// subject restriction, so non-admins can reach the launcher. The
// admin/non-admin split is then enforced HERE, route-by-route.
func requireAuth(cfg config, requireGroup bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Reject forged headers from anywhere that isn't NPM. This MUST
		// run before authFromRequest — otherwise a bridge-IP attacker
		// who sets Remote-User=raph + Remote-Groups=admins still wins
		// even with the secret check present elsewhere.
		if err := checkForwardAuthSecret(r, cfg); err != nil {
			http.Error(w,
				"401 — authentication required (forward-auth gate)",
				http.StatusUnauthorized)
			return
		}
		u, err := authFromRequest(r, cfg)
		if err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.Error(w,
				"401 — authentication required. "+
					"This service must be reached through the SSO portal at "+
					"https://auth."+cfg.domain+"/.",
				http.StatusUnauthorized)
			return
		}
		if requireGroup && !u.inGroup(cfg.requiredGroup) {
			http.Error(w,
				fmt.Sprintf("403 — group %q required", cfg.requiredGroup),
				http.StatusForbidden)
			return
		}
		// Stash auth identity on a cloned request via internal headers
		// for handler use; simpler than context plumbing at our scale.
		r = r.Clone(r.Context())
		r.Header.Set("X-Enrol-User", u.Name)
		r.Header.Set("X-Enrol-Groups", strings.Join(u.Groups, ","))
		h(w, r)
	}
}

// requireAdmin gates a handler on Authelia membership of the admin
// group (cfg.requiredGroup, default `admins`). Returns 401 on missing
// Remote-User and 403 on a non-admin authenticated user. Use this on
// every "create user" / "edit user" / "delete user" / launcher mutation
// / audit / wizard-day-2 surface — anything that should be invisible to
// a regular user. NOT used on /peers anymore: every authenticated user
// can manage their own devices (per-user scoping inside the handler).
func requireAdmin(cfg config, h http.HandlerFunc) http.HandlerFunc {
	return requireAuth(cfg, true, h)
}

// viewerIsAdmin returns true iff the X-Enrol-Groups header (set by
// requireAuth) names cfg.requiredGroup. Use this inside handlers reached
// via requireAuth(cfg, false, ...) when the rendered surface differs
// between admins and regular users (e.g. /peers shows all groups for
// admins, only the viewer's own peers for non-admins).
//
// Defence in depth: returns false fast when the header is absent OR when
// cfg.requiredGroup is misconfigured to "" — neither path should grant
// admin. requireAuth always sets the header, but a future refactor could
// remove that gate and this guard prevents anonymous callers from
// trivially clearing the membership check via "" matches "".
func viewerIsAdmin(r *http.Request, cfg config) bool {
	if cfg.requiredGroup == "" {
		return false
	}
	groups := r.Header.Get("X-Enrol-Groups")
	if groups == "" {
		return false
	}
	for _, g := range strings.Split(groups, ",") {
		if strings.TrimSpace(g) == cfg.requiredGroup {
			return true
		}
	}
	return false
}
