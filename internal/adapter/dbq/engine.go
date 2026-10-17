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
		QueryCommand: func(target string) string {
			return MariaQueryCommand(defaults, target)
		},
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
		QueryCommand: func(target string) string {
			return QueryCommand(helperPath, target)
		},
	}
}
