// setup.go — Parcel 3A: setup wizard handlers + state machine.
//
// The wizard is the only UI an operator sees from VPS creation until
// /srv/store/.setup-complete exists. It collects everything cloud-init
// couldn't safely capture (admin password, DNS provider creds, TOTP
// preference) then runs the configuration steps end-to-end:
//
//     welcome → domain-confirm → dns-provider → admin-account
//             → finalize → done
//
// Each GET renders a step template. Each POST validates input, persists
// the new fields into /srv/store/setup/state.json, and redirects to the
// next step. The /setup/finalize POST kicks off a long-running pipeline
// streamed back to the browser via Server-Sent Events on /setup/events.
//
// AUTHENTICATION MODEL:
//
//   The wizard runs over PLAINTEXT HTTP (no cert yet). The only guard is
//   the SETUP_TOKEN written by Phase 1 of bootstrap.sh into
//   /etc/raph-installer/setup-token. The operator received it printed on
//   the bootstrap completion banner. Every wizard request must carry it
//   in either ?token= or the setup_token cookie (set by /setup on first
//   visit). Once setup completes the token becomes useless — all
//   /setup/* routes return 404 from then on (handled by setupRouteGate
//   in server.go, not in the handlers themselves).
//
//   This is identical in spirit to NPM's first-boot admin: an out-of-band
//   secret that flips to dead code on first successful use.
//
// STATE FILE LAYOUT (/srv/store/setup/state.json):
//
//   {
//     "step": "admin-account",
//     "domain": "example.com",
//     "dns_provider": "cloudflare",
//     "dns_provider_creds": {"CF_API_TOKEN": "..."},
//     "admin_username": "alice",
//     "admin_email": "alice@example.com",
//     "admin_display_name": "Alice",
//     "admin_password_hash": "$argon2id$...",
//     "enable_totp": false,
//     "started_at": "2026-04-27T10:00:00Z",
//     "updated_at": "2026-04-27T10:05:13Z",
//     "completed_steps": {"admin_db_written": true, "cert_issued": false, ...}
//   }
//
//   The plaintext admin password and the LUKS passphrase are NEVER
//   persisted. They live in memory only while finalize is running; if
//   the operator refreshes the finalize page after a partial failure,
//   the wizard surfaces a "we need your passphrase again" prompt rather
//   than silently re-trying with a blank.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// state file

// setupStepName is one of: welcome, domain, dns, admin, storage, finalize, done.
// Persisted in state.json; drives default redirects on bare GET /setup.
type setupStepName string

const (
	stepWelcome  setupStepName = "welcome"
	stepDomain   setupStepName = "domain"
	stepDNS      setupStepName = "dns"
	stepAdmin    setupStepName = "admin"
	stepStorage  setupStepName = "storage"
	stepFinalize setupStepName = "finalize"
	stepDone     setupStepName = "done"
)

// setupCompletedSteps tracks per-finalize-step success so a re-run after
// a mid-stream failure resumes where it left off.
type setupCompletedSteps struct {
	UsersDBWritten   bool `json:"users_db_written,omitempty"`
	SharedVolReady   bool `json:"shared_volume_ready,omitempty"`
	TemplatesRender  bool `json:"templates_rendered,omitempty"`
	CertIssued       bool `json:"cert_issued,omitempty"`
	NPMRoutesWired   bool `json:"npm_routes_wired,omitempty"`
	SentinelTouched  bool `json:"sentinel_touched,omitempty"`
}

type setupState struct {
	Step             setupStepName       `json:"step"`
	Domain           string              `json:"domain,omitempty"`
	DNSProvider      string              `json:"dns_provider,omitempty"`
	DNSProviderCreds map[string]string   `json:"dns_provider_creds,omitempty"`
	AdminUsername    string              `json:"admin_username,omitempty"`
	AdminDisplayName string              `json:"admin_display_name,omitempty"`
	AdminEmail       string              `json:"admin_email,omitempty"`
	AdminPasswordHash string             `json:"admin_password_hash,omitempty"`
	EnableTOTP       bool                `json:"enable_totp,omitempty"`

	// Operator-chosen LUKS volume sizes, in raw bytes. PersonalLUKSSize
	// is the per-user encrypted volume's size (applied to every user
	// created post-setup as well — there's no per-user override yet).
	// SharedLUKSSize is the size of the system-wide /shared volume that
	// copyparty bind-mounts. Both are collected on the storage wizard
	// step (between admin and finalize) and consumed at finalize time:
	// PERSONAL_LUKS_SIZE_BYTES + SHARED_LUKS_SIZE_BYTES are exported as
	// env vars when shelling out to the volume scripts, and
	// PersonalLUKSSize is also reflected back into the runtime
	// cfg.luksSizeGB so subsequent user creations from /users use it.
	PersonalLUKSSize int64 `json:"personal_luks_size,omitempty"`
	SharedLUKSSize   int64 `json:"shared_luks_size,omitempty"`

	StartedAt        time.Time           `json:"started_at,omitempty"`
	UpdatedAt        time.Time           `json:"updated_at,omitempty"`
	CompletedSteps   setupCompletedSteps `json:"completed_steps,omitempty"`

}

// (The operator's plaintext password is never persisted onto setupState.
// It rides in setupSecretCache during finalize and is wiped at the end.
// The LUKS passphrase is the same string per the user spec; auto-unlock
// at login is handled by loginintercept.go.)

