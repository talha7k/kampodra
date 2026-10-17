package dbq

import (
	"strings"
	"testing"
)

// The Postgres leg: same env-file contract as MariaDB, PGPASSFILE staged
// 0600 (never argv), TSV output, tail-stamped dumps.
func TestPostgresEngineSteps(t *testing.T) {
	eng := PostgresEngine(testEnvFile)
	if eng.Name != "postgres" || !strings.Contains(eng.ProbeCommand, "command -v psql") {
		t.Fatalf("engine identity malformed: %+v", eng)
	}
	if !strings.Contains(eng.InstallHint, "postgresql-client") {
		t.Errorf("install hint must name the client packages: %s", eng.InstallHint)
	}
	// the pgpass line: host:port:*:user:password with escapes, 0600
	stage := eng.CredsStage
	for _, want := range []string{
		"umask 077",
		"esc() { sed 's/\\\\/\\\\\\\\/g; s/:/\\\\:/g'; }",
		"${h:-localhost}", "${p:-5432}",
		"chmod 600 " + PostgresDefaultsPath(),
		testEnvFile,
	} {
		if !strings.Contains(stage, want) {
			t.Errorf("creds stage missing %q:\n%s", want, stage)
		}
	}
	if err := eng.Validate("esellar"); err != nil {
		t.Errorf("valid schema rejected: %v", err)
	}
	if q := eng.QueryCommand("esellar"); !strings.Contains(q, "PGPASSFILE="+PostgresDefaultsPath()) ||
		!strings.Contains(q, "ON_ERROR_STOP=1") || !strings.Contains(q, "--dbname esellar") {
		t.Errorf("query composition malformed:\n%s", q)
	}
	if eng.Normalize == nil {
		t.Errorf("postgres output must normalize (CSV → flat JSON)")
	}
}

func TestPostgresDumpComposition(t *testing.T) {
	dump := PostgresDumpCommand(PostgresDefaultsPath(), "esellar", "/tmp/scratch.sql")
	for _, want := range []string{"--no-owner", "--no-privileges", "--dbname esellar > /tmp/scratch.sql"} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump missing %q:\n%s", want, dump)
		}
	}
	if v := PostgresVerifyDumpCommand("/tmp/scratch.sql"); !strings.Contains(v, PgDumpCompleteStamp) {
		t.Errorf("verify must gate on the completeness stamp:\n%s", v)
	}
}

// The grant-ro SQL: create-or-rotate, SELECT only, backtick-safe names.
func TestPostgresGrantROSQL(t *testing.T) {
	sql := PostgresGrantROSQL("kampodra_ro", "pw123", "esellar")
	for _, want := range []string{
		"CREATE ROLE", "ALTER ROLE", "LOGIN PASSWORD", "GRANT CONNECT ON DATABASE",
		"GRANT SELECT ON ALL TABLES", "ALTER DEFAULT PRIVILEGES",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("grant SQL missing %q:\n%s", want, sql)
		}
	}
}
