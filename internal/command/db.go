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
  kampodra db query    --db <path-on-vm> (--sql "<statement>" | --file <local.sql>)
                       [--profile <name>] [--host <user@ip>] [--ssh-key <path>]
  kampodra db snapshot --db <path-on-vm> [--out <local-path>]
                       [--profile <name>] [--host <user@ip>] [--ssh-key <path>]

Read-only sqlite access over SSH — the kampodra-owned surface for the
"hand-rolled read-only query binary over ssh" habit.

query runs ONE statement against the VM's sqlite file through a small
static-linux helper (kampodra-sqlq) that kampodra builds locally and
streams to the VM's /tmp on demand — cached at /tmp/kampodra-sqlq-*,
reused across runs; building needs the go toolchain once (the first
build may fetch the pure-Go sqlite driver). Read-only is enforced TWICE:
the gate rejects any statement whose first keyword is not SELECT or WITH
(exit 2, nothing is sent), and the helper opens the file with
mode=ro&immutable=0 so writes fail at the driver level. Rows print to
stdout as raw JSON ([{"col":val},...] — integers/reals as numbers, text
as strings, blobs base64, NULLs null); everything human rides stderr.

snapshot streams a consistent copy of the db to --out (default: the db's
basename in the current directory), written 0600. The VM's own sqlite3
does the copying (sqlite3 <db> ".backup <scratch>" — consistent under
concurrent writes); if sqlite3 is absent on the VM, snapshot falls back
to plain cp, which is NOT consistent against a writer — the fallback is
noted on stderr when it happens. An existing --out file is never
overwritten.

Host/key resolution is the shared ladder: --host | --profile <name> |
KAMPODRA_PROFILE | config defaultProfile | KAMPODRA_HOST; --ssh-key |
profile sshKey | KAMPODRA_SSH_KEY | agent / ssh config.

Examples:
  kampodra db query --db /data/root.db --sql "SELECT count(*) AS orgs FROM org"
  kampodra db query --db <dataDir>/acme/tenant_acme.db --sql "SELECT id, total FROM pos_order ORDER BY id DESC LIMIT 5"
  kampodra db query --db /data/root.db --file ./audit.sql --profile prod
  kampodra db snapshot --db <dataDir>/acme/tenant_acme.db --out ./acme-snapshot.db
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
	cmd.Flags().String("db", "", "the sqlite file's path ON THE VM (the root db or a tenant db under the project's dataDir)")
	cmd.Flags().String("sql", "", "the statement (exactly one of --sql/--file)")
	cmd.Flags().String("file", "", "a local .sql file holding the statement (exactly one of --sql/--file)")
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
	cmd.Flags().String("db", "", "the sqlite file's path ON THE VM (the root db or a tenant db under the project's dataDir)")
	cmd.Flags().String("out", "", "local destination (default: the db's basename in the current directory); never overwritten")
	return cmd
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

	// The gate FIRST — a rejected statement costs zero round-trips.
	if err := dbq.ValidateReadOnly(stmt); err != nil {
		fmt.Fprintf(d.Stderr, "[db] %v\n", err)
		return &exitError{code: 2}
	}
	if err := dbq.ValidateDbPath(dbPath); err != nil {
		return err
	}

	target, err := resolveTargetFromFlags(d, c)
	if err != nil {
		return err
	}
	// stdout IS the payload (the JSON rows) — provenance rides stderr.
	sayProvenance(Deps{Stdout: d.Stderr}, "db", target)
	if err := requireHost(target); err != nil {
		return err
	}

	ctx := c.Context()
	helper, err := ensureSQLHelper(d, ctx, target)
	if err != nil {
		return err
	}
	out, err := d.Runner.RunWithStdin(ctx, target.HostSpec, dbq.QueryCommand(helper, dbPath), strings.NewReader(stmt))
	if err != nil {
		return fmt.Errorf("query failed on %s: %w", target.HostSpec.Host, err)
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
	if err := dbq.ValidateDbPath(dbPath); err != nil {
		return err
	}
	if out == "" {
		out = filepath.Base(dbPath)
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("refusing to overwrite %s — remove it or pass --out <path>", out)
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
