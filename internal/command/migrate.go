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
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// migrateHelp is the shell migrate.sh usage heredoc, kampodra-fied for the
// over-SSH port.
const migrateHelp = `Usage:
  kampodra migrate [--allow-running] [--profile <name>] [--host <user@ip>] [--ssh-key <path>]
                   [--repo-root <path>] [--jobs <n>]

Tenant db migrations over SSH: apply per db file VIA THE REPO ON THE
VM (the app's own migrate script — the project's migrateScript config,
resolved like every project field; the same code path as local seeding,
no parallel implementation). Order: root.db first (the
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

// parseMigrateFlags validates the repo root (required — the VM checkout)
// and the bounded-parallel jobs count.
func parseMigrateFlags(d Deps, c *cobra.Command) (repoRoot string, jobs int, err error) {
	repoRoot = firstNonEmpty(flagString(c, "repo-root"), envValue(d.Env, "KAMPODRA_REPO_ROOT"))
	if repoRoot == "" {
		return "", 0, fmt.Errorf("the VM repo checkout is required for migrate: pass --repo-root <path> or set KAMPODRA_REPO_ROOT")
	}
	jobs = 4
	if v := firstNonEmpty(flagString(c, "jobs"), envValue(d.Env, "MIGRATE_JOBS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 64 {
			return "", 0, fmt.Errorf("--jobs must be a positive count of parallel migrations, 1-64 (got: %s)", v)
		}
		jobs = n
	}
	return repoRoot, jobs, nil
}

// migrateStopGuard enforces the stop-first contract: writing schema while
// the API serves risks SQLITE_BUSY on a single-writer engine. Detection
// order mirrors common.sh: rc-service (openrc) first, then systemctl
// (systemd); on a host with neither (local dev machines) state is
// unknowable — proceed with a loud warning rather than blocking.
func migrateStopGuard(ctx context.Context, d Deps, run func(string) (string, error), target Target, allowRunning bool) error {
	initSys, _, _ := initadapter.Detect(ctx, func(_ context.Context, remote string) (string, error) {
		return run(remote)
	}, target.ProfileInit)
	if initSys != initadapter.SystemOpenRC && initSys != initadapter.SystemSystemd {
		fmt.Fprintf(d.Stdout, "[migrate] service state unknown (neither rc-service nor systemctl found) — assuming %s is not running\n", target.Project.Container)
		return nil
	}
	states, err := initadapter.ServiceStates(ctx, func(_ context.Context, remote string) (string, error) {
		return run(remote)
	}, initSys, target.Project.Services)
	if err != nil {
		return nil
	}
	apiRunning := false
	for _, s := range states {
		if s.Name == target.Project.Container && s.Running {
			apiRunning = true
		}
	}
	if !apiRunning {
		return nil
	}
	if !allowRunning {
		stopCmd, _ := initadapter.ActionCommand(initSys, target.Project.Container, "stop")
		return fmt.Errorf("%s is running — stop it first (%s) or pass --allow-running", target.Project.Container, stopCmd)
	}
	fmt.Fprintf(d.Stdout, "WARNING: migrating while %s is running (--allow-running)\n", target.Project.Container)
	return nil
}

// checkMigrateProbe turns the one-round-trip probe verdict into the
// actionable errors: the shell's check order — repo root, pnpm, tenant
// dir.
func checkMigrateProbe(probe migrateadapter.Probe, target Target, repoRoot, migrateScript, dataDir string) error {
	if !probe.RepoOK {
		return fmt.Errorf("repo root not found at %s (migrateScript %q missing — configure migrateScript in kampodra.json, the profile project block, or KAMPODRA_MIGRATE_SCRIPT)", repoRoot, migrateScript)
	}
	if !probe.PnpmOK {
		return fmt.Errorf("pnpm not found in PATH on %s", target.HostSpec.Host)
	}
	if !probe.TenantDirOK {
		return fmt.Errorf("tenant dir %s does not exist on %s", dataDir, target.HostSpec.Host)
	}
	return nil
}

// migrateRunAll executes the migration plan: root.db (auth/org plane)
// FIRST, alone; then tenant files bounded-parallel (per-file write locks
// are independent — one writer per file by the store model), with the
// shared stdout writer mutex-guarded. Returns (migrated, failures).
func migrateRunAll(d Deps, run func(string) (string, error), repoRoot string, pj project.Config, rootJob *migrateadapter.Job, tenantJobs []migrateadapter.Job, jobs int) (int, int) {
	migrated, failures := 0, 0
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
		if _, err := run(migrateadapter.MigrateCommand(repoRoot, pj.MigrateScript, j)); err != nil {
			outMu.Lock()
			fmt.Fprintf(d.Stderr, "[migrate][FAIL] migration failed for %s\n", j.Path)
			outMu.Unlock()
			return false
		}
		return true
	}

	if rootJob != nil {
		if migrateOne(*rootJob) {
			migrated++
		} else {
			failures++
		}
	}
	tenantOK := runBounded(jobs, tenantJobs, migrateOne)
	migrated += tenantOK
	failures += len(tenantJobs) - tenantOK
	return migrated, failures
}

func runMigrate(d Deps, c *cobra.Command) error {
	allowRunning := flagBool(c, "allow-running")
	host, _ := c.Flags().GetString("host")
	key, _ := c.Flags().GetString("ssh-key")
	profile, _ := c.Flags().GetString("profile")
	repoRoot, jobs, err := parseMigrateFlags(d, c)
	if err != nil {
		return err
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

	if err := migrateStopGuard(ctx, d, run, target, allowRunning); err != nil {
		return err
	}

	// --- one-round-trip probe: preconditions + db listing ------------------
	probeOut, err := run(migrateadapter.ProbeCommand(repoRoot, pj.MigrateScript, pj.DataDir))
	if err != nil {
		return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to migrate", target.HostSpec.Host)
	}
	probe := migrateadapter.ParseProbe(probeOut)
	if err := checkMigrateProbe(probe, target, repoRoot, pj.MigrateScript, pj.DataDir); err != nil {
		return err
	}

	// --- plan: root first, tenants sorted ----------------------------------
	rootJob, tenantJobs := migrateadapter.Plan(probe.DbFiles)
	if rootJob == nil && len(tenantJobs) == 0 {
		fmt.Fprintf(d.Stdout, "[migrate] no .db files under %s; nothing to migrate\n", pj.DataDir)
		return nil
	}

	migrated, failures := migrateRunAll(d, run, repoRoot, pj, rootJob, tenantJobs, jobs)
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
