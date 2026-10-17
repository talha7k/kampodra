package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The support + billing db classes (esellar design, classes 3 + 4):
// <dataDir>/support/support.db and <dataDir>/billing/billing.db migrate as
// FIRST-CLASS planes — root first, then support, then billing, then the
// tenant pool bounded-parallel. A file matching no known shape is an
// explicit, warned skip — never a silent ignore.

const migrateProbeClasses = "db=/data/root/root.db\n" +
	"db=/data/billing/billing.db\n" +
	"db=/data/tenant_b.db\n" +
	"db=/data/support/support.db\n" +
	"db=/data/tenant_a.db\n"

func classInvocationIndex(t *testing.T, lines []string, marker string) int {
	t.Helper()
	for i, l := range lines {
		if strings.Contains(l, marker) {
			return i
		}
	}
	return -1
}

// Script mode: the class planes apply through the configured migrate
// script with their own --ns, root first, classes before the tenants.
func TestMigrateClassesRunRootFirstThenClassPlanes(t *testing.T) {
	deps, stdout, stderr, _, lines := setupMigrate(t, migrateProbeClasses)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}

	// Every plane migrates through the app's own script with its own ns.
	for _, want := range []string{
		"--db 'file:/data/support/support.db' --ns 'support'",
		"--db 'file:/data/billing/billing.db' --ns 'billing'",
	} {
		if classInvocationIndex(t, lines(), want) < 0 {
			t.Errorf("missing class invocation %q in:\n%s", want, strings.Join(lines(), "\n"))
		}
	}

	// Order pin: root (auth/org plane) → support → billing → tenants.
	rootIdx := classInvocationIndex(t, lines(), "--ns 'root'")
	supportIdx := classInvocationIndex(t, lines(), "--ns 'support'")
	billingIdx := classInvocationIndex(t, lines(), "--ns 'billing'")
	tenantAIdx := classInvocationIndex(t, lines(), "--ns 'tenant_a'")
	if rootIdx < 0 || supportIdx < 0 || billingIdx < 0 || tenantAIdx < 0 {
		t.Fatalf("missing invocations (root=%d support=%d billing=%d a=%d):\n%s",
			rootIdx, supportIdx, billingIdx, tenantAIdx, strings.Join(lines(), "\n"))
	}
	if rootIdx > supportIdx || supportIdx > billingIdx || billingIdx > tenantAIdx {
		t.Errorf("apply order must be root → support → billing → tenants (root=%d support=%d billing=%d a=%d)",
			rootIdx, supportIdx, billingIdx, tenantAIdx)
	}

	if !strings.Contains(stdout.String(), "[migrate] all db file(s) migrated cleanly (5 total)") {
		t.Errorf("missing the clean-summary line (5 planes):\n%s", stdout.String())
	}
}

// Binary mode: the class planes pair -dir with their own migrations dirs,
// exactly like the root/ + tenant/ pairing.
func TestMigrateBinaryModePairsClassDirs(t *testing.T) {
	deps, stdout, stderr, _, lines := setupMigrateWithProfile(t, BinaryMigrateProfile, migrateProbeClasses)
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}
	joined := strings.Join(lines(), "\n")
	for _, want := range []string{
		"-db '/data/support/support.db' -dir '/srv/app/apps/api/drizzle/support'",
		"-db '/data/billing/billing.db' -dir '/srv/app/apps/api/drizzle/billing'",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing class invocation %q in:\n%s", want, joined)
		}
	}
	if !strings.Contains(stdout.String(), "(5 total)") {
		t.Errorf("missing the clean-summary line:\n%s", stdout.String())
	}
}

// Fail closed: an unknown db shape surfaces as a warned explicit skip —
// it never migrates, never counts toward the total, and never fails the
// run (the old silent ignore is how a new class drifted with zero errors).
func TestMigrateWarnsOnUnknownDbShapes(t *testing.T) {
	deps, stdout, stderr, _, lines := setupMigrate(t, migrateProbeFull+"db=/data/scratch.db\n")
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "[migrate] WARNING: skipping /data/scratch.db — matches no known db class") {
		t.Errorf("missing the unknown-shape warning:\n%s", stdout.String())
	}
	for _, l := range lines() {
		if strings.Contains(l, "scratch.db") {
			t.Errorf("the unknown file must NEVER migrate:\n%s", l)
		}
	}
	if !strings.Contains(stdout.String(), "(3 total)") {
		t.Errorf("the skipped file must not count toward the total:\n%s", stdout.String())
	}
}

// Only unknown files under the data dir: warned skips, then the quiet
// nothing-to-migrate success.
func TestMigrateOnlyUnknownShapesWarnsAndSkips(t *testing.T) {
	deps, stdout, stderr, _, _ := setupMigrate(t, "db=/data/scratch.db\n")
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "WARNING: skipping /data/scratch.db") {
		t.Errorf("missing the unknown-shape warning:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "[migrate] no .db files under /data; nothing to migrate") {
		t.Errorf("missing the nothing-to-migrate line:\n%s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("a warned skip is not a failure: stderr = %q", stderr.String())
	}
}

// The class planes collect per-file failures like every other file: one
// bad class db must not block the tenants.
func TestMigrateClassPlaneFailureCollected(t *testing.T) {
	deps, stdout, stderr, stub, lines := setupMigrate(t, migrateProbeClasses)
	if err := os.WriteFile(filepath.Join(stub, "failon"),
		[]byte("--ns 'billing'"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runMigrate(t, deps, "--repo-root", "/srv/app"); code != 1 {
		t.Fatalf("exit = %d, want 1 (failures collected)", code)
	}
	joined := strings.Join(lines(), "\n")
	if !strings.Contains(joined, "--ns 'support'") || !strings.Contains(joined, "--ns 'tenant_a'") {
		t.Error("support and the tenants must still migrate (files are independent)")
	}
	if !strings.Contains(stderr.String(), "[migrate][FAIL] migration failed for /data/billing/billing.db") {
		t.Errorf("stderr = %q, want the per-file failure line", stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 db file(s) failed to migrate (migrated: 4)") {
		t.Errorf("stdout = %q, want the failure summary", stdout.String())
	}
}
