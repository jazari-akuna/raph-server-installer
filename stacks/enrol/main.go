// enrol — server admin UI (users, cloud storage usage, TOTP, devices).
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
//   storage.go   /srv/store statfs + per-user du snapshot for the dashboard
//   totp.go      docker-exec into authelia for TOTP generate/delete
//
// See DESIGN.md for the full design.

package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// configuration

type config struct {
	// domain is the apex domain the installer is hosting on (e.g. "example.com").
	// All user-facing URLs are derived from it: cloud.${domain}, auth.${domain},
	// console.${domain}, gw.${domain}:443. Set via ENROL_DOMAIN; bootstrap
	// writes this from $DOMAIN at install time.
	domain string

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

	// forwardAuthSecret is the shared secret NPM injects on every protected
	// proxy host as `X-Forward-Auth-Secret`. enrol's requireAuth refuses
	// requests that don't carry it BEFORE looking at Remote-User. This
	// blocks header forgery from any container that can reach
	// 172.17.0.1:8080 (the docker bridge gateway enrol binds to). Read
	// from $ENROL_FORWARD_AUTH_SECRET — generated host-side by
	// scripts/generate-enrol-forward-auth-secret.sh and threaded through
	// /opt/stacks/.env. Empty value = fail-closed (every protected
	// request 401s).
	forwardAuthSecret string

	// Users / Authelia integration.
	usersDBPath       string // /etc/authelia/users_database.yml inside container
	autheliaContainer string // "authelia"
	autheliaURL       string // base URL for proxying /api/firstfactor (loopback)

	// (Pre-Wave-1: LUKS storage layout fields lived here. With Nextcloud-
	// managed cloud-data the per-user LUKS volumes are gone; the only
	// host-side path the dashboard cares about is /srv/store/cloud-data,
	// hardcoded in storage.go's cloudDataRoot const.)

	// Launcher (post-login app tile grid).
	launcherDir string // /srv/store/enrol-launcher

	// Peer .conf archive — authoritative copy of rendered client .conf
	// files (with private keys), used to re-serve downloads after the
	// initial creation page is gone. See peers_archive.go.
	peersArchiveDir string // /srv/store/enrol-peers-archive

	// Setup wizard (Parcel 3A). The wizard is the only UI the operator
	// sees from VPS creation until install completion; it is gated by the
	// ABSENCE of `setupCompleteSentinel`. The token is the out-of-band
	// proof of operator identity (no Authelia until the wizard finishes).
	//
	// setupStateDir holds the per-step persisted JSON (state.json plus
	// any provider creds we don't want spread across .env). setupToken is
	// validated on every /setup/* request.
	setupStateDir         string // /srv/store/setup
	setupCompleteSentinel string // /srv/store/.setup-complete
	setupTokenFile        string // /etc/raph-installer/setup-token
	setupToken            string // resolved at startup; preferred over file reads per-request
	stacksDir             string // /opt/stacks (compose root for finalize shell-outs)
	repoDir               string // /opt/raph-server-installer (for wire-npm-routes.sh, render-templates.sh)

	// task (Vikunja) DB integration for the admin /users page.
	// The task stack's docker container name + DB credentials live on
	// the host filesystem (/opt/stacks/task/.env). enrol shells out
	// `docker exec task-db psql` to read per-user task counts +
	// attachment bytes — Vikunja has no admin REST endpoint for this,
	// see the task_client.go banner for the rationale. Empty values
	// (e.g. task stack not yet deployed) silently degrade to zeros in
	// the admin page.
	taskDBContainer string
	taskDBName      string
	taskDBUser      string
	taskEnvFile     string

	// Backup (admin /backup page + nightly systemd timer). All four
	// resolve from env at startup so the same struct value drives the
	// HTTP server AND the `enrol backup` CLI subcommand. Defaults match
	// the bind-mount layout managed by scripts/bootstrap-phase2.sh:
	//
	//   backupRepoDir          — restic repository on /srv/store (rsync
	//                            backup-friendly per ADR-008's bind-mount
	//                            policy). Initialised by bootstrap.
	//   backupPasswordFile     — root-owned 0600 file outside /srv/store
	//                            so the password is NOT itself swept up
	//                            into a snapshot.
	//   backupResticBin        — restic binary path. Defaults to bare
	//                            `restic` (resolved via PATH inside the
	//                            container; the Dockerfile's apt-get
	//                            install puts it at /usr/bin/restic).
	//   backupRetentionDaily   — `--keep-daily` value passed to
	//                            `restic forget` after every scheduled
	//                            backup. Per-tag retention is computed
	//                            in Go (snapshotsToForget); restic's
	//                            own forget rules are not used so the
	//                            math is unit-testable.
	backupRepoDir        string
	backupPasswordFile   string
	backupResticBin      string
	backupRetentionDaily int
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
	domain := os.Getenv("ENROL_DOMAIN")
	if domain == "" {
		log.Fatal("ENROL_DOMAIN is required (set by bootstrap from $DOMAIN)")
	}
	return config{
		domain:            domain,
		listen:            envOr("ENROL_LISTEN", ":8080"),
		awgDir:            envOr("ENROL_AWG_DIR", "/etc/amnezia/amneziawg"),
		awgIface:          envOr("ENROL_AWG_IFACE", "gw0"),
		awgEndpoint:       envOr("ENROL_AWG_ENDPOINT", "gw."+domain+":443"),
		peerSubnet:        envOr("ENROL_PEER_SUBNET", "10.99.0.0/24"),
		peerStart:         start,
		headerUser:        envOr("ENROL_HEADER_USER", "Remote-User"),
		headerGroups:      envOr("ENROL_HEADER_GROUPS", "Remote-Groups"),
		requiredGroup:     envOr("ENROL_REQUIRED_GROUP", "admins"),
		forwardAuthSecret: os.Getenv("ENROL_FORWARD_AUTH_SECRET"),
		templatesDir:      envOr("ENROL_TEMPLATES", "/app/web/templates"),
		staticDir:         envOr("ENROL_STATIC", "/app/web/static"),
		usersDBPath:       envOr("ENROL_USERS_DB", "/etc/authelia/users_database.yml"),
		autheliaContainer: envOr("ENROL_AUTHELIA_CONTAINER", "authelia"),
		autheliaURL:       envOr("ENROL_AUTHELIA_URL", "http://127.0.0.1:9091"),
		launcherDir:       envOr("ENROL_LAUNCHER_DIR", "/srv/store/enrol-launcher"),
		peersArchiveDir:   envOr("ENROL_PEERS_ARCHIVE_DIR", "/srv/store/enrol-peers-archive"),

		// Setup wizard wiring. Defaults match Wave 2A's bootstrap layout:
		// token file is at /etc/raph-installer/setup-token (persisted on
		// the host across reboots), state lives under /srv/store, and
		// the sentinel that flips us out of wizard mode is a peer file.
		setupStateDir:         envOr("ENROL_SETUP_STATE_DIR", "/srv/store/setup"),
		setupCompleteSentinel: envOr("ENROL_SETUP_COMPLETE", "/srv/store/.setup-complete"),
		setupTokenFile:        envOr("ENROL_SETUP_TOKEN_FILE", "/etc/raph-installer/setup-token"),
		setupToken:            resolveSetupToken(),
		stacksDir:             envOr("ENROL_STACKS_DIR", "/opt/stacks"),
		repoDir:               envOr("ENROL_REPO_DIR", "/opt/raph-server-installer"),

		taskDBContainer: envOr("ENROL_TASK_DB_CONTAINER", "task-db"),
		taskDBName:      envOr("ENROL_TASK_DB_NAME", "vikunja"),
		taskDBUser:      envOr("ENROL_TASK_DB_USER", "vikunja"),
		taskEnvFile:     envOr("ENROL_TASK_ENV_FILE", "/opt/stacks/task/.env"),

		backupRepoDir:        envOr("ENROL_BACKUP_REPO_DIR", "/srv/store/enrol-backups/restic"),
		backupPasswordFile:   envOr("ENROL_BACKUP_PASSWORD_FILE", "/etc/raph-installer/restic-password"),
		backupResticBin:      envOr("ENROL_BACKUP_RESTIC_BIN", "restic"),
		backupRetentionDaily: atoiOr(envOr("ENROL_BACKUP_KEEP_DAILY", "10"), 10),
	}
}

