// npm_client_test.go — unit coverage for the NPM admin client.
//
// All tests use httptest.NewServer to mock NPM responses; no live NPM is
// required and no docker network is involved. The mock server runs on
// 127.0.0.1, which the SSRF Control hook permits (loopback is private).

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMockNPM builds a tiny HTTP mux mimicking the subset of NPM endpoints
// the client exercises. handler is invoked for each request; tests
// register expectations against the returned mux.
func newMockNPM(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *NPMClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// Use the test server's own (loopback) URL; pass nil so the client
	// constructs its SSRF-safe default which accepts 127.0.0.1.
	c := NewNPMClient(srv.URL, nil)
	return srv, c
}

func TestLoginSuccess(t *testing.T) {
	_, c := newMockNPM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body loginRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Identity != "alice@example.com" || body.Secret != "hunter2hunter2" {
			t.Errorf("creds wrong: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(loginResponse{Token: "tok-abc"})
	})
	if err := c.Login(context.Background(), "alice@example.com", []byte("hunter2hunter2")); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if c.token != "tok-abc" {
		t.Errorf("token not stored, got %q", c.token)
	}
}

func TestLoginFailure(t *testing.T) {
	_, c := newMockNPM(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Invalid credentials"}}`, http.StatusUnauthorized)
	})
	err := c.Login(context.Background(), "alice@example.com", []byte("wrong"))
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "npm:") || !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention npm and status, got %v", err)
	}
}

