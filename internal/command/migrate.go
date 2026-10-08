package command

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	migrateadapter "github.com/talha7k/kampodra/internal/adapter/migrate"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// migrateHelp is the shell migrate.sh usage heredoc, kampodra-fied for the
// over-SSH port.
const migrateHelp = `Usage:
  kampodra migrate [--allow-running] [--profile <name>] [--host <user@ip>] [--ssh-key <path>]
                   [--repo-root <path>] [--jobs <n>]

Tenant db migrations over SSH: drizzle apply per db file VIA THE REPO ON THE
VM (the app's own scripts/libsql-migrate/migrate-db.ts — the same code path
as local seeding, no parallel implementation). Order: root.db first (the
auth/org plane; root/root.db preferred, legacy flat root.db honored), then
tenant_*.db sorted, bounded-parallel (--jobs, default 4; MIGRATE_JOBS env).
Per-file failures are COLLECTED (files are independent — one bad tenant must
not block the others) and the command exits 1 if any failed — do NOT start
the API on a half-migrated estate.

Writing schema while the API serves risks SQLITE_BUSY on a single-writer
engine — the stop-first guard refuses to run while the api service is up
(init-aware: rc-service on OpenRC, systemctl on systemd). --allow-running
overrides for deliberate rolling contexts.

The VM repo checkout is resolved from --repo-root or KAMPODRA_REPO_ROOT —
fail-closed when neither names one.

Examples:
  kampodra migrate                                # guard on; --repo-root required
  kampodra migrate --repo-root /srv/app           # stop the API first
  kampodra migrate --allow-running                # deliberate: migrate while serving
  kampodra migrate --jobs 1                       # serial tenants
`

func newMigrateCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate [--allow-running] [--repo-root <path>] [--jobs <n>]",
		Short: "tenant db migrations over SSH: root.db first, then tenants bounded-parallel; stop-first guard",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runMigrate(d, c)
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), migrateHelp)
	})
	cmd.Flags().String("host", "", "target VM (user@ip or ssh-config alias)")
	cmd.Flags().String("profile", "", "per-instance profile (~/.kampodra/config.json)")
	cmd.Flags().String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY")
	cmd.Flags().Bool("allow-running", false, "migrate while the api service is running (SQLITE_BUSY risk)")
	cmd.Flags().String("repo-root", "", "the app repo checkout ON THE VM (beats KAMPODRA_REPO_ROOT)")
	cmd.Flags().String("jobs", "", "bounded-parallel tenant migrations (default MIGRATE_JOBS env, else 4)")
	return cmd
}

