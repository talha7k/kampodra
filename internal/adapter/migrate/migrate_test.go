package migrate

import (
	"strings"
	"testing"
)

// The migrate adapter: the frozen migrate.sh loop, decomposed into a
// one-round-trip probe (preconditions + db listing), a pure ordering plan
// (root.db first, tenants sorted), and the per-file migrate command — all
// busybox-safe remote composition, no OS knowledge in the command layer.

func TestProbeCommandShape(t *testing.T) {
	cmd := ProbeCommand("/srv/app", "scripts/migrate-db.ts", "/data/tenants")
	for _, want := range []string{
		"/srv/app/scripts/migrate-db.ts",
		"command -v pnpm",
		"/data/tenants",
		"db=",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("probe command missing %q:\n%s", want, cmd)
		}
	}
}

func TestProbeCommandUsesTheConfiguredScript(t *testing.T) {
	cmd := ProbeCommand("/srv/app", "ops/migrate.ts", "/data")
	if !strings.Contains(cmd, "/srv/app/ops/migrate.ts") || strings.Contains(cmd, "migrate-db.ts") {
		t.Errorf("probe must check the CONFIGURED migrate script, not a hardcoded path:\n%s", cmd)
	}
}

func TestProbeCommandQuotesPaths(t *testing.T) {
	cmd := ProbeCommand("/srv/app's", "scripts/mi grate.ts", "/data/ten ants")
	if !strings.Contains(cmd, `/srv/app'\''s`) {
		t.Errorf("repo path must be single-quote escaped:\n%s", cmd)
	}
	if !strings.Contains(cmd, `/data/ten ants`) {
		t.Errorf("data dir must stay one argument:\n%s", cmd)
	}
	if !strings.Contains(cmd, `'/srv/app'\''s/scripts/mi grate.ts'`) {
		t.Errorf("the configured script path must be quote-escaped inside the check:\n%s", cmd)
	}
}

func TestParseProbeAllOkWithFiles(t *testing.T) {
	out := strings.Join([]string{
		"db=/data/tenants/root/root.db",
		"db=/data/tenants/tenant_b.db",
		"db=/data/tenants/tenant_a.db",
		"db=/data/tenants/not-a-db.txt", // must be filtered out by the remote glob already; tolerate
		"",
	}, "\n")
	p := ParseProbe(out)
	if !p.TenantDirOK {
		t.Error("tenant dir must be OK when db lines arrive")
	}
	if len(p.DbFiles) != 3 {
		t.Fatalf("db files = %v, want the 3 .db files", p.DbFiles)
	}
	if p.DbFiles[0] != "/data/tenants/root/root.db" {
		t.Errorf("first db = %q", p.DbFiles[0])
	}
}

func TestParseProbeErrorFlags(t *testing.T) {
	p := ParseProbe("ERR repo-root\nERR pnpm\n")
	if p.RepoOK {
		t.Error("repo-root flag must be false after ERR repo-root")
	}
	if p.PnpmOK {
		t.Error("pnpm flag must be false after ERR pnpm")
	}
	if p.TenantDirOK != true {
		t.Error("tenant dir must stay OK when no ERR tenant-dir line arrived")
	}
	if len(p.DbFiles) != 0 {
		t.Errorf("db files = %v, want none", p.DbFiles)
	}
}

func TestParseProbeEmptyOutput(t *testing.T) {
	p := ParseProbe("")
	if len(p.DbFiles) != 0 {
		t.Errorf("db files = %v, want none", p.DbFiles)
	}
}

