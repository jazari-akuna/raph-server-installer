// npm_client.go — Parcel 4A: typed Go client for the Nginx Proxy Manager
// admin API.
//
// Replaces the Wave-3A shell-out to stacks/authelia/scripts/wire-npm-routes.sh
// for the wizard's finalize step 5. The shell version is retained as a
// debugging fallback (see that script's banner comment), but the wizard now
// drives NPM through this typed client so:
//
//   1. Bootstrap is idempotent — first run rotates NPM's hard-coded default
//      admin (admin@example.com / changeme) to operator-supplied credentials;
//      subsequent runs use the new credentials directly.
//   2. The plaintext password's lifetime is bounded — Bootstrap takes a
//      []byte and zeroes it as soon as the API call returns.
//   3. Errors surface as typed Go errors that the SSE finalize stream can
//      render with a clickable "back to <step>" link instead of grepping
//      for curl exit codes.
//   4. SSRF-safe — every request to ingress:81 routes through a
//      net.Dialer.Control filter (copied verbatim from launcher.go) so an
//      attacker can't trick the wizard into talking to an internal-net
//      address it shouldn't.
//
// Design notes:
//
//   - Stdlib only. No third-party HTTP / JSON libs.
//   - Uses TEST_MODE=1 to short-circuit every API call. The harness asserts
//     on the "TEST_MODE: skipping <thing>" log lines.
//   - The four proxy hosts the wizard creates are encoded as a typed
//     ProxyHost slice in setup.go; this client only knows how to upsert one
//     ProxyHost at a time, plus the bootstrap login flow.
//   - The wildcard cert is uploaded once (Certificate.Upsert), referenced
//     from each ProxyHost via CertificateID.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// types

// ProxyHost mirrors the JSON shape NPM's /api/nginx/proxy-hosts endpoint
// expects on POST/PUT. Field names align with the API; only the fields the
// wizard actually sets are exposed (the API has a much larger surface — we
// pin a known-good subset to avoid drift across NPM versions).
type ProxyHost struct {
	DomainNames           []string `json:"domain_names"`
	ForwardScheme         string   `json:"forward_scheme"` // "http" or "https"
	ForwardHost           string   `json:"forward_host"`
	ForwardPort           int      `json:"forward_port"`
	BlockExploits         bool     `json:"block_exploits"`
	CachingEnabled        bool     `json:"caching_enabled"`
	AllowWebsocketUpgrade bool     `json:"allow_websocket_upgrade"`
	AccessListID          int      `json:"access_list_id"`
	CertificateID         int      `json:"certificate_id"`
	SSLForced             bool     `json:"ssl_forced"`
	HTTP2Support          bool     `json:"http2_support"`
	HSTSEnabled           bool     `json:"hsts_enabled"`
	HSTSSubdomains        bool     `json:"hsts_subdomains"`
	AdvancedConfig        string   `json:"advanced_config"`
	Meta                  map[string]any `json:"meta"`
	Locations             []any    `json:"locations"`
}

// Certificate is the body we POST to /api/nginx/certificates for a custom
// (already-issued, externally) wildcard cert. NPM also supports issuing via
// its own LE flow but our wizard issues with certbot first then uploads —
// the wizard owns the DNS-01 challenge so NPM doesn't need DNS creds.
type Certificate struct {
	Provider string         `json:"provider"`         // "other" for uploaded
	NiceName string         `json:"nice_name"`        // human label
	Meta     map[string]any `json:"meta,omitempty"`
}

// ---------------------------------------------------------------------------
// client

type NPMClient struct {
	baseURL string
	http    *http.Client

	// token, when non-empty, is sent as Authorization: Bearer on every
	// non-login request. Set by Login / Bootstrap.
	token string
}

// NewNPMClient returns a client targeting baseURL (e.g. "http://ingress:81"
// inside the docker network, or http://127.0.0.1:81 from the host). If
// httpClient is nil, an SSRF-hardened default is constructed that refuses
// to dial public IPs — the NPM admin API is only ever reachable via the
// internal docker network or loopback.
//
// We reuse the same private-block list as launcher.go but invert the
// semantics: launcher.go BLOCKS private addresses (it fetches user-supplied
// URLs), npm_client.go ALLOWS ONLY private addresses (it talks to internal
// services). Both share the post-resolution filter pattern.
func NewNPMClient(baseURL string, httpClient *http.Client) *NPMClient {
	if httpClient == nil {
		httpClient = newNPMHTTPClient()
	}
	return &NPMClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
	}
}

