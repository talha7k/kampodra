// db.go — kampodra's read-only sqlite surface over SSH. It exists because
// agents kept hand-rolling read-only query binaries and streaming them
// over raw ssh: the surface belongs HERE. `db query` pipes ONE SELECT/WITH
// statement through a small static-linux helper kampodra builds locally
// and streams to the VM's /tmp once (version-stamped cache); `db
// snapshot` streams a consistent copy back at 0600. stdout is DATA only
// (raw JSON rows / nothing); every human line rides stderr.

package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/dbq"
)

const dbHelp = `Usage:
  kampodra db query    --db <sqlite-path-on-vm | mariadb-schema> (--sql "<statement>" | --file <local.sql>)
                       [--engine auto|sqlite|mariadb] [--profile <name>] [--host <user@ip>] [--ssh-key <path>]
  kampodra db snapshot --db <sqlite-path-on-vm | mariadb-schema> [--out <local-path>]
                       [--engine auto|sqlite|mariadb] [--profile <name>] [--host <user@ip>] [--ssh-key <path>]

Read-only database access over SSH — the kampodra-owned surface for the
"hand-rolled read-only query binary over ssh" habit. Two engines:

sqlite (a FILE on the VM):
  query runs ONE statement through a small static-linux helper
  (kampodra-sqlq) kampodra builds locally and streams to the VM's /tmp
  once (version-stamped cache). Read-only is enforced TWICE: the gate
  rejects any statement not starting with SELECT/WITH (exit 2, nothing is
  sent), and the helper opens the file read-only at the driver level.
  Rows print to stdout as raw JSON. snapshot uses the VM's sqlite3
  ".backup" (consistent under concurrent writes; plain cp fallback noted
  on stderr when sqlite3 is absent). An existing --out is never
  overwritten.

mariadb (a SCHEMA on a server — same VM or a db host reachable from it):
  query runs ONE gate-checked statement through the VM's mariadb client
  (TSV on stdout — batch mode, header row); snapshot streams
  mariadb-dump --single-transaction --quick (routines+triggers) and
  verifies the "Dump completed" tail stamp before trusting the copy.
  Credentials come from the VM's unit env at run time — kampodra never
  stores them: set KAMPODRA_DB_NAME/HOST/USER/PASSWORD via
  ` + "`kampodra env set`" + `. They are copied into a 0600 defaults file on
  the VM for the duration of the command and never ride argv (no ` + "`ps`" + `
  leak). Same-VM topologies authenticate over the local socket (empty
  HOST/USER = unix_socket); a remote db host reachable FROM the app VM
  works with HOST set — kampodra only ever ssh's to the app VM.
  Provision a read-only role for agents:
    CREATE USER 'kampodra_ro'@'localhost' IDENTIFIED BY '…';
    GRANT SELECT ON <db>.* TO 'kampodra_ro'@'localhost';
  and point KAMPODRA_DB_USER at it — the gate is belt, the role is the
  suspender.

libsql (self-hosted sqld serving the db over HTTP on the same VM — no
cloud):
  query rides Hrana over the VM's curl: the request body is built
  Go-side (the statement's JSON escaping is the stdlib's, not a shell
  exercise) and the auth token is staged into a 0600 header file from
  the env file — never argv. Rows come back as the SAME flat JSON shape
  as sqlite. Set KAMPODRA_DB_URL (default http://127.0.0.1:8080),
  KAMPODRA_DB_TOKEN (empty = unauthenticated localhost), and — for
  snapshot — KAMPODRA_DB_PATH (the sqld --db-path on the VM): the copy
  reuses the consistent .backup flow and is gated on PRAGMA
  integrity_check before fetch. --db is unused (sqld serves one db).

Engine resolution (--engine auto, the default): the flag wins; then the
VM's unit env (KAMPODRA_DB_ENGINE=mariadb|libsql); then the file's magic
bytes (sqlite). --db is a PATH for sqlite, a schema NAME for mariadb,
and unused for libsql.

Host/key resolution is the shared ladder: --host | --profile <name> |
KAMPODRA_PROFILE | config defaultProfile | KAMPODRA_HOST; --ssh-key |
profile sshKey | KAMPODRA_SSH_KEY | agent / ssh config.

Examples:
  kampodra db query --db /data/root.db --sql "SELECT count(*) AS orgs FROM org"
  kampodra db query --db <dataDir>/acme/tenant_acme.db --file ./audit.sql --profile prod
  kampodra db snapshot --db <dataDir>/acme/tenant_acme.db --out ./acme-snapshot.db
  kampodra env set KAMPODRA_DB_ENGINE=mariadb KAMPODRA_DB_NAME=esellar KAMPODRA_DB_USER=kampodra_ro
  kampodra db query --db esellar --sql "SELECT id, total FROM pos_order ORDER BY id DESC LIMIT 5"
  kampodra db snapshot --db esellar --out ./esellar.sql
`

func newDBCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "read-only sqlite over SSH: query (SELECT/WITH → JSON rows) | snapshot (consistent copy, 0600)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), dbHelp)
			return nil
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), dbHelp)
	})
	cmd.AddCommand(newDBQuerySub(d), newDBSnapshotSub(d))
	return cmd
}

func newDBQuerySub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query --db <path-on-vm> (--sql \"<statement>\" | --file <local.sql>)",
		Short: "ONE read-only SELECT/WITH statement → raw JSON rows on stdout (exit 2 = gate rejection)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDBQuery(d, c)
		},
	}
	remoteTargetFlags(cmd)
	cmd.Flags().String("db", "", "the sqlite file's path ON THE VM, or the mariadb schema NAME")
	cmd.Flags().String("sql", "", "the statement (exactly one of --sql/--file)")
	cmd.Flags().String("file", "", "a local .sql file holding the statement (exactly one of --sql/--file)")
	cmd.Flags().String("engine", "auto", "sqlite | mariadb | auto (flag > unit env KAMPODRA_DB_ENGINE > file magic)")
	return cmd
}

func newDBSnapshotSub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot --db <path-on-vm> [--out <local-path>]",
		Short: "stream a consistent copy of the db back (0600); cp fallback when sqlite3 is absent",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDBSnapshot(d, c)
		},
	}
	remoteTargetFlags(cmd)
	cmd.Flags().String("db", "", "the sqlite file's path ON THE VM, or the mariadb schema NAME")
	cmd.Flags().String("out", "", "local destination (default: the db's basename in the current directory); never overwritten")
	cmd.Flags().String("engine", "auto", "sqlite | mariadb | auto (flag > unit env KAMPODRA_DB_ENGINE > file magic)")
	return cmd
}

// dbEngine is the resolved engine for one db invocation.
type dbEngine string

const (
	engineSQLite dbEngine = "sqlite"
	engineMaria  dbEngine = "mariadb"
	engineLibsql dbEngine = "libsql"
)

// validateDbArgByShape is the pre-ssh sanity gate on --db: path-shaped
// args (contain a separator) obey the sqlite path gate; bare tokens obey
// the maria identifier gate. The resolved engine's own validator runs
// again inside its branch — this pass exists so bad input costs zero
// round-trips even when --engine auto can't be resolved yet.
func validateDbArgByShape(dbArg string) error {
	if strings.ContainsRune(dbArg, '/') {
		return dbq.ValidateDbPath(dbArg)
	}
	return dbq.ValidateMariaDbName(dbArg)
}

// resolveDBEngine runs the auto ladder: explicit flag wins; then the VM's
// unit env declaration (KAMPODRA_DB_ENGINE=mariadb — one round-trip);
// then the file's magic bytes (one round-trip). Anything still ambiguous
// is an error NAMING the ladder — never a guess.
func resolveDBEngine(d Deps, ctx context.Context, target Target, engineFlag, dbArg string) (dbEngine, error) {
	switch engineFlag {
	case "sqlite":
		return engineSQLite, nil
	case "mariadb":
		return engineMaria, nil
	case "auto", "":
		// fall through to the probes
	default:
		return "", fmt.Errorf("unknown --engine %q (sqlite | mariadb | auto)", engineFlag)
	}
	run := func(remote string) string {
		out, err := d.Runner.Run(ctx, target.HostSpec, remote)
		if err != nil {
			return "" // probes answer by CONTENT; a failing probe is just "no"
		}
		return strings.TrimSpace(out)
	}
	switch strings.TrimSpace(run(dbq.EngineUnitEnvProbeCommand(target.Project.EnvFile))) {
	case "mariadb":
		return engineMaria, nil
	case "libsql":
		return engineLibsql, nil
	}
	if dbArg != "" && run(dbq.EngineMagicProbeCommand(dbArg)) == "sqlite" {
		return engineSQLite, nil
	}
	return "", fmt.Errorf(
		"cannot infer the engine: no KAMPODRA_DB_ENGINE=mariadb in the VM's unit env and %q has no sqlite magic — pass --engine sqlite|mariadb",
		dbArg)
}

