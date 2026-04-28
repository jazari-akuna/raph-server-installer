// server.go — HTTP routing + handlers.
//
// The server type holds:
//   - cfg: loaded config
//   - tmpl: parsed templates
//   - mu:   global mutex around all mutating operations (gw0.conf,
//           users_database.yml, occ user:delete shell-out, peer cascade).
//           The UI is operator-only with at most a couple of admins, so
//           serialising everything has zero practical cost and
//           eliminates a whole class of races.

package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type server struct {
	cfg  config
	tmpl *template.Template
	mu   sync.Mutex // serialises every mutating op

	// plane is the typed Plane REST client used by the admin /users
	// page to surface per-user issue counts + attachment bytes. May be
	// non-nil but unconfigured (empty token) before Wave C deploys
	// Plane and the operator drops a token at
	// /etc/raph-installer/plane-admin-token; every PlaneClient method
	// gracefully short-circuits to zero values + nil error in that
	// state so the admin page renders "—" rather than breaking.
	plane *PlaneClient
}

func newServer(cfg config) (*server, error) {
	tmpl := template.New("").Funcs(template.FuncMap{
		"join": strings.Join,
		// domain returns the apex domain for use in templates that need
		// to construct externally-facing URLs (e.g. the Authelia logout
		// link in _layout.html). Captured by closure so we don't have to
		// thread cfg.domain into every page-data struct.
		"domain": func() string { return cfg.domain },
		// awgEnabled reports whether AmneziaWG (regional split tunnel) is
		// installed on this host, by checking for the existence of
		// gw0.conf. Used by _layout.html to hide the "devices" nav entry
		// on installs that opted out of gw0 (the default; opt-in via
		// SKIP_GW0=0). Captured by closure for the same reason as domain.
		"awgEnabled": func() bool {
			path := filepath.Join(cfg.awgDir, cfg.awgIface+".conf")
			_, err := os.Stat(path)
			return err == nil
		},
		"gb": func(n int64) string {
			return fmt.Sprintf("%.1f", float64(n)/(1<<30))
		},
		"pct": func(part, whole int64) string {
			if whole <= 0 {
				return "0"
			}
			return fmt.Sprintf("%.2f", 100*float64(part)/float64(whole))
		},
		// setupStepDone reports whether `target` is BEFORE `cur` in the
		// canonical wizard order, i.e. has been left behind. Used by the
		// progress indicator to render past-step ticks. Accepts the
		// strongly-typed setupStepName as well as plain strings.
		"setupStepDone": func(cur any, target string) bool {
			order := []string{"welcome", "domain", "dns", "admin", "storage", "finalize", "done"}
			c := fmt.Sprintf("%v", cur)
			ci, ti := -1, -1
			for i, s := range order {
				if s == c {
					ci = i
				}
				if s == target {
					ti = i
				}
			}
			if ci < 0 || ti < 0 {
				return false
			}
			return ti < ci
		},
		// storageByName indexes a []UserStorage by Name so the per-user
		// table in users.html can look up the storage row inline. Returns
		// a map[string]UserStorage; missing keys yield the zero value when
		// dereferenced via `index`. Defined as a template func so the
		// template doesn't have to thread the index in via the page-data
		// struct. See ADR-005 for the rationale.
		"storageByName": func(rows []UserStorage) map[string]UserStorage {
			m := make(map[string]UserStorage, len(rows))
			for _, r := range rows {
				m[r.Name] = r
			}
			return m
		},
		"prettyBytes": func(n int64) string {
			const (
				kib = 1024
				mib = 1024 * kib
				gib = 1024 * mib
			)
			switch {
			case n >= gib:
				return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
			case n >= mib:
				return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
			case n >= kib:
				return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
			default:
				return fmt.Sprintf("%d B", n)
			}
		},
	})
	pattern := filepath.Join(cfg.templatesDir, "*.html")
	tmpl, err := tmpl.ParseGlob(pattern)
	if err != nil {
		return nil, fmt.Errorf("parse templates %s: %w", pattern, err)
	}
	if err := bootstrapLauncher(cfg.launcherDir, cfg.domain); err != nil {
		return nil, fmt.Errorf("bootstrap launcher: %w", err)
	}
	if err := ensureArchiveDir(cfg); err != nil {
		return nil, fmt.Errorf("ensure peers archive dir: %w", err)
	}
	plane := NewPlaneClient(cfg.planeAPIBaseURL, cfg.planeAPIToken)
	return &server{cfg: cfg, tmpl: tmpl, plane: plane}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Healthz reports liveness of the enrol service itself: in setup
		// mode we report "ok (setup mode)" so the dashboard knows the
		// wizard is up; once the wizard completes (sentinel exists), we
		// report plain "ok". We deliberately do NOT require gw0.conf:
		// AmneziaWG (regional split tunnel) is opt-in via SKIP_GW0=0
		// (default 1 — see scripts/bootstrap-phase2.sh), so on a default
		// install gw0.conf will never exist and a check on it would keep
		// the container "unhealthy" forever. The wizard's sentinel is
		// the better signal of "setup is done"; for an explicit awg
		// readiness probe see /healthz/awg.
		if s.setupModeActive() {
			fmt.Fprintln(w, "ok (setup mode)")
			return
		}
		fmt.Fprintln(w, "ok")
	})

	// /healthz/awg — explicit signal of AmneziaWG availability. Always
	// 200; the body indicates "present" or "absent". Use this from
	// monitoring if you specifically care that gw0 was provisioned (e.g.
	// alerting that an opt-in install silently failed). Healthz proper
	// stays oblivious to gw0.
	mux.HandleFunc("/healthz/awg", func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintln(w, "absent")
			return
		}
		fmt.Fprintln(w, "present")
	})

	mux.Handle("/static/", http.StripPrefix("/static/",
		http.FileServer(http.Dir(s.cfg.staticDir))))

	// Setup wizard routes (Parcel 3A). They're registered unconditionally;
	// the setupRouteGate middleware below is what hides them once setup
	// completes.
	s.registerSetupRoutes(mux)

	// (Pre-Wave-1: /login-intercept proxied Authelia's first-factor POST
	// and auto-unlocked the user's LUKS volume on success. With Nextcloud
	// + OIDC there's nothing to unlock — Nextcloud manages its own session
	// via user_oidc — so the route is gone.)

	// Two-tier access model (see auth.go):
	//   - requireAuth(cfg, false, ...): any authenticated user (admin OR
	//     regular). Used for the launcher landing page and the icon
	//     fileserver — non-admins land here after SSO and see only the
	//     tiles they're allowed to follow (the "users" tile is filtered
	//     out by handleLauncher; the "console" tile is filtered too,
	//     since the Authelia ACL on console.${DOMAIN} would 403 them
	//     anyway and we'd rather not surface the dead link).
	//   - requireAdmin(cfg, ...): admin-only. Anything that mutates
	//     users_database.yml, the launcher app list, the gw0 peer set,
	//     or reads the audit log. A non-admin who forges a direct GET
	//     /users / POST /users / GET /users/<x> / etc. gets a 403.
	//
	// The Authelia ACL on enrol.${DOMAIN} is `one_factor` with NO
	// subject restriction (see configuration.yml.template), so non-admins
	// can reach the launcher at all. Admin gating happens HERE.
	mux.HandleFunc("/", requireAuth(s.cfg, false, s.withCSRF(s.handleLauncher)))
	mux.HandleFunc("/launcher/icons/", requireAuth(s.cfg, false, s.handleLauncherIcon))
	mux.HandleFunc("/launcher/apps", requireAdmin(s.cfg, s.withCSRF(s.handleLauncherAddApp)))
	mux.HandleFunc("/launcher/apps/", requireAdmin(s.cfg, s.withCSRF(s.handleLauncherAppSub)))
	mux.HandleFunc("/audit", requireAdmin(s.cfg, s.handleAudit))

	// Users — admin-only. Non-admins MUST NOT see or hit any /users,
	// /users/* surface (per the two-tier model).
	mux.HandleFunc("/users", requireAdmin(s.cfg, s.withCSRF(s.handleUsers)))
	mux.HandleFunc("/users/", requireAdmin(s.cfg, s.withCSRF(s.handleUserSub)))

	// Peers (devices) — admin-only. Mirrors /users.
	// /peers is open to every authenticated user — each user manages
	// their own devices. Admins see all peers (current behaviour) via the
	// per-handler scope check (viewerIsAdmin), regular users only see/
	// modify peers whose name starts with `<username>-`.
	mux.HandleFunc("/peers", requireAuth(s.cfg, false, s.withCSRF(s.handlePeers)))
	mux.HandleFunc("/peers/", requireAuth(s.cfg, false, s.withCSRF(s.handlePeerSub)))

	return s.setupRouteGate(mux)
}

