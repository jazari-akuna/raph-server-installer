// launcher.go — app launcher (post-login landing).
//
// Tile grid of apps backed by <launcherDir>/apps.json + per-app icons under
// <launcherDir>/icons/<id>.<ext>. The dir is bootstrapped with the default
// tiles (cloud, enrol-users, console, task — cloud/task omitted when the
// stack is opted out) whose PNGs are seeded from the
// image repo at /app/web/static/launcher-defaults/<id>.png on first run; if
// a copy fails the tile falls back to CSS-initials.
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
		"0.0.0.0/8", // "this network" — Linux routes outbound 0.0.0.0 to localhost.
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"100.64.0.0/10",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		// We deliberately do NOT add "::ffff:0:0/96" — its (*net.IPNet).Contains
		// matches every IPv4-mapped IPv6, which under Go's representation is
		// also every IPv4 address (net.ParseIP("8.8.8.8") returns a 16-byte
		// 4-in-6 slice). Adding it would block all v4 traffic, public included.
		// IPv4-mapped IPv6 SSRF (e.g. http://[::ffff:127.0.0.1]/) is handled
		// in isPrivateIP() below via To4() normalisation before the CIDR sweep.
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

func bootstrapLauncher(dir, domain string, skipCloud, skipTask bool) error {
	if err := os.MkdirAll(filepath.Join(dir, "icons"), 0o750); err != nil {
		return fmt.Errorf("mkdir launcher dir: %w", err)
	}
	apps := filepath.Join(dir, "apps.json")
	if _, err := os.Stat(apps); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	icons := seedDefaultIcons(dir, "/app/web/static/launcher-defaults")
	var defaults []LauncherApp
	if !skipCloud {
		defaults = append(defaults, LauncherApp{ID: "cloud", Name: "Cloud", URL: "https://cloud." + domain + "/", Icon: icons["cloud"]})
	}
	defaults = append(defaults,
		LauncherApp{ID: "enrol-users", Name: "Enrol", URL: "https://enrol." + domain + "/users", Icon: icons["enrol-users"]},
		LauncherApp{ID: "console", Name: "Console", URL: "https://console." + domain + "/", Icon: icons["console"]},
	)
	if !skipTask {
		defaults = append(defaults, LauncherApp{ID: "task", Name: "Tasks", URL: "https://task." + domain + "/", Icon: icons["task"]})
	}
	return saveLauncher(dir, defaults)
}

func seedDefaultIcons(launcherDir, sourceDir string) map[string]string {
	out := map[string]string{}
	for _, id := range []string{"cloud", "enrol-users", "console", "task"} {
		src := filepath.Join(sourceDir, id+".png")
		dst := filepath.Join(launcherDir, "icons", id+".png")
		if err := copyFileMode(src, dst, 0o640); err != nil {
			fmt.Fprintf(os.Stderr, "launcher: seed icon %s: %v\n", id, err)
			continue
		}
		out[id] = "icons/" + id + ".png"
	}
	return out
}

func copyFileMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".seed.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func removeOldIcons(dir, id string) error {
	for _, ext := range []string{"png", "jpg", "webp", "gif"} {
		p := filepath.Join(dir, "icons", id+"."+ext)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	// Belt-and-braces: net.IP's own classifiers cover 0.0.0.0 / :: (Unspecified)
	// and 127.0.0.0/8 / ::1 (Loopback) regardless of how the address was parsed,
	// including IPv4-mapped forms like ::ffff:127.0.0.1.
	if ip.IsUnspecified() || ip.IsLoopback() {
		return true
	}
	// Normalise IPv4-mapped IPv6 to the canonical 4-byte form so the v4 CIDRs
	// (10/8, 172.16/12, …) catch hosts typed as e.g. "[::ffff:10.0.0.1]". For
	// non-mapped addresses To4() returns nil and we fall through to the raw ip.
	probe := ip
	if v4 := ip.To4(); v4 != nil {
		probe = v4
	}
	for _, n := range privateBlocks {
		if n.Contains(probe) {
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
	const maxIconBytes = 256 * 1024
	body := io.LimitReader(resp.Body, maxIconBytes+1)
	br := bufio.NewReader(body)
	peek, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return "", fmt.Errorf("read head: %w", err)
	}
	ext, err := detectImageType(peek)
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, "icons", id+".fetch.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, br)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if n > maxIconBytes {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("icon too large (>%d bytes)", maxIconBytes)
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