func runDBQuery(d Deps, c *cobra.Command) error {
	dbPath, _ := c.Flags().GetString("db")
	sqlFlag, _ := c.Flags().GetString("sql")
	fileFlag, _ := c.Flags().GetString("file")

	var stmt string
	switch {
	case sqlFlag != "" && fileFlag != "":
		return fmt.Errorf("query takes ONE statement: --sql or --file, never both")
	case fileFlag != "":
		data, err := os.ReadFile(fileFlag)
		if err != nil {
			return fmt.Errorf("cannot read --file %s: %w", fileFlag, err)
		}
		stmt = string(data)
	case sqlFlag != "":
		stmt = sqlFlag
	default:
		return fmt.Errorf("query needs a statement: --sql \"<statement>\" or --file <local.sql>")
	}

	// The gate FIRST — a rejected statement costs zero round-trips, on
	// both engines (the classification is engine-portable: SELECT/WITH).
	if err := dbq.ValidateReadOnly(stmt); err != nil {
		fmt.Fprintf(d.Stderr, "[db] %v\n", err)
		return &exitError{code: 2}
	}
	// Local input sanity BEFORE any ssh — the engine isn't resolved yet,
	// so validate by SHAPE: a path-shaped arg obeys the sqlite path gate,
	// a bare token obeys the maria name gate (the resolved engine's own
	// validator runs again in its branch — cheap, and the contract "bad
	// input costs zero round-trips" holds regardless of the flag).
	if dbPath != "" {
		if err := validateDbArgByShape(dbPath); err != nil {
			return err
		}
	}

	target, err := resolveTargetFromFlags(d, c)
	if err != nil {
		return err
	}
	// stdout IS the payload (the JSON rows / TSV) — provenance rides stderr.
	sayProvenance(Deps{Stdout: d.Stderr}, "db", target)
	if err := requireHost(target); err != nil {
		return err
	}

	ctx := c.Context()
	engineFlag, _ := c.Flags().GetString("engine")
	engine, err := resolveDBEngine(d, ctx, target, engineFlag, dbPath)
	if err != nil {
		return err
	}

	switch engine {
	case engineSQLite:
		if err := dbq.ValidateDbPath(dbPath); err != nil {
			return err
		}
		helper, err := ensureSQLHelper(d, ctx, target)
		if err != nil {
			return err
		}
		return runEngineQuery(d, ctx, target, dbq.SQLiteEngine(helper), engineSQLite, dbPath, stmt)
	case engineMaria:
		return runEngineQuery(d, ctx, target, dbq.MariaEngine(target.Project.EnvFile), engineMaria, dbPath, stmt)
	case engineLibsql:
		return runEngineQuery(d, ctx, target, dbq.LibsqlEngine(target.Project.EnvFile), engineLibsql, dbPath, stmt)
	}
	return fmt.Errorf("unreachable engine %q", engine)
}

