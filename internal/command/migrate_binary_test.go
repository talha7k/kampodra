package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// migrateMode "binary": the green estate migrates via the static Go
// binary (apps/api-go/cmd/migrater) streamed into the VM's repo checkout
// — the VM is strictly no-node. The probe relaxes (no pnpm check), the
// verdict tolerates a stale ERR pnpm line, and the per-file apply
// executes the configured binary with -db/-dir instead of shelling pnpm.

// BinaryMigrateProfile is the profile block a no-node estate carries:
// the binary path (repo-relative, where the runner was streamed) plus the
// migrations root (repo-relative, holding the migrater's root/ + tenant/
// pairing).
const BinaryMigrateProfile = `"project": {"migrateMode": "binary", "migrateScript": "apps/api-go/migrater", "migrateDir": "apps/api/drizzle"}`

func setupMigrateWithProfile(t *testing.T, projectBlock, probeFixture string) (command.Deps, *bytes.Buffer, *bytes.Buffer, string, func() []string) {
	t.Helper()
	deps, stdout, stderr, stubDir, lines := setupMigrate(t, probeFixture)
	cfgDir := filepath.Join(deps.Home, ".kampodra")
	cfg := `{"defaultProfile": "prod", "profiles": {"prod": {"host": "root@203.0.113.9", "sshKey": "/ops/id", ` + projectBlock + `}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return deps, stdout, stderr, stubDir, lines
}

// The binary run: the probe still runs (db listing + binary presence +
// data dir), the per-file applies execute the binary with the -db/-dir
// pairing, root first, and NO pnpm appears anywhere on the wire.
func TestMigrateBinaryModeRunsTheGoBinary(t *testing.T) {
	deps, stdout, stderr, _, lines := setupMigrateWithProfile(t, BinaryMigrateProfile, migrateProbeFull)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}

	joined := strings.Join(lines(), "\n")
	for _, want := range []string{
		// probe: checks the configured binary, never pnpm
		"/srv/app/apps/api-go/migrater",
		// root job pairs -dir with the root migrations, root.db first
		"'/srv/app/apps/api-go/migrater' -db '/data/root/root.db' -dir '/srv/app/apps/api/drizzle/root'",
		// tenant jobs pair -dir with the tenant migrations
		"-db '/data/tenant_a.db' -dir '/srv/app/apps/api/drizzle/tenant'",
		"-db '/data/tenant_b.db' -dir '/srv/app/apps/api/drizzle/tenant'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing invocation %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "pnpm") {
		t.Errorf("a binary-mode run must never touch pnpm:\n%s", joined)
	}

	// Root-first order still holds.
	var rootIdx, tenantAIdx = -1, -1
	for i, l := range lines() {
		if strings.Contains(l, "-db '/data/root/root.db'") {
			rootIdx = i
		}
		if strings.Contains(l, "-db '/data/tenant_a.db'") {
			tenantAIdx = i
		}
	}
	if rootIdx < 0 || tenantAIdx < 0 {
		t.Fatalf("missing invocations (root=%d a=%d):\n%s", rootIdx, tenantAIdx, joined)
	}
	if rootIdx > tenantAIdx {
		t.Errorf("root.db must migrate before the tenants (root=%d a=%d)", rootIdx, tenantAIdx)
	}

	if !strings.Contains(stdout.String(), "[migrate] all db file(s) migrated cleanly (3 total)") {
		t.Errorf("missing the clean-summary line:\n%s", stdout.String())
	}
}

// The verdict RELAXES in binary mode: a probe that reports ERR pnpm (an
// old fixture, a foreign probe) must not block the no-node runner.
func TestMigrateBinaryModeToleratesErrPnpm(t *testing.T) {
	deps, _, stderr, _, lines := setupMigrateWithProfile(t, BinaryMigrateProfile, "ERR pnpm\n"+migrateProbeFull)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s — binary mode must skip the pnpm verdict", code, stderr.String())
	}
	if n := len(lines()); n == 0 {
		t.Fatal("the run must proceed to the per-file applies")
	}
}

// Script mode keeps its verdict: the same ERR pnpm probe still fails the
// run (default behavior byte-identical — the relaxation is mode-gated).
func TestMigrateScriptModeStillFailsOnErrPnpm(t *testing.T) {
	deps, _, stderr, _, _ := setupMigrate(t, "ERR pnpm\n"+migrateProbeFull)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 1 {
		t.Fatalf("exit = %d, want 1 — script mode must keep the pnpm verdict", code)
	}
	if !strings.Contains(stderr.String(), "pnpm not found") {
		t.Errorf("stderr = %q, want the pnpm verdict message", stderr.String())
	}
}

// Binary mode without the migrations root fails closed BEFORE any probe
// or migration: the binary's -dir pairing cannot be derived.
func TestMigrateBinaryModeRequiresMigrateDir(t *testing.T) {
	deps, _, stderr, _, lines := setupMigrateWithProfile(t,
		`"project": {"migrateMode": "binary", "migrateScript": "apps/api-go/migrater"}`,
		migrateProbeFull)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 1 {
		t.Fatalf("exit = %d, want 1 (fail closed)", code)
	}
	if !strings.Contains(stderr.String(), "migrateDir") {
		t.Errorf("stderr = %q, want it to name the missing migrateDir config", stderr.String())
	}
	if n := len(lines()); n != 0 {
		t.Errorf("nothing may run before the config check:\n%s", strings.Join(lines(), "\n"))
	}
}

// An unknown migrateMode fails closed — a typo must stop the migrate,
// never silently fall back to the pnpm script.
func TestMigrateModeFailsClosedOnUnknownValue(t *testing.T) {
	deps, _, stderr, _, lines := setupMigrateWithProfile(t,
		`"project": {"migrateMode": "binarry", "migrateScript": "x", "migrateDir": "y"}`,
		migrateProbeFull)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 1 {
		t.Fatalf("exit = %d, want 1 (fail closed)", code)
	}
	if !strings.Contains(stderr.String(), "binarry") {
		t.Errorf("stderr = %q, want it to name the bad value", stderr.String())
	}
	if n := len(lines()); n != 0 {
		t.Errorf("nothing may run before the config check:\n%s", strings.Join(lines(), "\n"))
	}
}

// A per-file failure is still collected per db in binary mode (files are
// independent — one bad tenant must not block the others).
func TestMigrateBinaryModeCollectsPerFileFailures(t *testing.T) {
	deps, stdout, stderr, stub, lines := setupMigrateWithProfile(t, BinaryMigrateProfile, migrateProbeFull)
	if err := os.WriteFile(filepath.Join(stub, "failon"),
		[]byte("-db '/data/tenant_b.db'"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 1 {
		t.Fatalf("exit = %d, want 1 (failures collected)", code)
	}
	joined := strings.Join(lines(), "\n")
	if !strings.Contains(joined, "-db '/data/tenant_a.db'") {
		t.Error("tenant_a must still migrate (files are independent)")
	}
	if !strings.Contains(stderr.String(), "[migrate][FAIL] migration failed for /data/tenant_b.db") {
		t.Errorf("stderr = %q, want the per-file failure line", stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 db file(s) failed to migrate (migrated: 2)") {
		t.Errorf("stdout = %q, want the failure summary", stdout.String())
	}
}