func runMigrate(d Deps, c *cobra.Command) error {
	allowRunning := flagBool(c, "allow-running")
	host, _ := c.Flags().GetString("host")
	key, _ := c.Flags().GetString("ssh-key")
	profile, _ := c.Flags().GetString("profile")
	repoRoot := firstNonEmpty(flagString(c, "repo-root"), envValue(d.Env, "KAMPODRA_REPO_ROOT"))
	jobsFlag, _ := c.Flags().GetString("jobs")

	if repoRoot == "" {
		return fmt.Errorf("the VM repo checkout is required for migrate: pass --repo-root <path> or set KAMPODRA_REPO_ROOT")
	}
	jobs := 4
	if v := firstNonEmpty(jobsFlag, envValue(d.Env, "MIGRATE_JOBS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return fmt.Errorf("--jobs must be a positive count of parallel migrations, 1-64 (got: %s)", v)
		}
		jobs = n
	}

	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	mf, err := d.manifestFor()
	if err != nil {
		return err
	}
	target, err := ResolveTarget(cfg, host, key, profile, mf, d.Env)
	if err != nil {
		return err
	}
	if err := requireHost(target); err != nil {
		return err
	}

	ctx := c.Context()
	pj := target.Project
	run := func(remote string) (string, error) {
		return d.Runner.Run(ctx, target.HostSpec, remote)
	}

	// --- stop-first guard (init-aware, via the ProjectConfig services) ----
	// Writing schema while the API serves risks SQLITE_BUSY on a
	// single-writer engine. Detection order mirrors common.sh: rc-service
	// (openrc) first, then systemctl (systemd); on a host with neither
	// (local dev machines) state is unknowable — proceed with a loud
	// warning rather than blocking.
	initSys, _, _ := initadapter.Detect(ctx, func(_ context.Context, remote string) (string, error) {
		return run(remote)
	}, target.ProfileInit)
	if initSys == initadapter.SystemOpenRC || initSys == initadapter.SystemSystemd {
		states, err := initadapter.ServiceStates(ctx, func(_ context.Context, remote string) (string, error) {
			return run(remote)
		}, initSys, pj.Services)
		if err == nil {
			apiRunning := false
			for _, s := range states {
				if s.Name == pj.Container && s.Running {
					apiRunning = true
				}
			}
			if apiRunning {
				if !allowRunning {
					stopCmd, _ := initadapter.ActionCommand(initSys, pj.Container, "stop")
					return fmt.Errorf("%s is running — stop it first (%s) or pass --allow-running", pj.Container, stopCmd)
				}
				fmt.Fprintf(d.Stdout, "WARNING: migrating while %s is running (--allow-running)\n", pj.Container)
			}
		}
	} else {
		fmt.Fprintf(d.Stdout, "[migrate] service state unknown (neither rc-service nor systemctl found) — assuming %s is not running\n", pj.Container)
	}

	// --- one-round-trip probe: preconditions + db listing ------------------
	probeOut, err := run(migrateadapter.ProbeCommand(repoRoot, pj.DataDir))
	if err != nil {
		return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to migrate", target.HostSpec.Host)
	}
	probe := migrateadapter.ParseProbe(probeOut)
	// The shell's check order: repo root, pnpm, tenant dir.
	if !probe.RepoOK {
		return fmt.Errorf("repo root not found at %s (checked for %s)", repoRoot, "apps/api/scripts/libsql-migrate/migrate-db.ts")
	}
	if !probe.PnpmOK {
		return fmt.Errorf("pnpm not found in PATH on %s", target.HostSpec.Host)
	}
	if !probe.TenantDirOK {
		return fmt.Errorf("tenant dir %s does not exist on %s", pj.DataDir, target.HostSpec.Host)
	}

	// --- plan: root first, tenants sorted ----------------------------------
	rootJob, tenantJobs := migrateadapter.Plan(probe.DbFiles)
	if rootJob == nil && len(tenantJobs) == 0 {
		fmt.Fprintf(d.Stdout, "[migrate] no .db files under %s; nothing to migrate\n", pj.DataDir)
		return nil
	}

	failures := 0
	migrated := 0
	// The tenant pool calls migrateOne concurrently — the shared writers
	// (bytes.Buffer in tests, line interleaving otherwise) need a mutex.
	var outMu sync.Mutex
	say := func(format string, args ...any) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Fprintf(d.Stdout, format, args...)
	}
	migrateOne := func(j migrateadapter.Job) bool {
		rel := strings.TrimPrefix(j.Path, pj.DataDir+"/")
		say("[migrate] migrating %s (ns: %s)\n", rel, j.NS)
		if _, err := run(migrateadapter.MigrateCommand(repoRoot, j)); err != nil {
			outMu.Lock()
			fmt.Fprintf(d.Stderr, "[migrate][FAIL] migration failed for %s\n", j.Path)
			outMu.Unlock()
			return false
		}
		return true
	}

	// root.db (auth/org plane) migrates FIRST, alone.
	if rootJob != nil {
		if migrateOne(*rootJob) {
			migrated++
		} else {
			failures++
		}
	}

	// then tenant files bounded-parallel: per-file write locks are
	// independent (one writer per file by the store model), so parallelism
	// is safe; the failure gate stays all-or-nothing either way.
	tenantOK := runBounded(jobs, tenantJobs, migrateOne)
	migrated += tenantOK
	failures += len(tenantJobs) - tenantOK

	if failures > 0 {
		fmt.Fprintf(d.Stdout, "[migrate] %d db file(s) failed to migrate (migrated: %d) — do NOT start the API on a half-migrated estate; fix and re-run\n",
			failures, migrated)
		return &exitError{code: 1}
	}
	fmt.Fprintf(d.Stdout, "[migrate] all db file(s) migrated cleanly (%d total)\n", migrated)
	return nil
}

// runBounded runs fn over items with at most n concurrent workers and
// returns the success count. The migration commands are independent ssh
// round-trips — ordering of START lines is scheduling order, the tally is
// deterministic.
func runBounded(n int, items []migrateadapter.Job, fn func(migrateadapter.Job) bool) int {
	if len(items) == 0 {
		return 0
	}
	if n <= 1 {
		ok := 0
		for _, it := range items {
			if fn(it) {
				ok++
			}
		}
		return ok
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(j migrateadapter.Job) {
			defer wg.Done()
			success := fn(j)
			mu.Lock()
			if success {
				ok++
			}
			mu.Unlock()
			<-sem
		}(it)
	}
	wg.Wait()
	return ok
}
