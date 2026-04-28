// plane_client.go — typed Go client for the Plane REST API.
//
// Mirrors the shape of npm_client.go: stdlib-only, SSRF-safe dialer,
// constructor takes baseURL + bearer token. Used by the admin /users
// page to surface per-user issue counts + attachment bytes alongside
// the Nextcloud quota numbers.
//
// IMPORTANT: this client is **stub-with-graceful-fallback** at the
// moment Wave B lands. Plane itself isn't deployed until Wave C, and
// the operator-issued personal-access token at
// /etc/raph-installer/plane-admin-token is created by the operator
// post-godmode-claim. Until both exist:
//
//   - NewPlaneClient returns a non-nil client with token=="" set.
//   - Every API method short-circuits to (zero-value, nil) when the
//     bearer token is empty OR the upstream returns 4xx/5xx, so the
//     admin page renders zeros (rendered as "—" by the template) instead
//     of a 500.
//
// Once Plane is up and the token is on disk, the same code paths
// transparently start surfacing real numbers — no code change needed.
//
// TODO across the methods below: verify endpoint paths + response
// shapes against the live Plane API at https://developers.plane.so/api-reference
// once Wave C deploys the stack. Plane's open-source self-hosted API has
// historically diverged from their cloud docs in minor ways (path
// prefixes, pagination shape) so the first real-deploy QC will involve
// printing raw response bodies and adjusting the typed structs below.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// types

// PlaneUser is the subset of Plane's user record that the admin page
// needs. Only ID + Email are load-bearing; the rest is preserved for
// future panels (Display name, last-activity timestamp, etc.).
type PlaneUser struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name,omitempty"`
	LastActive  time.Time `json:"last_active,omitempty"`
}

// Workspace is the subset of Plane's workspace record we render. Slug
// is the load-bearing field — every other Plane API call scopes by
// workspace slug.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// ---------------------------------------------------------------------------
// client

type PlaneClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewPlaneClient returns a client targeting baseURL (e.g.
// "http://plane-proxy/api/v1" inside the docker network or
// "https://plane.example.com/api/v1" externally). token is Plane's
// personal-access bearer token issued from the god-mode admin panel;
// an empty token is tolerated — every method short-circuits to zero
// values + nil error when the token is missing, so the admin page
// silently degrades to zeros instead of 500-ing.
//
// The HTTP client is SSRF-hardened: it only allows private/loopback
// addresses (Plane is on the docker network, never external).
func NewPlaneClient(baseURL, token string) *PlaneClient {
	return &PlaneClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   strings.TrimSpace(token),
		http:    newPlaneHTTPClient(),
	}
}

// newPlaneHTTPClient mirrors newNPMHTTPClient: it only permits
// private/loopback dial addresses. Plane lives on the docker bridge so
// the admin client must not be coaxed into dialing public IPs.
func newPlaneHTTPClient() *http.Client {
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
				return fmt.Errorf("plane dialer: unresolvable address %q", address)
			}
			if !isPrivateIP(ip) {
				return fmt.Errorf("plane dialer: refusing to dial public IP %s "+
					"(plane API must be reached over the internal docker network)", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     true,
		},
	}
}

// ready reports whether the client has both a base URL and a token
// configured. When false, every API method should short-circuit to
// (zero, nil).
func (c *PlaneClient) ready() bool {
	if c == nil {
		return false
	}
	return c.baseURL != "" && c.token != ""
}

// ---------------------------------------------------------------------------
// generic request helper

// do performs a GET against path and decodes the JSON response into out.
// On any non-2xx status it returns errPlaneAPIStatus so callers can
// detect "Plane is up but said no" vs network/setup failure.
//
// In keeping with the graceful-fallback contract (see file banner),
// callers translate errPlaneAPIStatus + network errors into (zero, nil)
// — only in tests is the raw error inspected.
var errPlaneAPIStatus = errors.New("plane: non-2xx status")

