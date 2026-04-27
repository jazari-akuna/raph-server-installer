// setup_test.go — unit coverage for the setup-wizard helpers.
//
// Currently only covers dnsCredsINI: deterministic key order across runs
// (Go map iteration is randomised, so an unsorted writer would emit
// non-reproducible files and bust certbot's idempotency assumptions),
// and rejection of credential values containing CR/LF (an embedded
// newline is almost certainly a paste error or injection attempt — DNS
// provider tokens are opaque single-line strings).
//
// Tests use only the stdlib. No live certbot, no shell-out.

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDNSCredsINIDeterministicOrder runs dnsCredsINI repeatedly on the
// same input and asserts every render produces byte-identical output.
// With a sufficient number of keys the chance of a randomised map walk
// happening to match the sorted order on every iteration is negligible.
func TestDNSCredsINIDeterministicOrder(t *testing.T) {
	creds := map[string]string{
		"dns_cloudflare_api_token": "tok-1",
		"dns_cloudflare_email":     "ops@example.com",
		"dns_cloudflare_zone":      "example.com",
		"dns_cloudflare_extra_a":   "a",
		"dns_cloudflare_extra_b":   "b",
		"dns_cloudflare_extra_c":   "c",
		"dns_cloudflare_extra_d":   "d",
	}
	first, _, _, err := dnsCredsINI("cloudflare", creds)
	if err != nil {
		t.Fatalf("dnsCredsINI returned error on valid input: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, _, _, err := dnsCredsINI("cloudflare", creds)
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if got != first {
			t.Fatalf("iteration %d: output drifted; map iteration is "+
				"leaking through.\nfirst:\n%s\ngot:\n%s", i, first, got)
		}
	}

	// Also assert keys are in lexicographic order — if a future refactor
	// switches to a different deterministic scheme (e.g. insertion order)
	// it should be a deliberate change, not silent.
	lines := strings.Split(strings.TrimRight(first, "\n"), "\n")
	for i := 1; i < len(lines); i++ {
		prevKey := strings.SplitN(lines[i-1], " = ", 2)[0]
		curKey := strings.SplitN(lines[i], " = ", 2)[0]
		if prevKey >= curKey {
			t.Fatalf("keys not in ascending order: %q before %q", prevKey, curKey)
		}
	}
}

// TestDNSCredsINIRejectsNewline asserts that a CR or LF anywhere in a
// credential value produces an error rather than a multi-line INI body.
// Either character could let an attacker (or a careless paste) inject a
// fake INI directive — refusing the render is the right call here.
func TestDNSCredsINIRejectsNewline(t *testing.T) {
	cases := []struct {
		name  string
		creds map[string]string
	}{
		{"LF in value", map[string]string{"dns_cloudflare_api_token": "tok\nfake_key=evil"}},
		{"CR in value", map[string]string{"dns_cloudflare_api_token": "tok\rfake_key=evil"}},
		{"CRLF in value", map[string]string{"dns_cloudflare_api_token": "tok\r\nfake_key=evil"}},
		{"trailing LF", map[string]string{"dns_cloudflare_api_token": "tok\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := dnsCredsINI("cloudflare", tc.creds)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "newline") {
				t.Fatalf("error message %q should mention 'newline'", err.Error())
			}
		})
	}
}

// TestDNSCredsINIGoogleSidecar asserts the google branch still returns
// the sidecar JSON path + body and a tiny INI pointing at it. Lightly
// pinned because the google branch is the only one that doesn't go
// through the sort-and-render loop.
func TestDNSCredsINIGoogleSidecar(t *testing.T) {
	creds := map[string]string{"dns_google_credentials": `{"type":"service_account"}`}
	ini, sidecarPath, sidecarBody, err := dnsCredsINI("google", creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sidecarPath == "" {
		t.Fatalf("expected non-empty sidecar path for google")
	}
	if sidecarBody != creds["dns_google_credentials"] {
		t.Fatalf("sidecar body mismatch: got %q want %q", sidecarBody, creds["dns_google_credentials"])
	}
	if !strings.Contains(ini, "dns_google_credentials = "+sidecarPath) {
		t.Fatalf("ini body should reference sidecar path; got %q", ini)
	}
}

// TestRequireSetupTokenRejectsMissingHeader pins the new gate on the
// wizard endpoints. Pre-finalize there's no Authelia, so cookie/token
// gates would buy nothing — but enrol binds the docker bridge gateway IP
// (172.17.0.1:8080) so any container on the host can POST to /setup/*
// directly. Without the X-Forward-Auth-Secret check, a bridge-only
// attacker can drive the wizard end-to-end. With the check, the same
// request 401s.
func TestRequireSetupTokenRejectsMissingHeader(t *testing.T) {
	s := &server{cfg: config{forwardAuthSecret: "real-secret"}}

	req := httptest.NewRequest(http.MethodGet, "/setup/welcome", nil)
	// NO X-Forward-Auth-Secret header — simulates a docker-bridge attacker
	// curling enrol directly on 172.17.0.1:8080 without going through NPM.

	rr := httptest.NewRecorder()
	if s.requireSetupToken(rr, req) {
		t.Fatalf("requireSetupToken returned true on missing header — gate is broken")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "forward-auth header") {
		t.Errorf("error body should mention forward-auth header, got %q", rr.Body.String())
	}
}

// TestRequireSetupTokenAcceptsMatchingSecret confirms a request that
// arrives through the NPM setup proxy host (advanced_config injects the
// secret) passes the gate.
func TestRequireSetupTokenAcceptsMatchingSecret(t *testing.T) {
	s := &server{cfg: config{forwardAuthSecret: "real-secret"}}

	req := httptest.NewRequest(http.MethodGet, "/setup/welcome", nil)
	req.Header.Set("X-Forward-Auth-Secret", "real-secret")

	rr := httptest.NewRecorder()
	if !s.requireSetupToken(rr, req) {
		t.Fatalf("requireSetupToken returned false on authentic request (body: %s)",
			rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		// http.ResponseWriter defaults to 200 when nothing is written.
		t.Fatalf("expected default 200, got %d", rr.Code)
	}
}

// TestRequireSetupTokenFailsClosedOnEmptyServerSecret confirms the wizard
// inherits the same fail-closed behaviour as requireAuth: an unset
// server-side secret 401s every request, even one that carries a header.
// This is the safety net for the bootstrap-script-failed-but-container-
// came-up case during install.
func TestRequireSetupTokenFailsClosedOnEmptyServerSecret(t *testing.T) {
	s := &server{cfg: config{forwardAuthSecret: ""}} // empty — fail-closed

	req := httptest.NewRequest(http.MethodGet, "/setup/dns", nil)
	req.Header.Set("X-Forward-Auth-Secret", "anything")

	rr := httptest.NewRecorder()
	if s.requireSetupToken(rr, req) {
		t.Fatalf("requireSetupToken returned true with empty server secret — gate is broken")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (fail-closed), got %d (body: %s)",
			rr.Code, rr.Body.String())
	}
}