// atoiOr returns the integer parse of s, or def on any error / blank.
// Used for env-driven config knobs where a malformed value should fall
// back to the safe default rather than killing startup.
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// (resolveTaskAPIToken removed — Vikunja swap moved the integration
// from REST-with-bearer-token to docker-exec + psql. The DB password
// is read on demand from /opt/stacks/task/.env by task_client.go.)

// resolveSetupToken prefers the explicit env var (useful for tests + Parcel 3B
// harness) and falls back to reading /etc/raph-installer/setup-token. An
// empty token is tolerated at startup — setupModeActive() returns false once
// the operator finishes the wizard, so the token becomes irrelevant. Setup
// mode + missing token surfaces as a 503 in the wizard middleware.
func resolveSetupToken() string {
	if v := strings.TrimSpace(os.Getenv("SETUP_TOKEN")); v != "" {
		return v
	}
	path := os.Getenv("ENROL_SETUP_TOKEN_FILE")
	if path == "" {
		path = "/etc/raph-installer/setup-token"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ---------------------------------------------------------------------------
// main

func main() {
	// Subcommand routing. The systemd timer at host/systemd/raph-backup.*
	// invokes `enrol backup --scheduled --tag=daily` inside this same
	// container so the CLI shares one binary + one config struct with the
	// HTTP server. Anything not recognised falls through to runServer().
	if len(os.Args) > 1 && os.Args[1] == "backup" {
		runBackupCLI(os.Args[2:])
		return
	}

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
	log.Printf("enrol listening on %s; awgDir=%s iface=%s usersDB=%s",
		cfg.listen, cfg.awgDir, cfg.awgIface, cfg.usersDBPath)
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