// newNPMHTTPClient builds the SSRF-safe default client. The Control hook
// runs after DNS resolution and rejects any address that's NOT a private /
// loopback IP — we explicitly want the inverse of launcher.go because the
// NPM admin API lives on the docker bridge and must never be talked to
// over the public internet.
func newNPMHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 5 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("npm dialer: unresolvable address %q", address)
			}
			if !isPrivateIP(ip) {
				return fmt.Errorf("npm dialer: refusing to dial public IP %s "+
					"(NPM admin API must be internal-only)", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     true,
		},
	}
}

// validateBaseURL enforces that the configured base URL parses, uses
// http(s), and (after host resolution) only resolves to private addresses.
// Called from Bootstrap / Login on first use; cheaper than re-resolving on
// every request.
func (c *NPMClient) validateBaseURL(ctx context.Context) error {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("npm: parse base URL %q: %w", c.baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("npm: base URL must be http(s), got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("npm: base URL has empty host")
	}
	// Numeric IP — check directly.
	if ip := net.ParseIP(host); ip != nil {
		if !isPrivateIP(ip) {
			return fmt.Errorf("npm: base URL %s resolves to public IP %s "+
				"(refusing — admin API must be internal-only)", c.baseURL, ip)
		}
		return nil
	}
	// Hostname — resolve and require every result to be private.
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		// Inside the docker network "ingress" resolves via docker's
		// embedded DNS; failure here usually means the wizard is running
		// outside the docker network, which is a config bug. Pass it
		// through so the operator sees the error.
		return fmt.Errorf("npm: resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if !isPrivateIP(ip) {
			return fmt.Errorf("npm: %s resolves to public IP %s "+
				"(refusing — admin API must be internal-only)", host, ip)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// generic request helper

// do performs a JSON request against the NPM API. body, if non-nil, is
// marshalled to JSON. The response body, if non-nil and successful, is
// decoded into out. Errors are wrapped with the npm: prefix so the SSE
// stream renders a clear step label.
func (c *NPMClient) do(ctx context.Context, method, path string, body any, out any) error {
	if testModeEnabled() {
		// The harness asserts on this exact format.
		fmt.Fprintf(os.Stderr, "TEST_MODE: skipping NPM %s %s\n", method, path)
		return nil
	}
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("npm: marshal %s body: %w", path, err)
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, buf)
	if err != nil {
		return fmt.Errorf("npm: build request %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("npm: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Bound the body we surface — NPM's error envelopes are small but
		// a misbehaving proxy could return megabytes.
		bodyTxt := readBoundedString(resp.Body, 4*1024)
		return fmt.Errorf("npm: %s %s: status %d: %s",
			method, path, resp.StatusCode, bodyTxt)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("npm: decode %s response: %w", path, err)
		}
	} else {
		// Drain so the connection can be reused (DisableKeepAlives is on,
		// but it's still good hygiene).
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return nil
}

func readBoundedString(r io.Reader, max int64) string {
	b, _ := io.ReadAll(io.LimitReader(r, max))
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(empty body)"
	}
	return s
}

// testModeEnabled mirrors the bash harness contract — TEST_MODE=1 means
// skip everything that mutates external state.
func testModeEnabled() bool {
	return os.Getenv("TEST_MODE") == "1"
}

// ---------------------------------------------------------------------------
// Login / Bootstrap

type loginRequest struct {
	Identity string `json:"identity"`
	Secret   string `json:"secret"`
}

type loginResponse struct {
	Token string `json:"token"`
	// NPM 2.14 changed `expires` from a Unix epoch int64 to an ISO-8601
	// datetime string (e.g. "2026-04-28T11:29:11.068Z"). Decode as a raw
	// json.RawMessage so the schema change doesn't blow up bootstrap; we
	// don't actually use the field — the JWT's own exp claim is what
	// matters for refresh decisions.
	Expires json.RawMessage `json:"expires"`
}

// Login obtains an admin bearer token by POSTing to /api/tokens. The
// password is taken as []byte so callers can zero it after the call (Go's
// string immutability prevents zeroing a string-typed password reliably).
//
// On success, the token is stored on the client and used by every
// subsequent request automatically. The password slice IS NOT zeroed by
// this method — that's the caller's responsibility (typically via defer
// in the Bootstrap path).
func (c *NPMClient) Login(ctx context.Context, email string, password []byte) error {
	if testModeEnabled() {
		fmt.Fprintf(os.Stderr, "TEST_MODE: skipping NPM Login\n")
		c.token = "test-mode-token"
		return nil
	}
	if err := c.validateBaseURL(ctx); err != nil {
		return err
	}
	var resp loginResponse
	body := loginRequest{Identity: email, Secret: string(password)}
	if err := c.do(ctx, http.MethodPost, "/api/tokens", body, &resp); err != nil {
		return err
	}
	if resp.Token == "" {
		return errors.New("npm: login returned empty token")
	}
	c.token = resp.Token
	return nil
}

// Bootstrap brings NPM from its hard-coded default admin
// (admin@example.com / changeme) to operator-supplied credentials.
//
// Idempotent: if login with the defaults FAILS and login with the new
// credentials SUCCEEDS, we assume bootstrap already ran and return nil.
// Either way the client ends up authenticated as the new admin.
//
// The newPassword []byte is zeroed before this method returns — callers
// should not reuse it. currentPassword is treated the same way.
func (c *NPMClient) Bootstrap(
	ctx context.Context,
	currentEmail string,
	currentPassword []byte,
	newEmail string,
	newPassword []byte,
) (err error) {
	defer func() {
		// Defensive zero: regardless of error path, neither password slice
		// should outlive the call. The caller still owns the slice headers
		// but the bytes are gone.
		zeroBytes(currentPassword)
		zeroBytes(newPassword)
	}()

	if testModeEnabled() {
		fmt.Fprintf(os.Stderr, "TEST_MODE: skipping NPM Bootstrap\n")
		c.token = "test-mode-token"
		return nil
	}

	// Try login with the operator's NEW credentials first. If that works,
	// we're already bootstrapped — no rotation needed.
	if loginErr := c.Login(ctx, newEmail, newPassword); loginErr == nil {
		return nil
	}

	// Fall back to the default admin. NPM 2.x ships with a hard-coded
	// admin@example.com / changeme that's flagged as "expired" — the
	// first action expected after login is to update the user.
	if err := c.Login(ctx, currentEmail, currentPassword); err != nil {
		return fmt.Errorf("npm bootstrap: cannot log in with defaults or "+
			"new creds (NPM may not be ready yet): %w", err)
	}

	// Find the default admin's user id. /api/users returns the list; we
	// look up the one matching currentEmail.
	var users []struct {
		ID    int    `json:"id"`
		Email string `json:"email"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/users", nil, &users); err != nil {
		return fmt.Errorf("npm bootstrap: list users: %w", err)
	}
	var adminID int
	for _, u := range users {
		if strings.EqualFold(u.Email, currentEmail) {
			adminID = u.ID
			break
		}
	}
	if adminID == 0 {
		return fmt.Errorf("npm bootstrap: default admin %q not found in user list", currentEmail)
	}

	// Step 1: update the user's email + name. NPM's PUT /api/users/:id
	// accepts a partial body; we send the new email + a generic name.
	updateBody := map[string]any{
		"name":     "Administrator",
		"nickname": "Admin",
		"email":    newEmail,
		"roles":    []string{"admin"},
		"is_disabled": false,
	}
	if err := c.do(ctx, http.MethodPut, fmt.Sprintf("/api/users/%d", adminID), updateBody, nil); err != nil {
		return fmt.Errorf("npm bootstrap: update user: %w", err)
	}

	// Step 2: rotate the password via /api/users/:id/auth.
	pwBody := map[string]any{
		"type":    "password",
		"current": string(currentPassword),
		"secret":  string(newPassword),
	}
	if err := c.do(ctx, http.MethodPut, fmt.Sprintf("/api/users/%d/auth", adminID), pwBody, nil); err != nil {
		return fmt.Errorf("npm bootstrap: rotate password: %w", err)
	}

	// Step 3: re-login under the new credentials so subsequent calls go
	// through with a fresh token. We can't reuse the old token because
	// NPM may invalidate it after the password change.
	c.token = ""
	// Re-login; we must re-marshal the new password without using the
	// already-zeroed slice. Copy out before zero-ing happens in defer.
	// NOTE: the defer above zeroes AFTER we return; the slice is still
	// readable here.
	if err := c.Login(ctx, newEmail, newPassword); err != nil {
		return fmt.Errorf("npm bootstrap: re-login post-rotation: %w", err)
	}
	return nil
}

// zeroBytes wipes a byte slice in-place. Used to scrub plaintext passwords
// from memory immediately after the API call that needed them.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ---------------------------------------------------------------------------
// Proxy hosts

type proxyHostListEntry struct {
	ID          int      `json:"id"`
	DomainNames []string `json:"domain_names"`
}

// ListProxyHosts returns every (non-deleted) proxy host visible to the
// authenticated admin. Used by the finalize verification to assert the
// four wizard-managed hosts actually exist post-wireup, rather than
// trusting that UpsertProxyHost's nil-error meant the row landed.
//
// Failure modes the verification specifically defends against:
//   - Bearer token silently expired between hosts (shouldn't happen with
//     a 24h NPM token, but a clock skew or container restart between
//     UpsertCertificate and the proxy-hosts loop would surface as 401s
//     that an earlier version of the loop misclassified as success).
//   - JSON-decode tolerated a wrong-typed `enabled` field and skipped
//     rows. ListProxyHosts uses the same proxyHostListEntry shape as the
//     internal upsert listing — kept narrow on purpose.
func (c *NPMClient) ListProxyHosts(ctx context.Context) ([]proxyHostListEntry, error) {
	if testModeEnabled() {
		return nil, nil
	}
	if c.token == "" {
		return nil, errors.New("npm: not logged in")
	}
	var out []proxyHostListEntry
	if err := c.do(ctx, http.MethodGet, "/api/nginx/proxy-hosts", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpsertProxyHost creates the proxy host if no host with the same primary
// domain exists, else updates the existing record. Returns the host's ID.
//
// Domain matching uses ProxyHost.DomainNames[0] — the wizard creates one
// domain per host so this is unambiguous.
func (c *NPMClient) UpsertProxyHost(ctx context.Context, host ProxyHost) (int, error) {
	if testModeEnabled() {
		fmt.Fprintf(os.Stderr, "TEST_MODE: skipping NPM UpsertProxyHost %v\n", host.DomainNames)
		return 0, nil
	}
	if c.token == "" {
		return 0, errors.New("npm: not logged in (call Login or Bootstrap first)")
	}
	if len(host.DomainNames) == 0 {
		return 0, errors.New("npm: proxy host has no domain_names")
	}
	primary := host.DomainNames[0]

	// List existing hosts to find a matching domain.
	var existing []proxyHostListEntry
	if err := c.do(ctx, http.MethodGet, "/api/nginx/proxy-hosts", nil, &existing); err != nil {
		return 0, err
	}
	var existingID int
	for _, e := range existing {
		for _, d := range e.DomainNames {
			if d == primary {
				existingID = e.ID
				break
			}
		}
		if existingID != 0 {
			break
		}
	}
	// Defaults expected by NPM's API — these aren't in the typed struct
	// because they're invariant for the wizard's four hosts.
	if host.Meta == nil {
		host.Meta = map[string]any{
			"letsencrypt_agree": false,
			"dns_challenge":     false,
		}
	}
	if host.Locations == nil {
		host.Locations = []any{}
	}

	if existingID > 0 {
		var resp struct {
			ID int `json:"id"`
		}
		err := c.do(ctx, http.MethodPut,
			fmt.Sprintf("/api/nginx/proxy-hosts/%d", existingID),
			host, &resp)
		if err != nil {
			return 0, err
		}
		return existingID, nil
	}
	var resp struct {
		ID int `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/nginx/proxy-hosts", host, &resp); err != nil {
		return 0, err
	}
	return resp.ID, nil
}

// ---------------------------------------------------------------------------
// Certificates

type certificateListEntry struct {
	ID       int      `json:"id"`
	NiceName string   `json:"nice_name"`
	Provider string   `json:"provider"`
	DomainNames []string `json:"domain_names,omitempty"`
}

// UpsertCertificate registers (or updates) an "other"-provider certificate
// in NPM and uploads the PEM bytes. NPM does NOT auto-discover PEM files
// from /etc/letsencrypt for "other"-provider certs; the bytes must be
// POSTed to /api/nginx/certificates/<id>/upload as a multipart form. Until
// that POST lands, the cert row exists in NPM's DB but the renderer
// silently refuses to emit nginx server blocks for any proxy_host that
// references it (resulting in a 404 on every protected host). See
// scripts/bootstrap-npm-setup-route.sh for the canonical curl invocation.
//
// Match key: NiceName. The wizard uses a stable name like "wildcard-<domain>"
// so re-runs upsert cleanly. We always (re-)upload the PEM bytes when paths
// are supplied — NPM accepts overwrites and a redundant write costs less
// than a half-rendered cert sitting around forever.
//
// fullchainPath / privkeyPath may be empty (e.g. in TEST_MODE or when the
// caller wants to register-only); in that case we skip the upload step.
func (c *NPMClient) UpsertCertificate(ctx context.Context, cert Certificate, fullchainPath, privkeyPath string) (int, error) {
	if testModeEnabled() {
		fmt.Fprintf(os.Stderr, "TEST_MODE: skipping NPM UpsertCertificate %s\n", cert.NiceName)
		return 0, nil
	}
	if c.token == "" {
		return 0, errors.New("npm: not logged in")
	}
	if cert.NiceName == "" {
		return 0, errors.New("npm: certificate nice_name required")
	}
	if cert.Provider == "" {
		cert.Provider = "other"
	}
	var existing []certificateListEntry
	if err := c.do(ctx, http.MethodGet, "/api/nginx/certificates", nil, &existing); err != nil {
		return 0, err
	}
	var id int
	for _, e := range existing {
		if e.NiceName == cert.NiceName {
			id = e.ID
			break
		}
	}
	if id == 0 {
		var resp struct {
			ID int `json:"id"`
		}
		if err := c.do(ctx, http.MethodPost, "/api/nginx/certificates", cert, &resp); err != nil {
			return 0, err
		}
		id = resp.ID
	}
	if fullchainPath != "" && privkeyPath != "" {
		if err := c.UploadCertificate(ctx, id, fullchainPath, privkeyPath); err != nil {
			return id, fmt.Errorf("upload pem: %w", err)
		}
	}
	return id, nil
}

// UploadCertificate POSTs the fullchain + privkey PEMs to
// /api/nginx/certificates/<id>/upload as a multipart form. NPM uses these
// to populate /data/custom_ssl/npm-<id>/{fullchain.pem,privkey.pem}, which
// the proxy_host renderer reads when emitting `ssl_certificate` lines.
// Without this call the cert row exists in the DB but no nginx conf gets
// rendered for any proxy_host using it. NPM accepts re-uploads idempotently.
func (c *NPMClient) UploadCertificate(ctx context.Context, certID int, fullchainPath, privkeyPath string) error {
	if testModeEnabled() {
		fmt.Fprintf(os.Stderr, "TEST_MODE: skipping NPM UploadCertificate id=%d\n", certID)
		return nil
	}
	if c.token == "" {
		return errors.New("npm: not logged in")
	}
	if certID == 0 {
		return errors.New("npm: cert id required")
	}
	cert, err := os.ReadFile(fullchainPath)
	if err != nil {
		return fmt.Errorf("read fullchain %s: %w", fullchainPath, err)
	}
	key, err := os.ReadFile(privkeyPath)
	if err != nil {
		return fmt.Errorf("read privkey %s: %w", privkeyPath, err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if part, err := mw.CreateFormFile("certificate", "fullchain.pem"); err != nil {
		return err
	} else if _, err := part.Write(cert); err != nil {
		return err
	}
	if part, err := mw.CreateFormFile("certificate_key", "privkey.pem"); err != nil {
		return err
	} else if _, err := part.Write(key); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/nginx/certificates/%d/upload", c.baseURL, certID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return fmt.Errorf("npm: build upload request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("npm: POST upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("npm: upload status %d: %s",
			resp.StatusCode, readBoundedString(resp.Body, 4*1024))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