func TestPlanRootFirstThenTenantsSorted(t *testing.T) {
	plan := Plan([]string{
		"/data/tenants/tenant_charlie.db",
		"/data/tenants/root/root.db",
		"/data/tenants/tenant_alpha.db",
		"/data/tenants/tenant_bravo.db",
	})
	if plan.Root == nil || plan.Root.Path != "/data/tenants/root/root.db" {
		t.Fatalf("root = %+v, want root/root.db first", plan.Root)
	}
	if plan.Root.NS != "root" {
		t.Errorf("root ns = %q, want root (the auth/org plane)", plan.Root.NS)
	}
	if len(plan.Tenants) != 3 {
		t.Fatalf("tenants = %+v, want 3", plan.Tenants)
	}
	wantOrder := []string{"/data/tenants/tenant_alpha.db", "/data/tenants/tenant_bravo.db", "/data/tenants/tenant_charlie.db"}
	for i, w := range wantOrder {
		if plan.Tenants[i].Path != w {
			t.Errorf("tenants[%d] = %q, want %q", i, plan.Tenants[i].Path, w)
		}
	}
	// ns = file basename minus .db (shell parity: the namespace IS the file)
	if plan.Tenants[0].NS != "tenant_alpha" {
		t.Errorf("tenant ns = %q, want tenant_alpha", plan.Tenants[0].NS)
	}
}

func TestPlanLegacyFlatRoot(t *testing.T) {
	plan := Plan([]string{
		"/data/tenants/root.db",
		"/data/tenants/tenant_a.db",
	})
	if plan.Root == nil || plan.Root.Path != "/data/tenants/root.db" {
		t.Fatalf("root = %+v, want the legacy flat root.db", plan.Root)
	}
	if plan.Root.NS != "root" {
		t.Errorf("root ns = %q, want root", plan.Root.NS)
	}
	if len(plan.Tenants) != 1 || plan.Tenants[0].Path != "/data/tenants/tenant_a.db" {
		t.Errorf("tenants = %+v — root.db must NOT be re-migrated as a tenant", plan.Tenants)
	}
}

func TestPlanRootPreferredOverLegacy(t *testing.T) {
	plan := Plan([]string{
		"/data/tenants/root.db",
		"/data/tenants/root/root.db",
	})
	if plan.Root == nil || plan.Root.Path != "/data/tenants/root/root.db" {
		t.Fatalf("root = %+v, want the nested root/root.db to win", plan.Root)
	}
}

// The support + billing class dirs (esellar classes 3 + 4:
// <data>/support/support.db, <data>/billing/billing.db) are FIRST-CLASS
// planes, handled exactly like the root pairing: dedicated jobs with their
// own ns, enumerated after root and before the tenant pool — never
// silently dropped.
func TestPlanSupportAndBillingClassesAreFirstClass(t *testing.T) {
	plan := Plan([]string{
		"/data/tenants/billing/billing.db",
		"/data/tenants/tenant_charlie.db",
		"/data/tenants/support/support.db",
		"/data/tenants/root/root.db",
		"/data/tenants/tenant_alpha.db",
	})
	wantClasses := []Job{
		{Path: "/data/tenants/support/support.db", NS: "support"},
		{Path: "/data/tenants/billing/billing.db", NS: "billing"},
	}
	if len(plan.Classes) != len(wantClasses) {
		t.Fatalf("classes = %+v, want %+v", plan.Classes, wantClasses)
	}
	for i, w := range wantClasses {
		if plan.Classes[i] != w {
			t.Errorf("classes[%d] = %+v, want %+v (fixed apply order: support, billing)", i, plan.Classes[i], w)
		}
	}
	if len(plan.Tenants) != 2 || plan.Tenants[0].Path != "/data/tenants/tenant_alpha.db" {
		t.Errorf("tenants = %+v — the class dbs must NOT join the tenant pool", plan.Tenants)
	}
	if plan.Root == nil || plan.Root.NS != "root" {
		t.Errorf("root = %+v — the class wiring must not disturb the root plane", plan.Root)
	}
	if len(plan.Unknown) != 0 {
		t.Errorf("unknown = %v — class files are known shapes, not warnings", plan.Unknown)
	}
}