func (c *PlaneClient) do(ctx context.Context, path string, query url.Values, out any) error {
	if c == nil || c.baseURL == "" {
		return errors.New("plane: client not configured")
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("plane: build request %s: %w", path, err)
	}
	if c.token != "" {
		req.Header.Set("X-API-Key", c.token)
		// Plane's docs show bearer-token style on /api/v1, so set both
		// to maximise compatibility — the unused header is ignored by
		// the API.
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("plane: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Drain (bounded) so the connection can close cleanly without
		// dragging a multi-MB error body across the loop. We surface
		// the first 256 bytes for the rare case a caller logs the err.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%w: %s %d: %s", errPlaneAPIStatus, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("plane: decode %s response: %w", path, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return nil
}

// ---------------------------------------------------------------------------
// API methods

// ListWorkspaces returns every workspace visible to the bearer token.
// Returns (nil, nil) when the client is unconfigured (graceful fallback).
//
// TODO: this needs Wave C deploy + a real token at
// /etc/raph-installer/plane-admin-token to surface real numbers; verify
// against actual Plane API at https://developers.plane.so/api-reference
// once deployed.
func (c *PlaneClient) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	if !c.ready() {
		return nil, nil
	}
	var resp struct {
		Results []Workspace `json:"results"`
	}
	if err := c.do(ctx, "/workspaces/", nil, &resp); err != nil {
		// Graceful fallback: API is down or saying no — caller sees nil.
		if errors.Is(err, errPlaneAPIStatus) {
			return nil, nil
		}
		return nil, err
	}
	if resp.Results != nil {
		return resp.Results, nil
	}
	// Some Plane endpoints return a top-level array instead of a
	// pagination envelope. Tolerate both by re-trying as a bare array.
	var arr []Workspace
	if err := c.do(ctx, "/workspaces/", nil, &arr); err == nil {
		return arr, nil
	}
	return nil, nil
}

// UserByEmail looks up a Plane user by their email address. Returns
// (nil, nil) when the user doesn't exist OR the client is unconfigured.
//
// The email is URL-encoded via url.Values.Set — operator emails can
// contain `+` (gmail-style addressing); without encoding the API would
// see a space in place of the plus and miss the lookup.
//
// TODO: this needs Wave C deploy + a real token at
// /etc/raph-installer/plane-admin-token to surface real numbers; verify
// against actual Plane API at https://developers.plane.so/api-reference
// once deployed. The /users/?email=<email> path is plausible but Plane
// has historically gated user lookup behind workspace context — may
// need to switch to per-workspace member list iteration.
func (c *PlaneClient) UserByEmail(ctx context.Context, email string) (*PlaneUser, error) {
	if !c.ready() {
		return nil, nil
	}
	if email == "" {
		return nil, nil
	}
	q := url.Values{}
	q.Set("email", email)
	var resp struct {
		Results []PlaneUser `json:"results"`
	}
	if err := c.do(ctx, "/users/", q, &resp); err != nil {
		if errors.Is(err, errPlaneAPIStatus) {
			return nil, nil
		}
		return nil, err
	}
	for _, u := range resp.Results {
		if strings.EqualFold(u.Email, email) {
			cp := u
			return &cp, nil
		}
	}
	return nil, nil
}

// IssueCount returns the number of issues in workspaceSlug authored by
// userID. Returns (0, nil) on unconfigured/missing.
//
// TODO: this needs Wave C deploy + a real token at
// /etc/raph-installer/plane-admin-token to surface real numbers; verify
// against actual Plane API at https://developers.plane.so/api-reference
// once deployed. Plane's per-workspace issue listing requires a project
// scope; the real implementation may need to iterate projects and sum,
// or use a workspace-wide /issues/ endpoint if one exists.
func (c *PlaneClient) IssueCount(ctx context.Context, workspaceSlug, userID string) (int, error) {
	if !c.ready() || workspaceSlug == "" || userID == "" {
		return 0, nil
	}
	q := url.Values{}
	q.Set("created_by", userID)
	var resp struct {
		Count int `json:"count"`
	}
	path := fmt.Sprintf("/workspaces/%s/issues/", url.PathEscape(workspaceSlug))
	if err := c.do(ctx, path, q, &resp); err != nil {
		if errors.Is(err, errPlaneAPIStatus) {
			return 0, nil
		}
		return 0, err
	}
	return resp.Count, nil
}

// FileAssetBytes returns the total bytes of file_assets in workspaceSlug
// owned by userID. Returns (0, nil) on unconfigured/missing.
//
// TODO: this needs Wave C deploy + a real token at
// /etc/raph-installer/plane-admin-token to surface real numbers; verify
// against actual Plane API at https://developers.plane.so/api-reference
// once deployed. The "size" attribute name is a guess based on Plane's
// upstream serializers; may need to switch to "file_size" or sum a
// nested asset record.
func (c *PlaneClient) FileAssetBytes(ctx context.Context, workspaceSlug, userID string) (int64, error) {
	if !c.ready() || workspaceSlug == "" || userID == "" {
		return 0, nil
	}
	q := url.Values{}
	q.Set("created_by", userID)
	var resp struct {
		Results []struct {
			Size int64 `json:"size"`
		} `json:"results"`
	}
	path := fmt.Sprintf("/workspaces/%s/file-assets/", url.PathEscape(workspaceSlug))
	if err := c.do(ctx, path, q, &resp); err != nil {
		if errors.Is(err, errPlaneAPIStatus) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, r := range resp.Results {
		total += r.Size
	}
	return total, nil
}

// LastActivity is a best-effort lookup of the user's most recent Plane
// activity timestamp. Returns (zero, nil) on unconfigured/missing —
// callers should max() it against the Nextcloud last-login timestamp.
//
// TODO: this needs Wave C deploy + a real token at
// /etc/raph-installer/plane-admin-token to surface real numbers; verify
// against actual Plane API at https://developers.plane.so/api-reference
// once deployed. Plane's "last_active" attribute is not part of the
// public user serializer in older releases — may need a workspace-
// member-list endpoint instead.
func (c *PlaneClient) LastActivity(ctx context.Context, userID string) (time.Time, error) {
	if !c.ready() || userID == "" {
		return time.Time{}, nil
	}
	var resp struct {
		LastActive time.Time `json:"last_active"`
	}
	path := fmt.Sprintf("/users/%s/", url.PathEscape(userID))
	if err := c.do(ctx, path, nil, &resp); err != nil {
		if errors.Is(err, errPlaneAPIStatus) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return resp.LastActive, nil
}
