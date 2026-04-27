// auth.go — Remote-User / Remote-Groups middleware.

package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

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
func viewerIsAdmin(r *http.Request, cfg config) bool {
	for _, g := range strings.Split(r.Header.Get("X-Enrol-Groups"), ",") {
		if strings.TrimSpace(g) == cfg.requiredGroup {
			return true
		}
	}
	return false
}
