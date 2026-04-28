// envfile.go — minimal POSTGRES_PASSWORD reader for stack .env files.
//
// docker-compose .env files in this repo carry a single load-bearing
// secret per Postgres-using stack: `POSTGRES_PASSWORD=...`, optionally
// quoted. Both task_client.go (Vikunja stats) and backup.go (pg_dump
// for the cloud + task recipes) need to recover that value to talk to
// the database from outside the container. Originally inlined in
// TaskClient.password(); extracted here so backup.go's per-recipe pg
// dump pipeline reuses the EXACT same parsing rules — a divergence
// between the two readers would produce confusing "task stats work but
// task backup fails" mode after a future quoting tweak.
//
// First-match wins: the parser stops at the first POSTGRES_PASSWORD=
// line. This matches docker-compose's own behaviour, which silently
// uses the first declaration when a key is repeated. Quoted values
// (single or double, matched pair) get the outer quotes stripped.

package main

import (
	"bufio"
	"os"
	"strings"
)

// readPostgresPassword opens the env file at `path` and returns the
// value of the first `POSTGRES_PASSWORD=...` line (with surrounding
// whitespace and a single matched pair of quotes stripped). Returns
// ("", os.ErrNotExist) — wrapped — when the file cannot be opened, and
// ("", nil) when the key is simply absent so callers can distinguish
// "stack not deployed yet" from "stack present, empty password".
//
// IO errors during scanning surface as the scanner's Err() value;
// EOF without a match returns ("", nil).
func readPostgresPassword(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "POSTGRES_PASSWORD=") {
			continue
		}
		val := strings.TrimPrefix(line, "POSTGRES_PASSWORD=")
		val = strings.TrimSpace(val)
		// Strip a single matched pair of surrounding quotes — .env files
		// in this repo sometimes single- or double-quote passwords with
		// special characters; docker-compose strips them too.
		if len(val) >= 2 {
			if (val[0] == '\'' && val[len(val)-1] == '\'') ||
				(val[0] == '"' && val[len(val)-1] == '"') {
				val = val[1 : len(val)-1]
			}
		}
		return val, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}