// runEngineQuery is the shared query flow — identical shape for every
// engine; the Engine struct supplies the steps (probe? creds stage? —
// then the gated statement rides stdin). Output is the engine's data
// payload (JSON rows / TSV); human lines ride stderr.
func runEngineQuery(d Deps, ctx context.Context, target Target, eng dbq.Engine, engine dbEngine, dbTarget, stmt string) error {
	if err := eng.Validate(dbTarget); err != nil {
		return err
	}
	if eng.ProbeCommand != "" {
		probe, err := d.Runner.Run(ctx, target.HostSpec, eng.ProbeCommand)
		if err != nil {
			return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to query", target.HostSpec.Host)
		}
		if strings.TrimSpace(probe) != "yes" {
			return fmt.Errorf("the %s client stack is missing on %s — install it (%s) or run kampodra vm-prepare with KAMPODRA_DB_ENGINE set in the unit env", eng.Name, target.HostSpec.Host, eng.InstallHint)
		}
	}
	if eng.CredsStage != "" {
		defer func() {
			if _, err := d.Runner.Run(context.Background(), target.HostSpec, eng.CredsCleanup); err != nil {
				fmt.Fprintf(d.Stderr, "[db] WARNING: creds cleanup failed on %s: %v\n", target.HostSpec.Host, err)
			}
		}()
		if _, err := d.Runner.RunWithStdin(ctx, target.HostSpec, eng.CredsStage, strings.NewReader("")); err != nil {
			return fmt.Errorf("cannot stage %s credentials on %s (is %s present? set the KAMPODRA_DB_* keys via kampodra env set): %w", eng.Name, target.HostSpec.Host, target.Project.EnvFile, err)
		}
	}
	payload := stmt
	if eng.EncodeStatement != nil {
		payload = eng.EncodeStatement(stmt)
	}
	out, err := d.Runner.RunWithStdin(ctx, target.HostSpec, eng.QueryCommand(dbTarget), strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("query failed on %s (target %q): %w", target.HostSpec.Host, dbTarget, err)
	}
	if engine == engineLibsql {
		flat, nerr := dbq.NormalizeHranaResponse(out)
		if nerr != nil {
			return fmt.Errorf("query failed on %s: %w", target.HostSpec.Host, nerr)
		}
		out = flat + "\n"
	}
	fmt.Fprint(d.Stdout, out)
	return nil
}

// ensureSQLHelper returns the helper's on-VM path: present → reuse (the
// version-stamped name is the cache key); missing → build locally (go
// toolchain, linux/arm64, CGO off — the estate's cross-build shape) and
// stream it with 0755 perms.
func ensureSQLHelper(d Deps, ctx context.Context, target Target) (string, error) {
	remote := dbq.HelperRemotePath(dbqSQLArch)
	if _, err := d.Runner.Run(ctx, target.HostSpec, dbq.HelperCheckCommand(remote)); err == nil {
		fmt.Fprintf(d.Stderr, "[db] sqlq helper already on the VM (%s)\n", remote)
		return remote, nil
	}
	local, err := dbq.BuildLinuxHelper(ctx, d.CacheDir, d.Stderr)
	if err != nil {
		return "", err
	}
	f, err := os.Open(local)
	if err != nil {
		return "", fmt.Errorf("cannot read the built helper: %w", err)
	}
	defer f.Close()
	fmt.Fprintf(d.Stderr, "[db] streaming sqlq helper → %s:%s\n", target.HostSpec.Host, remote)
	if _, err := d.Runner.RunWithStdin(ctx, target.HostSpec, dbq.HelperInstallCommand(remote), f); err != nil {
		return "", fmt.Errorf("helper upload to %s failed: %w", target.HostSpec.Host, err)
	}
	fmt.Fprintf(d.Stderr, "[db] sqlq helper installed (cached on the VM for future runs)\n")
	return remote, nil
}

