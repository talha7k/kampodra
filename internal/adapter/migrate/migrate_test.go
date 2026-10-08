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
	cmd := ProbeCommand("/srv/app", "/data/tenants")
	for _, want := range []string{
		"/srv/app/apps/api/scripts/libsql-migrate/migrate-db.ts",
		"command -v pnpm",
		"/data/tenants",
		"db=",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("probe command missing %q:\n%s", want, cmd)
		}
	}
}

func TestProbeCommandQuotesPaths(t *testing.T) {
	cmd := ProbeCommand("/srv/app's", "/data/ten ants")
	if !strings.Contains(cmd, `/srv/app'\''s`) {
		t.Errorf("repo path must be single-quote escaped:\n%s", cmd)
	}
	if !strings.Contains(cmd, `/data/ten ants`) {
		t.Errorf("data dir must stay one argument:\n%s", cmd)
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
	root, tenants := Plan([]string{
		"/data/tenants/tenant_charlie.db",
		"/data/tenants/root/root.db",
		"/data/tenants/tenant_alpha.db",
		"/data/tenants/tenant_bravo.db",
	})
	if root == nil || root.Path != "/data/tenants/root/root.db" {
		t.Fatalf("root = %+v, want root/root.db first", root)
	}
	if root.NS != "root" {
		t.Errorf("root ns = %q, want root (the auth/org plane)", root.NS)
	}
	if len(tenants) != 3 {
		t.Fatalf("tenants = %+v, want 3", tenants)
	}
	wantOrder := []string{"/data/tenants/tenant_alpha.db", "/data/tenants/tenant_bravo.db", "/data/tenants/tenant_charlie.db"}
	for i, w := range wantOrder {
		if tenants[i].Path != w {
			t.Errorf("tenants[%d] = %q, want %q", i, tenants[i].Path, w)
		}
	}
	// ns = file basename minus .db (shell parity: the namespace IS the file)
	if tenants[0].NS != "tenant_alpha" {
		t.Errorf("tenant ns = %q, want tenant_alpha", tenants[0].NS)
	}
}

func TestPlanLegacyFlatRoot(t *testing.T) {
	root, tenants := Plan([]string{
		"/data/tenants/root.db",
		"/data/tenants/tenant_a.db",
	})
	if root == nil || root.Path != "/data/tenants/root.db" {
		t.Fatalf("root = %+v, want the legacy flat root.db", root)
	}
	if root.NS != "root" {
		t.Errorf("root ns = %q, want root", root.NS)
	}
	if len(tenants) != 1 || tenants[0].Path != "/data/tenants/tenant_a.db" {
		t.Errorf("tenants = %+v — root.db must NOT be re-migrated as a tenant", tenants)
	}
}

func TestPlanRootPreferredOverLegacy(t *testing.T) {
	root, _ := Plan([]string{
		"/data/tenants/root.db",
		"/data/tenants/root/root.db",
	})
	if root == nil || root.Path != "/data/tenants/root/root.db" {
		t.Fatalf("root = %+v, want the nested root/root.db to win", root)
	}
}

func TestPlanNoRootNoTenants(t *testing.T) {
	root, tenants := Plan([]string{"/data/tenants/scratch.db"})
	if root != nil {
		t.Errorf("root = %+v, want nil (scratch.db is neither)", root)
	}
	if len(tenants) != 0 {
		t.Errorf("tenants = %+v, want none", tenants)
	}
}

func TestMigrateCommandShape(t *testing.T) {
	cmd := MigrateCommand("/srv/app", Job{Path: "/data/tenants/tenant_a.db", NS: "tenant_a"})
	for _, want := range []string{
		"cd '/srv/app'",
		"pnpm --filter api exec tsx scripts/libsql-migrate/migrate-db.ts",
		"--db 'file:/data/tenants/tenant_a.db'",
		"--ns 'tenant_a'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("migrate command missing %q:\n%s", want, cmd)
		}
	}
}
