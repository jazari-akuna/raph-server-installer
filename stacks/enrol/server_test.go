// server_test.go — coverage for the per-platform "set up your device"
// helper appended to peer-created.html and peer-detail.html.
//
// Three concerns:
//   1. detectPlatform / DefaultPlatform routing (string-match over UA).
//   2. buildSetupHelp.QRDataURI is a real PNG-bearing data: URI when
//      qrencode is on PATH (skipped otherwise — mirrors the CI shape).
//   3. peer-created.html renders the {{template "setup-help"}} partial
//      with the user's literal peer name and the per-platform copy.

package main

import (
	"bytes"
	"encoding/base64"
	"html/template"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSetupHelp_DetectsPlatform(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want string
	}{
		{"iPhone Safari", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15", "ios"},
		{"iPad Safari", "Mozilla/5.0 (iPad; CPU OS 17_4 like Mac OS X)", "ios"},
		{"iPod touch", "Mozilla/5.0 (iPod touch; CPU iPhone OS 16_0 like Mac OS X)", "ios"},
		{"Android Chrome", "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36", "android"},
		{"macOS Safari", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15", "mac"},
		{"Windows Firefox", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:125.0) Gecko/20100101 Firefox/125.0", "win"},
		{"Linux Firefox", "Mozilla/5.0 (X11; Linux x86_64; rv:125.0) Gecko/20100101 Firefox/125.0", "linux"},
		{"empty", "", "unknown"},
		{"junk", "curl/8.5.0", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectPlatform(tc.ua); got != tc.want {
				t.Errorf("detectPlatform(%q) = %q, want %q", tc.ua, got, tc.want)
			}
		})
	}
}

func TestBuildSetupHelp_QRDataURI(t *testing.T) {
	if _, err := exec.LookPath("qrencode"); err != nil {
		t.Skip("qrencode binary not on PATH; skipping (the Dockerfile installs it for production)")
	}
	s := &server{cfg: config{
		domain:      "example.com",
		awgEndpoint: "gw.example.com:443",
	}}
	req := httptest.NewRequest("GET", "/peers/raph-laptop", nil)
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")
	p := peer{Name: "raph-laptop", IP: "10.0.0.2", PublicKey: "PUB", PrivateKey: "PRIV"}

	help, err := s.buildSetupHelp(req, p, "[Interface]\nPrivateKey = PRIV\n[Peer]\nPublicKey = PUB\n")
	if err != nil {
		t.Fatalf("buildSetupHelp: %v", err)
	}
	if help.PeerName != "raph-laptop" {
		t.Errorf("PeerName = %q, want raph-laptop", help.PeerName)
	}
	if help.Endpoint != "gw.example.com:443" {
		t.Errorf("Endpoint = %q", help.Endpoint)
	}
	if help.ApexDomain != "example.com" {
		t.Errorf("ApexDomain = %q", help.ApexDomain)
	}
	if help.DefaultPlatform != "mac" {
		t.Errorf("DefaultPlatform = %q, want mac", help.DefaultPlatform)
	}

	const prefix = "data:image/png;base64,"
	uri := string(help.QRDataURI)
	if !strings.HasPrefix(uri, prefix) {
		t.Fatalf("QRDataURI does not start with %q: %q...", prefix, firstN(uri, 40))
	}
	raw, err := base64.StdEncoding.DecodeString(uri[len(prefix):])
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	// PNG magic: 89 50 4E 47 0D 0A 1A 0A
	wantMagic := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	if len(raw) < len(wantMagic) || !bytes.Equal(raw[:len(wantMagic)], wantMagic) {
		t.Errorf("decoded bytes do not start with PNG magic: %v", raw[:min(len(raw), len(wantMagic))])
	}
}

func TestPeerCreatedRendersSetupHelp(t *testing.T) {
	// Locate the templates dir relative to this test file (project layout
	// is stable: stacks/enrol/web/templates/*.html).
	tmpl := template.New("").Funcs(template.FuncMap{
		"join":          strings.Join,
		"domain":        func() string { return "example.com" },
		"awgEnabled":    func() bool { return true },
		"gb":            func(int64) string { return "0" },
		"pct":           func(int64, int64) string { return "0" },
		"setupStepDone": func(any, string) bool { return false },
		"prettyBytes":   func(int64) string { return "0" },
		"storageByName": func([]UserStorage) map[string]UserStorage { return nil },
	})
	pattern := filepath.Join("web", "templates", "*.html")
	tmpl, err := tmpl.ParseGlob(pattern)
	if err != nil {
		t.Fatalf("ParseGlob %s: %v", pattern, err)
	}

	// Stub QR data-URI so the test passes even when qrencode is absent
	// — that's covered by TestBuildSetupHelp_QRDataURI separately.
	help := setupHelpData{
		PeerName:        "raph-laptop",
		ClientConf:      "[Interface]\nPrivateKey = STUB\n[Peer]\nPublicKey = STUB\n",
		QRDataURI:       template.URL("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="),
		Endpoint:        "gw.example.com:443",
		ApexDomain:      "example.com",
		DefaultPlatform: "win",
	}
	data := struct {
		Title         string
		User          string
		CSRF          string
		ViewerIsAdmin bool
		Peer          peer
		ClientConf    string
		ReloadNote    string
		SetupHelp     setupHelpData
	}{
		Title:         "device added",
		User:          "raph",
		CSRF:          "csrf",
		ViewerIsAdmin: false,
		Peer:          peer{Name: "raph-laptop", IP: "10.0.0.2", PublicKey: "PUB"},
		ClientConf:    help.ClientConf,
		ReloadNote:    "",
		SetupHelp:     help,
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "peer-created.html", data); err != nil {
		t.Fatalf("execute peer-created.html: %v", err)
	}
	body := buf.String()
	wants := []string{
		"AmneziaWG client for Windows",
		"Scan QR code",
		"raph-laptop",
		"awg-quick up raph-laptop",
		"id=\"setup-help\"",
		"data:image/png;base64,",
		"https://amnezia.org/downloads",
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("rendered body missing %q", w)
		}
	}
	// Hard rules: no <script>, no inline style attrs in the new partial.
	if strings.Contains(body, "<script") {
		t.Error("rendered body contains <script tag")
	}
	// We deliberately don't assert absence of style="..." globally (the
	// peers.html row uses a CSS class, but other future templates may
	// legitimately use inline styles); the partial itself is what matters.
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
