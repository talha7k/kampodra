// The Postgres leg: a server engine like MariaDB — same env-file contract,
// same staged-credentials discipline, same TSV output (normalized to the
// flat JSON shape), same dump-tail-stamp snapshot gate. pg_dump uses an
// MVCC snapshot natively (no flag needed) and plain-format dumps always
// end with a "PostgreSQL database dump complete" line.

package dbq

import "strings"

// Env-file contract keys — the SAME KAMPODRA_DB_* family as MariaDB
// (ENGINE=postgres selects this leg).
const (
	PgDumpCompleteStamp = "PostgreSQL database dump complete"
)

// PostgresProbeCommand: client stack presence in ONE round-trip.
const PostgresProbeCommand = `if command -v psql >/dev/null 2>&1 && command -v pg_dump >/dev/null 2>&1; then echo yes; else echo no; fi`

// PostgresInstallHint names the client packages for the probe-fail error.
const PostgresInstallHint = "apk add postgresql-client / apt-get install postgresql-client"

// PostgresDefaultsPath is the on-VM PGPASSFILE — 0600, scratch-named.
func PostgresDefaultsPath() string {
	return "/tmp/kampodra-pgpass"
}

// PostgresDefaultsFromUnitEnvCommand builds the PGPASSFILE from the env
// file in one compound sh: the pgpass format is
// host:port:db:user:password with backslash and colon escaped as
// backslash sequences; `*` wildcards the db field (the -d flag names it
// at connect time). The password goes env-file → pgpass on the VM; it
// never transits argv or stdout.
func PostgresDefaultsFromUnitEnvCommand(envFile, defaults string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`esc() { sed 's/\\/\\\\/g; s/:/\\:/g'; }; ` +
		`umask 077; ` +
		`h=$(get ` + MariaHostEnvKey + ` | esc); p=$(get ` + MariaPortEnvKey + ` | esc); ` +
		`u=$(get ` + MariaUserEnvKey + ` | esc); w=$(get ` + MariaPasswordEnvKey + ` | esc); ` +
		`printf '%s:%s:*:%s:%s\n' "${h:-localhost}" "${p:-5432}" "${u:-$(id -un)}" "$w" > ` + defaults + ` && chmod 600 ` + defaults
}

// PostgresDefaultsCleanupCommand removes the pgpass file (Background).
func PostgresDefaultsCleanupCommand(defaults string) string {
	return "rm -f " + defaults
}

// PostgresQueryCommand runs the statement arriving on stdin: unaligned,
// tab-separated WITH header row (the MariaDB --batch contract), failing
// closed on any server error.
func PostgresQueryCommand(defaults, dbName string) string {
	// --csv: proper quoting (embedded newlines/tabs/commas survive — the
	// unaligned TSV form corrupts multi-line values); --set=null=NULL
	// makes NULLs visible (psql renders them empty otherwise — a real
	// trap the live smoke caught). PGPASSFILE rides as a psql variable.
	return "psql --no-psqlrc --set=ON_ERROR_STOP=1 --set=PGPASSFILE=" + defaults +
		" --set=null=NULL --csv --dbname " + dbName
}

// PostgresDumpCommand streams a consistent logical dump to the scratch:
// plain format (restorable with psql, and its final line is the
// completeness stamp), MVCC-consistent by construction.
func PostgresDumpCommand(defaults, dbName, scratch string) string {
	// PGPASSFILE is an ENV assignment — pg_dump has no --set flag (the
	// live smoke caught this: the invalid flag failed the dump silently
	// and the tail-stamp gate refused the empty file, exactly as designed).
	return "PGPASSFILE=" + defaults + " pg_dump" +
		" --no-owner --no-privileges" +
		" --dbname " + dbName + " > " + scratch
}

// PostgresVerifyDumpCommand checks the dump's tail stamp — an incomplete
// dump must never be fetched.
func PostgresVerifyDumpCommand(scratch string) string {
	return "tail -c 200 " + scratch + " | grep -q '" + PgDumpCompleteStamp + "'"
}