// setupRouteGate enforces the wizard-mode invariant:
//
//   - While /srv/store/.setup-complete is ABSENT, /healthz and /static/*
//     pass through; the bare root path `/` is dispatched to the wizard
//     root handler (so the operator hits `http://setup.${DOMAIN}/`);
//     `/setup` and `/setup/*` continue to work as the wizard's internal
//     step paths (templates link to `/setup/welcome`, `/setup/dns`, etc.,
//     so the prefix is preserved internally even though the entry URL is
//     just the subdomain root); everything else 302s to `/` so the
//     operator can't accidentally poke at /users (which would 401 anyway,
//     since Authelia isn't yet wired) and a half-open browser tab from a
//     previous install lands them back on the wizard rather than on an
//     error page.
//
//   - Once the sentinel exists, /setup/* returns 404 (not 410 — we don't
//     want to leak that the wizard ever existed at this URL). This is
//     belt-and-braces: the wizard's token-check would 401 anyway since
//     setupToken is wiped from the env after finalize, but defence in
//     depth is cheap. The bare `/` falls through to the launcher.
//
// The /healthz endpoint bypasses the gate entirely in both directions.
//
// The public URL of the wizard is `http://setup.${DOMAIN}/` — fronted by
// an NPM proxy host whose host header matches `setup.${DOMAIN}` and which
// forwards `/` to enrol's host endpoint. The wizard's path prefix on the
// underlying enrol service is still `/setup/<step>` for the step pages,
// but operators only ever see the subdomain root URL.
func (s *server) setupRouteGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		isSetup := path == "/setup" || strings.HasPrefix(path, "/setup/")
		isStatic := strings.HasPrefix(path, "/static/")
		isHealth := path == "/healthz"
		isRoot := path == "/"

		if s.setupModeActive() {
			// Wizard mode: serve the wizard at `/` (the subdomain root) by
			// dispatching to handleSetupRoot, which 303s to the step the
			// operator left off at. The internal `/setup/*` paths are kept
			// intact so the existing template/form action links keep working
			// without a sweeping rewrite.
			if isRoot {
				s.handleSetupRoot(w, r)
				return
			}
			// Hide the day-2 surface entirely; bounce stragglers back to the
			// wizard root.
			if !isSetup && !isStatic && !isHealth {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		} else {
			// Day-2 mode: hide the wizard surface entirely.
			if isSetup {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// /users — list + create

func (s *server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderUsersList(w, r, "")
	case http.MethodPost:
		s.handleCreateUser(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type usersListData struct {
	Title         string
	User          string
	CSRF          string
	ViewerIsAdmin bool
	Users         []userRow
	Storage       StorageInfo
	Flash         string
	TOTPEnabled   bool
}

type userRow struct {
	Name        string
	DisplayName string
	Email       string
	Groups      []string
	IsAdmin     bool
}

func (s *server) renderUsersList(w http.ResponseWriter, r *http.Request, flash string) {
	csrf := ensureCSRF(w, r)
	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	names := db.listSorted()
	rows := make([]userRow, 0, len(names))
	specs := make([]storageUserSpec, 0, len(names))
	for _, name := range names {
		u := db.Users[name]
		rows = append(rows, userRow{
			Name: name, DisplayName: u.DisplayName, Email: u.Email,
			Groups: u.Groups, IsAdmin: isAdminUser(u),
		})
		specs = append(specs, storageUserSpec{Name: name, Email: u.Email})
	}
	data := usersListData{
		// Title flows into _layout.html's <title> tag — see ADR-005:
		// the page is the unified Admin view (users + cloud usage +
		// Plane usage), the URL stays at /users so existing bookmarks
		// don't break.
		Title:         "Admin",
		User:          r.Header.Get("X-Enrol-User"),
		CSRF:          csrf,
		ViewerIsAdmin: true, // requireAdmin gates this route
		Users:         rows,
		Storage:       storageSnapshot(s.cfg, specs, s.plane),
		Flash:         flash,
		TOTPEnabled:   s.totpEnabled(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "users.html", data); err != nil {
		log.Printf("template users.html: %v", err)
	}
}

// handleCreateUser is the orchestration of: validate → argon2id hash →
// users_database.yml write → flush → optional TOTP. Nextcloud's user_oidc
// app auto-provisions the cloud-side user on first login (uid, name,
// email, groups come from the OIDC claims), so enrol does NOT call
// `occ user:add` here.
func (s *server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	displayname := strings.TrimSpace(r.Form.Get("displayname"))
	email := strings.TrimSpace(r.Form.Get("email"))
	password := r.Form.Get("password")
	isAdmin := r.Form.Get("is_admin") != ""

	if !validUsername(name) {
		http.Error(w, "invalid username (allowed: lowercase a-z, then a-z0-9_-, 1..32)",
			http.StatusBadRequest)
		return
	}
	if !validDisplayName(displayname) {
		http.Error(w, "invalid display name (1..64 chars, letters/digits/space/.-_')",
			http.StatusBadRequest)
		return
	}
	if !validEmail(email) {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}
	if len(password) < 12 {
		http.Error(w, "password must be at least 12 characters",
			http.StatusBadRequest)
		return
	}

	// 1. users_database.yml — check the user doesn't already exist.
	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, ok := db.Users[name]; ok {
		http.Error(w, "user already exists", http.StatusConflict)
		return
	}

	// 2. Hash password.
	hash, err := argon2idHash(password)
	if err != nil {
		http.Error(w, "argon2 hash: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// 3. Splice into YAML and atomic-rename write.
	//
	// Group convention (matches setup.go finalizeWriteAdmin and the
	// `admins`-only Authelia ACL on console.${DOMAIN}): admins get
	// `[admins]`; non-admins get `[users]`. The `users` group has no
	// special meaning in Authelia today — it's a placeholder so the
	// Remote-Groups header is non-empty and so a future regular-user
	// ACL has something to subject-match against. The is_admin
	// distinction is what actually gates enrol's admin surface (see
	// requireAdmin in auth.go) and Portainer access (Authelia ACL on
	// console.${DOMAIN} requires `group:admins`, so non-admins can't
	// reach Portainer at all — strongest possible enforcement).
	groups := []string{"users"}
	if isAdmin {
		groups = []string{"admins"}
	}
	u := User{
		Disabled: false, DisplayName: displayname, Password: hash,
		Email: email, Groups: groups,
	}
	if err := db.upsert(name, u); err != nil {
		http.Error(w, "upsert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := db.flush(); err != nil {
		http.Error(w, "write users db: "+err.Error(), http.StatusInternalServerError)
		writeAudit(auditPath, auditEntry{Action: "user.create", Actor: actor,
			Target: name, Result: "fail", Note: "write yaml: " + err.Error()})
		return
	}
	writeAudit(auditPath, auditEntry{Action: "user.create", Actor: actor,
		Target: name, Result: "ok", Note: "displayname=" + displayname})

	// 4. Nextcloud-side provisioning is intentionally NOT done here. The
	// user_oidc app auto-creates the Nextcloud user on first sign-in via
	// auth.<domain>; the operator hands the credentials to the user and
	// asks them to visit https://cloud.<domain> once.

	// 5. TOTP generate — only if the operator opted into 2FA at setup.
	// When EnableTOTP is false the user authenticates with password only
	// (Authelia policy stays one_factor); minting a secret here would just
	// produce a confusing QR for an unenforced second factor.
	if !s.totpEnabled() {
		s.renderUserCreated(w, r, name, "", nil, "")
		return
	}
	otpauth, qrPNG, err := totpGenerate(s.cfg, name)
	if err != nil {
		writeAudit(auditPath, auditEntry{Action: "totp.generate", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		s.renderUserCreated(w, r, name, "", nil, fmt.Sprintf(
			"TOTP generation FAILED: %v — retry from the user page.", err))
		return
	}
	writeAudit(auditPath, auditEntry{Action: "totp.generate", Actor: actor,
		Target: name, Result: "ok"})

	s.renderUserCreated(w, r, name, otpauth, qrPNG, "")
}

// totpEnabled returns whether the operator opted into TOTP at setup time.
// On any read/parse error we conservatively return false: an unknown
// state is treated the same as "disabled" so a missing setup file never
// produces a confusing QR for an unenforced second factor.
func (s *server) totpEnabled() bool {
	st, err := s.loadSetupState()
	if err != nil || st == nil {
		return false
	}
	return st.EnableTOTP
}

func (s *server) renderUserCreated(w http.ResponseWriter, r *http.Request,
	name, otpauth string, qrPNG []byte, flash string) {
	csrf := ensureCSRF(w, r)
	// template.URL marks the data: URI as safe so html/template doesn't
	// rewrite it to "#ZgotmplZ" — the package's default policy treats
	// data:-URIs as untrusted unless explicitly trusted by the caller.
	var qrDataURI template.URL
	if len(qrPNG) > 0 {
		qrDataURI = template.URL("data:image/png;base64," +
			base64.StdEncoding.EncodeToString(qrPNG))
	}
	data := struct {
		Title         string
		User          string
		CSRF          string
		ViewerIsAdmin bool
		Created       string
		OtpauthURI    string
		QRDataURI     template.URL
		Flash         string
		TOTPEnabled   bool
	}{
		Title:         "user created: " + name,
		User:          r.Header.Get("X-Enrol-User"),
		CSRF:          csrf,
		ViewerIsAdmin: true, // requireAdmin gates this route
		Created:       name,
		OtpauthURI:    otpauth,
		QRDataURI:     qrDataURI,
		Flash:         flash,
		TOTPEnabled:   s.totpEnabled(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "user-created.html", data); err != nil {
		log.Printf("template user-created.html: %v", err)
	}
}

// ---------------------------------------------------------------------------
// /users/<name>/...

func (s *server) handleUserSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/users/")
	if rest == "" {
		http.Redirect(w, r, "/users", http.StatusFound)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	if !validUsername(name) {
		http.Error(w, "invalid username", http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "":
		s.handleUserDetail(w, r, name)
	case "edit":
		s.handleUserEdit(w, r, name)
	case "password":
		s.handleUserPassword(w, r, name)
	case "totp":
		s.handleUserTOTP(w, r, name)
	case "delete":
		s.handleUserDelete(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

type userDetailData struct {
	Title         string
	User          string
	CSRF          string
	ViewerIsAdmin bool
	Target        User
	Name          string
	IsAdmin       bool // target user's admin status (drives checkbox)
	Devices       []peer
	Flash         string
	TOTPEnabled   bool
}

// isAdminUser returns true iff the user has the `admins` Authelia group.
// Single source-of-truth for the admin/non-admin distinction the
// modernized /users form exposes as a checkbox.
func isAdminUser(u User) bool {
	for _, g := range u.Groups {
		if g == "admins" {
			return true
		}
	}
	return false
}

func (s *server) handleUserDetail(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	csrf := ensureCSRF(w, r)
	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	u, ok := db.Users[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	devices := s.devicesForUser(name)
	data := userDetailData{
		Title:         "user " + name,
		User:          r.Header.Get("X-Enrol-User"),
		CSRF:          csrf,
		ViewerIsAdmin: true, // requireAdmin gates this route
		Target:        u, Name: name, IsAdmin: isAdminUser(u),
		Devices:     devices,
		TOTPEnabled: s.totpEnabled(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "user-detail.html", data); err != nil {
		log.Printf("template user-detail.html: %v", err)
	}
}

func (s *server) handleUserEdit(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	displayname := strings.TrimSpace(r.Form.Get("displayname"))
	email := strings.TrimSpace(r.Form.Get("email"))
	isAdmin := r.Form.Get("is_admin") != ""
	if !validDisplayName(displayname) {
		http.Error(w, "invalid display name", http.StatusBadRequest)
		return
	}
	if !validEmail(email) {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}
	// Same group convention as handleCreateUser. Self-demotion is
	// allowed at the data layer; if it locks the operator out of the
	// admin surface that's fine — they can SSH in and edit the YAML
	// directly. We don't try to be cleverer than that.
	groups := []string{"users"}
	if isAdmin {
		groups = []string{"admins"}
	}

	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	u, ok := db.Users[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	u.DisplayName = displayname
	u.Email = email
	u.Groups = groups
	if err := db.upsert(name, u); err != nil {
		http.Error(w, "upsert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := db.flush(); err != nil {
		http.Error(w, "write users db: "+err.Error(), http.StatusInternalServerError)
		writeAudit(auditPath, auditEntry{Action: "user.update", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		return
	}
	writeAudit(auditPath, auditEntry{Action: "user.update", Actor: actor,
		Target: name, Result: "ok"})
	http.Redirect(w, r, "/users/"+name, http.StatusSeeOther)
}

func (s *server) handleUserPassword(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	pw := r.Form.Get("password")
	// Single password field — no confirm. Browser autofill is the source
	// of truth for "did I type the right thing"; the operator verifies on
	// the user's first sign-in. Nextcloud picks up the new password on
	// the next OIDC sign-in — no separate Nextcloud-side rotation needed.
	if pw == "" {
		http.Error(w, "password required", http.StatusBadRequest)
		return
	}
	if len(pw) < 12 {
		http.Error(w, "password must be at least 12 characters",
			http.StatusBadRequest)
		return
	}
	hash, err := argon2idHash(pw)
	if err != nil {
		http.Error(w, "argon2 hash: "+err.Error(), http.StatusInternalServerError)
		return
	}

	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	u, ok := db.Users[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	u.Password = hash
	if err := db.upsert(name, u); err != nil {
		http.Error(w, "upsert: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := db.flush(); err != nil {
		writeAudit(auditPath, auditEntry{Action: "user.password", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		http.Error(w, "write users db: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{Action: "user.password", Actor: actor,
		Target: name, Result: "ok"})
	http.Redirect(w, r, "/users/"+name, http.StatusSeeOther)
}

func (s *server) handleUserTOTP(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.totpEnabled() {
		http.Error(w, "TOTP is disabled for this installation — enable it in setup state before regenerating",
			http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	otpauth, qrPNG, err := totpGenerate(s.cfg, name)
	if err != nil {
		writeAudit(auditPath, auditEntry{Action: "totp.generate", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		http.Error(w, "totp generate: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{Action: "totp.generate", Actor: actor,
		Target: name, Result: "ok"})
	s.renderUserCreated(w, r, name, otpauth, qrPNG, "TOTP secret regenerated.")
}

func (s *server) handleUserDelete(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	confirm := strings.TrimSpace(r.Form.Get("confirm_name"))
	if confirm != name {
		http.Error(w, "type the username exactly to confirm deletion",
			http.StatusBadRequest)
		return
	}

	// 1. Nextcloud-side delete via `occ user:delete`. Tolerate non-zero
	// exit because the user may never have logged into Nextcloud, in
	// which case user_oidc has not auto-provisioned them yet and there
	// is no NC user to remove. Surface the result to the audit log
	// either way.
	occCmd := exec.Command("docker", "exec", "-u", "www-data", "cloud", "php", "occ", "user:delete", name)
	if out, err := occCmd.CombinedOutput(); err != nil {
		writeAudit(auditPath, auditEntry{Action: "cloud.user.delete", Actor: actor,
			Target: name, Result: "fail",
			Note: "occ user:delete: " + strings.TrimSpace(string(out))})
		// Continue — the user may simply not exist in Nextcloud yet.
	} else {
		writeAudit(auditPath, auditEntry{Action: "cloud.user.delete", Actor: actor,
			Target: name, Result: "ok"})
	}

	// 2. Belt-and-braces wipe of the user's Nextcloud datadir. occ
	// user:delete already removes /srv/store/cloud-data/<name> when it
	// succeeds, but we re-assert the wipe here so a never-logged-in
	// user (no NC entity) still has any stray files removed. Tolerates
	// not-exists.
	dataDir := filepath.Join(cloudDataRoot, name)
	if err := os.RemoveAll(dataDir); err != nil && !os.IsNotExist(err) {
		writeAudit(auditPath, auditEntry{Action: "cloud.user.delete", Actor: actor,
			Target: name, Result: "fail",
			Note: "rm cloud-data: " + err.Error()})
		// Continue — YAML drop is more important.
	}

	// 3. remove all peers prefixed `<name>-`.
	if err := s.removeUserPeers(name, actor, auditPath); err != nil {
		log.Printf("removeUserPeers(%s): %v", name, err)
	}

	// 4. TOTP delete.
	if err := totpDelete(s.cfg, name); err != nil {
		writeAudit(auditPath, auditEntry{Action: "totp.delete", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
	} else {
		writeAudit(auditPath, auditEntry{Action: "totp.delete", Actor: actor,
			Target: name, Result: "ok"})
	}

	// 5. users_database.yml drop entry.
	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := db.remove(name); err != nil {
		writeAudit(auditPath, auditEntry{Action: "user.delete", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := db.flush(); err != nil {
		writeAudit(auditPath, auditEntry{Action: "user.delete", Actor: actor,
			Target: name, Result: "fail", Note: "write yaml: " + err.Error()})
		http.Error(w, "write users db: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{Action: "user.delete", Actor: actor,
		Target: name, Result: "ok"})

	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *server) removeUserPeers(name, actor, auditPath string) error {
	confPath := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	pc, err := loadConf(confPath)
	if err != nil {
		return err
	}
	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		return err
	}
	prefix := name + "-"
	var toRemove []peer
	for _, p := range pc.listPeersWithMeta(meta) {
		if strings.HasPrefix(p.Name, prefix) {
			toRemove = append(toRemove, p)
		}
	}
	if len(toRemove) == 0 {
		return nil
	}
	// Single-pass rewrite. The previous loop called removePeerByPubkey
	// per peer and then `pc, _ = loadConf(...)` to refresh state — but
	// that swallowed the reload error, so a transient read failure left
	// pc nil and the next iteration nil-deref'd. Rewriting all matched
	// peers in one read+one write also collapses N file rewrites + N
	// races against any concurrent reader of gw0.conf into one.
	drop := make(map[string]bool, len(toRemove))
	for _, p := range toRemove {
		drop[p.PublicKey] = true
	}
	if err := pc.removePeersByPubkeys(confPath, drop); err != nil {
		// Mirror the operator's intent in the audit log so a failed
		// cascade doesn't disappear silently.
		for _, p := range toRemove {
			writeAudit(auditPath, auditEntry{Action: "peer.remove", Actor: actor,
				Target: p.Name, Result: "fail", Note: err.Error()})
		}
		return err
	}
	for _, p := range toRemove {
		_ = meta.delete(p.PublicKey)
		writeAudit(auditPath, auditEntry{Action: "peer.remove", Actor: actor,
			Target: p.Name, Result: "ok",
			Pubkey: p.PublicKey, IP: p.IP})
	}
	_ = reloadInterface(s.cfg.awgDir, s.cfg.awgIface)
	return nil
}

// ---------------------------------------------------------------------------
// /peers — devices, grouped by user

type peerGroup struct {
	User    string
	Display string
	Peers   []peer
}

type peersListData struct {
	Title         string
	User          string
	CSRF          string
	ViewerIsAdmin bool
	Groups        []peerGroup
	Users         []string
	Tags          []string
	Flash         string
}

// setupHelpData feeds web/templates/_setup-help.html. Everything is
// computed per-request from the peer + cfg + UA — nothing here is
// persisted. The QR is rendered inline as a base64 data-URI so it can't
// be fetched separately or cached as its own resource (the existing
// /peers/<name>/qr.png endpoint stays as is for the "QR" block above).
type setupHelpData struct {
	PeerName        string
	ClientConf      string
	QRDataURI       template.URL
	Endpoint        string
	ApexDomain      string
	DefaultPlatform string // "win"|"mac"|"linux"|"android"|"ios"|"unknown"
}

// detectPlatform returns a coarse platform tag for the User-Agent. Only
// used to decide which <details> block opens by default in the setup-
// help section. Order matters: iPad/iPhone before "Mac OS X" (recent
// iPadOS reports "Mac OS X"; the iPad keyword check above catches
// most), and "Android" before "Linux" (Android UAs include "Linux").
// We never log the UA — see buildSetupHelp.
func detectPlatform(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"), strings.Contains(ua, "iPod"):
		return "ios"
	case strings.Contains(ua, "Android"):
		return "android"
	case strings.Contains(ua, "Windows"):
		return "win"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		return "mac"
	case strings.Contains(ua, "Linux"):
		return "linux"
	default:
		return "unknown"
	}
}

// buildSetupHelp shells out to qrencode (already in the Dockerfile;
// reused from totp.go) and base64-encodes the PNG into a data: URI so
// the partial template can <img src="data:..."> it inline.
//
// We deliberately do NOT log the conf, the QR, the data-URI, or the
// User-Agent — the conf carries the peer's private key and the UA is
// fingerprinting surface we don't need to retain.
func (s *server) buildSetupHelp(r *http.Request, p peer, clientConf string) (setupHelpData, error) {
	d := setupHelpData{
		PeerName:        p.Name,
		ClientConf:      clientConf,
		Endpoint:        s.cfg.awgEndpoint,
		ApexDomain:      s.cfg.domain,
		DefaultPlatform: detectPlatform(r.UserAgent()),
	}
	png, err := qrencode(clientConf)
	if err != nil {
		// QR is convenience for mobile; the .conf import path still
		// works on every platform without it. Surface the error so
		// callers can decide whether to fail the whole render.
		return d, err
	}
	d.QRDataURI = template.URL("data:image/png;base64," +
		base64.StdEncoding.EncodeToString(png))
	return d, nil
}

// noStore sets cache headers that prevent any intermediate cache from
// retaining responses that contain peer secrets (.conf private key, QR
// data-URI). Applied on peer-created and peer-detail.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
}

// awgEnabled reports whether AmneziaWG (gw0) is provisioned on this host
// by checking for gw0.conf. AmneziaWG is opt-in via SKIP_GW0=0 (default 1
// — see scripts/bootstrap-phase2.sh "Step 2 — gw0 install"); on a default
// install this returns false and the /peers UI renders an empty-state
// page rather than 500ing on the missing file.
func (s *server) awgEnabled() bool {
	path := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	_, err := os.Stat(path)
	return err == nil
}

// renderPeersDisabled renders the empty-state page for installs that
// haven't enabled AmneziaWG. Keeps the layout chrome (header + nav) so
// the operator can navigate away cleanly. Used by both /peers and
// /peers/* handlers when gw0.conf is absent.
func (s *server) renderPeersDisabled(w http.ResponseWriter, r *http.Request) {
	csrf := ensureCSRF(w, r)
	data := struct {
		Title         string
		User          string
		CSRF          string
		ViewerIsAdmin bool
	}{
		Title:         "devices",
		User:          r.Header.Get("X-Enrol-User"),
		CSRF:          csrf,
		ViewerIsAdmin: viewerIsAdmin(r, s.cfg),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "peers-disabled.html", data); err != nil {
		log.Printf("template peers-disabled.html: %v", err)
	}
}

func (s *server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if !s.awgEnabled() {
		// Mutating verbs (POST/etc.) get a 503; the GET path renders the
		// empty-state. POST shouldn't normally reach here since the form
		// lives on the empty-state page, but defence in depth.
		if r.Method != http.MethodGet {
			http.Error(w, "AmneziaWG (gw0) is not installed; "+
				"run scripts/install-gw0.sh on the host to enable devices",
				http.StatusServiceUnavailable)
			return
		}
		s.renderPeersDisabled(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderPeerList(w, r, "")
	case http.MethodPost:
		s.handleAddPeer(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) renderPeerList(w http.ResponseWriter, r *http.Request, flash string) {
	// Defence-in-depth: requireAuth already 401s when X-Enrol-User is
	// missing, but a future routing change must not silently render every
	// peer as if "" were a viewer. (For non-admins, an empty viewer would
	// match the prefix "-" against every peer name, leaking nothing in
	// today's data shape but still wrong-by-construction.)
	if r.Header.Get("X-Enrol-User") == "" {
		http.Error(w, "401 — authentication required", http.StatusUnauthorized)
		return
	}
	csrf := ensureCSRF(w, r)
	confPath := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	pc, err := loadConf(confPath)
	if err != nil {
		http.Error(w, "load conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		http.Error(w, "load meta: "+err.Error(), http.StatusInternalServerError)
		return
	}
	all := pc.listPeersWithMeta(meta)

	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}

	viewer := r.Header.Get("X-Enrol-User")
	isAdmin := viewerIsAdmin(r, s.cfg)

	// Admins see everyone; regular users see only their own peers (and
	// their username is the only option in the create-peer dropdown).
	knownUsers := db.listSorted()
	if !isAdmin {
		knownUsers = []string{viewer}
		filtered := all[:0]
		prefix := viewer + "-"
		for _, p := range all {
			if strings.HasPrefix(p.Name, prefix) {
				filtered = append(filtered, p)
			}
		}
		all = filtered
	}

	groups := groupPeersByUser(all, knownUsers)

	data := peersListData{
		Title:         "devices",
		User:          viewer,
		CSRF:          csrf,
		ViewerIsAdmin: isAdmin,
		Groups:        groups,
		Users:         knownUsers,
		Tags:          []string{"laptop", "phone", "tablet", "other"},
		Flash:         flash,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "peers.html", data); err != nil {
		log.Printf("template peers.html: %v", err)
	}
}

func groupPeersByUser(all []peer, knownUsers []string) []peerGroup {
	known := map[string]bool{}
	for _, u := range knownUsers {
		known[u] = true
	}
	byUser := map[string][]peer{}
	order := append([]string{}, knownUsers...)
	for _, p := range all {
		u := p.User()
		if u == "" || !known[u] {
			byUser["(unassigned)"] = append(byUser["(unassigned)"], p)
			continue
		}
		byUser[u] = append(byUser[u], p)
	}
	out := []peerGroup{}
	for _, u := range order {
		ps := byUser[u]
		out = append(out, peerGroup{User: u, Display: u, Peers: ps})
	}
	if ps := byUser["(unassigned)"]; len(ps) > 0 {
		out = append(out, peerGroup{User: "(unassigned)", Display: "(unassigned)", Peers: ps})
	}
	// Sort each group's peers by IP for stability.
	for i := range out {
		sort.Slice(out[i].Peers, func(a, b int) bool {
			return out[i].Peers[a].IP < out[i].Peers[b].IP
		})
	}
	return out
}

func (s *server) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	// Defence-in-depth: see renderPeerList. requireAuth gates this
	// handler today; the per-handler guard protects against a future
	// routing change exposing the create-peer surface anonymously.
	if r.Header.Get("X-Enrol-User") == "" {
		http.Error(w, "401 — authentication required", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	user := strings.TrimSpace(r.Form.Get("user"))
	tag := strings.TrimSpace(r.Form.Get("device_tag"))
	actor := r.Header.Get("X-Enrol-User")
	// Non-admins can only enrol devices for themselves: ignore whatever
	// the form posts and pin user=actor. Admins keep the dropdown.
	if !viewerIsAdmin(r, s.cfg) {
		user = actor
	}
	name, err := peerNameFor(user, tag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	s.mu.Lock()
	defer s.mu.Unlock()

	// Verify the user actually exists in users_database.yml.
	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, ok := db.Users[user]; !ok {
		http.Error(w, "no such user: "+user, http.StatusBadRequest)
		return
	}

	confPath := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	pc, err := loadConf(confPath)
	if err != nil {
		http.Error(w, "load conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Reject duplicate name.
	meta, metaErr := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	for _, p := range pc.listPeersWithMeta(meta) {
		if p.Name == name {
			http.Error(w, "device "+name+" already exists",
				http.StatusConflict)
			return
		}
	}
	prefix, err := subnetPrefix(s.cfg.peerSubnet)
	if err != nil {
		http.Error(w, "bad subnet: "+err.Error(), http.StatusInternalServerError)
		return
	}
	octet, err := pc.pickFreeOctet(prefix, s.cfg.peerStart)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	priv, pub, err := genKeypair()
	if err != nil {
		http.Error(w, "keygen: "+err.Error(), http.StatusInternalServerError)
		return
	}
	p := peer{
		Name: name, DeviceTag: tag, PublicKey: pub, PrivateKey: priv,
		IP:      fmt.Sprintf("%s%d", prefix, octet),
		AddedBy: actor, AddedAt: time.Now().UTC(),
	}
	if err := pc.appendPeer(confPath, p); err != nil {
		http.Error(w, "write conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if metaErr != nil {
		// Meta store init failed earlier — skip the put. The peer is
		// already on the wire (gw0.conf appended above); failing the
		// request now would be a ghost-peer surprise for the operator
		// (UI says fail, but the device is live). Loud-log instead so
		// removeUserPeers can be re-tried after the store is repaired.
		log.Printf("WARNING: meta store init failed: %v — peer %s won't be tracked for owner %s", metaErr, name, user)
	} else if err := meta.put(p); err != nil {
		log.Printf("meta put: %v (peer added to gw0.conf anyway)", err)
	}
	writeAudit(auditPath, auditEntry{Action: "peer.add", Actor: actor,
		Target: p.Name, Pubkey: p.PublicKey, IP: p.IP})

	reloadNote := ""
	if err := reloadInterface(s.cfg.awgDir, s.cfg.awgIface); err != nil {
		log.Printf("reload: %v", err)
		reloadNote = "config saved; reload needed: sudo systemctl restart awg-quick@" +
			s.cfg.awgIface + " on host"
	}
	clientConf := renderClientConf(p, pc, s.cfg)

	if err := archiveWrite(s.cfg, p.Name, []byte(clientConf)); err != nil {
		log.Printf("archive write %s: %v", p.Name, err)
		writeAudit(auditPath, auditEntry{Action: "peer.archive", Actor: actor,
			Target: p.Name, Result: "fail", Note: err.Error()})
	} else {
		writeAudit(auditPath, auditEntry{Action: "peer.archive", Actor: actor,
			Target: p.Name, Result: "ok", Note: "auto on create"})
	}

	csrf := ensureCSRF(w, r)
	help, helpErr := s.buildSetupHelp(r, p, clientConf)
	if helpErr != nil {
		log.Printf("buildSetupHelp %s: %v (rendering without QR)", p.Name, helpErr)
	}
	data := struct {
		Title         string
		User          string
		CSRF          string
		ViewerIsAdmin bool
		Peer          peer
		ClientConf    string
		ReloadNote    string
		SetupHelp     setupHelpData
	}{
		Title:         "device added",
		User:          actor,
		CSRF:          csrf,
		ViewerIsAdmin: viewerIsAdmin(r, s.cfg),
		Peer:          p,
		ClientConf:    clientConf,
		ReloadNote:    reloadNote,
		SetupHelp:     help,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	noStore(w)
	if err := s.tmpl.ExecuteTemplate(w, "peer-created.html", data); err != nil {
		log.Printf("template peer-created.html: %v", err)
	}
}

func (s *server) devicesForUser(user string) []peer {
	confPath := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	pc, err := loadConf(confPath)
	if err != nil {
		return nil
	}
	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		return nil
	}
	prefix := user + "-"
	out := []peer{}
	for _, p := range pc.listPeersWithMeta(meta) {
		if strings.HasPrefix(p.Name, prefix) {
			out = append(out, p)
		}
	}
	return out
}

func (s *server) handlePeerSub(w http.ResponseWriter, r *http.Request) {
	// Defence-in-depth: requireAuth normally gates this handler — but the
	// /peers/<name>/conf and /peers/<name>/qr.png paths leak per-peer
	// secrets, so 401 here too if the upstream gate is ever bypassed.
	if r.Header.Get("X-Enrol-User") == "" {
		http.Error(w, "401 — authentication required", http.StatusUnauthorized)
		return
	}
	if !s.awgEnabled() {
		// Same rationale as handlePeers: render the empty-state for
		// safe (GET) verbs so the operator sees the explanation rather
		// than a hard error if they bookmark or paste a /peers/<name>
		// URL on a default install.
		if r.Method != http.MethodGet {
			http.Error(w, "AmneziaWG (gw0) is not installed; "+
				"run scripts/install-gw0.sh on the host to enable devices",
				http.StatusServiceUnavailable)
			return
		}
		s.renderPeersDisabled(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/peers/")
	if rest == "" {
		http.Redirect(w, r, "/peers", http.StatusFound)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	if !validName(name) {
		http.Error(w, "invalid peer name", http.StatusBadRequest)
		return
	}
	// Per-user scoping: non-admins can only see/modify peers whose name
	// starts with `<their-username>-`. 404 (not 403) so the existence of
	// other users' peers isn't probeable.
	if !viewerIsAdmin(r, s.cfg) {
		viewer := r.Header.Get("X-Enrol-User")
		if !strings.HasPrefix(name, viewer+"-") {
			http.NotFound(w, r)
			return
		}
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "":
		s.handlePeerDetail(w, r, name)
	case "delete":
		s.handleDeletePeer(w, r, name)
	case "conf", "config":
		s.handleDownloadConf(w, r, name)
	case "qr.png":
		s.handleQR(w, r, name)
	case "upload-conf":
		s.handleUploadPeerConf(w, r, name)
	case "forget-conf":
		s.handleForgetPeerConf(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) findPeerByName(name string) (peer, *parsedConf, error) {
	pc, err := loadConf(filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf"))
	if err != nil {
		return peer{}, nil, err
	}
	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		return peer{}, nil, err
	}
	for _, p := range pc.listPeersWithMeta(meta) {
		if p.Name == name {
			return p, pc, nil
		}
	}
	return peer{}, pc, errors.New("not found")
}

func (s *server) handlePeerDetail(w http.ResponseWriter, r *http.Request, name string) {
	csrf := ensureCSRF(w, r)
	p, _, err := s.findPeerByName(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	clientConf, _, err := s.effectiveClientConf(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	help, helpErr := s.buildSetupHelp(r, p, clientConf)
	if helpErr != nil {
		log.Printf("buildSetupHelp %s: %v (rendering without QR)", p.Name, helpErr)
	}
	data := struct {
		Title         string
		User          string
		CSRF          string
		ViewerIsAdmin bool
		Peer          peer
		ClientConf    string
		ArchiveExists bool
		SetupHelp     setupHelpData
	}{
		Title:         "peer " + p.Name,
		User:          r.Header.Get("X-Enrol-User"),
		CSRF:          csrf,
		ViewerIsAdmin: viewerIsAdmin(r, s.cfg),
		Peer:          p,
		ClientConf:    clientConf,
		ArchiveExists: archiveExists(s.cfg, p.Name),
		SetupHelp:     help,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	noStore(w)
	if err := s.tmpl.ExecuteTemplate(w, "peer-detail.html", data); err != nil {
		log.Printf("template peer-detail.html: %v", err)
	}
}

func (s *server) handleDeletePeer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	s.mu.Lock()
	defer s.mu.Unlock()

	confPath := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	pc, err := loadConf(confPath)
	if err != nil {
		http.Error(w, "load conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		http.Error(w, "load meta: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var target peer
	for _, p := range pc.listPeersWithMeta(meta) {
		if p.Name == name {
			target = p
			break
		}
	}
	if target.PublicKey == "" {
		http.Error(w, "peer not found", http.StatusNotFound)
		return
	}
	ok, err := pc.removePeerByPubkey(confPath, target.PublicKey)
	if err != nil {
		writeAudit(auditPath, auditEntry{Action: "peer.remove", Actor: actor,
			Target: target.Name, Result: "fail", Note: err.Error()})
		http.Error(w, "rewrite conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "peer not found in gw0.conf", http.StatusNotFound)
		return
	}
	_ = meta.delete(target.PublicKey)
	writeAudit(auditPath, auditEntry{Action: "peer.remove", Actor: actor,
		Target: target.Name, Result: "ok",
		Pubkey: target.PublicKey, IP: target.IP})
	if err := reloadInterface(s.cfg.awgDir, s.cfg.awgIface); err != nil {
		log.Printf("reload after delete: %v", err)
	}
	http.Redirect(w, r, "/peers", http.StatusSeeOther)
}

// effectiveClientConf returns the rendered .conf for a peer, preferring
// the on-disk archive (which has the real PrivateKey written at create
// time) over re-rendering from gw0.conf (which would emit the
// "<PRIVATE_KEY_NOT_AVAILABLE_AFTER_CREATION>" placeholder, causing the
// AmneziaWG mobile parser to fail with "incorrect key length"). Returns
// the second value true iff the archive was used (caller may want to
// surface a "re-create or upload .conf" hint when false + no live key).
func (s *server) effectiveClientConf(name string) (string, bool, error) {
	if archiveExists(s.cfg, name) {
		if b, err := archiveRead(s.cfg, name); err == nil {
			return string(b), true, nil
		} else {
			log.Printf("archive read %s: %v (falling back to render)", name, err)
		}
	}
	p, pc, err := s.findPeerByName(name)
	if err != nil {
		return "", false, err
	}
	return renderClientConf(p, pc, s.cfg), false, nil
}

func (s *server) handleDownloadConf(w http.ResponseWriter, r *http.Request, name string) {
	conf, _, err := s.effectiveClientConf(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.conf"`, name))
	if _, err := w.Write([]byte(conf)); err != nil {
		log.Printf("download conf %s: write failed: %v", name, err)
		return
	}
}

func (s *server) handleUploadPeerConf(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	if _, _, err := s.findPeerByName(name); err != nil {
		http.Error(w, "peer not found", http.StatusNotFound)
		return
	}
	if err := r.ParseMultipartForm(256 << 10); err != nil {
		http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("conf")
	if err != nil {
		http.Error(w, "missing form file 'conf': "+err.Error(), http.StatusBadRequest)
		return
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 16<<10))
	if err != nil {
		http.Error(w, "read upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !looksLikePeerConf(body) {
		writeAudit(auditPath, auditEntry{Action: "peer.archive", Actor: actor,
			Target: name, Result: "fail", Note: "via upload: failed sanity check"})
		http.Error(w, "uploaded file does not look like a peer .conf "+
			"(expected [Interface], PrivateKey =, and a [Peer] block)",
			http.StatusBadRequest)
		return
	}
	if err := archiveWrite(s.cfg, name, body); err != nil {
		writeAudit(auditPath, auditEntry{Action: "peer.archive", Actor: actor,
			Target: name, Result: "fail", Note: "via upload: " + err.Error()})
		http.Error(w, "write archive: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{Action: "peer.archive", Actor: actor,
		Target: name, Result: "ok", Note: "via upload"})
	http.Redirect(w, r, "/peers/"+name, http.StatusSeeOther)
}

func (s *server) handleForgetPeerConf(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")
	if err := archiveDelete(s.cfg, name); err != nil {
		writeAudit(auditPath, auditEntry{Action: "peer.archive.forget", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		http.Error(w, "delete archive: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{Action: "peer.archive.forget", Actor: actor,
		Target: name, Result: "ok"})
	http.Redirect(w, r, "/peers/"+name, http.StatusSeeOther)
}

func looksLikePeerConf(b []byte) bool {
	s := string(b)
	if !strings.Contains(s, "[Interface]") {
		return false
	}
	if !strings.Contains(s, "[Peer]") {
		return false
	}
	if !strings.Contains(s, "PrivateKey") {
		return false
	}
	return true
}

func (s *server) handleQR(w http.ResponseWriter, r *http.Request, name string) {
	conf, _, err := s.effectiveClientConf(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	out, err := qrencode(conf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(out)
}

func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries := readAudit(filepath.Join(s.cfg.awgDir, "peers-audit.log"), 200)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// ---------------------------------------------------------------------------
// /  + /launcher/...

type launcherTileData struct {
	ID       string
	Name     string
	URL      string
	IconURL  string
	Initials string
}

type launcherData struct {
	Title         string
	User          string
	CSRF          string
	Apps          []launcherTileData
	ViewerIsAdmin bool
	Flash         string
}

func (s *server) handleLauncher(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.renderLauncher(w, r, "")
}

// adminTileIDs are the bootstrap tiles that point at admin-only
// surfaces. Non-admin users have those surfaces blocked at the Authelia
// ACL layer (console.${DOMAIN} -> group:admins) or at the enrol routing
// layer (enrol-users -> requireAdmin). Listing the dead tiles to a
// non-admin would just be a footgun, so the launcher filters them out.
// Operator-added custom tiles are NOT filtered — if an operator adds a
// tile they presumably want all users to see it. Toggle on a per-tile
// basis later if that turns out to be wrong.
var adminTileIDs = map[string]bool{
	"enrol-users": true,
	"console":     true,
}

func (s *server) renderLauncher(w http.ResponseWriter, r *http.Request, flash string) {
	csrf := ensureCSRF(w, r)
	apps, err := loadLauncher(s.cfg.launcherDir)
	if err != nil {
		http.Error(w, "load launcher: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-derive admin status from the Remote-Groups header set by
	// requireAuth; this is the canonical source for the request lifetime.
	groups := r.Header.Get("X-Enrol-Groups")
	isAdmin := false
	for _, g := range strings.Split(groups, ",") {
		if strings.TrimSpace(g) == s.cfg.requiredGroup {
			isAdmin = true
			break
		}
	}
	tiles := make([]launcherTileData, 0, len(apps))
	for _, a := range apps {
		if !isAdmin && adminTileIDs[a.ID] {
			continue
		}
		t := launcherTileData{
			ID:       a.ID,
			Name:     a.Name,
			URL:      a.URL,
			Initials: initialOf(a.Name),
		}
		switch {
		case a.Icon == "":
			t.IconURL = ""
		case strings.HasPrefix(a.Icon, "icons/"):
			t.IconURL = "/launcher/" + a.Icon
		default:
			log.Printf("launcher: app %q has unexpected icon path %q; using initials", a.ID, a.Icon)
			t.IconURL = ""
		}
		tiles = append(tiles, t)
	}
	data := launcherData{
		Title:         "apps",
		User:          r.Header.Get("X-Enrol-User"),
		CSRF:          csrf,
		Apps:          tiles,
		ViewerIsAdmin: isAdmin,
		Flash:         flash,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "launcher.html", data); err != nil {
		log.Printf("template launcher.html: %v", err)
	}
}

func (s *server) handleLauncherAddApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := strings.TrimSpace(r.Form.Get("id"))
	name := strings.TrimSpace(r.Form.Get("name"))
	urlStr := strings.TrimSpace(r.Form.Get("url"))
	iconURL := strings.TrimSpace(r.Form.Get("icon_url"))

	if !validAppID(id) {
		http.Error(w, "invalid app id (allowed: lowercase letter, then a-z0-9-, 1..32 chars)",
			http.StatusBadRequest)
		writeAudit(auditPath, auditEntry{Action: "launcher.app.add", Actor: actor,
			Target: id, Result: "fail", Note: "invalid id"})
		return
	}
	if name == "" || len(name) > 64 {
		http.Error(w, "name required (1..64 chars)", http.StatusBadRequest)
		return
	}
	if !validAppURL(urlStr) {
		http.Error(w, "invalid url (must be http/https)", http.StatusBadRequest)
		return
	}

	apps, err := loadLauncher(s.cfg.launcherDir)
	if err != nil {
		http.Error(w, "load launcher: "+err.Error(), http.StatusInternalServerError)
		return
	}
	for _, a := range apps {
		if a.ID == id {
			http.Error(w, "app already exists: "+id, http.StatusConflict)
			writeAudit(auditPath, auditEntry{Action: "launcher.app.add", Actor: actor,
				Target: id, Result: "fail", Note: "duplicate"})
			return
		}
	}

	icon := ""
	if iconURL != "" {
		got, err := fetchIcon(s.cfg.launcherDir, id, iconURL)
		if err != nil {
			http.Error(w, "fetch icon: "+err.Error(), http.StatusBadRequest)
			writeAudit(auditPath, auditEntry{Action: "launcher.app.add", Actor: actor,
				Target: id, Result: "fail", Note: "icon: " + err.Error()})
			return
		}
		icon = got
	}
	apps = append(apps, LauncherApp{ID: id, Name: name, URL: urlStr, Icon: icon})
	if err := saveLauncher(s.cfg.launcherDir, apps); err != nil {
		http.Error(w, "save launcher: "+err.Error(), http.StatusInternalServerError)
		writeAudit(auditPath, auditEntry{Action: "launcher.app.add", Actor: actor,
			Target: id, Result: "fail", Note: "save: " + err.Error()})
		return
	}
	writeAudit(auditPath, auditEntry{Action: "launcher.app.add", Actor: actor,
		Target: id, Result: "ok", Note: "url=" + urlStr})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleLauncherAppSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/launcher/apps/")
	if rest == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if !validAppID(id) {
		http.Error(w, "invalid app id", http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "delete":
		s.handleLauncherAppDelete(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleLauncherAppDelete(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")

	apps, err := loadLauncher(s.cfg.launcherDir)
	if err != nil {
		http.Error(w, "load launcher: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := apps[:0]
	found := false
	for _, a := range apps {
		if a.ID == id {
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		http.Error(w, "app not found", http.StatusNotFound)
		return
	}
	if err := saveLauncher(s.cfg.launcherDir, out); err != nil {
		http.Error(w, "save launcher: "+err.Error(), http.StatusInternalServerError)
		writeAudit(auditPath, auditEntry{Action: "launcher.app.delete", Actor: actor,
			Target: id, Result: "fail", Note: err.Error()})
		return
	}
	if err := removeOldIcons(s.cfg.launcherDir, id); err != nil {
		log.Printf("launcher: remove icons for %s: %v", id, err)
	}
	writeAudit(auditPath, auditEntry{Action: "launcher.app.delete", Actor: actor,
		Target: id, Result: "ok"})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleLauncherIcon(w http.ResponseWriter, r *http.Request) {
	basename := strings.TrimPrefix(r.URL.Path, "/launcher/icons/")
	if !reIconPath.MatchString(basename) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.cfg.launcherDir, "icons", basename))
}
