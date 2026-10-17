// The MariaDB leg of the db surface: a server engine, not a file — the
// flow composes mariadb-dump/mariadb ON the VM (or FROM the app VM against
// a db host), with credentials read from the VM's env file at run time.
// kampodra never stores DB credentials itself: they ride the project's env
// file (path injected by the caller) via `kampodra env set/push`, are
// copied into a 0600 defaults file on the VM, and never touch argv
// (nothing leaks into `ps`).

package dbq

import (
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
)

// Unit-env contract keys (the VM's env file — the project adapter's
// EnvFile, path injected — carries them via `kampodra env set`/`env
// push`: the same surface the app itself uses).
const (
	EngineEnvKey        = "KAMPODRA_DB_ENGINE"   // "mariadb" selects this leg
	MariaNameEnvKey     = "KAMPODRA_DB_NAME"     // the schema to dump/query
	MariaHostEnvKey     = "KAMPODRA_DB_HOST"     // empty = local socket (same-VM)
	MariaPortEnvKey     = "KAMPODRA_DB_PORT"     // empty = client default
	MariaUserEnvKey     = "KAMPODRA_DB_USER"     // empty = ssh user (unix_socket auth)
	MariaPasswordEnvKey = "KAMPODRA_DB_PASSWORD" // empty = no password line
)

// MariaProbeCommand answers yes/no in ONE round-trip: is the MariaDB
// client stack present? (mariadb-dump AND the mariadb client — the dump
// alone can't run queries.)
const MariaProbeCommand = `if command -v mariadb-dump >/dev/null 2>&1 && command -v mariadb >/dev/null 2>&1; then echo yes; else echo no; fi`

// EngineMagicProbeCommand classifies a FILE argument by its magic: the
// sqlite leg's signature is the 16-byte header. Used by auto engine
// resolution when the unit env declares nothing.
func EngineMagicProbeCommand(dbPath string) string {
	return "head -c 15 " + dbPath + " 2>/dev/null | grep -q 'SQLite format 3' && echo sqlite || echo other"
}

// EngineUnitEnvProbeCommand echoes the engine the unit env declares
// (mariadb | libsql | empty — the operator's explicit choice rides
// KAMPODRA_DB_ENGINE). The env file's path is the project adapter's
// (profile-configurable) — injected, never hardcoded here.
func EngineUnitEnvProbeCommand(envFile string) string {
	return "grep -E '^" + EngineEnvKey + "=(mariadb|libsql)' " + envFile + " 2>/dev/null | head -1 | cut -d= -f2-"
}

// MariaDefaultsPath is the on-VM defaults file holding the client creds —
// 0600, scratch-named, cleaned up by the flow that created it.
func MariaDefaultsPath() string {
	return "/tmp/kampodra-maria-defaults"
}

// MariaDefaultsFromUnitEnvCommand builds the client defaults file FROM the
// unit env in ONE compound sh run: grep (never source — the unit env is
// env-file semantics, values are raw after the first '='), percent-escape
// for the option-file format, umask 077 + chmod 600. The password goes
// env-file → defaults-file on the VM; it never transits argv or stdout.
func MariaDefaultsFromUnitEnvCommand(envFile, defaults string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`esc() { sed 's/%/%%/g'; }; ` +
		`umask 077; { ` +
		`printf '[client]\n'; ` +
		`v=$(get ` + MariaHostEnvKey + `); [ -n "$v" ] && printf 'host=%s\n' "$v"; ` +
		`v=$(get ` + MariaPortEnvKey + `); [ -n "$v" ] && printf 'port=%s\n' "$v"; ` +
		`v=$(get ` + MariaUserEnvKey + `); [ -n "$v" ] && printf 'user=%s\n' "$v"; ` +
		`v=$(get ` + MariaPasswordEnvKey + ` | esc); [ -n "$v" ] && printf 'password=%s\n' "$v"; ` +
		`} > ` + defaults + ` && chmod 600 ` + defaults
}

// MariaDefaultsCleanupCommand removes the defaults file (rides
// context.Background() — must survive ctrl-C, never masks the verdict).
func MariaDefaultsCleanupCommand(defaults string) string {
	return "rm -f " + defaults
}

// MariaScratchPath mints the dump's scratch file — same crypto/rand
// discipline as the sqlite snapshot scratch, .sql-suffixed.
func MariaScratchPath() string {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return "/tmp/kampodra-dump-" + hex.EncodeToString(b[:]) + ".sql"
}

// ValidateMariaDbName gates the schema NAME before it rides a remote
// command: a single shell-safe token (no whitespace, no metacharacters —
// the name is composed into `mariadb-dump NAME > scratch`).
func ValidateMariaDbName(name string) error {
	if name == "" {
		return fmt.Errorf("mariadb leg: a database name is required (--db, or %s in the unit env)", MariaNameEnvKey)
	}
	if len(name) > 64 {
		return fmt.Errorf("mariadb leg: database name too long (%d chars, max 64)", len(name))
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '$':
		default:
			return fmt.Errorf("mariadb leg: database name must be a bare identifier (got %.20q — whitespace and shell metacharacters are rejected)", name)
		}
	}
	return nil
}

// MariaDumpCommand streams a consistent logical dump to the scratch file:
// --single-transaction gives InnoDB a point-in-time without locking,
// --quick streams row-by-row (no VM-side memory blowup), routines and
// triggers ride along. stderr passes through to the operator.
func MariaDumpCommand(defaults, dbName, scratch string) string {
	return "mariadb-dump --defaults-extra-file=" + defaults +
		" --single-transaction --quick --routines --triggers" +
		" " + dbName + " > " + scratch
}

// MariaVerifyDumpCommand checks the dump's tail — a complete mariadb-dump
// always ends with a "Dump completed" stamp; its absence means the dump
// died mid-stream (disk, conn) and the copy must NOT be trusted.
func MariaVerifyDumpCommand(scratch string) string {
	return "tail -c 200 " + scratch + " | grep -q 'Dump completed'"
}

// MariaQueryCommand runs the statement arriving on stdin — batch mode
// (tab-separated, header row), SQL never rides argv.
func MariaQueryCommand(defaults, dbName string) string {
	return "mariadb --defaults-extra-file=" + defaults + " --batch " + dbName
}

// MariaDbNameFromUnitEnvCommand prints the declared schema (the query/
// snapshot flows need a NAME, not a path — resolved once, named here).
func MariaDbNameFromUnitEnvCommand(envFile string) string {
	return "grep -E '^" + MariaNameEnvKey + "=' " + envFile + " 2>/dev/null | head -1 | cut -d= -f2-"
}
