package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// The migrate command: tenant db migrations over SSH — root.db first, then
// tenant_*.db bounded-parallel, per-file failures collected → exit 1; the
// init-aware stop-first guard with --allow-running. Every remote step is
// answered by one dispatching ssh shim (detect / service roll call / probe
// / per-file migrate).

func setupMigrate(t *testing.T, probeFixture string) (command.Deps, *bytes.Buffer, *bytes.Buffer, string, func() []string) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".kampodra")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(
		`{"defaultProfile": "prod", "profiles": {"prod": {"host": "root@203.0.113.9", "sshKey": "/ops/id"}}}`,
	), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := t.TempDir()
	inv := filepath.Join(stub, "invocations")
	for name, content := range map[string]string{
		"probe":  probeFixture,
		"svcs":   "kampodine-api: not-running\nkamal-proxy: not-running\nwalshipper: not-running\n",
		"failon": "",
		"detect": "openrc\nhttpc=wget\n",
	} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"cmd=\"$*\"\n" +
		"failon=\"$(cat '" + filepath.Join(stub, "failon") + "' 2>/dev/null)\"\n" +
		"if [ -n \"$failon\" ] && [[ \"$cmd\" == *\"$failon\"* ]]; then exit 1; fi\n" +
		"case \"$cmd\" in\n" +
		"  *\"command -v rc-service\"*) cat '" + filepath.Join(stub, "detect") + "' ;;\n" +
		"  *\"for s in\"*) cat '" + filepath.Join(stub, "svcs") + "' ;;\n" +
		"  *\"db=\"*) cat '" + filepath.Join(stub, "probe") + "' ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(stub, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stub+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_PROFILE", "KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_REPO_ROOT", "MIGRATE_JOBS"} {
		t.Setenv(k, "")
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	deps := command.Deps{
		Home:   home,
		Dir:    t.TempDir(), // outside any repo manifest tree
		Env:    systemLookup,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
	}
	lines := func() []string {
		data, err := os.ReadFile(inv)
		if err != nil {
			return nil
		}
		var out []string
		for _, l := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(l) != "" {
				out = append(out, l)
			}
		}
		return out
	}
	return deps, stdout, stderr, stub, lines
}

func runMigrate(t *testing.T, deps command.Deps, args ...string) int {
	t.Helper()
	return command.Execute("test", deps, append([]string{"migrate"}, args...))
}

const migrateProbeFull = "db=/data/tenants/root/root.db\n" +
	"db=/data/tenants/tenant_b.db\n" +
	"db=/data/tenants/tenant_a.db\n"

func TestMigrateHappyPathRootFirstThenTenants(t *testing.T) {
	deps, stdout, stderr, _, lines := setupMigrate(t, migrateProbeFull)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}
	out := stdout.String()

	// Order pin: root.db (auth/org plane) migrates BEFORE any tenant.
	var rootIdx, tenantAIdx, tenantBIdx = -1, -1, -1
	for i, l := range lines() {
		if strings.Contains(l, "--ns 'root'") && strings.Contains(l, "root/root.db") {
			rootIdx = i
		}
		if strings.Contains(l, "--ns 'tenant_a'") {
			tenantAIdx = i
		}
		if strings.Contains(l, "--ns 'tenant_b'") {
			tenantBIdx = i
		}
	}
	if rootIdx < 0 || tenantAIdx < 0 || tenantBIdx < 0 {
		t.Fatalf("missing migrate invocations (root=%d a=%d b=%d):\n%s", rootIdx, tenantAIdx, tenantBIdx, strings.Join(lines(), "\n"))
	}
	if rootIdx > tenantAIdx || rootIdx > tenantBIdx {
		t.Errorf("root.db must migrate before the tenants (root=%d a=%d b=%d)", rootIdx, tenantAIdx, tenantBIdx)
	}

	// The per-file command reuses the app's own applier from the repo on
	// the VM — no parallel implementation.
	for _, want := range []string{
		"cd '/srv/app' && pnpm --filter api exec tsx scripts/libsql-migrate/migrate-db.ts --db 'file:/data/tenants/root/root.db' --ns 'root'",
		"--db 'file:/data/tenants/tenant_a.db'",
	} {
		found := false
		for _, l := range lines() {
			if strings.Contains(l, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing invocation %q", want)
		}
	}

	if !strings.Contains(out, "[migrate] all db file(s) migrated cleanly (3 total)") {
		t.Errorf("missing the clean-summary line:\n%s", out)
	}
}

