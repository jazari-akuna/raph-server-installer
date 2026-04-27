// peers.go — gw0.conf parser/writer + peer keypair gen + reload.
//
// Kept very close to the prior version (which is the audited working
// code). Two semantic additions:
//
//   - peer.User is filled in at render-time from the prefix of peer.Name,
//     enabling the grouped-by-user UI without changing on-disk state.
//   - peerNameFor(user, tag) returns the canonical "<user>-<tag>" name
//     used by the new "Add device" form.

package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// User returns the inferred owning username (the prefix before the first
// "-"), or "" if the name has no "-".
func (p peer) User() string {
	i := strings.IndexByte(p.Name, '-')
	if i <= 0 {
		return ""
	}
	return p.Name[:i]
}

// peerNameFor builds a canonical "<user>-<tag>" peer name and ensures the
// component pieces are individually valid.
func peerNameFor(user, tag string) (string, error) {
	if !validUsername(user) {
		return "", fmt.Errorf("invalid username %q", user)
	}
	if !validDeviceTag(tag) {
		return "", fmt.Errorf("invalid device tag %q (allowed: a-z 0-9, 1..16 chars)", tag)
	}
	return user + "-" + tag, nil
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
	raw            []byte
	peers          []peerEntry // [Peer] blocks, in file order
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
	dropFrom := target.startLine
	if dropFrom > 0 && strings.HasPrefix(strings.TrimSpace(lines[dropFrom-1]), "# peer:") {
		dropFrom--
	}
	dropTo := target.endLine
	if dropFrom > 0 && strings.TrimSpace(lines[dropFrom-1]) == "" {
		dropFrom--
	}
	keep := append([]string{}, lines[:dropFrom]...)
	keep = append(keep, lines[dropTo+1:]...)
	out := strings.Join(keep, "\n")
	return true, atomicWrite(path, []byte(out), 0o600)
}

// removePeersByPubkeys rewrites gw0.conf with EVERY matching [Peer] block
// stripped, in a single pass: one read, one write. Use this for bulk
// deletes (e.g. removeUserPeers cascading from a /users delete) — the
// per-peer loop pattern (call removePeerByPubkey then re-loadConf) can
// nil-deref if a single reload fails, and racks up N writes for N peers
// each racing any concurrent read of gw0.conf.
//
// Pubkeys absent from the parsed conf are silently skipped (idempotent
// end-state). Caller must not have mutated pc since loadConf.
func (pc *parsedConf) removePeersByPubkeys(path string, pubkeys map[string]bool) error {
	if len(pubkeys) == 0 {
		return nil
	}
	lines := strings.Split(string(pc.raw), "\n")
	drop := make([]bool, len(lines))
	for i := range pc.peers {
		e := &pc.peers[i]
		if !pubkeys[e.publicKey] {
			continue
		}
		dropFrom := e.startLine
		if dropFrom > 0 && strings.HasPrefix(strings.TrimSpace(lines[dropFrom-1]), "# peer:") {
			dropFrom--
		}
		dropTo := e.endLine
		if dropFrom > 0 && strings.TrimSpace(lines[dropFrom-1]) == "" {
			dropFrom--
		}
		for k := dropFrom; k <= dropTo && k < len(lines); k++ {
			drop[k] = true
		}
	}
	keep := make([]string, 0, len(lines))
	for i, ln := range lines {
		if !drop[i] {
			keep = append(keep, ln)
		}
	}
	out := strings.Join(keep, "\n")
	return atomicWrite(path, []byte(out), 0o600)
}

// listPeersWithMeta returns the merged view of [Peer] blocks + sidecar
// metadata. Peers that have no sidecar entry (e.g. ones added by
// scripts/provision-peer.sh, or by the prior version of enrol whose
// metadata file has since been wiped) are adopted by parsing the
// "# peer: <name> (added <ts> by <actor>)" comment line above the
// [Peer] header. If no such comment is present, the peer is rendered
// as "(unmanaged)" so the operator can still see + remove it.
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
		name, addedBy, addedAt := parsePeerComment(e.comment)
		if name == "" {
			name = "(unmanaged)"
		}
		out = append(out, peer{
			Name:      name,
			PublicKey: e.publicKey,
			IP:        ip,
			AddedBy:   addedBy,
			AddedAt:   addedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// parsePeerComment extracts (name, addedBy, addedAt) from a comment of
// the form
//
//	# peer: <name> (added <RFC3339> by <actor>)
//
// Returns ("", "", zero) on any parse failure — caller treats that as
// an unmanaged peer.
var rePeerComment = regexp.MustCompile(
	`^# peer:\s*(\S+)\s*(?:\(added\s+(\S+)(?:\s+by\s+(\S+))?\))?\s*$`)

func parsePeerComment(comment string) (name, addedBy string, addedAt time.Time) {
	m := rePeerComment.FindStringSubmatch(comment)
	if m == nil {
		return "", "", time.Time{}
	}
	name = m[1]
	if len(m) >= 3 && m[2] != "" {
		if t, err := time.Parse(time.RFC3339, m[2]); err == nil {
			addedAt = t
		}
	}
	if len(m) >= 4 {
		addedBy = m[3]
	}
	return name, addedBy, addedAt
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
// awg syncconf — reload host interface

func reloadInterface(awgDir, iface string) error {
	confPath := filepath.Join(awgDir, iface+".conf")
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
// rendering peer .conf

func renderClientConf(p peer, pc *parsedConf, cfg config) string {
	get := func(k string) string {
		if v, ok := pc.rawIfaceParams[k]; ok {
			return v
		}
		return ""
	}
	var serverPub string
	if b, err := os.ReadFile(filepath.Join(cfg.awgDir, cfg.awgIface+"_public.key")); err == nil {
		serverPub = strings.TrimSpace(string(b))
	}
	priv := p.PrivateKey
	if priv == "" {
		priv = "<PRIVATE_KEY_NOT_AVAILABLE_AFTER_CREATION>"
	}
	allowed := "0.0.0.0/0,::/0"
	for _, candidate := range []string{
		"/var/cache/raph/allowed-ips.txt",
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

// ---------------------------------------------------------------------------
// helpers (validation, sanitize, atomic write, subnet prefix)

var (
	rePeerName    = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
	reUsername    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	reDeviceTag   = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	reEmail       = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	reDisplayName = regexp.MustCompile(`^[\p{L}\p{N} ._'\-]{1,64}$`)
)

func validName(s string) bool        { return rePeerName.MatchString(s) }
func validUsername(s string) bool    { return reUsername.MatchString(s) }
func validDeviceTag(s string) bool   { return reDeviceTag.MatchString(s) }
func validEmail(s string) bool       { return reEmail.MatchString(s) }
func validDisplayName(s string) bool { return reDisplayName.MatchString(s) }

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

// atomicWrite — write+rename, preserving file mode 0600.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
