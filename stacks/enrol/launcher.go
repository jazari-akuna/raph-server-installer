// launcher.go — app launcher (post-login landing).
//
// Tile grid of apps backed by <launcherDir>/apps.json + per-app icons under
// <launcherDir>/icons/<id>.<ext>. The dir is bootstrapped with three default
// tiles (cloud, enrol-users, console) — those use CSS-initials fallback.
//
// Custom icons are fetched server-side from a user-supplied URL via an
// SSRF-hardened HTTP client: pre-resolution and post-resolution IP filtering
// rejects RFC1918 / loopback / link-local / CGNAT / IPv6 ULA + link-local,
// plus redirects (capped at 3) get the same treatment on each hop.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type LauncherApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Icon string `json:"icon"` // "icons/<id>.<ext>" or "" for initials fallback
}

var (
	reAppID       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reIconPath    = regexp.MustCompile(`^[a-z0-9-]+\.(png|jpg|webp|gif)$`)
	privateBlocks []*net.IPNet
)

func init() {
	for _, c := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"100.64.0.0/10",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("launcher: bad CIDR " + c + ": " + err.Error())
		}
		privateBlocks = append(privateBlocks, n)
	}
}

func validAppID(s string) bool { return reAppID.MatchString(s) }

func validAppURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return true
}

func loadLauncher(dir string) ([]LauncherApp, error) {
	b, err := os.ReadFile(filepath.Join(dir, "apps.json"))
	if err != nil {
		return nil, err
	}
	var apps []LauncherApp
	if err := json.Unmarshal(b, &apps); err != nil {
		return nil, fmt.Errorf("parse apps.json: %w", err)
	}
	return apps, nil
}

func saveLauncher(dir string, apps []LauncherApp) error {
	b, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "apps.json.tmp")
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "apps.json"))
}

func bootstrapLauncher(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "icons"), 0o750); err != nil {
		return fmt.Errorf("mkdir launcher dir: %w", err)
	}
	apps := filepath.Join(dir, "apps.json")
	if _, err := os.Stat(apps); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	defaults := []LauncherApp{
		{ID: "cloud", Name: "Cloud", URL: "https://cloud.antarctica-engineering.com/", Icon: ""},
		{ID: "enrol-users", Name: "Enrol", URL: "https://enrol.antarctica-engineering.com/users", Icon: ""},
		{ID: "console", Name: "Console", URL: "https://console.antarctica-engineering.com/", Icon: ""},
	}
	return saveLauncher(dir, defaults)
}

func removeOldIcons(dir, id string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "icons", id+".*"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	for _, n := range privateBlocks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func detectImageType(p []byte) (string, error) {
	if len(p) >= 8 && bytes.HasPrefix(p, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
		return "png", nil
	}
	if len(p) >= 3 && bytes.HasPrefix(p, []byte{0xFF, 0xD8, 0xFF}) {
		return "jpg", nil
	}
	if len(p) >= 12 && bytes.Equal(p[0:4], []byte("RIFF")) && bytes.Equal(p[8:12], []byte("WEBP")) {
		return "webp", nil
	}
	if len(p) >= 6 && (bytes.HasPrefix(p, []byte("GIF87a")) || bytes.HasPrefix(p, []byte("GIF89a"))) {
		return "gif", nil
	}
	low := bytes.ToLower(p)
	if bytes.Contains(low, []byte("<?xml")) || bytes.Contains(low, []byte("<svg")) {
		return "", errors.New("svg not allowed")
	}
	return "", errors.New("unrecognised image format")
}

func safeHTTPClient() *http.Client {
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
				return fmt.Errorf("dialer: unresolvable address %q", address)
			}
			if isPrivateIP(ip) {
				return fmt.Errorf("dialer: blocked private/loopback address %s", ip)
			}
			return nil
		},
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableKeepAlives:     true,
	}
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			host := req.URL.Hostname()
			if host == "" {
				return errors.New("redirect: empty host")
			}
			ips, err := net.DefaultResolver.LookupIP(req.Context(), "ip", host)
			if err != nil {
				return fmt.Errorf("redirect resolve %s: %w", host, err)
			}
			for _, ip := range ips {
				if isPrivateIP(ip) {
					return fmt.Errorf("redirect: blocked private address %s", ip)
				}
			}
			return nil
		},
	}
}

func fetchIcon(dir, id, rawURL string) (string, error) {
	if !validAppURL(rawURL) {
		return "", errors.New("invalid icon url")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "enrol-launcher/1.0")
	resp, err := safeHTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("icon fetch: status %d", resp.StatusCode)
	}
	body := io.LimitReader(resp.Body, 256*1024)
	br := bufio.NewReader(body)
	peek, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return "", fmt.Errorf("read head: %w", err)
	}
	ext, err := detectImageType(peek)
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "icons", id+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, br); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := removeOldIcons(dir, id); err != nil {
		os.Remove(tmp)
		return "", err
	}
	final := filepath.Join(dir, "icons", id+"."+ext)
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return "icons/" + id + "." + ext, nil
}

// initialOf returns the upper-cased first rune of name, or "?" if empty.
func initialOf(name string) string {
	for _, r := range name {
		return strings.ToUpper(string(r))
	}
	return "?"
}