// One sighting per class dir: a later sighting replaces an earlier one —
// the nested-root rule's parity (only one class dir exists on a real
// estate).
func TestPlanClassDuplicateSightingLastWins(t *testing.T) {
	plan := Plan([]string{
		"/data/tenants/support/support.db",
		"/data/other/support/support.db",
	})
	if len(plan.Classes) != 1 {
		t.Fatalf("classes = %+v, want exactly one support job", plan.Classes)
	}
	if plan.Classes[0].Path != "/data/other/support/support.db" {
		t.Errorf("class = %+v, want the later sighting to win", plan.Classes[0])
	}
}

// Fail closed: a file matching NO known shape must SURFACE as an explicit
// skip (the command layer warns) — the old silent ignore is how a new db
// class drifted with zero errors.
func TestPlanUnknownShapesAreReported(t *testing.T) {
	plan := Plan([]string{"/data/tenants/scratch.db"})
	if plan.Root != nil {
		t.Errorf("root = %+v, want nil (scratch.db is neither)", plan.Root)
	}
	if len(plan.Tenants) != 0 {
		t.Errorf("tenants = %+v, want none", plan.Tenants)
	}
	if len(plan.Classes) != 0 {
		t.Errorf("classes = %+v, want none", plan.Classes)
	}
	if len(plan.Unknown) != 1 || plan.Unknown[0] != "/data/tenants/scratch.db" {
		t.Errorf("unknown = %v, want scratch.db reported as an explicit skip", plan.Unknown)
	}
}

func TestMigrateCommandShape(t *testing.T) {
	cmd := MigrateCommand("/srv/app", "scripts/migrate-db.ts", Job{Path: "/data/tenants/tenant_a.db", NS: "tenant_a"})
	for _, want := range []string{
		"cd '/srv/app'",
		"pnpm exec tsx 'scripts/migrate-db.ts'",
		"--db 'file:/data/tenants/tenant_a.db'",
		"--ns 'tenant_a'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("migrate command missing %q:\n%s", want, cmd)
		}
	}
}

func TestMigrateCommandUsesTheConfiguredScript(t *testing.T) {
	cmd := MigrateCommand("/srv/app", "ops/migrate.ts", Job{Path: "/data/tenant_a.db", NS: "tenant_a"})
	if !strings.Contains(cmd, "pnpm exec tsx 'ops/migrate.ts'") || strings.Contains(cmd, "migrate-db.ts") {
		t.Errorf("migrate command must run the CONFIGURED script, not a hardcoded path:\n%s", cmd)
	}
}

// --- binary mode (migrateMode "binary"): the no-node runner -----------------
//
// The Go binary needs no node on the host: the probe omits the pnpm check
// and the per-file apply executes the configured binary with the
// migrater's -db (PLAIN sqlite path — no file: URI) / -dir pairing (root
// job → <root>/root, tenant job → <root>/tenant).

func TestProbeCommandBinaryOmitsPnpmCheck(t *testing.T) {
	cmd := ProbeCommandBinary("/srv/app", "apps/api-go/migrater", "/data/tenants")
	if strings.Contains(cmd, "pnpm") {
		t.Errorf("the binary probe must not check pnpm (the VM is strictly no-node):\n%s", cmd)
	}
	for _, want := range []string{
		"/srv/app/apps/api-go/migrater", // the binary's presence is still checked
		"/data/tenants",
		"db=",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("binary probe command missing %q:\n%s", want, cmd)
		}
	}
}

func TestProbeCommandScriptModeKeepsPnpmCheck(t *testing.T) {
	// The script-mode probe keeps its check byte-identical — the shared
	// composer must not leak the binary mode's relaxation.
	if cmd := ProbeCommand("/srv/app", "scripts/migrate-db.ts", "/data"); !strings.Contains(cmd, "command -v pnpm") {
		t.Errorf("script-mode probe lost the pnpm check:\n%s", cmd)
	}
}

