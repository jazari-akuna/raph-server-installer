// plane_client_test.go — table-driven tests for the Plane REST client.
//
// All HTTP-level tests use httptest.NewServer; no live Plane is touched.
// The mock server runs on 127.0.0.1, which the SSRF Control hook on the
// PlaneClient.http transport permits (loopback is "private").

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMockPlane builds an httptest server with the supplied handler,
// returns it plus a PlaneClient pointed at the mock with a non-empty
// token so the .ready() guard doesn't short-circuit.
func newMockPlane(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *PlaneClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewPlaneClient(srv.URL, "test-token")
	return srv, c
}

// ----- constructor -----

func TestNewPlaneClient(t *testing.T) {
	cases := []struct {
		name        string
		baseURL     string
		token       string
		wantBaseURL string
		wantToken   string
		wantReady   bool
	}{
		{
			name:        "trailing slash trimmed",
			baseURL:     "http://plane-proxy/api/v1/",
			token:       "abc",
			wantBaseURL: "http://plane-proxy/api/v1",
			wantToken:   "abc",
			wantReady:   true,
		},
		{
			name:        "no trailing slash preserved",
			baseURL:     "http://plane-proxy/api/v1",
			token:       "abc",
			wantBaseURL: "http://plane-proxy/api/v1",
			wantToken:   "abc",
			wantReady:   true,
		},
		{
			name:        "empty token → not ready",
			baseURL:     "http://plane-proxy/api/v1",
			token:       "",
			wantBaseURL: "http://plane-proxy/api/v1",
			wantToken:   "",
			wantReady:   false,
		},
		{
			name:        "whitespace token trimmed",
			baseURL:     "http://plane-proxy/api/v1",
			token:       "  spaced  \n",
			wantBaseURL: "http://plane-proxy/api/v1",
			wantToken:   "spaced",
			wantReady:   true,
		},
		{
			name:        "empty baseURL → not ready",
			baseURL:     "",
			token:       "abc",
			wantBaseURL: "",
			wantToken:   "abc",
			wantReady:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewPlaneClient(tc.baseURL, tc.token)
			if c.baseURL != tc.wantBaseURL {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tc.wantBaseURL)
			}
			if c.token != tc.wantToken {
				t.Errorf("token = %q, want %q", c.token, tc.wantToken)
			}
			if got := c.ready(); got != tc.wantReady {
				t.Errorf("ready() = %v, want %v", got, tc.wantReady)
			}
		})
	}
}

// ready() must also tolerate the nil receiver — the admin page may
// invoke methods on a nil client when config wiring is partial.
func TestPlaneClientNilReady(t *testing.T) {
	var c *PlaneClient
	if c.ready() {
		t.Errorf("nil client should not be ready")
	}
}

// ----- email encoding -----

// UserByEmail must URL-encode the email so `+` survives transit (gmail
// addressing). The mock asserts the raw query string contains the
// percent-encoded form.
func TestUserByEmailEncoding(t *testing.T) {
	cases := []struct {
		name      string
		email     string
		wantQuery string
	}{
		{
			name:      "plain email",
			email:     "alice@example.com",
			wantQuery: "email=alice%40example.com",
		},
		{
			name:      "plus addressing (gmail)",
			email:     "alice+plane@example.com",
			wantQuery: "email=alice%2Bplane%40example.com",
		},
		{
			name:      "space in local-part (rare but legal)",
			email:     "a b@example.com",
			wantQuery: "email=a+b%40example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := ""
			_, c := newMockPlane(t, func(w http.ResponseWriter, r *http.Request) {
				seen = r.URL.RawQuery
				_ = json.NewEncoder(w).Encode(map[string]any{
					"results": []map[string]any{
						{"id": "u1", "email": tc.email},
					},
				})
			})
			user, err := c.UserByEmail(context.Background(), tc.email)
			if err != nil {
				t.Fatalf("UserByEmail: %v", err)
			}
			if user == nil || user.ID != "u1" {
				t.Fatalf("expected user id=u1, got %+v", user)
			}
			if !strings.Contains(seen, tc.wantQuery) {
				t.Errorf("query %q does not contain %q", seen, tc.wantQuery)
			}
		})
	}
}

// UserByEmail returns nil/nil when the API returns 404 (graceful
// fallback so the admin page silently shows zeros for users who
// haven't logged into Plane yet).
func TestUserByEmailNotFound(t *testing.T) {
	_, c := newMockPlane(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"not found"}`, http.StatusNotFound)
	})
	user, err := c.UserByEmail(context.Background(), "ghost@example.com")
	if err != nil {
		t.Fatalf("expected nil error on 404 (graceful fallback), got %v", err)
	}
	if user != nil {
		t.Errorf("expected nil user on 404, got %+v", user)
	}
}

// UserByEmail with an unconfigured client (empty token) returns nil/nil
// without ever issuing an HTTP request. Critical for Wave B to land
// before Plane is deployed.
func TestUserByEmailUnconfigured(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	c := NewPlaneClient(srv.URL, "") // empty token → not ready
	user, err := c.UserByEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("expected nil error on unconfigured client, got %v", err)
	}
	if user != nil {
		t.Errorf("expected nil user on unconfigured client, got %+v", user)
	}
	if called {
		t.Errorf("unconfigured client should NOT issue an HTTP request")
	}
}

// ----- JSON parsing -----

// ListWorkspaces must decode Plane's pagination envelope ({"results":
// [...]}) into the typed Workspace slice.
func TestListWorkspacesParsesEnvelope(t *testing.T) {
	_, c := newMockPlane(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspaces/" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		// Bearer header is mandatory for the live API; assert we send it.
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("missing Bearer header, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": "w1", "name": "Engineering", "slug": "eng"},
				{"id": "w2", "name": "Design", "slug": "design"},
			},
		})
	})
	ws, err := c.ListWorkspaces(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(ws) != 2 {
		t.Fatalf("got %d workspaces, want 2", len(ws))
	}
	if ws[0].Slug != "eng" || ws[1].Slug != "design" {
		t.Errorf("workspaces[].Slug = [%q,%q], want [eng,design]", ws[0].Slug, ws[1].Slug)
	}
}

// IssueCount + FileAssetBytes both return zero/nil on a 5xx so the
// admin page degrades cleanly when Plane is up-but-broken.
func TestIssueCountGraceful5xx(t *testing.T) {
	_, c := newMockPlane(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	})
	n, err := c.IssueCount(context.Background(), "eng", "u1")
	if err != nil {
		t.Errorf("expected nil err on 500, got %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 issues on 500, got %d", n)
	}
}

func TestFileAssetBytesSums(t *testing.T) {
	_, c := newMockPlane(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"size": 1024},
				{"size": 2048},
				{"size": 4096},
			},
		})
	})
	n, err := c.FileAssetBytes(context.Background(), "eng", "u1")
	if err != nil {
		t.Fatalf("FileAssetBytes: %v", err)
	}
	if n != 1024+2048+4096 {
		t.Errorf("got sum %d, want %d", n, 1024+2048+4096)
	}
}
