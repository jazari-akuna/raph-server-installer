// enrol — gw0 peer-management web UI.
//
// Trusts Authelia-injected `Remote-User` and `Remote-Groups` headers.
// All mutating routes additionally require membership of $ENROL_REQUIRED_GROUP.
// Auth headers MUST be present on every request — missing means the
// upstream NPM forward-auth was bypassed (misconfig); we 401 hard.
//
// Source of truth: $ENROL_AWG_DIR/$ENROL_AWG_IFACE.conf  (default
// /etc/amnezia/amneziawg/gw0.conf). Sidecar metadata: peers-meta.json.
// Audit log: peers-audit.log (one JSON line per change).
//
// After mutating gw0.conf we attempt a live reload via:
//   awg syncconf gw0 <(awg-quick strip /etc/amnezia/amneziawg/gw0.conf)
// When $ENROL_RELOAD_NSENTER is unset/"true" we wrap the command in
//   nsenter --target 1 --net --mount --
// to enter the host's namespaces (used when running on a private docker
// network). When set to "false" — e.g. with network_mode: host — the
// command runs directly in the already-shared host net+mount namespace.
// If awg is unavailable, fall back to vanilla wg / wg-quick. On any
// failure, log a warning and instruct the operator to
// `sudo systemctl restart awg-quick@gw0` on the host.
//
// No external Go deps. Curve25519 keypair generation via crypto/ecdh.
// QR code generation by shelling out to `qrencode` in the runtime image.

package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// configuration

type config struct {
	listen        string
	awgDir        string
	awgIface      string
	awgEndpoint   string
	peerSubnet    string // e.g. "10.99.0.0/24"
	peerStart     int
	headerUser    string
	headerGroups  string
	requiredGroup string
	templatesDir  string
	staticDir     string
}

func loadConfig() config {
	envOr := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	startStr := envOr("ENROL_PEER_START", "10")
	start, err := strconv.Atoi(startStr)
	if err != nil {
		log.Fatalf("ENROL_PEER_START: %v", err)
	}
	return config{
		listen:        envOr("ENROL_LISTEN", ":8080"),
		awgDir:        envOr("ENROL_AWG_DIR", "/etc/amnezia/amneziawg"),
		awgIface:      envOr("ENROL_AWG_IFACE", "gw0"),
		awgEndpoint:   envOr("ENROL_AWG_ENDPOINT", "gw.antarctica-engineering.com:51820"),
		peerSubnet:    envOr("ENROL_PEER_SUBNET", "10.99.0.0/24"),
		peerStart:     start,
		headerUser:    envOr("ENROL_HEADER_USER", "Remote-User"),
		headerGroups:  envOr("ENROL_HEADER_GROUPS", "Remote-Groups"),
		requiredGroup: envOr("ENROL_REQUIRED_GROUP", "admins"),
		templatesDir:  envOr("ENROL_TEMPLATES", "/app/web/templates"),
		staticDir:     envOr("ENROL_STATIC", "/app/web/static"),
	}
}

// ---------------------------------------------------------------------------
// peer model

type peer struct {
	Name       string    `json:"name"`
	DeviceTag  string    `json:"device_tag,omitempty"`
	PublicKey  string    `json:"public_key"`
	PrivateKey string    `json:"-"` // never persisted; only on creation
	IP         string    `json:"ip"`
	AddedBy    string    `json:"added_by"`
	AddedAt    time.Time `json:"added_at"`
}

// metaStore — sidecar JSON keyed by public key.
type metaStore struct {
	path string
	mu   sync.Mutex
	data map[string]peer
}

func newMetaStore(path string) (*metaStore, error) {
	m := &metaStore{path: path, data: map[string]peer{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m.data); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return m, nil
}

func (m *metaStore) get(pubkey string) (peer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.data[pubkey]
	return p, ok
}

func (m *metaStore) put(p peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[p.PublicKey] = p
	return m.flush()
}

func (m *metaStore) delete(pubkey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, pubkey)
	return m.flush()
}

