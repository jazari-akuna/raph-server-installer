// auth_test.go — unit coverage for the X-Forward-Auth-Secret gate that
// prevents direct-from-bridge forgery of Remote-User / Remote-Groups.
//
// The threat: enrol binds 172.17.0.1:8080 (the docker bridge gateway IP),
// reachable from EVERY container on the bridge. Without the secret check
// in requireAuth, an attacker who lands code execution in any container
// on the host (e.g. a copyparty CVE) can curl enrol with forged Remote-*
// headers and become root on the host (enrol is privileged + holds the
// docker socket). The fix: NPM injects X-Forward-Auth-Secret on every
// protected proxy host; enrol's requireAuth refuses requests that don't
// carry it, BEFORE looking at Remote-User.
//
// These tests use only the stdlib + httptest. No live NPM, no Authelia,
// no docker.

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testAuthCfg returns a config with the bare minimum requireAuth needs.
// The secret is fixed so the tests are deterministic.
func testAuthCfg(secret string) config {
	return config{
		domain:            "example.com",
		headerUser:        "Remote-User",
		headerGroups:      "Remote-Groups",
		requiredGroup:     "admins",
		forwardAuthSecret: secret,
	}
}

// okHandler is the inner handler we pass to requireAuth — succeeds with
// 200 and a body the caller can scan for "OK". If this fires, the gate
// LET THE REQUEST THROUGH.
var okHandler http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// TestRequireAuthRejectsForgedHeadersWithoutSecret pins the regression:
// before this fix, requireAuth happily accepted any Remote-User /
// Remote-Groups pair; the bridge-IP attacker would win. With the fix,
// the same request 401s because it doesn't carry X-Forward-Auth-Secret.
func TestRequireAuthRejectsForgedHeadersWithoutSecret(t *testing.T) {
	cfg := testAuthCfg("real-secret")

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.Header.Set("Remote-User", "raph")
	req.Header.Set("Remote-Groups", "admins")
	// NO X-Forward-Auth-Secret header.

	rr := httptest.NewRecorder()
	requireAuth(cfg, true, okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (forged Remote-User without secret), got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "OK") {
		t.Errorf("inner handler fired despite missing secret — gate is broken")
	}
}

// TestRequireAuthRejectsWrongSecret confirms a mismatched secret is also
// 401'd — not just missing. crypto/subtle constant-time compare matters
// because length-leak via timing IS a known vector for this header style.
func TestRequireAuthRejectsWrongSecret(t *testing.T) {
	cfg := testAuthCfg("real-secret")

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.Header.Set("Remote-User", "raph")
	req.Header.Set("Remote-Groups", "admins")
	req.Header.Set("X-Forward-Auth-Secret", "guess-secret")

	rr := httptest.NewRecorder()
	requireAuth(cfg, true, okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (wrong secret), got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
}

// TestRequireAuthAcceptsMatchingSecret confirms a request that DOES carry
// the correct secret AND the right Remote-User / Remote-Groups passes
// through to the wrapped handler. This is the happy path — what NPM does
// on every legit forwarded request.
func TestRequireAuthAcceptsMatchingSecret(t *testing.T) {
	cfg := testAuthCfg("real-secret")

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.Header.Set("Remote-User", "raph")
	req.Header.Set("Remote-Groups", "admins")
	req.Header.Set("X-Forward-Auth-Secret", "real-secret")

	rr := httptest.NewRecorder()
	requireAuth(cfg, true, okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (authentic request), got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "OK") {
		t.Errorf("inner handler did not fire on authentic request")
	}
}

// TestRequireAuthFailsClosedOnEmptyServerSecret confirms the fail-closed
// behaviour: when the SERVER's configured secret is empty (env var unset
// at startup), every request is 401'd regardless of what header it
// carries. This is the safety net for the bootstrap-script-failed-but-
// container-came-up case.
func TestRequireAuthFailsClosedOnEmptyServerSecret(t *testing.T) {
	cfg := testAuthCfg("") // empty — fail-closed

	cases := []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"empty header", ""},
		{"any value", "anything"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/users", nil)
			req.Header.Set("Remote-User", "raph")
			req.Header.Set("Remote-Groups", "admins")
			if tc.header != "" {
				req.Header.Set("X-Forward-Auth-Secret", tc.header)
			}
			rr := httptest.NewRecorder()
			requireAuth(cfg, true, okHandler).ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 (server fail-closed), got %d (body: %s)",
					rr.Code, rr.Body.String())
			}
		})
	}
}