func TestMigrateCommandBinaryPairsDbAndDir(t *testing.T) {
	root := MigrateCommandBinary("/srv/app", "apps/api-go/migrater", "apps/api/drizzle",
		Job{Path: "/data/tenants/root/root.db", NS: "root"})
	for _, want := range []string{
		"'/srv/app/apps/api-go/migrater'",
		"-db '/data/tenants/root/root.db'",
		"-dir '/srv/app/apps/api/drizzle/root'",
	} {
		if !strings.Contains(root, want) {
			t.Errorf("root binary command missing %q:\n%s", want, root)
		}
	}
	if strings.Contains(root, "pnpm") || strings.Contains(root, "file:") {
		t.Errorf("the binary command must not shell pnpm or pass a file: URI:\n%s", root)
	}

	tenant := MigrateCommandBinary("/srv/app", "apps/api-go/migrater", "apps/api/drizzle",
		Job{Path: "/data/tenants/tenant_a.db", NS: "tenant_a"})
	if !strings.Contains(tenant, "-dir '/srv/app/apps/api/drizzle/tenant'") {
		t.Errorf("tenant binary command must pair -dir with the tenant migrations:\n%s", tenant)
	}
}

func TestMigrateCommandBinaryQuotesPaths(t *testing.T) {
	cmd := MigrateCommandBinary("/srv/a pp's", "bin/x", "mi grants",
		Job{Path: "/data/ten ants/a.db", NS: "root"})
	if !strings.Contains(cmd, `'/srv/a pp'\''s/bin/x'`) {
		t.Errorf("the binary path must be quote-escaped:\n%s", cmd)
	}
	if !strings.Contains(cmd, `-db '/data/ten ants/a.db'`) {
		t.Errorf("the db path must stay one argument:\n%s", cmd)
	}
	if !strings.Contains(cmd, `-dir '/srv/a pp'\''s/mi grants/root'`) {
		t.Errorf("the migrations dir must be quote-escaped and repo-rooted:\n%s", cmd)
	}
}

// The support + billing classes pair -dir with THEIR OWN migrations dirs
// (apps/api/drizzle/support/, apps/api/drizzle/billing/) — the same
// one-composition-cannot-mispair contract as root/ + tenant/.
func TestMigrateCommandBinaryPairsClassDirs(t *testing.T) {
	support := MigrateCommandBinary("/srv/app", "apps/api-go/migrater", "apps/api/drizzle",
		Job{Path: "/data/tenants/support/support.db", NS: "support"})
	if !strings.Contains(support, "-dir '/srv/app/apps/api/drizzle/support'") {
		t.Errorf("support binary command must pair -dir with the support migrations:\n%s", support)
	}
	if !strings.Contains(support, "-db '/data/tenants/support/support.db'") {
		t.Errorf("support binary command must carry the class db path:\n%s", support)
	}

	billing := MigrateCommandBinary("/srv/app", "apps/api-go/migrater", "apps/api/drizzle",
		Job{Path: "/data/tenants/billing/billing.db", NS: "billing"})
	if !strings.Contains(billing, "-dir '/srv/app/apps/api/drizzle/billing'") {
		t.Errorf("billing binary command must pair -dir with the billing migrations:\n%s", billing)
	}
}

// The probe lists the class dirs as their own globs — nested db files are
// invisible to a flat glob, so each class slots in explicitly (busybox
// -f guard skips absent dirs).
func TestProbeCommandListsClassDirs(t *testing.T) {
	script := ProbeCommand("/srv/app", "scripts/migrate-db.ts", "/data/tenants")
	binary := ProbeCommandBinary("/srv/app", "apps/api-go/migrater", "/data/tenants")
	for name, cmd := range map[string]string{"script probe": script, "binary probe": binary} {
		for _, want := range []string{
			`'/data/tenants'/*.db`,
			`'/data/tenants'/root/*.db`,
			`'/data/tenants'/support/*.db`,
			`'/data/tenants'/billing/*.db`,
		} {
			if !strings.Contains(cmd, want) {
				t.Errorf("%s missing the class-dir glob %q:\n%s", name, want, cmd)
			}
		}
	}
}
