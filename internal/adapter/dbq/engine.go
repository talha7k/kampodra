// The engine seam: db.go's shared flows (query/snapshot) drive a leg
// through this struct instead of knowing its dialect. A leg is DATA here —
// command strings composed by its constructor — while ALL execution stays
// in the command layer (dbq remains a pure composer: no Runner, no cobra).
// Steps a leg lacks are empty strings; nothing is optional by interface
// trickery, only by honest absence.
//
// Adding an engine (e.g. Postgres) = one constructor beside the two below
// + one snapshot branch in db.go (snapshot branches stay explicit because
// they genuinely differ: sqlite's cp-fallback vs maria's tail-stamp gate).
// The gate, the auto ladder, the scratch/cleanup discipline, and the
// pre-ssh input gates are engine-neutral and live in db.go.

package dbq

// Engine carries a leg's steps into db.go's flows.
type Engine struct {
	// Name is the --engine spelling ("sqlite" | "mariadb").
	Name string

	// ProbeCommand is the one-round-trip yes/no client check. Empty = the
	// engine's provisioning is flow-specific (sqlite's helper is built and
	// streamed by db.go's ensureSQLHelper, not probed).
	ProbeCommand string

	// CredsStage runs via RunWithStdin before the statement ("" = the
	// engine needs no credential staging — sqlite opens a file). MariaDB
	// composes its 0600 defaults file here, from the unit env.
	CredsStage string

	// CredsCleanup removes the staged credentials — it MUST ride
	// context.Background() in the caller (survives ctrl-C; never masks the
	// verdict). Empty = none.
	CredsCleanup string

	// Validate gates the --db argument (sqlite: a path; maria: an
	// identifier). db.go's shape gate runs pre-ssh for BOTH; this is the
	// engine's own final word.
	Validate func(target string) error

	// QueryCommand composes the remote command whose stdin is the gated
	// statement. For sqlite, db.go provisions the helper FIRST and passes
	// its path to the constructor, so this closure is total.
	QueryCommand func(target string) string

	// InstallHint names the client packages the probe-failure error
	// suggests (engine-specific, actionable).
	InstallHint string

	// EncodeStatement transforms the gated statement into the command's
	// stdin payload. nil = the statement rides verbatim (sqlite, maria,
	// postgres); libsql wraps it into the Hrana pipeline body Go-side
	// (perfect JSON escaping, no VM-side quoting).
	EncodeStatement func(stmt string) string

	// Normalize flattens the engine's wire output to the flat JSON row
	// shape (maria: escaped TSV; postgres: RFC4180 CSV; libsql: the
	// Hrana envelope). nil = the output is already flat JSON (sqlite).
	Normalize func(raw string) (string, error)
}

// PostgresEngine is the Postgres leg: same env-file contract as MariaDB
// (KAMPODRA_DB_*), PGPASSFILE staged 0600 (never argv), psql query in the
// shared TSV contract, pg_dump with the completeness tail stamp.
func PostgresEngine(envFile string) Engine {
	defaults := PostgresDefaultsPath()
	return Engine{
		Name:         "postgres",
		ProbeCommand: PostgresProbeCommand,
		InstallHint:  PostgresInstallHint,
		CredsStage:   PostgresDefaultsFromUnitEnvCommand(envFile, defaults),
		CredsCleanup: PostgresDefaultsCleanupCommand(defaults),
		Validate:     ValidateMariaDbName, // same bare-identifier gate
		QueryCommand: func(target string) string {
			return PostgresQueryCommand(defaults, target)
		},
		Normalize: NormalizeTSV,
	}
}

// MariaEngine is the MariaDB leg: server engine, credentials read from
// the VM's env file at run time into a 0600 defaults file (never argv),
// query and dump through the VM's client stack. The env file's path is
// the project's (profile-configurable) — the caller injects it.
func MariaEngine(envFile string) Engine {
	defaults := MariaDefaultsPath()
	return Engine{
		Name:         "mariadb",
		ProbeCommand: MariaProbeCommand,
		CredsStage:   MariaDefaultsFromUnitEnvCommand(envFile, defaults),
		CredsCleanup: MariaDefaultsCleanupCommand(defaults),
		Validate:     ValidateMariaDbName,
		InstallHint:  "apk add mariadb-client / apt-get install mariadb-client",
		QueryCommand: func(target string) string {
			return MariaQueryCommand(defaults, target)
		},
	}
}

// LibsqlEngine is the self-hosted libsql leg: sqld serving the db over
// Hrana (HTTP) on the same VM. Query rides curl with the token staged to
// a 0600 header file (never argv); --db is unused (sqld serves one db —
// the URL and token come from the env file). Snapshots treat the db as
// the file it is (db.go branches on the engine for the snapshot flow).
func LibsqlEngine(envFile string) Engine {
	stage := LibsqlAuthHeaderStage
	return Engine{
		Name:            EngineLibsql,
		ProbeCommand:    LibsqlProbeCommand,
		InstallHint:     "apk add curl / apt-get install curl",
		CredsStage:      LibsqlAuthStageCommand(envFile, stage),
		Normalize:       NormalizeHranaResponse,
		CredsCleanup:    LibsqlAuthCleanupCommand(stage),
		Validate:        func(string) error { return nil }, // sqld serves one db; --db is unused
		QueryCommand:    func(string) string { return LibsqlQueryCommand(stage) },
		EncodeStatement: HranaPipelineBody,
	}
}

// SQLiteEngine is the sqlite leg: a file on the VM, no credentials — the
// read-only enforcement is the gate plus the helper's ro open mode. The
// helper path arrives provisioned (EnsureHelper in db.go).
func SQLiteEngine(helperPath string) Engine {
	return Engine{
		Name:         "sqlite",
		ProbeCommand: "", // provisioning is flow-specific: build + stream the helper
		Validate:     ValidateDbPath,
		InstallHint:  "the sqlq helper is built and streamed by kampodra (no client packages)",
		QueryCommand: func(target string) string {
			return QueryCommand(helperPath, target)
		},
	}
}