// PostgresGrantROSQL returns the idempotent read-only role SQL for
// `db grant-ro` (postgres dialect). The role gets LOGIN + a password and
// SELECT on the schema's tables/sequences plus default privileges for
// future tables.
func PostgresGrantROSQL(role, password, dbName string) string {
	// Postgres string LITERALS are single-quoted with '' escapes — the
	// live-smoke execution caught PASSWORD "…" (JSON-style double quotes)
	// as a syntax error. Identifiers stay double-quoted (jsonString).
	return "DO $$ BEGIN" +
		" IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = ' + pgString(role) + ') THEN" +
		" CREATE ROLE " + jsonString(role) + " LOGIN PASSWORD " + pgString(password) + ";" +
		" ELSE ALTER ROLE " + jsonString(role) + " LOGIN PASSWORD " + pgString(password) + ";" +
		" END IF; END $$;" +
		" GRANT CONNECT ON DATABASE " + jsonString(dbName) + " TO " + jsonString(role) + ";" +
		" GRANT USAGE ON SCHEMA public TO " + jsonString(role) + ";" +
		" GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + jsonString(role) + ";" +
		" GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO " + jsonString(role) + ";" +
		" ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO " + jsonString(role) + ";"
}

// pgString renders a Postgres string literal: single quotes with ”
// escapes (a password containing a quote must survive).
func pgString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// --- the podman container path -------------------------------------------
// Postgres deployed as a podman container: the client tools live IN the
// image — psql/pg_dump run via `podman exec`, authenticating against an
// in-container PGPASSFILE staged from the env file (the official image's
// exec user is `postgres` = DB superuser via local trust; the pgpass
// covers scram-configured estates).

// PostgresContainerResolveCommand answers ONLY the container name (the
// explicit pod mode's single question; empty = none running). The env
// file is the project's — injected.
func PostgresContainerResolveCommand(envFile string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`c=$(get ` + MariaContainerEnvKey + `); ` +
		`if [ -z "$c" ]; then c=$(podman ps --format '{{.Names}} {{.Image}}' 2>/dev/null | awk 'tolower($0) ~ /postgres|pgsql/ {print $1; exit}'); fi; ` +
		`echo "container $c"`
}

// PostgresModeProbeCommand: the three-state probe (auto prefers the
// running container — same reasoning as MariaDB's).
func PostgresModeProbeCommand(envFile string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`if command -v podman >/dev/null 2>&1; then ` +
		`c=$(get ` + MariaContainerEnvKey + `); ` +
		`if [ -z "$c" ]; then c=$(podman ps --format '{{.Names}} {{.Image}}' 2>/dev/null | awk 'tolower($0) ~ /postgres|pgsql/ {print $1; exit}'); fi; ` +
		`if [ -n "$c" ]; then echo "container $c"; exit 0; fi; ` +
		`fi; ` +
		`if command -v psql >/dev/null 2>&1 && command -v pg_dump >/dev/null 2>&1; then echo host; exit 0; fi; ` +
		`echo no`
}

// PostgresContainerCredsStageCommand stages the PGPASSFILE inside the
// container (0600; the password transits the pipe, never argv).
func PostgresContainerCredsStageCommand(envFile, container, defaults string) string {
	return `get() { grep -E "^$1=" ` + envFile + ` 2>/dev/null | head -1 | cut -d= -f2-; }; ` +
		`esc() { sed 's/\\/\\\\/g; s/:/\\:/g'; }; ` +
		`h=$(get ` + MariaHostEnvKey + ` | esc); p=$(get ` + MariaPortEnvKey + ` | esc); ` +
		`u=$(get ` + MariaUserEnvKey + ` | esc); w=$(get ` + MariaPasswordEnvKey + ` | esc); ` +
		`{ printf '%s:%s:*:%s:%s\n' "${h:-localhost}" "${p:-5432}" "${u:-postgres}" "$w"; } | ` +
		`podman exec -i ` + container + ` sh -c 'umask 077; cat > ` + defaults + ` && chmod 600 ` + defaults + `'`
}

// PostgresContainerCredsCleanupCommand removes the staged pgpass.
func PostgresContainerCredsCleanupCommand(container, defaults string) string {
	return "podman exec " + container + " rm -f " + defaults
}

// PostgresContainerQueryCommand / DumpCommand: the clients run in-container.
func PostgresContainerQueryCommand(container, defaults, dbName string) string {
	// --user postgres: the exec default is root (the official image's
	// container user varies) and pg_dump/psql derive the DB role from the
	// OS user — role "root" does not exist (live smoke finding).
	return "podman exec --user postgres -i " + container + " psql --no-psqlrc --set=ON_ERROR_STOP=1 --set=PGPASSFILE=" + defaults +
		" --set=null=NULL --csv --dbname " + dbName
}

func PostgresContainerDumpCommand(container, defaults, dbName, scratch string) string {
	return "podman exec --user postgres " + container + " sh -c 'PGPASSFILE=" + defaults + " pg_dump --no-owner --no-privileges --dbname " + dbName + "' > " + scratch
}
