// The libsql leg: self-hosted sqld serving the app's db file over Hrana
// (HTTP) on the same VM — no cloud dependency. Query rides the VM's curl
// against the sqld HTTP endpoint; the REQUEST BODY IS BUILT GO-SIDE and
// rides stdin (perfect JSON escaping — no VM-side quoting), and the auth
// token is staged into a 0600 header file from the VM's env file, never
// argv. Snapshot treats the db as the file it is: consistent copy + an
// integrity_check gate stronger than any tail stamp.

package dbq

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
)

// Env-file contract keys (path injected by the caller — the project's
// env file, never hardcoded here).
const (
	EngineLibsql          = "libsql"
	LibsqlURLEnvKey       = "KAMPODRA_DB_URL"   // default http://127.0.0.1:8080 (co-located sqld)
	LibsqlTokenEnvKey     = "KAMPODRA_DB_TOKEN" // empty = unauthenticated localhost sqld
	LibsqlPathEnvKey      = "KAMPODRA_DB_PATH"  // the sqld --db-path on the VM (snapshot source)
	LibsqlDefaultURL      = "http://127.0.0.1:8080"
	LibsqlHranaPath       = "/v2/pipeline"
	LibsqlAuthHeaderStage = "/tmp/kampodra-libsql-auth"
)

// LibsqlProbeCommand: the leg's only VM requirement is curl (busybox wget
// can't take headers from a file — the token must never ride argv).
const LibsqlProbeCommand = `if command -v curl >/dev/null 2>&1; then echo yes; else echo no; fi`

// LibsqlAuthStageCommand stages the Authorization header file from the
// env file — 0600, percent-free (HTTP headers need no escaping, only
// newline stripping). Empty token → an EMPTY header file: curl -H @empty
// adds no header, so one command shape serves both auth and no-auth.
func LibsqlAuthStageCommand(envFile, stage string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2- | tr -d '\r\n'; }; ` +
		`umask 077; ` +
		`t=$(get ` + LibsqlTokenEnvKey + `); ` +
		`if [ -n "$t" ]; then printf 'Authorization: Bearer %s\n' "$t" > ` + stage + `; else : > ` + stage + `; fi; ` +
		`chmod 600 ` + stage
}

// LibsqlAuthCleanupCommand removes the staged header (Background cleanup).
func LibsqlAuthCleanupCommand(stage string) string {
	return "rm -f " + stage
}

// LibsqlQueryCommand composes the curl invocation: the Hrana pipeline body
// rides stdin (--data-binary @-), the staged header file carries auth.
// The URL is read env-side with the co-located default.
func LibsqlQueryCommand(stage string) string {
	return `u=$(get ` + LibsqlURLEnvKey + `); ` +
		`curl -sS -H @` + stage + ` -H 'Content-Type: application/json' --data-binary @- "` + `"${u:-` + LibsqlDefaultURL + `}` + LibsqlHranaPath + `"`
}

// LibsqlScratchPath mints the snapshot scratch (same crypto/rand
// discipline as the other legs).
func LibsqlScratchPath() string {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return "/tmp/kampodra-snapshot-" + hex.EncodeToString(b[:]) + ".db"
}

// LibsqlDbPathFromEnvCommand prints the declared sqld db file (the
// snapshot source; query needs no path).
func LibsqlDbPathFromEnvCommand(envFile string) string {
	return "grep -E '^" + LibsqlPathEnvKey + "=' " + envFile + " 2>/dev/null | head -1 | cut -d= -f2-"
}

// LibsqlIntegrityCheckCommand gates the copied file: the VM's sqlite3
// runs PRAGMA integrity_check; anything but "ok" refuses the snapshot.
func LibsqlIntegrityCheckCommand(scratch string) string {
	return "sqlite3 " + scratch + " 'PRAGMA integrity_check;'"
}

// HranaPipelineBody builds the v2 pipeline request for ONE statement —
// Go-side, so the SQL's JSON escaping is the stdlib's problem, not a
// shell quoting exercise.
func HranaPipelineBody(stmt string) string {
	return `{"requests":[{"type":"execute","stmt":{"sql":` + jsonString(stmt) + `}}]}`
}

// NormalizeHranaResponse flattens the v2 pipeline envelope into the same
// row shape the sqlite helper prints ([{"col":val},...] — numbers as
// numbers, text as strings, blobs base64, nulls null). An error result
// surfaces as a Go error carrying the server's message.
func NormalizeHranaResponse(raw string) (string, error) { return normalizeHrana(raw) }