func TestBootstrapAlreadyDone(t *testing.T) {
	// Mock: login with NEW creds succeeds → bootstrap should return nil
	// without ever touching default creds.
	called := 0
	_, c := newMockNPM(t, func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != "/api/tokens" {
			t.Errorf("bootstrap-already-done should only hit /api/tokens, got %s", r.URL.Path)
		}
		var body loginRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Identity != "alice@example.com" {
			http.Error(w, "wrong identity", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(loginResponse{Token: "tok-already"})
	})
	err := c.Bootstrap(context.Background(),
		"admin@example.com", []byte("changeme"),
		"alice@example.com", []byte("hunter2hunter2"))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if called != 1 {
		t.Errorf("expected 1 request (the new-creds login), got %d", called)
	}
}

func TestBootstrapFresh(t *testing.T) {
	// Mock the full bootstrap dance:
	//   POST /api/tokens with NEW creds → 401
	//   POST /api/tokens with default → 200
	//   GET  /api/users → returns the default admin
	//   PUT  /api/users/1 → 200 (update email)
	//   PUT  /api/users/1/auth → 200 (rotate password)
	//   POST /api/tokens with NEW creds → 200 (post-rotation re-login)
	step := 0
	_, c := newMockNPM(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tokens":
			var body loginRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Identity == "alice@example.com" && step == 0 {
				step++
				http.Error(w, "no such user", http.StatusUnauthorized)
				return
			}
			if body.Identity == "admin@example.com" && body.Secret == "changeme" {
				step++
				_ = json.NewEncoder(w).Encode(loginResponse{Token: "tok-default"})
				return
			}
			if body.Identity == "alice@example.com" && body.Secret == "hunter2hunter2" {
				step++
				_ = json.NewEncoder(w).Encode(loginResponse{Token: "tok-new"})
				return
			}
			t.Errorf("unexpected /api/tokens body at step %d: %+v", step, body)
			http.Error(w, "bad", http.StatusBadRequest)
		case r.Method == http.MethodGet && r.URL.Path == "/api/users":
			step++
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "email": "admin@example.com"},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/users/1":
			step++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["email"] != "alice@example.com" {
				t.Errorf("update body email wrong: %v", body["email"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPut && r.URL.Path == "/api/users/1/auth":
			step++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["secret"] != "hunter2hunter2" {
				t.Errorf("rotate body secret wrong: %v", body["secret"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad", http.StatusBadRequest)
		}
	})
	err := c.Bootstrap(context.Background(),
		"admin@example.com", []byte("changeme"),
		"alice@example.com", []byte("hunter2hunter2"))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if step < 5 {
		t.Errorf("expected at least 5 mock steps, hit %d", step)
	}
	if c.token != "tok-new" {
		t.Errorf("expected post-rotation token, got %q", c.token)
	}
}

func TestUpsertProxyHostCreate(t *testing.T) {
	_, c := newMockNPM(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/nginx/proxy-hosts":
			_ = json.NewEncoder(w).Encode([]proxyHostListEntry{
				{ID: 1, DomainNames: []string{"other.example.com"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/nginx/proxy-hosts":
			var got ProxyHost
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got.DomainNames[0] != "auth.example.com" {
				t.Errorf("wrong domain: %v", got.DomainNames)
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"id": 42})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c.token = "test-token"
	id, err := c.UpsertProxyHost(context.Background(), ProxyHost{
		DomainNames:   []string{"auth.example.com"},
		ForwardScheme: "http",
		ForwardHost:   "authelia",
		ForwardPort:   9091,
	})
	if err != nil {
		t.Fatalf("UpsertProxyHost: %v", err)
	}
	if id != 42 {
		t.Errorf("expected id 42, got %d", id)
	}
}

func TestUpsertProxyHostUpdate(t *testing.T) {
	_, c := newMockNPM(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/nginx/proxy-hosts":
			_ = json.NewEncoder(w).Encode([]proxyHostListEntry{
				{ID: 7, DomainNames: []string{"auth.example.com"}},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/api/nginx/proxy-hosts/7":
			_ = json.NewEncoder(w).Encode(map[string]int{"id": 7})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c.token = "test-token"
	id, err := c.UpsertProxyHost(context.Background(), ProxyHost{
		DomainNames:   []string{"auth.example.com"},
		ForwardScheme: "http",
		ForwardHost:   "authelia",
		ForwardPort:   9091,
	})
	if err != nil {
		t.Fatalf("UpsertProxyHost: %v", err)
	}
	if id != 7 {
		t.Errorf("expected id 7 (existing), got %d", id)
	}
}

func TestUpsertCertificateIdempotent(t *testing.T) {
	_, c := newMockNPM(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("idempotent path should be GET only, got %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode([]certificateListEntry{
			{ID: 3, NiceName: "wildcard-example.com", Provider: "other"},
		})
	})
	c.token = "test-token"
	id, err := c.UpsertCertificate(context.Background(), Certificate{
		NiceName: "wildcard-example.com",
	})
	if err != nil {
		t.Fatalf("UpsertCertificate: %v", err)
	}
	if id != 3 {
		t.Errorf("expected existing cert id 3, got %d", id)
	}
}

func TestTestModeShortCircuits(t *testing.T) {
	t.Setenv("TEST_MODE", "1")
	c := NewNPMClient("http://ingress:81", nil)
	// All four ops should succeed without ever dialing the network — the
	// hostname "ingress" wouldn't resolve in this test env if we tried.
	if err := c.Login(context.Background(), "alice@example.com", []byte("x")); err != nil {
		t.Errorf("Login (TEST_MODE): %v", err)
	}
	if err := c.Bootstrap(context.Background(),
		"admin@example.com", []byte("changeme"),
		"alice@example.com", []byte("hunter2hunter2")); err != nil {
		t.Errorf("Bootstrap (TEST_MODE): %v", err)
	}
	if _, err := c.UpsertProxyHost(context.Background(), ProxyHost{
		DomainNames: []string{"auth.example.com"},
	}); err != nil {
		t.Errorf("UpsertProxyHost (TEST_MODE): %v", err)
	}
	if _, err := c.UpsertCertificate(context.Background(), Certificate{NiceName: "wildcard-example.com"}); err != nil {
		t.Errorf("UpsertCertificate (TEST_MODE): %v", err)
	}
}

func TestZeroBytes(t *testing.T) {
	b := []byte("hunter2hunter2")
	zeroBytes(b)
	for i, c := range b {
		if c != 0 {
			t.Errorf("byte %d not zeroed: %d", i, c)
		}
	}
}