func TestMigrateGuardRefusesRunningService(t *testing.T) {
	deps, _, stderr, stub, lines := setupMigrate(t, migrateProbeFull)
	if err := os.WriteFile(filepath.Join(stub, "svcs"),
		[]byte("kampodine-api: running\nkamal-proxy: not-running\nwalshipper: not-running\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 1 {
		t.Fatalf("exit = %d, want 1 (the stop-first guard)", code)
	}
	if !strings.Contains(stderr.String(), "kampodine-api is running — stop it first (rc-service kampodine-api stop) or pass --allow-running") {
		t.Errorf("stderr = %q, want the shell's guard message", stderr.String())
	}
	for _, l := range lines() {
		if strings.Contains(l, "pnpm --filter api exec tsx") {
			t.Fatalf("the guard must refuse BEFORE any migration:\n%s", l)
		}
	}
}

func TestMigrateAllowRunningWarnsAndProceeds(t *testing.T) {
	deps, stdout, stderr, stub, _ := setupMigrate(t, migrateProbeFull)
	if err := os.WriteFile(filepath.Join(stub, "svcs"),
		[]byte("kampodine-api: running\nkamal-proxy: not-running\nwalshipper: not-running\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(t, deps, "--repo-root", "/srv/app", "--allow-running"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "WARNING: migrating while kampodine-api is running (--allow-running)") {
		t.Errorf("stdout = %q, want the --allow-running warning", stdout.String())
	}
}

func TestMigrateInitUnknownWarnsAndProceeds(t *testing.T) {
	deps, stdout, stderr, stub, _ := setupMigrate(t, migrateProbeFull)
	// The host reports NEITHER init tool (a local dev machine) — state is
	// unknowable: proceed with the loud warning, never block.
	if err := os.WriteFile(filepath.Join(stub, "detect"), []byte("none\nhttpc=none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[migrate] service state unknown (neither rc-service nor systemctl found) — assuming kampodine-api is not running") {
		t.Errorf("stdout = %q, want the unknowable-state warning", stdout.String())
	}
}

func TestMigratePreconditionFailures(t *testing.T) {
	tests := []struct {
		name   string
		probe  string
		action string
	}{
		{"repo checkout missing", "ERR repo-root\n", "repo root not found"},
		{"pnpm missing", "ERR pnpm\n", "pnpm not found"},
		{"tenant dir missing", "ERR tenant-dir\n", "tenant dir"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, stderr, _, lines := setupMigrate(t, tc.probe)
			if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), tc.action) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.action)
			}
			for _, l := range lines() {
				if strings.Contains(l, "pnpm --filter api exec tsx") {
					t.Fatalf("failed preconditions must abort BEFORE any migration:\n%s", l)
				}
			}
		})
	}
}

func TestMigratePerFileFailureCollectedExitOne(t *testing.T) {
	deps, stdout, stderr, stub, lines := setupMigrate(t, migrateProbeFull)
	if err := os.WriteFile(filepath.Join(stub, "failon"), []byte("--ns 'tenant_b'"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 1 {
		t.Fatalf("exit = %d, want 1 (failures collected)", code)
	}
	// One bad tenant must NOT block the others: both other files migrated.
	joined := strings.Join(lines(), "\n")
	if !strings.Contains(joined, "--ns 'tenant_a'") {
		t.Error("tenant_a must still migrate (files are independent)")
	}
	if !strings.Contains(joined, "--ns 'root'") {
		t.Error("root.db must still migrate")
	}
	if !strings.Contains(stderr.String(), "[migrate][FAIL] migration failed for /data/tenants/tenant_b.db") {
		t.Errorf("stderr = %q, want the per-file failure line", stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 db file(s) failed to migrate (migrated: 2) — do NOT start the API on a half-migrated estate; fix and re-run") {
		t.Errorf("stdout = %q, want the shell's failure summary", stdout.String())
	}
}

func TestMigrateNoDbFilesIsQuietSuccess(t *testing.T) {
	deps, stdout, stderr, _, _ := setupMigrate(t, "")
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[migrate] no .db files under /data/tenants; nothing to migrate") {
		t.Errorf("stdout = %q, want the nothing-to-migrate line", stdout.String())
	}
}

func TestMigrateRepoRootRequired(t *testing.T) {
	deps, _, stderr, _, _ := setupMigrate(t, migrateProbeFull)
	if code := runMigrate(t, deps); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--repo-root") || !strings.Contains(stderr.String(), "KAMPODRA_REPO_ROOT") {
		t.Errorf("stderr = %q, want both resolution routes named", stderr.String())
	}
}

func TestMigrateRepoRootFromEnv(t *testing.T) {
	deps, stdout, stderr, _, lines := setupMigrate(t, migrateProbeFull)
	t.Setenv("KAMPODRA_REPO_ROOT", "/env/app")
	if code := runMigrate(t, deps); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	found := false
	for _, l := range lines() {
		if strings.Contains(l, "cd '/env/app' && pnpm --filter api exec tsx") {
			found = true
		}
	}
	if !found {
		t.Errorf("KAMPODRA_REPO_ROOT must feed the repo root:\n%s", stdout.String())
	}
}

func TestMigrateJobsValidation(t *testing.T) {
	deps, _, stderr, _, _ := setupMigrate(t, migrateProbeFull)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app", "--jobs", "0"); code != 1 {
		t.Fatalf("exit = %d, want 1 (--jobs 0)", code)
	}
	if !strings.Contains(stderr.String(), "--jobs") {
		t.Errorf("stderr = %q, want the --jobs validation error", stderr.String())
	}
	// MIGRATE_JOBS env feeds the default.
	t.Setenv("MIGRATE_JOBS", "2")
	deps2, stdout, stderr2, _, _ := setupMigrate(t, migrateProbeFull)
	t.Setenv("MIGRATE_JOBS", "2")
	if code := runMigrate(t, deps2, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr2.String())
	}
	if !strings.Contains(stdout.String(), "(3 total)") {
		t.Errorf("stdout = %q", stdout.String())
	}
}