func runDBSnapshot(d Deps, c *cobra.Command) error {
	dbPath, _ := c.Flags().GetString("db")
	out, _ := c.Flags().GetString("out")
	// Local input sanity BEFORE any ssh (same shape gate as query — and
	// same empty-skip: libsql needs no --db at all), and an explicit --out
	// that exists is refused before any round-trip.
	if dbPath != "" {
		if err := validateDbArgByShape(dbPath); err != nil {
			return err
		}
	}
	if out != "" {
		if _, err := os.Stat(out); err == nil {
			return fmt.Errorf("refusing to overwrite %s — remove it or pass --out <path>", out)
		}
	}

	target, err := resolveTargetFromFlags(d, c)
	if err != nil {
		return err
	}
	sayProvenance(Deps{Stdout: d.Stderr}, "db", target)
	if err := requireHost(target); err != nil {
		return err
	}

	ctx := c.Context()
	engineFlag, _ := c.Flags().GetString("engine")
	engine, err := resolveDBEngine(d, ctx, target, engineFlag, dbPath)
	if err != nil {
		return err
	}
	if engine == engineMaria {
		return runMariaSnapshot(d, ctx, target, dbq.MariaEngine(target.Project.EnvFile), dbPath, out)
	}
	if engine == engineLibsql {
		return runLibsqlSnapshot(d, ctx, target, target.Project.EnvFile, out)
	}

	if err := dbq.ValidateDbPath(dbPath); err != nil {
		return err
	}
	if out == "" {
		out = filepath.Base(dbPath)
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("refusing to overwrite %s — remove it or pass --out <path>", out)
	}
	run := func(remote string) (string, error) {
		return d.Runner.Run(ctx, target.HostSpec, remote)
	}

	probe, err := run(dbq.Sqlite3ProbeCommand)
	if err != nil {
		return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to snapshot", target.HostSpec.Host)
	}
	scratch := dbq.SnapshotScratchPath()
	// Cleanup rides context.Background() — it must survive ctrl-C — and
	// never masks the verdict (the verify-run discipline).
	defer func() {
		if _, err := d.Runner.Run(context.Background(), target.HostSpec, dbq.SnapshotCleanupCommand(scratch)); err != nil {
			fmt.Fprintf(d.Stderr, "[db] WARNING: scratch cleanup failed (%s): %v\n", scratch, err)
		}
	}()

	if strings.TrimSpace(probe) == "yes" {
		fmt.Fprintf(d.Stderr, "[db] consistent copy via sqlite3 .backup → %s:%s\n", target.HostSpec.Host, scratch)
		if _, err := run(dbq.SnapshotBackupCommand(dbPath, scratch)); err != nil {
			return fmt.Errorf("sqlite3 .backup failed for %s: %w", dbPath, err)
		}
	} else {
		fmt.Fprintf(d.Stderr, "[db] sqlite3 not found on %s — falling back to plain cp, which is NOT consistent against concurrent writes (install sqlite3 on the VM for safe snapshots)\n", target.HostSpec.Host)
		if _, err := run(dbq.SnapshotCpCommand(dbPath, scratch)); err != nil {
			return fmt.Errorf("cp fallback failed for %s: %w", dbPath, err)
		}
	}

	data, err := run(dbq.SnapshotFetchCommand(scratch))
	if err != nil {
		return fmt.Errorf("cannot fetch the snapshot from %s: %w", target.HostSpec.Host, err)
	}
	if err := writeFile0600(out, []byte(data)); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[db] wrote %s (0600) — %d bytes\n", out, len(data))
	return nil
}

// dbqSQLArch mirrors the helper's cross-build arch (dbq's build target) —
// the on-VM cache name must agree with what was built.
const dbqSQLArch = dbq.SQLQGOARCH

// runMariaSnapshot: probe client → defaults from the unit env → single-
// transaction dump to a random scratch → PROVE the "Dump completed" tail
// stamp (an incomplete dump is a failure, not a warning) → fetch → 0600
// → cleanup of BOTH scratch and defaults, riding Background so ctrl-C
// never strands credentials or half-dumps on the VM.
func runMariaSnapshot(d Deps, ctx context.Context, target Target, eng dbq.Engine, dbName, out string) error {
	if err := eng.Validate(dbName); err != nil {
		return err
	}
	if out == "" {
		out = dbName + ".sql"
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("refusing to overwrite %s — remove it or pass --out <path>", out)
	}
	probe, err := d.Runner.Run(ctx, target.HostSpec, dbq.MariaProbeCommand)
	if err != nil {
		return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to snapshot", target.HostSpec.Host)
	}
	if strings.TrimSpace(probe) != "yes" {
		return fmt.Errorf("mariadb client stack missing on %s — install it (alpine: apk add mariadb-client; ubuntu: apt-get install mariadb-client) or run kampodra vm-prepare with KAMPODRA_DB_ENGINE=mariadb in the unit env", target.HostSpec.Host)
	}
	defaults := dbq.MariaDefaultsPath()
	scratch := dbq.MariaScratchPath()
	defer func() {
		for _, remote := range []string{eng.CredsCleanup, dbq.SnapshotCleanupCommand(scratch)} {
			if _, err := d.Runner.Run(context.Background(), target.HostSpec, remote); err != nil {
				fmt.Fprintf(d.Stderr, "[db] WARNING: cleanup failed on %s: %v\n", target.HostSpec.Host, err)
			}
		}
	}()
	if _, err := d.Runner.RunWithStdin(ctx, target.HostSpec, eng.CredsStage, strings.NewReader("")); err != nil {
		return fmt.Errorf("cannot stage %s credentials on %s (is %s present? set the KAMPODRA_DB_* keys via kampodra env set): %w", eng.Name, target.HostSpec.Host, target.Project.EnvFile, err)
	}
	fmt.Fprintf(d.Stderr, "[db] consistent dump via mariadb-dump --single-transaction → %s:%s\n", target.HostSpec.Host, scratch)
	if _, err := d.Runner.Run(ctx, target.HostSpec, dbq.MariaDumpCommand(defaults, dbName, scratch)); err != nil {
		return fmt.Errorf("mariadb-dump failed for %q on %s: %w", dbName, target.HostSpec.Host, err)
	}
	if verify, err := d.Runner.Run(ctx, target.HostSpec, dbq.MariaVerifyDumpCommand(scratch)); err != nil || strings.TrimSpace(verify) == "" {
		return fmt.Errorf("the dump of %q on %s is INCOMPLETE (no 'Dump completed' tail stamp) — refusing to fetch a partial copy", dbName, target.HostSpec.Host)
	}
	data, err := d.Runner.Run(ctx, target.HostSpec, dbq.SnapshotFetchCommand(scratch))
	if err != nil {
		return fmt.Errorf("cannot fetch the dump from %s: %w", target.HostSpec.Host, err)
	}
	if err := writeFile0600(out, []byte(data)); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[db] wrote %s (0600) — %d bytes\n", out, len(data))
	return nil
}

