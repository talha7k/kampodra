// The Postgres leg: a server engine like MariaDB — same env-file contract,
// same staged-credentials discipline, same TSV output (normalized to the
// flat JSON shape), same dump-tail-stamp snapshot gate. pg_dump uses an
// MVCC snapshot natively (no flag needed) and plain-format dumps always
// end with a "PostgreSQL database dump complete" line.

package dbq

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
	return "psql --no-psqlrc --set=ON_ERROR_STOP=1 --set=PGPASSFILE=" + defaults +
		" --no-align --field-separator='\t' --dbname " + dbName
}

// PostgresDumpCommand streams a consistent logical dump to the scratch:
// plain format (restorable with psql, and its final line is the
// completeness stamp), MVCC-consistent by construction.
func PostgresDumpCommand(defaults, dbName, scratch string) string {
	return "pg_dump --set=PGPASSFILE=" + defaults +
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
	return "DO $$ BEGIN" +
		" IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = " + jsonString(role) + ") THEN" +
		" CREATE ROLE " + jsonString(role) + " LOGIN PASSWORD " + jsonString(password) + ";" +
		" ELSE ALTER ROLE " + jsonString(role) + " LOGIN PASSWORD " + jsonString(password) + ";" +
		" END IF; END $$;" +
		" GRANT CONNECT ON DATABASE " + jsonString(dbName) + " TO " + jsonString(role) + ";" +
		" GRANT USAGE ON SCHEMA public TO " + jsonString(role) + ";" +
		" GRANT SELECT ON ALL TABLES IN SCHEMA public TO " + jsonString(role) + ";" +
		" GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO " + jsonString(role) + ";" +
		" ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO " + jsonString(role) + ";"
}
