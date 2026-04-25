// audit.go — append-only JSONL audit log.
//
// Same file as the prior version (peers-audit.log on the host); we
// expanded the action namespace. Schema:
//
//   { "time":   RFC3339,
//     "action": "user.create|user.delete|peer.add|...",
//     "actor":  Authelia username,
//     "target": short identifier (peer name, username, ...),
//     "result": "ok" | "fail",
//     "note":   free-form detail / failure reason }
//
// Old peer-only entries used `peer`/`pubkey`/`ip` — keep parsing them
// for the audit page but new entries use the unified shape above.

package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"
)

type auditEntry struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Actor  string    `json:"actor"`
	Target string    `json:"target,omitempty"`
	Result string    `json:"result,omitempty"` // "ok" / "fail"
	Note   string    `json:"note,omitempty"`

	// Legacy fields, kept so old entries parse cleanly.
	Peer   string `json:"peer,omitempty"`
	Pubkey string `json:"pubkey,omitempty"`
	IP     string `json:"ip,omitempty"`
}

func writeAudit(path string, e auditEntry) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Result == "" {
		e.Result = "ok"
	}
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
			// Backfill: pre-expansion entries used .Peer/.Pubkey/.IP
			// instead of .Target/.Note. Map them so the renderer doesn't
			// have to special-case.
			if e.Target == "" && e.Peer != "" {
				e.Target = e.Peer
			}
			if e.Note == "" && e.IP != "" {
				e.Note = e.IP
			}
			out = append(out, e)
		}
	}
	if len(out) > max {
		out = out[len(out)-max:]
	}
	// Newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