// TestRequireAuthRejectsAuthenticSecretButMissingRemoteUser confirms the
// secret check is necessary BUT NOT SUFFICIENT — the caller still needs
// to be authenticated by Authelia. NPM injects the secret on every
// proxied request; if Authelia hasn't authenticated the user yet, NPM's
// auth_request snippet 302s to /authelia BEFORE the request even reaches
// us. But just in case (e.g. operator misconfigures the auth_request
// snippet out of an advanced_config), we still 401 on missing Remote-User.
func TestRequireAuthRejectsAuthenticSecretButMissingRemoteUser(t *testing.T) {
	cfg := testAuthCfg("real-secret")

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.Header.Set("X-Forward-Auth-Secret", "real-secret")
	// NO Remote-User.

	rr := httptest.NewRecorder()
	requireAuth(cfg, true, okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (no Remote-User), got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
}

// TestRequireForwardAuthRejectsMissingHeader pins the regression for
// /login-intercept (the SSO POST→Authelia proxy with the LUKS-unlock side-
// effect). It is NOT wrapped in requireAuth because Authelia hasn't issued
// a session cookie yet, so we can't gate on Remote-User. But a request
// from the docker bridge with no X-Forward-Auth-Secret MUST still 401 —
// otherwise any container can drive the LUKS-unlock side-effect by POSTing
// forged credentials to 172.17.0.1:8080/login-intercept.
func TestRequireForwardAuthRejectsMissingHeader(t *testing.T) {
	cfg := testAuthCfg("real-secret")

	req := httptest.NewRequest(http.MethodPost, "/login-intercept", nil)
	// NO X-Forward-Auth-Secret. NO Remote-User either — this endpoint
	// doesn't expect Authelia identity headers.

	rr := httptest.NewRecorder()
	requireForwardAuth(cfg, okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (missing forward-auth header), got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "OK") {
		t.Errorf("inner handler fired despite missing secret — gate is broken")
	}
}

// TestRequireForwardAuthAcceptsMatchingSecret confirms the happy path:
// NPM injects the correct X-Forward-Auth-Secret on the /api/firstfactor
// rewrite, the inner handler runs.
func TestRequireForwardAuthAcceptsMatchingSecret(t *testing.T) {
	cfg := testAuthCfg("real-secret")

	req := httptest.NewRequest(http.MethodPost, "/login-intercept", nil)
	req.Header.Set("X-Forward-Auth-Secret", "real-secret")

	rr := httptest.NewRecorder()
	requireForwardAuth(cfg, okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "OK") {
		t.Errorf("inner handler did not fire on authentic request")
	}
}

// TestRequireForwardAuthFailsClosedOnEmptyServerSecret confirms the
// fail-closed behaviour for the bridge-only login endpoint too: an unset
// server-side secret 401s every request, including ones that look benign.
func TestRequireForwardAuthFailsClosedOnEmptyServerSecret(t *testing.T) {
	cfg := testAuthCfg("") // empty — fail-closed

	req := httptest.NewRequest(http.MethodPost, "/login-intercept", nil)
	req.Header.Set("X-Forward-Auth-Secret", "anything")

	rr := httptest.NewRecorder()
	requireForwardAuth(cfg, okHandler).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (server fail-closed), got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
}

// TestCheckForwardAuthSecretConstantTime exercises checkForwardAuthSecret
// directly to make sure each error path returns a non-nil error and the
// happy path returns nil. We don't try to measure timing here — that's a
// property of crypto/subtle, not of our wrapper.
func TestCheckForwardAuthSecret(t *testing.T) {
	cases := []struct {
		name      string
		serverCfg string
		header    string
		wantErr   bool
	}{
		{"empty server (fail-closed)", "", "anything", true},
		{"empty server, empty header", "", "", true},
		{"missing client header", "real", "", true},
		{"wrong client header", "real", "wrong", true},
		{"matching", "real", "real", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testAuthCfg(tc.serverCfg)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("X-Forward-Auth-Secret", tc.header)
			}
			err := checkForwardAuthSecret(req, cfg)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Errorf("got err=%v want err=%v", err, tc.wantErr)
			}
		})
	}
}
