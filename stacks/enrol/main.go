// enrol — server admin UI (users, LUKS storage, TOTP, devices).
//
// Trusts Authelia-injected `Remote-User` and `Remote-Groups` headers.
// All mutating routes require membership of $ENROL_REQUIRED_GROUP and
// a matching CSRF token. Auth headers MUST be present on every request —
// missing means the upstream NPM forward-auth was bypassed (misconfig);
// we 401 hard.
//
// File layout (this version splits the monolith from the prior revision
// into focused files; main.go is just config + server bootstrap):
//
//   main.go      this file: config loading + bootstrap
//   server.go    HTTP routes, render helpers, all handlers
//   auth.go      Remote-* header middleware
//   csrf.go      CSRF token mint/verify middleware + form helper
//   audit.go     append-only JSONL audit log
//   peers.go     gw0.conf parser/writer, peer keypair gen, reload
//   users.go     users_database.yml read/write + argon2id hashing
//   luks.go      cryptsetup / mkfs / mount / shred host-side ops
//   totp.go      docker-exec into authelia for TOTP generate/delete
//
// See DESIGN.md for the full design.

package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------------
// configuration

type config struct {
	listen        string
	awgDir        string
	awgIface      string
	awgEndpoint   string
	peerSubnet    string // e.g. "10.99.0.0/24"
	peerStart     int
	headerUser    string
	headerGroups  string
	requiredGroup string
	templatesDir  string
	staticDir     string

	// Users / Authelia integration.
	usersDBPath       string // /etc/authelia/users_database.yml inside container
	autheliaContainer string // "authelia"

	// LUKS storage layout.
	storeDataDir string // /srv/store/data
	storeMntDir  string // /srv/store/mnt
	luksSizeGB   int

	// Launcher (post-login app tile grid).
	launcherDir string // /srv/store/enrol-launcher
}

func loadConfig() config {
	envOr := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	startStr := envOr("ENROL_PEER_START", "10")
	start, err := strconv.Atoi(startStr)
	if err != nil {
		log.Fatalf("ENROL_PEER_START: %v", err)
	}
	luksSizeStr := envOr("ENROL_LUKS_SIZE_GB", "50")
	luksSize, err := strconv.Atoi(luksSizeStr)
	if err != nil || luksSize < 1 {
		log.Fatalf("ENROL_LUKS_SIZE_GB: must be a positive integer, got %q", luksSizeStr)
	}
	return config{
		listen:            envOr("ENROL_LISTEN", ":8080"),
		awgDir:            envOr("ENROL_AWG_DIR", "/etc/amnezia/amneziawg"),
		awgIface:          envOr("ENROL_AWG_IFACE", "gw0"),
		awgEndpoint:       envOr("ENROL_AWG_ENDPOINT", "gw.antarctica-engineering.com:51820"),
		peerSubnet:        envOr("ENROL_PEER_SUBNET", "10.99.0.0/24"),
		peerStart:         start,
		headerUser:        envOr("ENROL_HEADER_USER", "Remote-User"),
		headerGroups:      envOr("ENROL_HEADER_GROUPS", "Remote-Groups"),
		requiredGroup:     envOr("ENROL_REQUIRED_GROUP", "admins"),
		templatesDir:      envOr("ENROL_TEMPLATES", "/app/web/templates"),
		staticDir:         envOr("ENROL_STATIC", "/app/web/static"),
		usersDBPath:       envOr("ENROL_USERS_DB", "/etc/authelia/users_database.yml"),
		autheliaContainer: envOr("ENROL_AUTHELIA_CONTAINER", "authelia"),
		storeDataDir:      envOr("ENROL_STORE_DATA_DIR", "/srv/store/data"),
		storeMntDir:       envOr("ENROL_STORE_MNT_DIR", "/srv/store/mnt"),
		luksSizeGB:        luksSize,
		launcherDir:       envOr("ENROL_LAUNCHER_DIR", "/srv/store/enrol-launcher"),
	}
}

// ---------------------------------------------------------------------------
// main

func main() {
	cfg := loadConfig()
	srv, err := newServer(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	hs := &http.Server{
		Addr:              cfg.listen,
		Handler:           logMiddleware(srv.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("enrol listening on %s; awgDir=%s iface=%s usersDB=%s storeData=%s",
		cfg.listen, cfg.awgDir, cfg.awgIface, cfg.usersDBPath, cfg.storeDataDir)
	if err := hs.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func logMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s (%v)", r.Method, r.URL.Path,
			r.Header.Get("X-Enrol-User"), time.Since(start))
	})
}