// runLibsqlSnapshot: the sqld db is (still) a FILE on the VM — the copy
// reuses the sqlite snapshot machinery (consistent .backup, cp fallback),
// then gates on PRAGMA integrity_check — a corrupted or torn copy is
// refused before fetch. The db path comes from the env file
// (KAMPODRA_DB_PATH): query needed a URL, snapshot needs the file.
func runLibsqlSnapshot(d Deps, ctx context.Context, target Target, envFile, out string) error {
	pathOut, err := d.Runner.Run(ctx, target.HostSpec, dbq.LibsqlDbPathFromEnvCommand(envFile))
	if err != nil {
		return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to snapshot", target.HostSpec.Host)
	}
	dbPath := strings.TrimSpace(pathOut)
	if dbPath == "" {
		return fmt.Errorf("snapshot needs %s (the sqld --db-path on the VM) in the env file — set it via kampodra env set; a Hrana URL alone cannot be snapshotted", dbq.LibsqlPathEnvKey)
	}
	if err := dbq.ValidateDbPath(dbPath); err != nil {
		return err
	}
	if out == "" {
		out = filepath.Base(dbPath)
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("refusing to overwrite %s — remove it or pass --out <path>", out)
	}
	run := func(remote string) (string, error) {
		return d.Runner.Run(ctx, target.HostSpec, remote)
	}
	probe, err := run(dbq.Sqlite3ProbeCommand)
	if err != nil {
		return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to snapshot", target.HostSpec.Host)
	}
	scratch := dbq.LibsqlScratchPath()
	defer func() {
		if _, err := d.Runner.Run(context.Background(), target.HostSpec, dbq.SnapshotCleanupCommand(scratch)); err != nil {
			fmt.Fprintf(d.Stderr, "[db] WARNING: scratch cleanup failed (%s): %v\n", scratch, err)
		}
	}()
	if strings.TrimSpace(probe) == "yes" {
		fmt.Fprintf(d.Stderr, "[db] consistent copy via sqlite3 .backup → %s:%s\n", target.HostSpec.Host, scratch)
		if _, err := run(dbq.SnapshotBackupCommand(dbPath, scratch)); err != nil {
			return fmt.Errorf("sqlite3 .backup failed for %s: %w", dbPath, err)
		}
		if ic, err := run(dbq.LibsqlIntegrityCheckCommand(scratch)); err != nil || strings.TrimSpace(ic) != "ok" {
			return fmt.Errorf("the snapshot of %s failed PRAGMA integrity_check — refusing to fetch a corrupt copy", dbPath)
		}
	} else {
		fmt.Fprintf(d.Stderr, "[db] sqlite3 not found on %s — falling back to plain cp, which is NOT consistent against concurrent writes (install sqlite3 on the VM for safe snapshots)\n", target.HostSpec.Host)
		if _, err := run(dbq.SnapshotCpCommand(dbPath, scratch)); err != nil {
			return fmt.Errorf("cp fallback failed for %s: %w", dbPath, err)
		}
	}
	data, err := run(dbq.SnapshotFetchCommand(scratch))
	if err != nil {
		return fmt.Errorf("cannot fetch the snapshot from %s: %w", target.HostSpec.Host, err)
	}
	if err := writeFile0600(out, []byte(data)); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[db] wrote %s (0600) — %d bytes\n", out, len(data))
	return nil
}