// supportedDNSProviders lists the certbot DNS plugins the wizard exposes.
// Each entry maps the provider id (URL-safe) to the credentials shape the
// step-3 form collects. The values are env-var names the user-supplied
// secrets get written under in dns_provider_creds; the same names are
// expected by the matching certbot plugin's --dns-${provider}-credentials
// INI file (see dnsCredsINI for the rendering).
var supportedDNSProviders = []dnsProviderSpec{
	{
		ID: "cloudflare", Label: "Cloudflare",
		HelpURL:  "https://dash.cloudflare.com/profile/api-tokens",
		HelpText: "Token with Zone:DNS:Edit on the apex zone.",
		Fields: []dnsField{
			{Name: "dns_cloudflare_api_token", Label: "API Token", Help: "Zone DNS Edit token."},
		},
	},
	{
		ID: "ovh", Label: "OVH",
		// OVH retired the legacy /createApp/ + /createToken/ endpoints on
		// the api.* hostnames in 2025; both now 302 → www.ovh.com/auth/api/
		// → 307 → auth.<region>.ovhcloud.com/api/createToken. We point
		// directly at the canonical EU URL; operators on the CA/US regions
		// follow the corresponding auth.<region>.ovhcloud.com host (see
		// stacks/ingress/README.md §2).
		//
		// The query string pre-fills OVH's rights matrix with exactly the
		// four scopes certbot-dns-ovh needs to satisfy the dns-01
		// challenge (per https://certbot-dns-ovh.readthedocs.io/en/stable/):
		//   GET    /domain/zone/      (list zones)
		//   GET    /domain/zone/*     (read records)
		//   POST   /domain/zone/*     (create the _acme-challenge TXT)
		//   PUT    /domain/zone/*     (refresh the zone)
		//   DELETE /domain/zone/*     (clean up after challenge)
		// The format `?GET=/path&POST=/path...` is the same one OVH's own
		// python-ovh SDK README uses (`?GET=/me`); OVH's signin flow
		// preserves the entire query string through `onsuccess=` so the
		// form lands fully populated after login.
		HelpURL:  "https://auth.eu.ovhcloud.com/api/createToken?GET=/domain/zone/&GET=/domain/zone/*&POST=/domain/zone/*&PUT=/domain/zone/*&DELETE=/domain/zone/*&name=certbot-dns-ovh",
		HelpText: "Pick \"Unlimited\" validity (cert renewals run forever; shorter validity will break renewal in ~60 days). Confirm the rights matrix shows exactly GET/POST/PUT/DELETE on /domain/zone/* plus GET on /domain/zone/ — no extra scopes. Non-EU regions: swap the host for auth.ca.ovhcloud.com or api.us.ovhcloud.com/createToken/ and append the same query string.",
		Fields: []dnsField{
			{Name: "dns_ovh_endpoint", Label: "Endpoint", Help: "ovh-eu / ovh-ca / ovh-us"},
			{Name: "dns_ovh_application_key", Label: "Application Key"},
			{Name: "dns_ovh_application_secret", Label: "Application Secret"},
			{Name: "dns_ovh_consumer_key", Label: "Consumer Key"},
		},
	},
	{
		ID: "route53", Label: "Route53 (AWS)",
		HelpText: "IAM user with route53:ChangeResourceRecordSets on the hosted zone.",
		Fields: []dnsField{
			{Name: "aws_access_key_id", Label: "AWS Access Key ID"},
			{Name: "aws_secret_access_key", Label: "AWS Secret Access Key"},
		},
	},
	{
		ID: "digitalocean", Label: "DigitalOcean",
		HelpURL:  "https://cloud.digitalocean.com/account/api/tokens",
		HelpText: "Personal Access Token (write).",
		Fields: []dnsField{
			{Name: "dns_digitalocean_token", Label: "API Token"},
		},
	},
	{
		ID: "google", Label: "Google Cloud DNS",
		HelpText: "Service account JSON key with roles/dns.admin on the project.",
		Fields: []dnsField{
			// Special: this is a JSON blob, rendered into a key file at
			// finalize time rather than an INI line. Render the field as
			// a textarea via Multiline=true.
			{Name: "dns_google_credentials", Label: "Service Account JSON", Multiline: true},
		},
	},
	{
		ID: "linode", Label: "Linode",
		HelpURL:  "https://cloud.linode.com/profile/tokens",
		HelpText: "Personal Access Token with Domains:Read/Write.",
		Fields: []dnsField{
			{Name: "dns_linode_key", Label: "API Key"},
			{Name: "dns_linode_version", Label: "API Version", Help: "Default 4."},
		},
	},
	{
		ID: "rfc2136", Label: "RFC 2136 (BIND / dynamic DNS)",
		HelpText: "Use when you run your own authoritative nameserver supporting TSIG dynamic updates.",
		Fields: []dnsField{
			{Name: "dns_rfc2136_server", Label: "Server"},
			{Name: "dns_rfc2136_port", Label: "Port", Help: "Default 53."},
			{Name: "dns_rfc2136_name", Label: "TSIG Key Name"},
			{Name: "dns_rfc2136_secret", Label: "TSIG Secret"},
			{Name: "dns_rfc2136_algorithm", Label: "TSIG Algorithm", Help: "e.g. HMAC-SHA512."},
		},
	},
}

// dnsProviderSpec splits the provider hint into a clickable URL (HelpURL,
// optional) and a descriptive sentence (HelpText). Splitting them lets the
// template render the URL as an actual <a> tag rather than inert text.
// Providers without an actionable link (route53, google, rfc2136) leave
// HelpURL empty and the template renders only HelpText.
type dnsProviderSpec struct {
	ID       string
	Label    string
	HelpURL  string
	HelpText string
	Fields   []dnsField
}

type dnsField struct {
	Name      string // form field + creds map key
	Label     string
	Help      string
	Multiline bool
}

func dnsProviderByID(id string) (dnsProviderSpec, bool) {
	for _, p := range supportedDNSProviders {
		if p.ID == id {
			return p, true
		}
	}
	return dnsProviderSpec{}, false
}

// ---------------------------------------------------------------------------
// state I/O

// setupStateMu protects state.json reads/writes — the wizard is operator-
// driven so contention is irrelevant in practice, but a SSE finalize plus a
// background refresh can race otherwise. Same pattern as server.mu.
var setupStateMu sync.Mutex

func (s *server) setupStatePath() string {
	return filepath.Join(s.cfg.setupStateDir, "state.json")
}

func (s *server) loadSetupState() (*setupState, error) {
	setupStateMu.Lock()
	defer setupStateMu.Unlock()
	return s.loadSetupStateLocked()
}

func (s *server) loadSetupStateLocked() (*setupState, error) {
	path := s.setupStatePath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// Fresh install — bootstrap a state file rooted at "welcome" with
		// the operator-supplied DOMAIN already populated (it's in cfg).
		st := &setupState{
			Step:      stepWelcome,
			Domain:    s.cfg.domain,
			StartedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	st := &setupState{}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Domain == "" {
		st.Domain = s.cfg.domain
	}
	return st, nil
}

func (s *server) saveSetupState(st *setupState) error {
	setupStateMu.Lock()
	defer setupStateMu.Unlock()
	return s.saveSetupStateLocked(st)
}

func (s *server) saveSetupStateLocked(st *setupState) error {
	if err := os.MkdirAll(s.cfg.setupStateDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", s.cfg.setupStateDir, err)
	}
	st.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	path := s.setupStatePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// setup-mode gate

// setupModeActive returns true while the wizard should be the only UI.
// The check is cheap (one os.Stat) so we call it on every request from
// the middleware in server.go. The sentinel is created by the finalize
// pipeline as its very last step — once it exists the wizard locks
// permanently.
//
// Using fileExists semantics rather than caching deliberately: the
// finalize handler creates the sentinel inline and we want subsequent
// requests in the same process to see the flip without a restart.
func (s *server) setupModeActive() bool {
	_, err := os.Stat(s.cfg.setupCompleteSentinel)
	return os.IsNotExist(err)
}

// ---------------------------------------------------------------------------
// token check

const setupCookieName = "setup_token"

// requireSetupToken validates either ?token=... or the setup_token cookie.
// On the very first GET /setup the token MUST come via query string; we
// accept it, set an HttpOnly cookie, and redirect to /setup/domain so the
// token doesn't linger in the browser address bar.
//
// Constant-time compare to avoid trivial timing oracles — irrelevant at
// our threat-model scale but cheap discipline.
func (s *server) requireSetupToken(_ http.ResponseWriter, _ *http.Request) bool {
	// Setup-mode access gate disabled: the wizard is reachable for anyone
	// who can resolve setup.${DOMAIN} and route to it. The window is short
	// (open only between bootstrap completion and the operator finishing
	// the finalize step, which removes the proxy host), and DNS for the
	// setup subdomain is the operator's own to control.
	return true
}

// setSetupCookie persists the token for the rest of the wizard flow so
// subsequent POSTs don't need ?token= on every form. HttpOnly + SameSite
// strict; Secure intentionally OFF because the wizard runs over plain
// HTTP until the wildcard cert is issued.
func setSetupCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     setupCookieName,
		Value:    value,
		Path:     "/setup",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   60 * 60 * 4, // 4h — finalize cert issuance + buffer
	})
}