func (m *metaStore) flush() error {
	b, err := json.MarshalIndent(m.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// ---------------------------------------------------------------------------
// gw0.conf parser/writer

type parsedConf struct {
	raw     []byte
	peers   []peerEntry // [Peer] blocks, in file order
	rawIfaceParams map[string]string
}

type peerEntry struct {
	publicKey  string
	allowedIPs string
	startLine  int
	endLine    int
	comment    string // line preceding [Peer], if any
}

var (
	rePeerHdr   = regexp.MustCompile(`(?m)^\[Peer\]\s*$`)
	reIfaceHdr  = regexp.MustCompile(`(?m)^\[Interface\]\s*$`)
	reSection   = regexp.MustCompile(`(?m)^\[(\w+)\]\s*$`)
	reKVPubKey  = regexp.MustCompile(`(?m)^\s*PublicKey\s*=\s*(.+?)\s*$`)
	reKVAllowed = regexp.MustCompile(`(?m)^\s*AllowedIPs\s*=\s*(.+?)\s*$`)
	reKVAny     = regexp.MustCompile(`(?m)^\s*([A-Za-z][A-Za-z0-9]*)\s*=\s*(.+?)\s*$`)
)

func loadConf(path string) (*parsedConf, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pc := &parsedConf{raw: b, rawIfaceParams: map[string]string{}}

	lines := strings.Split(string(b), "\n")
	section := ""
	var cur *peerEntry
	for i, ln := range lines {
		if m := reSection.FindStringSubmatch(ln); m != nil {
			if cur != nil {
				cur.endLine = i - 1
				pc.peers = append(pc.peers, *cur)
				cur = nil
			}
			section = strings.ToLower(m[1])
			if section == "peer" {
				cur = &peerEntry{startLine: i}
				// collect leading comment line(s)
				if i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#") {
					cur.comment = strings.TrimSpace(lines[i-1])
				}
			}
			continue
		}
		if section == "interface" {
			if m := reKVAny.FindStringSubmatch(ln); m != nil {
				pc.rawIfaceParams[m[1]] = m[2]
			}
		}
		if section == "peer" && cur != nil {
			if m := reKVPubKey.FindStringSubmatch(ln); m != nil {
				cur.publicKey = strings.TrimSpace(m[1])
			}
			if m := reKVAllowed.FindStringSubmatch(ln); m != nil {
				cur.allowedIPs = strings.TrimSpace(m[1])
			}
		}
	}
	if cur != nil {
		cur.endLine = len(lines) - 1
		pc.peers = append(pc.peers, *cur)
	}
	return pc, nil
}

// usedOctets returns the set of host octets used in [Peer] AllowedIPs
// within $peerSubnet (e.g. "10.99.0.").
func (pc *parsedConf) usedOctets(subnetPrefix string) map[int]bool {
	used := map[int]bool{}
	for _, p := range pc.peers {
		for _, cidr := range strings.Split(p.allowedIPs, ",") {
			cidr = strings.TrimSpace(cidr)
			if cidr == "" {
				continue
			}
			ip := strings.SplitN(cidr, "/", 2)[0]
			if !strings.HasPrefix(ip, subnetPrefix) {
				continue
			}
			tail := strings.TrimPrefix(ip, subnetPrefix)
			if n, err := strconv.Atoi(tail); err == nil {
				used[n] = true
			}
		}
	}
	return used
}

// pickFreeOctet finds the lowest free octet ≥ start, ≤ 254, in subnet.
func (pc *parsedConf) pickFreeOctet(subnetPrefix string, start int) (int, error) {
	used := pc.usedOctets(subnetPrefix)
	for i := start; i <= 254; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free peer octet in subnet starting %s%d", subnetPrefix, start)
}

// appendPeer appends a [Peer] block to gw0.conf atomically.
func (pc *parsedConf) appendPeer(path string, p peer) error {
	out := &strings.Builder{}
	out.Write(pc.raw)
	if !strings.HasSuffix(string(pc.raw), "\n") {
		out.WriteString("\n")
	}
	fmt.Fprintf(out, "\n# peer: %s (added %s by %s)\n",
		sanitize(p.Name),
		p.AddedAt.UTC().Format(time.RFC3339),
		sanitize(p.AddedBy))
	fmt.Fprintf(out, "[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n",
		p.PublicKey, p.IP)
	return atomicWrite(path, []byte(out.String()), 0o600)
}

// removePeerByPubkey rewrites gw0.conf with the matching [Peer] block stripped.
func (pc *parsedConf) removePeerByPubkey(path, pubkey string) (bool, error) {
	lines := strings.Split(string(pc.raw), "\n")
	var target *peerEntry
	for i := range pc.peers {
		if pc.peers[i].publicKey == pubkey {
			target = &pc.peers[i]
			break
		}
	}
	if target == nil {
		return false, nil
	}
	// also drop a single comment line immediately above the [Peer] header
	dropFrom := target.startLine
	if dropFrom > 0 && strings.HasPrefix(strings.TrimSpace(lines[dropFrom-1]), "# peer:") {
		dropFrom--
	}
	dropTo := target.endLine
	// trim leading blank line
	if dropFrom > 0 && strings.TrimSpace(lines[dropFrom-1]) == "" {
		dropFrom--
	}
	keep := append([]string{}, lines[:dropFrom]...)
	keep = append(keep, lines[dropTo+1:]...)
	out := strings.Join(keep, "\n")
	return true, atomicWrite(path, []byte(out), 0o600)
}

// listPeersWithMeta returns the merged view of [Peer] blocks + sidecar metadata.
func (pc *parsedConf) listPeersWithMeta(meta *metaStore) []peer {
	out := make([]peer, 0, len(pc.peers))
	for _, e := range pc.peers {
		ip := strings.SplitN(strings.TrimSpace(e.allowedIPs), "/", 2)[0]
		if m, ok := meta.get(e.publicKey); ok {
			m.IP = ip
			m.PublicKey = e.publicKey
			out = append(out, m)
			continue
		}
		out = append(out, peer{
			Name:      "(unmanaged)",
			PublicKey: e.publicKey,
			IP:        ip,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// ---------------------------------------------------------------------------
// keypair generation (Curve25519, WireGuard-compatible)

func genKeypair() (priv, pub string, err error) {
	curve := ecdh.X25519()
	k, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	priv = base64.StdEncoding.EncodeToString(k.Bytes())
	pub = base64.StdEncoding.EncodeToString(k.PublicKey().Bytes())
	return
}

// ---------------------------------------------------------------------------
// audit log

type auditEntry struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Actor  string    `json:"actor"`
	Peer   string    `json:"peer,omitempty"`
	Pubkey string    `json:"pubkey,omitempty"`
	IP     string    `json:"ip,omitempty"`
	Note   string    `json:"note,omitempty"`
}

func writeAudit(path string, e auditEntry) {
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("audit: open %s: %v", path, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("audit: write: %v", err)
	}
}

func readAudit(path string, max int) []auditEntry {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	out := []auditEntry{}
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var e auditEntry
		if err := json.Unmarshal([]byte(ln), &e); err == nil {
			out = append(out, e)
		}
	}
	// last $max entries, newest-first
	if len(out) > max {
		out = out[len(out)-max:]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---------------------------------------------------------------------------
// awg syncconf — reload host interface

func reloadInterface(awgDir, iface string) error {
	confPath := filepath.Join(awgDir, iface+".conf")
	// Pipeline: awg-quick strip <conf> | awg syncconf <iface> /dev/stdin
	// When ENROL_RELOAD_NSENTER=true (default), wrap in nsenter to enter
	// the host's net+mount namespaces. When "false" (e.g. when the
	// container uses network_mode: host), run directly.
	useNsenter := os.Getenv("ENROL_RELOAD_NSENTER") != "false"
	mkCmd := func(script string) *exec.Cmd {
		if useNsenter {
			return exec.Command("nsenter", "--target", "1", "--net", "--mount", "--",
				"bash", "-c", script)
		}
		return exec.Command("bash", "-c", script)
	}
	script := fmt.Sprintf(
		`awg syncconf %s <(awg-quick strip %s)`, iface, confPath)
	cmd := mkCmd(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Fall back to vanilla wg if awg/awg-quick unavailable on host.
		script = fmt.Sprintf(
			`wg syncconf %s <(wg-quick strip %s)`, iface, confPath)
		cmd2 := mkCmd(script)
		out2, err2 := cmd2.CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("syncconf failed: awg=%v (%s) wg=%v (%s)",
				err, strings.TrimSpace(string(out)),
				err2, strings.TrimSpace(string(out2)))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// auth middleware

type authedUser struct {
	Name   string
	Groups []string
}

func (u authedUser) inGroup(g string) bool {
	for _, x := range u.Groups {
		if x == g {
			return true
		}
	}
	return false
}

func authFromRequest(r *http.Request, cfg config) (authedUser, error) {
	user := r.Header.Get(cfg.headerUser)
	if user == "" {
		return authedUser{}, errors.New("missing " + cfg.headerUser)
	}
	groupsRaw := r.Header.Get(cfg.headerGroups)
	var groups []string
	for _, g := range strings.Split(groupsRaw, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			groups = append(groups, g)
		}
	}
	return authedUser{Name: user, Groups: groups}, nil
}

func requireAuth(cfg config, requireGroup bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := authFromRequest(r, cfg)
		if err != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.Error(w,
				"401 — authentication required. "+
					"This service must be reached through the SSO portal at "+
					"https://auth.antarctica-engineering.com/.",
				http.StatusUnauthorized)
			return
		}
		if requireGroup && !u.inGroup(cfg.requiredGroup) {
			http.Error(w,
				fmt.Sprintf("403 — group %q required", cfg.requiredGroup),
				http.StatusForbidden)
			return
		}
		// Stash auth identity on a cloned request via internal headers for
		// handler use; simpler than context plumbing at our scale.
		r = r.Clone(r.Context())
		r.Header.Set("X-Enrol-User", u.Name)
		r.Header.Set("X-Enrol-Groups", strings.Join(u.Groups, ","))
		h(w, r)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers

type server struct {
	cfg    config
	tmpl   *template.Template
	muConf sync.Mutex // serialize gw0.conf rewrites
}

func newServer(cfg config) (*server, error) {
	tmpl := template.New("").Funcs(template.FuncMap{
		"join": strings.Join,
	})
	pattern := filepath.Join(cfg.templatesDir, "*.html")
	tmpl, err := tmpl.ParseGlob(pattern)
	if err != nil {
		return nil, fmt.Errorf("parse templates %s: %w", pattern, err)
	}
	return &server{cfg: cfg, tmpl: tmpl}, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
		if _, err := os.Stat(path); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})

	mux.Handle("/static/", http.StripPrefix("/static/",
		http.FileServer(http.Dir(s.cfg.staticDir))))

	mux.HandleFunc("/", requireAuth(s.cfg, false, s.handleIndex))
	mux.HandleFunc("/peers", requireAuth(s.cfg, true, s.handlePeers))
	mux.HandleFunc("/peers/", requireAuth(s.cfg, true, s.handlePeerSub))
	mux.HandleFunc("/audit", requireAuth(s.cfg, true, s.handleAudit))

	return mux
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/peers", http.StatusFound)
}

func (s *server) handlePeers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderPeerList(w, r, "")
	case http.MethodPost:
		s.handleAddPeer(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) renderPeerList(w http.ResponseWriter, r *http.Request, flash string) {
	confPath := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	pc, err := loadConf(confPath)
	if err != nil {
		http.Error(w, "load conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		http.Error(w, "load meta: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := struct {
		Title string
		User  string
		Peers []peer
		Flash string
	}{
		Title: "peers",
		User:  r.Header.Get("X-Enrol-User"),
		Peers: pc.listPeersWithMeta(meta),
		Flash: flash,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "peers.html", data); err != nil {
		log.Printf("template peers.html: %v", err)
	}
}

func (s *server) handleAddPeer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.Form.Get("name"))
	tag := strings.TrimSpace(r.Form.Get("device_tag"))
	if !validName(name) {
		http.Error(w, "invalid name (allowed: a-z A-Z 0-9 . _ -; 1..64 chars)",
			http.StatusBadRequest)
		return
	}
	actor := r.Header.Get("X-Enrol-User")

	s.muConf.Lock()
	defer s.muConf.Unlock()

	confPath := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	pc, err := loadConf(confPath)
	if err != nil {
		http.Error(w, "load conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	prefix, err := subnetPrefix(s.cfg.peerSubnet)
	if err != nil {
		http.Error(w, "bad subnet: "+err.Error(), http.StatusInternalServerError)
		return
	}
	octet, err := pc.pickFreeOctet(prefix, s.cfg.peerStart)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	priv, pub, err := genKeypair()
	if err != nil {
		http.Error(w, "keygen: "+err.Error(), http.StatusInternalServerError)
		return
	}
	p := peer{
		Name:       name,
		DeviceTag:  tag,
		PublicKey:  pub,
		PrivateKey: priv,
		IP:         fmt.Sprintf("%s%d", prefix, octet),
		AddedBy:    actor,
		AddedAt:    time.Now().UTC(),
	}
	if err := pc.appendPeer(confPath, p); err != nil {
		http.Error(w, "write conf: "+err.Error(), http.StatusInternalServerError)
		return
	}

	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		http.Error(w, "load meta: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := meta.put(p); err != nil {
		log.Printf("meta put: %v (peer added to gw0.conf anyway)", err)
	}

	writeAudit(filepath.Join(s.cfg.awgDir, "peers-audit.log"), auditEntry{
		Time: time.Now().UTC(), Action: "add", Actor: actor,
		Peer: p.Name, Pubkey: p.PublicKey, IP: p.IP,
	})

	reloadNote := ""
	if err := reloadInterface(s.cfg.awgDir, s.cfg.awgIface); err != nil {
		log.Printf("reload: %v", err)
		reloadNote = "config saved; reload needed: sudo systemctl restart awg-quick@" +
			s.cfg.awgIface + " on host"
	}

	clientConf := renderClientConf(p, pc, s.cfg)
	data := struct {
		Title      string
		User       string
		Peer       peer
		ClientConf string
		ReloadNote string
	}{
		Title:      "peer added",
		User:       actor,
		Peer:       p,
		ClientConf: clientConf,
		ReloadNote: reloadNote,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "peer-created.html", data); err != nil {
		log.Printf("template peer-created.html: %v", err)
	}
}

func (s *server) handlePeerSub(w http.ResponseWriter, r *http.Request) {
	// /peers/<name>           GET  detail
	// /peers/<name>/delete    POST remove
	// /peers/<name>/config    GET  download conf
	// /peers/<name>/qr.png    GET  QR PNG
	rest := strings.TrimPrefix(r.URL.Path, "/peers/")
	if rest == "" {
		http.Redirect(w, r, "/peers", http.StatusFound)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	if !validName(name) {
		http.Error(w, "invalid peer name", http.StatusBadRequest)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "":
		s.handlePeerDetail(w, r, name)
	case "delete":
		s.handleDeletePeer(w, r, name)
	case "config":
		s.handleDownloadConf(w, r, name)
	case "qr.png":
		s.handleQR(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

// findPeerByName looks up a peer by its sidecar metadata name.
func (s *server) findPeerByName(name string) (peer, *parsedConf, error) {
	pc, err := loadConf(filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf"))
	if err != nil {
		return peer{}, nil, err
	}
	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		return peer{}, nil, err
	}
	for _, p := range pc.listPeersWithMeta(meta) {
		if p.Name == name {
			return p, pc, nil
		}
	}
	return peer{}, pc, errors.New("not found")
}

func (s *server) handlePeerDetail(w http.ResponseWriter, r *http.Request, name string) {
	p, pc, err := s.findPeerByName(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	clientConf := renderClientConf(p, pc, s.cfg) // PrivateKey unknown; placeholder
	data := struct {
		Title      string
		User       string
		Peer       peer
		ClientConf string
	}{
		Title:      "peer " + p.Name,
		User:       r.Header.Get("X-Enrol-User"),
		Peer:       p,
		ClientConf: clientConf,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "peer-detail.html", data); err != nil {
		log.Printf("template peer-detail.html: %v", err)
	}
}

func (s *server) handleDeletePeer(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actor := r.Header.Get("X-Enrol-User")

	s.muConf.Lock()
	defer s.muConf.Unlock()

	confPath := filepath.Join(s.cfg.awgDir, s.cfg.awgIface+".conf")
	pc, err := loadConf(confPath)
	if err != nil {
		http.Error(w, "load conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	meta, err := newMetaStore(filepath.Join(s.cfg.awgDir, "peers-meta.json"))
	if err != nil {
		http.Error(w, "load meta: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var target peer
	for _, p := range pc.listPeersWithMeta(meta) {
		if p.Name == name {
			target = p
			break
		}
	}
	if target.PublicKey == "" {
		http.Error(w, "peer not found", http.StatusNotFound)
		return
	}
	ok, err := pc.removePeerByPubkey(confPath, target.PublicKey)
	if err != nil {
		http.Error(w, "rewrite conf: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "peer not found in gw0.conf", http.StatusNotFound)
		return
	}
	_ = meta.delete(target.PublicKey)
	writeAudit(filepath.Join(s.cfg.awgDir, "peers-audit.log"), auditEntry{
		Time: time.Now().UTC(), Action: "remove", Actor: actor,
		Peer: target.Name, Pubkey: target.PublicKey, IP: target.IP,
	})
	if err := reloadInterface(s.cfg.awgDir, s.cfg.awgIface); err != nil {
		log.Printf("reload after delete: %v", err)
	}
	http.Redirect(w, r, "/peers", http.StatusSeeOther)
}

func (s *server) handleDownloadConf(w http.ResponseWriter, r *http.Request, name string) {
	// PrivateKey is only available at creation time — we never store it.
	// This endpoint serves the config WITHOUT the private key, suitable
	// for re-import on a device that already has its key. Useful for
	// re-pulling AllowedIPs after a chnroutes refresh.
	p, pc, err := s.findPeerByName(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	conf := renderClientConf(p, pc, s.cfg)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s.conf"`, name))
	io.WriteString(w, conf)
}

func (s *server) handleQR(w http.ResponseWriter, r *http.Request, name string) {
	p, pc, err := s.findPeerByName(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	conf := renderClientConf(p, pc, s.cfg)
	cmd := exec.Command("qrencode", "-t", "PNG", "-o", "-", "-s", "6", "-l", "L")
	cmd.Stdin = strings.NewReader(conf)
	out, err := cmd.Output()
	if err != nil {
		http.Error(w, "qrencode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(out)
}

func (s *server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries := readAudit(filepath.Join(s.cfg.awgDir, "peers-audit.log"), 200)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// ---------------------------------------------------------------------------
// helpers

var reValidName = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)

func validName(s string) bool { return reValidName.MatchString(s) }

func sanitize(s string) string {
	out := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
	if len(out) > 256 {
		out = out[:256]
	}
	return out
}

func subnetPrefix(cidr string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return "", errors.New("only IPv4 subnets supported")
	}
	return fmt.Sprintf("%d.%d.%d.", ip[0], ip[1], ip[2]), nil
}

func renderClientConf(p peer, pc *parsedConf, cfg config) string {
	get := func(k string) string {
		if v, ok := pc.rawIfaceParams[k]; ok {
			return v
		}
		return ""
	}
	var serverPub string
	// best-effort: read <iface>_public.key sidecar if present
	if b, err := os.ReadFile(filepath.Join(cfg.awgDir, cfg.awgIface+"_public.key")); err == nil {
		serverPub = strings.TrimSpace(string(b))
	}
	priv := p.PrivateKey
	if priv == "" {
		priv = "<PRIVATE_KEY_NOT_AVAILABLE_AFTER_CREATION>"
	}
	allowed := "0.0.0.0/0,::/0"
	// If the host has a cached chnroutes complement file, prefer it.
	for _, candidate := range []string{
		"/var/cache/rarcus/allowed-ips.txt",
		filepath.Join(cfg.awgDir, "allowed-ips.txt"),
	} {
		if b, err := os.ReadFile(candidate); err == nil && len(b) > 0 {
			allowed = strings.TrimSpace(string(b))
			break
		}
	}
	out := &strings.Builder{}
	fmt.Fprintf(out, "# enrol peer: %s\n", sanitize(p.Name))
	fmt.Fprintf(out, "# generated %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintln(out, "[Interface]")
	fmt.Fprintf(out, "PrivateKey = %s\n", priv)
	fmt.Fprintf(out, "Address = %s/32\n", p.IP)
	fmt.Fprintln(out, "DNS = 1.1.1.1, 8.8.8.8")
	for _, k := range []string{"Jc", "Jmin", "Jmax", "S1", "S2", "H1", "H2", "H3", "H4"} {
		if v := get(k); v != "" {
			fmt.Fprintf(out, "%s = %s\n", k, v)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "[Peer]")
	if serverPub != "" {
		fmt.Fprintf(out, "PublicKey = %s\n", serverPub)
	} else {
		fmt.Fprintln(out, "PublicKey = <SERVER_PUBLIC_KEY_NOT_FOUND>")
	}
	fmt.Fprintf(out, "Endpoint = %s\n", cfg.awgEndpoint)
	fmt.Fprintln(out, "PersistentKeepalive = 25")
	fmt.Fprintln(out, "# AllowedIPs managed by update-route-tables.sh")
	fmt.Fprintf(out, "AllowedIPs = %s\n", allowed)
	return out.String()
}

// atomicWrite — write+rename, preserving file mode 0600.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}


// ---------------------------------------------------------------------------
// main

func main() {
	cfg := loadConfig()
	srv, err := newServer(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	hs := &http.Server{
		Addr:              cfg.listen,
		Handler:           logMiddleware(srv.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("enrol listening on %s; awgDir=%s iface=%s",
		cfg.listen, cfg.awgDir, cfg.awgIface)
	if err := hs.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func logMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		log.Printf("%s %s %s (%v)", r.Method, r.URL.Path,
			r.Header.Get("X-Enrol-User"), time.Since(start))
	})
}
