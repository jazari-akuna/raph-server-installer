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