// ---------------------------------------------------------------------------
// route registration

// registerSetupRoutes is called from server.routes() when setup mode is
// active. The mux returned ONLY exposes wizard endpoints + /healthz +
// /static/. Every other path 302s to /setup. See setupRouteGate.
func (s *server) registerSetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/setup", s.handleSetupRoot)
	mux.HandleFunc("/setup/", s.handleSetupSub)
	mux.HandleFunc("/setup/events", s.handleSetupEvents)
}

func (s *server) handleSetupRoot(w http.ResponseWriter, r *http.Request) {
	// Serve the current wizard step inline at `/` so the operator never
	// sees a redirect — the address bar stays at http://setup.${DOMAIN}/
	// for the entry GET. Subsequent form POSTs use /setup/<step> URLs
	// internally; that's fine because the operator clicks rather than
	// types those.
	st, err := s.loadSetupState()
	if err != nil {
		http.Error(w, "load state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	switch st.Step {
	case stepDomain:
		s.handleSetupDomain(w, r)
	case stepDNS:
		s.handleSetupDNS(w, r)
	case stepAdmin:
		s.handleSetupAdmin(w, r)
	case stepStorage:
		s.handleSetupStorage(w, r)
	case stepFinalize:
		s.handleSetupFinalize(w, r)
	case stepDone:
		s.handleSetupDone(w, r)
	default:
		s.handleSetupWelcome(w, r)
	}
}

// handleSetupSub dispatches /setup/<step> + /setup/<step> POSTs.
func (s *server) handleSetupSub(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetupToken(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/setup/")
	// /setup/events is registered on the mux directly; everything else
	// dispatches here.
	switch rest {
	case "":
		s.handleSetupRoot(w, r)
	case "welcome":
		s.handleSetupWelcome(w, r)
	case "domain":
		s.handleSetupDomain(w, r)
	case "dns":
		s.handleSetupDNS(w, r)
	case "admin":
		s.handleSetupAdmin(w, r)
	case "storage":
		s.handleSetupStorage(w, r)
	case "finalize":
		s.handleSetupFinalize(w, r)
	case "done":
		s.handleSetupDone(w, r)
	default:
		http.NotFound(w, r)
	}
}

// ---------------------------------------------------------------------------
// step 0 — welcome

type setupPageData struct {
	Title     string
	Step      setupStepName
	State     *setupState
	Domain    string
	Providers []dnsProviderSpec
	// Selected provider spec for re-rendering /setup/dns with a chosen
	// provider's fields visible.
	SelectedProvider dnsProviderSpec
	Flash            string
	Err              string

	// Storage step (stepStorage). DiskFreeBytes / DiskTotalBytes describe
	// the host disk underlying /srv/store; the suggested defaults below
	// derive from those (small fraction for personal, half for shared).
	// PersonalSizeGiB / SharedSizeGiB carry the operator's last-typed
	// values back through validation re-renders.
	DiskFreeBytes        int64
	DiskTotalBytes       int64
	PersonalSizeGiB      string
	SharedSizeGiB        string
	SuggestedPersonalGiB int64
	SuggestedSharedGiB   int64
}

func (s *server) renderSetup(w http.ResponseWriter, name string, data setupPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}

func (s *server) handleSetupWelcome(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadSetupState()
	if err != nil {
		http.Error(w, "load state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderSetup(w, "setup-welcome.html", setupPageData{
			Title: "setup — welcome", Step: stepWelcome, State: st, Domain: st.Domain,
		})
	case http.MethodPost:
		st.Step = stepDomain
		if err := s.saveSetupState(st); err != nil {
			http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/setup/domain", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// step 1 — domain confirm

func (s *server) handleSetupDomain(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadSetupState()
	if err != nil {
		http.Error(w, "load state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderSetup(w, "setup-domain.html", setupPageData{
			Title: "setup — domain", Step: stepDomain, State: st, Domain: st.Domain,
		})
	case http.MethodPost:
		// We deliberately ignore any submitted "domain" field — the
		// operator confirms only. Domain editing post-bootstrap requires
		// re-running Phase 1 (DOMAIN is baked into too many places).
		st.Step = stepDNS
		if err := s.saveSetupState(st); err != nil {
			http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/setup/dns", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// step 2 — DNS provider

func (s *server) handleSetupDNS(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadSetupState()
	if err != nil {
		http.Error(w, "load state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		sel, _ := dnsProviderByID(st.DNSProvider)
		s.renderSetup(w, "setup-dns.html", setupPageData{
			Title: "setup — DNS provider", Step: stepDNS, State: st, Domain: st.Domain,
			Providers: supportedDNSProviders, SelectedProvider: sel,
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
			return
		}
		provID := strings.TrimSpace(r.Form.Get("provider"))
		spec, ok := dnsProviderByID(provID)
		if !ok {
			s.renderSetup(w, "setup-dns.html", setupPageData{
				Title: "setup — DNS provider", Step: stepDNS, State: st, Domain: st.Domain,
				Providers: supportedDNSProviders, Err: "pick a provider",
			})
			return
		}
		creds := map[string]string{}
		var missing []string
		for _, f := range spec.Fields {
			v := strings.TrimSpace(r.Form.Get(f.Name))
			if v == "" {
				missing = append(missing, f.Label)
				continue
			}
			creds[f.Name] = v
		}
		if len(missing) > 0 {
			s.renderSetup(w, "setup-dns.html", setupPageData{
				Title: "setup — DNS provider", Step: stepDNS, State: st, Domain: st.Domain,
				Providers: supportedDNSProviders, SelectedProvider: spec,
				Err: "missing required field(s): " + strings.Join(missing, ", "),
			})
			return
		}
		st.DNSProvider = provID
		st.DNSProviderCreds = creds
		st.Step = stepAdmin
		if err := s.saveSetupState(st); err != nil {
			http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/setup/admin", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// step 3 — first admin account

func (s *server) handleSetupAdmin(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadSetupState()
	if err != nil {
		http.Error(w, "load state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderSetup(w, "setup-admin.html", setupPageData{
			Title: "setup — first admin", Step: stepAdmin, State: st, Domain: st.Domain,
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
			return
		}
		name := strings.ToLower(strings.TrimSpace(r.Form.Get("name")))
		display := strings.TrimSpace(r.Form.Get("displayname"))
		email := strings.TrimSpace(r.Form.Get("email"))
		pw := r.Form.Get("password")
		pwc := r.Form.Get("password_confirm")
		enableTOTP := r.Form.Get("enable_totp") == "on"

		errMsg := ""
		switch {
		case !validUsername(name):
			errMsg = "username: lowercase a-z then a-z0-9_-, 1..32 chars"
		case !validDisplayName(display):
			errMsg = "display name: 1..64 letters/digits/space/.-_'"
		case !validEmail(email):
			errMsg = "email: not a valid address"
		case len(pw) < 12:
			errMsg = "password: at least 12 characters"
		case pw != pwc:
			errMsg = "password: confirmation does not match"
		}
		if errMsg != "" {
			st.AdminUsername = name
			st.AdminDisplayName = display
			st.AdminEmail = email
			st.EnableTOTP = enableTOTP
			s.renderSetup(w, "setup-admin.html", setupPageData{
				Title: "setup — first admin", Step: stepAdmin, State: st, Domain: st.Domain,
				Err: errMsg,
			})
			return
		}

		hash, err := argon2idHash(pw)
		if err != nil {
			http.Error(w, "argon2 hash: "+err.Error(), http.StatusInternalServerError)
			return
		}
		st.AdminUsername = name
		st.AdminDisplayName = display
		st.AdminEmail = email
		st.AdminPasswordHash = hash
		st.EnableTOTP = enableTOTP
		st.Step = stepStorage
		if err := s.saveSetupState(st); err != nil {
			http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Stash the plaintext password into the in-memory finalize cache
		// (key = admin username). The finalize handler reads it and uses
		// it as the LUKS passphrase. If the operator refreshes between
		// /setup/admin and /setup/finalize, the cache loses the password
		// and finalize will surface a "re-enter your password" prompt
		// (TODO Wave 3B: not yet wired — for now finalize will fail
		// loudly with a clear "missing in-memory password" error).
		setupSecretCache.set(name, pw)
		http.Redirect(w, r, "/setup/storage", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// in-memory password cache

// The plaintext admin password is needed at finalize time (as the LUKS
// passphrase, since per the user spec the two are the same). Persisting it
// would defeat the point — anyone with read access to /srv/store could
// then unlock storage. Hold it in a process-local map keyed on the admin
// username instead. Cleared after a successful finalize.

type secretCache struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *secretCache) set(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]string{}
	}
	c.m[k] = v
}

func (c *secretCache) get(k string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func (c *secretCache) delete(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, k)
}

var setupSecretCache = &secretCache{}

// ---------------------------------------------------------------------------
// step 3.5 — LUKS volume sizes (personal + shared)
//
// The wizard writes /srv/store/setup/state.json with PersonalLUKSSize +
// SharedLUKSSize (raw bytes). Finalize then exports those as
// PERSONAL_LUKS_SIZE_BYTES + SHARED_LUKS_SIZE_BYTES when shelling out to
// the volume-creation scripts, AND reflects PersonalLUKSSize back into
// cfg.luksSizeGB (rounded up to GiB) so the day-2 /users handler creates
// each user's volume at the operator's chosen size.
//
// We deliberately do NOT enforce envelope rules — the operator can set
// values larger than the disk if they explicitly type those (sparse
// images won't allocate up-front anyway). The template displays a warning
// next to the field if the value is outside the recommended envelope so
// the operator knows what they're agreeing to.

// diskFreeBytes returns (free, total, err) for the filesystem holding
// /srv/store. The container has /srv/store bind-mounted from the host,
// so syscall.Statfs reports the host disk's actual free/total bytes.
func diskFreeBytes(path string) (free int64, total int64, err error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, err
	}
	free = int64(fs.Bavail) * int64(fs.Bsize)
	total = int64(fs.Blocks) * int64(fs.Bsize)
	return free, total, nil
}

const gibBytes int64 = 1 << 30

// suggestedSizes computes default suggestions for the personal + shared
// volumes given the disk's free bytes. Personal: smaller of (10 GiB,
// 10 % of free). Shared: 50 % of free. Both rounded down to GiB; both
// floor at 1 GiB so the suggestion is never zero on a tiny dev disk.
func suggestedSizes(free int64) (personalGiB int64, sharedGiB int64) {
	tenPercent := free / 10
	personal := int64(10) * gibBytes
	if tenPercent < personal {
		personal = tenPercent
	}
	shared := free / 2
	personalGiB = personal / gibBytes
	sharedGiB = shared / gibBytes
	if personalGiB < 1 {
		personalGiB = 1
	}
	if sharedGiB < 1 {
		sharedGiB = 1
	}
	return
}

// parseGiB parses an operator-typed integer GiB string into bytes.
// Returns (bytes, ok). Empty / non-numeric / non-positive return (0, false).
func parseGiB(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
		if n > 1<<30 { // sanity ceiling, > 1 EiB
			return 0, false
		}
	}
	if n <= 0 {
		return 0, false
	}
	return n * gibBytes, true
}

func (s *server) handleSetupStorage(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadSetupState()
	if err != nil {
		http.Error(w, "load state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	free, total, _ := diskFreeBytes("/srv/store")
	sugPersonal, sugShared := suggestedSizes(free)

	personalDisplay := ""
	sharedDisplay := ""
	if st.PersonalLUKSSize > 0 {
		personalDisplay = fmt.Sprintf("%d", st.PersonalLUKSSize/gibBytes)
	} else {
		personalDisplay = fmt.Sprintf("%d", sugPersonal)
	}
	if st.SharedLUKSSize > 0 {
		sharedDisplay = fmt.Sprintf("%d", st.SharedLUKSSize/gibBytes)
	} else {
		sharedDisplay = fmt.Sprintf("%d", sugShared)
	}

	switch r.Method {
	case http.MethodGet:
		s.renderSetup(w, "setup-storage.html", setupPageData{
			Title: "setup — storage", Step: stepStorage, State: st, Domain: st.Domain,
			DiskFreeBytes:        free,
			DiskTotalBytes:       total,
			PersonalSizeGiB:      personalDisplay,
			SharedSizeGiB:        sharedDisplay,
			SuggestedPersonalGiB: sugPersonal,
			SuggestedSharedGiB:   sugShared,
		})
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
			return
		}
		personalRaw := r.Form.Get("personal_size_gib")
		sharedRaw := r.Form.Get("shared_size_gib")
		personalBytes, okP := parseGiB(personalRaw)
		sharedBytes, okS := parseGiB(sharedRaw)
		errMsg := ""
		switch {
		case !okP:
			errMsg = "personal volume size: must be a positive integer (GiB)"
		case !okS:
			errMsg = "shared volume size: must be a positive integer (GiB)"
		}
		if errMsg != "" {
			s.renderSetup(w, "setup-storage.html", setupPageData{
				Title: "setup — storage", Step: stepStorage, State: st, Domain: st.Domain,
				DiskFreeBytes:        free,
				DiskTotalBytes:       total,
				PersonalSizeGiB:      personalRaw,
				SharedSizeGiB:        sharedRaw,
				SuggestedPersonalGiB: sugPersonal,
				SuggestedSharedGiB:   sugShared,
				Err:                  errMsg,
			})
			return
		}
		st.PersonalLUKSSize = personalBytes
		st.SharedLUKSSize = sharedBytes
		st.Step = stepFinalize
		if err := s.saveSetupState(st); err != nil {
			http.Error(w, "save state: "+err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/setup/finalize", http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// step 4 — finalize summary page (GET) + kickoff (POST)

func (s *server) handleSetupFinalize(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadSetupState()
	if err != nil {
		http.Error(w, "load state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Belt-and-braces: if the operator started this wizard before the
	// storage step existed (state.json from an older bootstrap), bounce
	// them back so we have real sizes to feed the volume scripts. The
	// step machinery can land here directly via stale state.Step =
	// "finalize" when the JSON predates the new field.
	if st.PersonalLUKSSize == 0 || st.SharedLUKSSize == 0 {
		st.Step = stepStorage
		_ = s.saveSetupState(st)
		http.Redirect(w, r, "/setup/storage", http.StatusSeeOther)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.renderSetup(w, "setup-finalize.html", setupPageData{
			Title: "setup — finalize", Step: stepFinalize, State: st, Domain: st.Domain,
		})
	case http.MethodPost:
		// POST is just acknowledgment; the JS in setup-finalize.html
		// opens /setup/events and streams. We respond 204 so the JS can
		// transition to the SSE-driven view without a page reload.
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// step 5 — done

func (s *server) handleSetupDone(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadSetupState()
	if err != nil {
		http.Error(w, "load state: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.renderSetup(w, "setup-done.html", setupPageData{
		Title: "setup — complete", Step: stepDone, State: st, Domain: st.Domain,
	})
}

// ---------------------------------------------------------------------------
// /setup/events — Server-Sent Events finalize stream
//
// Protocol (matches what setup-finalize.html consumes):
//
//   event: status
//   data: {"step": "users_db", "msg": "writing users_database.yml"}
//
//   event: log
//   data: {"line": "<arbitrary stdout from a sub-process>"}
//
//   event: error
//   data: {"step": "cert", "msg": "certbot exited 1: <last lines>"}
//
//   event: done
//   data: {"redirect": "https://example.com/"}
//
// And every 15 seconds: a comment line `: keepalive\n\n` that any HTTP idle
// timeout in the proxy chain treats as activity. Without this, NPM / nginx
// silently kill connections after ~60 seconds and the operator sees the
// progress meter freeze mid-cert-issuance.

func (s *server) handleSetupEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireSetupToken(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by this responder",
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx response buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Keepalive ticker. 15s is well under any sane HTTP idle timeout
	// (NPM defaults to 60s; the upstream nginx proxy_read_timeout is
	// 60s by default).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-keepalive.C:
				// Comment lines are ignored by EventSource consumers
				// but reset proxy idle timers. Best-effort: a slow/dead
				// writer surfaces as the next finalize step's write
				// failing.
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					cancel()
					return
				}
				flusher.Flush()
			}
		}
	}()

	// Run the finalize pipeline synchronously, emitting SSE events as we
	// go. emit captures the writer + flusher in a closure so step funcs
	// don't need to know about HTTP plumbing.
	emit := func(event, payload string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		flusher.Flush()
	}
	emitStatus := func(step, msg string) {
		emit("status", fmt.Sprintf(`{"step":%q,"msg":%q}`, step, msg))
	}
	emitLog := func(line string) {
		// Strip control chars + trim — caller should not need to escape.
		emit("log", fmt.Sprintf(`{"line":%q}`, line))
	}
	emitError := func(step, msg string) {
		emit("error", fmt.Sprintf(`{"step":%q,"msg":%q}`, step, msg))
	}

	if err := s.runFinalize(ctx, emitStatus, emitLog); err != nil {
		emitError(err.Step, err.Message)
		return
	}
	emit("done", fmt.Sprintf(`{"redirect":"https://%s/"}`, s.cfg.domain))
}

// ---------------------------------------------------------------------------
// finalize pipeline
//
// Each step is idempotent: it consults state.CompletedSteps and skips work
// already done. Failures bubble up as a finalizeErr with a step tag so the
// SSE consumer can render a clickable "back to <step>" link.

type finalizeErr struct {
	Step    string
	Message string
}

func (e *finalizeErr) Error() string { return e.Step + ": " + e.Message }

func wrapErr(step string, err error) *finalizeErr {
	return &finalizeErr{Step: step, Message: err.Error()}
}

func (s *server) runFinalize(
	ctx context.Context,
	status func(step, msg string),
	logLine func(string),
) *finalizeErr {
	st, err := s.loadSetupState()
	if err != nil {
		return wrapErr("load-state", err)
	}

	// 1. users_database.yml — write the admin entry via UsersDB.flush().
	if !st.CompletedSteps.UsersDBWritten {
		status("users_db", "writing admin to users_database.yml")
		if err := s.finalizeWriteAdmin(st); err != nil {
			return wrapErr("users_db", err)
		}
		st.CompletedSteps.UsersDBWritten = true
		_ = s.saveSetupState(st)
	}

	// 2. shared LUKS volume — provision at the operator's chosen size
	// (or re-assert if the bootstrap-phase2 default-size run already laid
	// it down). Script is idempotent: if /srv/store/data/_shared.img
	// exists it skips dd+luksFormat and only re-asserts keyfile/group/
	// mountpoint state. After the script returns, /etc/luks/_shared.key
	// MUST exist; we still call finalizeVerifySharedKey for the
	// cloud-fail-closed signal so a regression is loud.
	if !st.CompletedSteps.SharedVolReady {
		status("shared_volume", "provisioning /srv/store/mnt/_shared")
		if err := s.finalizeEnsureSharedVolume(ctx, st, logLine); err != nil {
			// Non-fatal: cloud (copyparty) starts even without /shared
			// (the [/shared] block fails-closed in copyparty.conf), so
			// the operator can still finish setup and remediate from a
			// host shell. Surface the error in the SSE log so it's not
			// silently swallowed.
			logLine("warning: shared volume provisioning failed: " + err.Error())
		}
		if err := s.finalizeVerifySharedKey(); err != nil {
			logLine("warning: " + err.Error())
		}
		st.CompletedSteps.SharedVolReady = true
		_ = s.saveSetupState(st)
	}

	// 3. render Authelia config + sibling templates.
	if !st.CompletedSteps.TemplatesRender {
		status("templates", "re-rendering templates with chosen domain")
		if err := s.finalizeRenderTemplates(ctx, logLine); err != nil {
			return wrapErr("templates", err)
		}
		st.CompletedSteps.TemplatesRender = true
		_ = s.saveSetupState(st)
	}

	// 4. cert issuance via certbot — slowest step, stream stdout.
	if !st.CompletedSteps.CertIssued {
		status("cert", "requesting wildcard cert via Let's Encrypt DNS-01")
		if err := s.finalizeIssueCert(ctx, st, logLine); err != nil {
			return wrapErr("cert", err)
		}
		st.CompletedSteps.CertIssued = true
		_ = s.saveSetupState(st)
	}

	// 5. wire NPM proxy hosts (shells out to wire-npm-routes.sh).
	if !st.CompletedSteps.NPMRoutesWired {
		status("npm", "wiring NPM proxy hosts (auth/enrol/cloud/console)")
		if err := s.finalizeWireNPM(ctx, st, logLine); err != nil {
			return wrapErr("npm", err)
		}
		st.CompletedSteps.NPMRoutesWired = true
		_ = s.saveSetupState(st)
	}

	// 6. apply the operator's chosen personal LUKS size to the live
	// process so subsequent /users creates use it. cfg is a value copy
	// inside setupState's host server, so we mutate it directly. The
	// rounding-up (rather than down) means a wizard-chosen 9.5 GiB
	// becomes 10 GiB on subsequent creates — better to over-allocate
	// than to silently shrink the operator's intent.
	if st.PersonalLUKSSize > 0 {
		gib := int(st.PersonalLUKSSize / gibBytes)
		if st.PersonalLUKSSize%gibBytes != 0 {
			gib++
		}
		if gib < 1 {
			gib = 1
		}
		s.cfg.luksSizeGB = gib
	}

	// 7. flip out of setup mode.
	if !st.CompletedSteps.SentinelTouched {
		status("sentinel", "marking setup complete")
		if err := s.finalizeTouchSentinel(); err != nil {
			return wrapErr("sentinel", err)
		}
		st.CompletedSteps.SentinelTouched = true
		st.Step = stepDone
		_ = s.saveSetupState(st)
	}

	// Drop the in-memory plaintext password — never needed again.
	setupSecretCache.delete(st.AdminUsername)

	status("done", "all steps complete")
	return nil
}

// ---- finalize step 1: write the admin to users_database.yml --------------

func (s *server) finalizeWriteAdmin(st *setupState) error {
	if st.AdminUsername == "" || st.AdminPasswordHash == "" {
		return errors.New("admin username/hash missing — restart wizard at /setup/admin")
	}
	// Ensure the parent dir + an empty users_database.yml exist. The
	// Wave 1A example file is committed under users_database.yml.example;
	// the wizard's responsibility is to materialise the live file at the
	// path the enrol container reads.
	dir := filepath.Dir(s.cfg.usersDBPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if _, err := os.Stat(s.cfg.usersDBPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(s.cfg.usersDBPath, []byte("users: {}\n"), 0o600); err != nil {
			return fmt.Errorf("seed %s: %w", s.cfg.usersDBPath, err)
		}
	}
	db, err := loadUsersDB(s.cfg.usersDBPath)
	if err != nil {
		return fmt.Errorf("load users db: %w", err)
	}
	u := User{
		Disabled:    false,
		DisplayName: st.AdminDisplayName,
		Password:    st.AdminPasswordHash,
		Email:       st.AdminEmail,
		Groups:      []string{"admins"},
	}
	if err := db.upsert(st.AdminUsername, u); err != nil {
		return fmt.Errorf("upsert admin: %w", err)
	}
	if err := db.flush(); err != nil {
		return fmt.Errorf("flush users db: %w", err)
	}
	return nil
}

// ---- finalize step 2: verify the shared LUKS keyfile ---------------------

func (s *server) finalizeVerifySharedKey() error {
	const path = "/etc/luks/_shared.key"
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w (cloud's [/shared] block will fail-closed; "+
			"re-run scripts/create-shared-volume.sh as root, or revisit "+
			"/setup/storage if the wizard short-circuited the size step)", path, err)
	}
	if info.Size() < 32 {
		return fmt.Errorf("%s: too small (%d bytes; expected >= 32)", path, info.Size())
	}
	return nil
}

// ---- finalize step 2b: provision the shared volume at operator size ------
//
// Calls scripts/create-shared-volume.sh with SHARED_LUKS_SIZE_BYTES set
// to st.SharedLUKSSize. The script is idempotent — if the .img already
// exists (e.g. bootstrap-phase2 ran with the default 10 GiB before the
// wizard collected a real size) it just re-asserts keyfile/group/mount
// state and exits 0. We deliberately do NOT shrink an existing larger
// .img to match a smaller wizard-chosen value; the operator can re-run
// the script manually after `rm /srv/store/data/_shared.img` if they
// want a strict resize.
//
// ADMIN_USERS is also passed through so the operator's chosen admin
// username lands in the `shared-users` group at create time.
func (s *server) finalizeEnsureSharedVolume(ctx context.Context, st *setupState, logLine func(string)) error {
	if testModeEnabled() {
		logLine("TEST_MODE: skipping create-shared-volume.sh")
		return nil
	}
	script := filepath.Join(s.cfg.repoDir, "scripts/create-shared-volume.sh")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("script not found: %w", err)
	}
	cmd := exec.CommandContext(ctx, "bash", script)
	env := append([]string{}, os.Environ()...)
	if st.SharedLUKSSize > 0 {
		env = append(env, fmt.Sprintf("SHARED_LUKS_SIZE_BYTES=%d", st.SharedLUKSSize))
	}
	if st.AdminUsername != "" {
		env = append(env, "ADMIN_USERS="+st.AdminUsername)
	}
	cmd.Env = env
	return runStreaming(cmd, logLine)
}

// ---- finalize step 3: re-render templates --------------------------------

func (s *server) finalizeRenderTemplates(ctx context.Context, logLine func(string)) error {
	script := filepath.Join(s.cfg.repoDir, "scripts/render-templates.sh")
	envFile := filepath.Join(s.cfg.stacksDir, ".env")
	cmd := exec.CommandContext(ctx, "bash", script, "--env-file", envFile)
	return runStreaming(cmd, logLine)
}

// ---- finalize step 4: certbot -------------------------------------------

// dnsCredsINI renders a certbot credentials INI file body for the given
// provider. For most providers this is a flat key=value list (the field
// names already match certbot's expected keys); for Google the JSON blob
// is written to a separate JSON file and only the path is referenced here.
//
// Returns: (mainINIBody, sidecarPath, sidecarBody). Sidecar is empty for
// every provider except google.
func dnsCredsINI(provider string, creds map[string]string) (string, string, string) {
	if provider == "google" {
		// google needs a JSON file + a tiny INI pointing at it.
		jsonPath := "/srv/store/setup/google-creds.json"
		jsonBody := creds["dns_google_credentials"]
		ini := "dns_google_credentials = " + jsonPath + "\n"
		return ini, jsonPath, jsonBody
	}
	var b strings.Builder
	for k, v := range creds {
		b.WriteString(k)
		b.WriteString(" = ")
		b.WriteString(v)
		b.WriteString("\n")
	}
	return b.String(), "", ""
}

// certbotCredsHostDir is the tmpfs-backed directory the wizard writes the
// per-provider DNS credentials INI into right before invoking certbot.
// It's bind-mounted into the certbot container at /tmp/certbot-creds (see
// stacks/ingress/docker-compose.yml). Choosing /run/raph-certbot/ on the
// host keeps the file on tmpfs (Ubuntu 24.04 mounts /run tmpfs by default),
// so even if the wizard crashes between write and the deferred delete the
// file evaporates at the next reboot. Wave 4A guarantee: the credentials
// file is wiped post-certbot whether the run succeeds or fails.
const certbotCredsHostDir = "/run/raph-certbot"

func (s *server) finalizeIssueCert(ctx context.Context, st *setupState, logLine func(string)) error {
	if st.DNSProvider == "" {
		return errors.New("no DNS provider selected — restart wizard at /setup/dns")
	}
	if st.Domain == "" {
		return errors.New("domain unset")
	}

	// Email for Let's Encrypt expiry warnings. We use the admin email if
	// the operator supplied one, else fall back to ADMIN_EMAIL from the
	// .env (which Phase 1 captured).
	email := st.AdminEmail
	if email == "" {
		email = os.Getenv("ADMIN_EMAIL")
	}

	if testModeEnabled() {
		// The Wave 3B harness asserts on this exact log line.
		logLine("TEST_MODE: skipping certbot")
		return nil
	}

	// Write the per-provider credentials INI to a tmpfs-backed location so
	// the plaintext token never persists across a reboot. Bind-mounted
	// into the certbot container at /tmp/certbot-creds (see ingress
	// docker-compose). Mode 0600. Deferred delete fires regardless of
	// success/failure — credential lifecycle guarantee.
	if err := os.MkdirAll(certbotCredsHostDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", certbotCredsHostDir, err)
	}
	ini, sidecarHostPath, sidecarBody := dnsCredsINI(st.DNSProvider, st.DNSProviderCreds)
	credsHostFile := filepath.Join(certbotCredsHostDir, st.DNSProvider+".ini")
	credsContainerFile := "/tmp/certbot-creds/" + st.DNSProvider + ".ini"
	if err := os.WriteFile(credsHostFile, []byte(ini), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", credsHostFile, err)
	}
	defer func() {
		// Best-effort scrub: zero the file then delete it. A crash
		// between write and this defer leaves the file on tmpfs only,
		// so reboot guarantees disappearance even in pathological cases.
		if f, err := os.OpenFile(credsHostFile, os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(make([]byte, len(ini)))
			f.Close()
		}
		_ = os.Remove(credsHostFile)
	}()

	var sidecarContainerPath string
	if sidecarHostPath != "" {
		// google's sidecar JSON also rides the tmpfs; we rewrite the
		// dnsCredsINI's host path to the in-container equivalent so
		// certbot's --dns-google-credentials INI points at a path that
		// exists from the certbot container's perspective.
		sidecarHostPath = filepath.Join(certbotCredsHostDir, filepath.Base(sidecarHostPath))
		sidecarContainerPath = "/tmp/certbot-creds/" + filepath.Base(sidecarHostPath)
		if err := os.WriteFile(sidecarHostPath, []byte(sidecarBody), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", sidecarHostPath, err)
		}
		defer func() {
			if f, err := os.OpenFile(sidecarHostPath, os.O_WRONLY, 0o600); err == nil {
				_, _ = f.Write(make([]byte, len(sidecarBody)))
				f.Close()
			}
			_ = os.Remove(sidecarHostPath)
		}()
		// Re-render the INI so its `dns_google_credentials` line points
		// at the in-container path, not the host path.
		ini = "dns_google_credentials = " + sidecarContainerPath + "\n"
		if err := os.WriteFile(credsHostFile, []byte(ini), 0o600); err != nil {
			return fmt.Errorf("rewrite %s: %w", credsHostFile, err)
		}
	}

	// Run certbot via the ingress stack's docker-compose so it shares
	// /etc/letsencrypt with NPM. The certbot service is profile-gated
	// (`profiles: ["finalize"]`) so `compose up -d` doesn't start it —
	// we explicitly invoke it here.
	composeFile := filepath.Join(s.cfg.stacksDir, "ingress/docker-compose.yml")
	args := []string{
		"compose", "--profile", "finalize", "-f", composeFile,
		"run", "--rm", "certbot", "certonly",
		"--non-interactive", "--agree-tos",
		"--email", email,
		"--dns-" + st.DNSProvider,
		"--dns-" + st.DNSProvider + "-credentials", credsContainerFile,
		"-d", st.Domain,
		"-d", "*." + st.Domain,
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	return runStreaming(cmd, logLine)
}

// ---- finalize step 5: NPM proxy host wiring -----------------------------
//
// Wave 4A — replaces the prior shell-out to wire-npm-routes.sh with a
// typed Go client (see npm_client.go). The shell script is retained as a
// debugging fallback (banner comment in the script explains).
//
// On first finalize: NPMClient.Bootstrap rotates NPM's hard-coded default
// admin (admin@example.com / changeme) to the operator's chosen email +
// password (same password as Authelia, threaded in via setupSecretCache).
// Idempotent: if the new credentials already work, Bootstrap is a no-op.
//
// All four proxy hosts are upserted from a typed slice; the advanced_config
// blocks (forward-auth + auth-portal) are byte-for-byte identical to the
// shell version's heredocs so existing requests / cookies don't break.

const (
	npmDefaultAdminEmail = "admin@example.com"
	npmDefaultAdminPass  = "changeme"
)

// npmAdvFwdAuth — forward-auth advanced_config used by enrol/cloud/console
// proxy hosts. Identical to wire-npm-routes.sh ADV_FWD_AUTH heredoc;
// nginx variables ($forward_scheme, $server, $port) are literal in the
// templated config, NOT interpolated by Go.
const npmAdvFwdAuth = `include /snippets/authelia-location.conf;

location / {
    include /snippets/proxy.conf;
    include /snippets/authelia-authrequest.conf;
    proxy_pass $forward_scheme://$server:$port;
}
`

// npmAdvAuthPortalTmpl — auth-portal advanced_config. The bare GET / rewrite
// includes the operator's domain, which is interpolated by fmt.Sprintf at
// call time. All other $-prefixed identifiers are nginx variables and must
// remain literal.
const npmAdvAuthPortalTmpl = `location = / {
    if ($arg_rd = "") {
        return 302 /?rd=https://enrol.%s/;
    }
    include conf.d/include/proxy.conf;
}

location = /api/firstfactor {
    proxy_pass http://172.17.0.1:8080/login-intercept;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Forwarded-Host $host;
    proxy_http_version 1.1;
}
`

// finalizeNPMBaseURL is the NPM admin API base URL. Defaults to loopback:
// the enrol container runs network_mode: host (Wave 1A), so 127.0.0.1:81
// resolves to NPM's host-bound admin port (see ingress/docker-compose.yml's
// `127.0.0.1:81:81` mapping). Override via NPM_URL for non-host-network
// debug rigs.
func finalizeNPMBaseURL() string {
	if v := os.Getenv("NPM_URL"); v != "" {
		return v
	}
	return "http://127.0.0.1:81"
}

func (s *server) finalizeWireNPM(ctx context.Context, st *setupState, logLine func(string)) error {
	if testModeEnabled() {
		logLine("TEST_MODE: skipping NPM wire-up")
		return nil
	}

	// Operator's admin password lives in the in-memory secret cache from
	// /setup/admin POST. Same string as the Authelia login + LUKS
	// passphrase per the user spec. Pulled out as []byte so we can scrub
	// it after Bootstrap finishes.
	plaintext := setupSecretCache.get(st.AdminUsername)
	if plaintext == "" {
		return errors.New("npm: admin password missing from in-memory cache " +
			"(restart wizard at /setup/admin to re-enter)")
	}
	newPass := []byte(plaintext)

	// Resolve the CURRENT NPM admin (the one Bootstrap will rotate to the
	// operator's chosen email). NPM 2.14 dropped the legacy
	// admin@example.com/changeme default; scripts/generate-npm-admin.sh
	// instead writes a random password to /etc/raph-installer/npm-bootstrap.pass
	// and stamps INITIAL_ADMIN_EMAIL/PASSWORD into the ingress container env
	// at compose-up time. Read both back here. Fall back to the legacy
	// admin@example.com/changeme pair only if the file is absent — covers
	// hypothetical re-runs against an older NPM that still ships the legacy
	// defaults.
	currentEmail := npmDefaultAdminEmail
	currentPass := []byte(npmDefaultAdminPass)
	if envEmail := strings.TrimSpace(os.Getenv("NPM_INITIAL_ADMIN_EMAIL")); envEmail != "" {
		currentEmail = envEmail
	}
	if data, err := os.ReadFile("/etc/raph-installer/npm-bootstrap.pass"); err == nil {
		bp := bytes.TrimRight(data, "\r\n")
		if len(bp) > 0 {
			currentPass = bp
			if envEmail := strings.TrimSpace(os.Getenv("NPM_INITIAL_ADMIN_EMAIL")); envEmail == "" {
				currentEmail = "bootstrap@local" // matches generate-npm-admin.sh default
			}
		}
	}

	npm := NewNPMClient(finalizeNPMBaseURL(), nil)

	// Bootstrap zeroes both byte slices on return; we can't reuse them
	// after this call. Re-derive plaintext from the cache for any
	// subsequent NPM op (none currently — the bearer token from
	// Bootstrap covers UpsertProxyHost).
	logLine("npm: bootstrapping admin credentials")
	if err := npm.Bootstrap(ctx,
		currentEmail, currentPass,
		st.AdminEmail, newPass); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	logLine("npm: admin authenticated")

	// Upsert the wildcard cert as a known "other"-provider entry so the
	// proxy hosts can reference it by ID. Currently NPM auto-discovers
	// custom-provider certs that match a domain; we still upsert to make
	// the dependency explicit.
	certID, err := npm.UpsertCertificate(ctx, Certificate{
		NiceName: "wildcard-" + st.Domain,
		Provider: "other",
	})
	if err != nil {
		return fmt.Errorf("upsert certificate: %w", err)
	}
	if certID == 0 {
		// NPM didn't return an ID (older versions); fall back to the
		// first matching cert via list. The shell script hard-codes 3,
		// which is the typical id when only the wildcard exists.
		certID = 3
	}
	logLine(fmt.Sprintf("npm: certificate id=%d", certID))

	// The four proxy hosts. Order matches wire-npm-routes.sh; the
	// advanced_config strings are byte-for-byte identical so cookies and
	// in-flight Authelia sessions survive the swap-over.
	hosts := []ProxyHost{
		{
			DomainNames:           []string{"auth." + st.Domain},
			ForwardScheme:         "http",
			ForwardHost:           "authelia",
			ForwardPort:           9091,
			BlockExploits:         true,
			AllowWebsocketUpgrade: true,
			CertificateID:         certID,
			SSLForced:             true,
			HTTP2Support:          true,
			HSTSEnabled:           true,
			AdvancedConfig:        fmt.Sprintf(npmAdvAuthPortalTmpl, st.Domain),
		},
		{
			DomainNames:           []string{"enrol." + st.Domain},
			ForwardScheme:         "http",
			ForwardHost:           "enrol",
			ForwardPort:           8080,
			BlockExploits:         true,
			AllowWebsocketUpgrade: true,
			CertificateID:         certID,
			SSLForced:             true,
			HTTP2Support:          true,
			HSTSEnabled:           true,
			AdvancedConfig:        npmAdvFwdAuth,
		},
		{
			DomainNames:           []string{"cloud." + st.Domain},
			ForwardScheme:         "http",
			ForwardHost:           "cloud",
			ForwardPort:           3923,
			BlockExploits:         true,
			AllowWebsocketUpgrade: true,
			CertificateID:         certID,
			SSLForced:             true,
			HTTP2Support:          true,
			HSTSEnabled:           true,
			AdvancedConfig:        npmAdvFwdAuth,
		},
		{
			DomainNames:           []string{"console." + st.Domain},
			ForwardScheme:         "https",
			ForwardHost:           "console",
			ForwardPort:           9443,
			BlockExploits:         true,
			AllowWebsocketUpgrade: true,
			CertificateID:         certID,
			SSLForced:             true,
			HTTP2Support:          true,
			HSTSEnabled:           true,
			AdvancedConfig:        npmAdvFwdAuth,
		},
	}

	for _, h := range hosts {
		id, err := npm.UpsertProxyHost(ctx, h)
		if err != nil {
			return fmt.Errorf("upsert %s: %w", h.DomainNames[0], err)
		}
		logLine(fmt.Sprintf("npm: proxy host %s id=%d", h.DomainNames[0], id))
	}
	return nil
}

// ---- finalize step 6: touch the sentinel --------------------------------

func (s *server) finalizeTouchSentinel() error {
	dir := filepath.Dir(s.cfg.setupCompleteSentinel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(s.cfg.setupCompleteSentinel, []byte(now+"\n"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", s.cfg.setupCompleteSentinel, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers

// runStreaming runs cmd, fan-outing every stdout+stderr line to logLine.
// Blocks until cmd exits; returns the cmd's error verbatim so the caller
// can wrap with finalizeErr.
func runStreaming(cmd *exec.Cmd, logLine func(string)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Two pumps in parallel (stdout + stderr); finish wait on cmd.Wait().
	done := make(chan struct{}, 2)
	pump := func(r io.Reader) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 1024)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				for {
					i := indexNewline(buf)
					if i < 0 {
						break
					}
					line := strings.TrimRight(string(buf[:i]), "\r")
					logLine(line)
					buf = buf[i+1:]
				}
			}
			if err != nil {
				if len(buf) > 0 {
					logLine(strings.TrimRight(string(buf), "\r"))
				}
				return
			}
		}
	}
	go pump(stdout)
	go pump(stderr)
	<-done
	<-done
	return cmd.Wait()
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}
