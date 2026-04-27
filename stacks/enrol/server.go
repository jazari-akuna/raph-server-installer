// server.go — HTTP routing + handlers.
//
// The server type holds:
//   - cfg: loaded config
//   - tmpl: parsed templates
//   - mu:   global mutex around all mutating operations (gw0.conf,
//           users_database.yml, LUKS, host useradd/userdel). The UI
//           is operator-only with at most a couple of admins, so
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
}

func newServer(cfg config) (*server, error) {
	tmpl := template.New("").Funcs(template.FuncMap{
		"join": strings.Join,
		// domain returns the apex domain for use in templates that need
		// to construct externally-facing URLs (e.g. the Authelia logout
		// link in _layout.html). Captured by closure so we don't have to
		// thread cfg.domain into every page-data struct.
		"domain": func() string { return cfg.domain },
		"gb": func(n int64) string {
			return fmt.Sprintf("%.1f", float64(n)/(1<<30))
		},
		"pct": func(part, whole int64) string {
			if whole <= 0 {
				return "0"
			}
			return fmt.Sprintf("%.2f", 100*float64(part)/float64(whole))
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
	return &server{cfg: cfg, tmpl: tmpl}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
		if _, err := os.Stat(path); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	mux.Handle("/static/", http.StripPrefix("/static/",
		http.FileServer(http.Dir(s.cfg.staticDir))))

	// IS the auth endpoint — no requireAuth, no CSRF. NPM rewrites only
	// POST /api/firstfactor here; we proxy verbatim to Authelia and on
	// success fire-and-forget a LUKS unlock for the user.
	mux.HandleFunc("/login-intercept", s.handleLoginIntercept)

	mux.HandleFunc("/", requireAuth(s.cfg, true, s.withCSRF(s.handleLauncher)))
	mux.HandleFunc("/launcher/apps", requireAuth(s.cfg, true, s.withCSRF(s.handleLauncherAddApp)))
	mux.HandleFunc("/launcher/apps/", requireAuth(s.cfg, true, s.withCSRF(s.handleLauncherAppSub)))
	mux.HandleFunc("/launcher/icons/", requireAuth(s.cfg, true, s.handleLauncherIcon))
	mux.HandleFunc("/audit", requireAuth(s.cfg, true, s.handleAudit))

	// Users
	mux.HandleFunc("/users", requireAuth(s.cfg, true, s.withCSRF(s.handleUsers)))
	mux.HandleFunc("/users/", requireAuth(s.cfg, true, s.withCSRF(s.handleUserSub)))

	// Peers (devices)
	mux.HandleFunc("/peers", requireAuth(s.cfg, true, s.withCSRF(s.handlePeers)))
	mux.HandleFunc("/peers/", requireAuth(s.cfg, true, s.withCSRF(s.handlePeerSub)))

	return mux
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
	Title   string
	User    string
	CSRF    string
	Users   []userRow
	Storage StorageInfo
	Flash   string
}

type userRow struct {
	Name        string
	DisplayName string
	Email       string
	Groups      []string
	HasVolume   bool
	VolMounted  bool
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
	for _, name := range names {
		u := db.Users[name]
		v := describeVolume(s.cfg, name)
		rows = append(rows, userRow{
			Name: name, DisplayName: u.DisplayName, Email: u.Email,
			Groups: u.Groups, HasVolume: v.Exists, VolMounted: v.Mounted,
		})
	}
	data := usersListData{
		Title:   "users",
		User:    r.Header.Get("X-Enrol-User"),
		CSRF:    csrf,
		Users:   rows,
		Storage: storageSnapshot(s.cfg, names),
		Flash:   flash,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "users.html", data); err != nil {
		log.Printf("template users.html: %v", err)
	}
}

// handleCreateUser is the orchestration of: validate → useradd →
// users_database.yml write → cryptsetup → mount → totp generate.
// On any step failure we audit the partial state so the operator can
// remediate.
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
	luksPass := r.Form.Get("luks_passphrase")

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
	if len(luksPass) < 12 {
		http.Error(w, "LUKS passphrase must be at least 12 characters",
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

	// 3. host useradd (idempotent).
	if err := hostUserAdd(name); err != nil {
		http.Error(w, "useradd: "+err.Error(), http.StatusInternalServerError)
		writeAudit(auditPath, auditEntry{Action: "user.create", Actor: actor,
			Target: name, Result: "fail", Note: "useradd: " + err.Error()})
		return
	}

	// 4. Splice into YAML and atomic-rename write.
	u := User{
		Disabled: false, DisplayName: displayname, Password: hash,
		Email: email, Groups: []string{"admins"},
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

	// 5. LUKS create.
	if err := luksCreate(s.cfg, name, luksPass); err != nil {
		writeAudit(auditPath, auditEntry{Action: "luks.create", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		// Render the user-created page anyway with the LUKS error
		// surfaced; the operator can retry from /users/<name>.
		s.renderUserCreated(w, r, name, "", nil, fmt.Sprintf(
			"LUKS volume creation FAILED: %v — fix and retry from the user page.", err))
		return
	}
	writeAudit(auditPath, auditEntry{Action: "luks.create", Actor: actor,
		Target: name, Result: "ok"})

	// 6. TOTP generate.
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
		Title      string
		User       string
		CSRF       string
		Created    string
		OtpauthURI string
		QRDataURI  template.URL
		Flash      string
	}{
		Title:      "user created: " + name,
		User:       r.Header.Get("X-Enrol-User"),
		CSRF:       csrf,
		Created:    name,
		OtpauthURI: otpauth,
		QRDataURI:  qrDataURI,
		Flash:      flash,
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
	case "luks/passphrase":
		s.handleLUKSPassphrase(w, r, name)
	case "luks/unlock":
		s.handleLUKSUnlock(w, r, name)
	case "luks/lock":
		s.handleLUKSLock(w, r, name)
	case "delete":
		s.handleUserDelete(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

type userDetailData struct {
	Title   string
	User    string
	CSRF    string
	Target  User
	Name    string
	Volume  VolumeInfo
	Devices []peer
	Flash   string
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
	v := describeVolume(s.cfg, name)
	devices := s.devicesForUser(name)
	data := userDetailData{
		Title:  "user " + name,
		User:   r.Header.Get("X-Enrol-User"),
		CSRF:   csrf,
		Target: u, Name: name, Volume: v, Devices: devices,
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
	groupsRaw := strings.TrimSpace(r.Form.Get("groups"))
	if !validDisplayName(displayname) {
		http.Error(w, "invalid display name", http.StatusBadRequest)
		return
	}
	if !validEmail(email) {
		http.Error(w, "invalid email", http.StatusBadRequest)
		return
	}
	groups := []string{}
	for _, g := range strings.Split(groupsRaw, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
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
	pwc := r.Form.Get("password_confirm")
	if pw == "" || pw != pwc {
		http.Error(w, "password missing or doesn't match confirmation",
			http.StatusBadRequest)
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

func (s *server) handleLUKSPassphrase(w http.ResponseWriter, r *http.Request, name string) {
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
	old := r.Form.Get("old")
	newp := r.Form.Get("new")
	confirm := r.Form.Get("new_confirm")
	if newp == "" || newp != confirm {
		http.Error(w, "new passphrase missing or doesn't match confirmation",
			http.StatusBadRequest)
		return
	}
	if len(newp) < 12 {
		http.Error(w, "passphrase must be at least 12 characters",
			http.StatusBadRequest)
		return
	}
	if err := luksChangePassphrase(s.cfg, name, old, newp); err != nil {
		writeAudit(auditPath, auditEntry{Action: "luks.passphrase", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		http.Error(w, "luks passphrase: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{Action: "luks.passphrase", Actor: actor,
		Target: name, Result: "ok"})
	http.Redirect(w, r, "/users/"+name, http.StatusSeeOther)
}

func (s *server) handleLUKSUnlock(w http.ResponseWriter, r *http.Request, name string) {
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
	pass := r.Form.Get("passphrase")
	if pass == "" {
		http.Error(w, "passphrase required", http.StatusBadRequest)
		return
	}
	if err := luksUnlock(s.cfg, name, pass); err != nil {
		writeAudit(auditPath, auditEntry{Action: "luks.unlock", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		http.Error(w, "luks unlock: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{Action: "luks.unlock", Actor: actor,
		Target: name, Result: "ok"})
	http.Redirect(w, r, "/users/"+name, http.StatusSeeOther)
}

func (s *server) handleLUKSLock(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	actor := r.Header.Get("X-Enrol-User")
	auditPath := filepath.Join(s.cfg.awgDir, "peers-audit.log")
	if err := luksLock(s.cfg, name); err != nil {
		writeAudit(auditPath, auditEntry{Action: "luks.lock", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		http.Error(w, "luks lock: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeAudit(auditPath, auditEntry{Action: "luks.lock", Actor: actor,
		Target: name, Result: "ok"})
	http.Redirect(w, r, "/users/"+name, http.StatusSeeOther)
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

	// 1. lock+remove LUKS volume.
	if err := luksDelete(s.cfg, name); err != nil {
		writeAudit(auditPath, auditEntry{Action: "luks.delete", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
		// Continue — the YAML edit is more important to complete.
	} else {
		writeAudit(auditPath, auditEntry{Action: "luks.delete", Actor: actor,
			Target: name, Result: "ok"})
	}

	// 2. remove all peers prefixed `<name>-`.
	if err := s.removeUserPeers(name, actor, auditPath); err != nil {
		log.Printf("removeUserPeers(%s): %v", name, err)
	}

	// 3. TOTP delete.
	if err := totpDelete(s.cfg, name); err != nil {
		writeAudit(auditPath, auditEntry{Action: "totp.delete", Actor: actor,
			Target: name, Result: "fail", Note: err.Error()})
	} else {
		writeAudit(auditPath, auditEntry{Action: "totp.delete", Actor: actor,
			Target: name, Result: "ok"})
	}

	// 4. users_database.yml drop entry.
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

	// 5. host userdel (best-effort).
	if err := hostUserDel(name); err != nil {
		log.Printf("hostUserDel(%s): %v", name, err)
	}

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
	for _, p := range toRemove {
		_, err := pc.removePeerByPubkey(confPath, p.PublicKey)
		if err != nil {
			writeAudit(auditPath, auditEntry{Action: "peer.remove", Actor: actor,
				Target: p.Name, Result: "fail", Note: err.Error()})
			continue
		}
		_ = meta.delete(p.PublicKey)
		writeAudit(auditPath, auditEntry{Action: "peer.remove", Actor: actor,
			Target: p.Name, Result: "ok",
			Pubkey: p.PublicKey, IP: p.IP})
		// Re-read pc so the next iteration sees the rewritten file.
		pc, _ = loadConf(confPath)
	}
	if len(toRemove) > 0 {
		_ = reloadInterface(s.cfg.awgDir, s.cfg.awgIface)
	}
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
	Title  string
	User   string
	CSRF   string
	Groups []peerGroup
	Users  []string
	Tags   []string
	Flash  string
}

func (s *server) handlePeers(w http.ResponseWriter, r *http.Request) {
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
	knownUsers := db.listSorted()

	groups := groupPeersByUser(all, knownUsers)

	data := peersListData{
		Title:  "devices",
		User:   r.Header.Get("X-Enrol-User"),
		CSRF:   csrf,
		Groups: groups,
		Users:  knownUsers,
		Tags:   []string{"laptop", "phone", "tablet", "other"},
		Flash:  flash,
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	user := strings.TrimSpace(r.Form.Get("user"))
	tag := strings.TrimSpace(r.Form.Get("device_tag"))
	name, err := peerNameFor(user, tag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	actor := r.Header.Get("X-Enrol-User")
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
	meta, _ := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
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
	if err := meta.put(p); err != nil {
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
	data := struct {
		Title      string
		User       string
		CSRF       string
		Peer       peer
		ClientConf string
		ReloadNote string
	}{
		Title:      "device added",
		User:       actor,
		CSRF:       csrf,
		Peer:       p,
		ClientConf: clientConf,
		ReloadNote: reloadNote,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
	p, pc, err := s.findPeerByName(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	clientConf := renderClientConf(p, pc, s.cfg)
	data := struct {
		Title         string
		User          string
		CSRF          string
		Peer          peer
		ClientConf    string
		ArchiveExists bool
	}{
		Title:         "peer " + p.Name,
		User:          r.Header.Get("X-Enrol-User"),
		CSRF:          csrf,
		Peer:          p,
		ClientConf:    clientConf,
		ArchiveExists: archiveExists(s.cfg, p.Name),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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

func (s *server) handleDownloadConf(w http.ResponseWriter, r *http.Request, name string) {
	if archiveExists(s.cfg, name) {
		if b, err := archiveRead(s.cfg, name); err == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Disposition",
				fmt.Sprintf(`attachment; filename="%s.conf"`, name))
			w.Write(b)
			return
		} else {
			log.Printf("archive read %s: %v (falling back to render)", name, err)
		}
	}
	p, pc, err := s.findPeerByName(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	conf := renderClientConf(p, pc, s.cfg)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.conf"`, name))
	io.WriteString(w, conf)
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
	p, pc, err := s.findPeerByName(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	conf := renderClientConf(p, pc, s.cfg)
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
	Title string
	User  string
	CSRF  string
	Apps  []launcherTileData
	Flash string
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

func (s *server) renderLauncher(w http.ResponseWriter, r *http.Request, flash string) {
	csrf := ensureCSRF(w, r)
	apps, err := loadLauncher(s.cfg.launcherDir)
	if err != nil {
		http.Error(w, "load launcher: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tiles := make([]launcherTileData, 0, len(apps))
	for _, a := range apps {
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
		Title: "apps",
		User:  r.Header.Get("X-Enrol-User"),
		CSRF:  csrf,
		Apps:  tiles,
		Flash: flash,
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
