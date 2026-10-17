package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// vm gc: dry-run-by-default VM garbage sweep. The inventory phase classifies
// every host artifact (typed table); --apply deletes ONLY the
// scratch/stale-script/residue classes. The allowlists (mandatory estate,
// owner-decision items, kampodra helpers, OS paths, tenant data) are
// never-touch, podman inventory is strictly report-only, and entries that
// are in use by a live process/container mount are blocked from deletion.

const vmGCSweepFixture = `16678342|1792230323|regular file|/tmp/zatcaretrigger
674|1792190726|regular file|/tmp/fc2.cjs
120|1792190235|directory|/tmp/migrations
7274656|1792190373|regular file|/tmp/migrater
7209120|1792224198|regular file|/tmp/dbquery
100|1792189042|directory|/tmp/walbuild
40|1791186792|directory|/tmp/.X11-unix
1776148292|1791534755|regular file|/tmp/live-exit28-report.txt
114254646|1791487774|regular file|/tmp/pre-rebase-2229.tgz
120|1791487651|directory|/tmp/green-rebase
289357824|1791487774|regular file|/tmp/green-rebase/tenant_indian-valley-restaurant-2.db
97|1742908603|regular file|/etc/profile.d/20locale.sh
97|1742908603|regular file|/usr/local/sbin/esellar-anchor.sh
4096|1792224080|regular file|/data/app/esellar-api-go.prev
4096|1792230269|regular file|/data/app/esellar-api-go
4096|1791677601|directory|/data/app/releases
4096|1791677601|directory|/data/tenants
4096|1791487774|directory|/data/tenants-preflip-snap-20261016
9|1791487774|regular file|/data/web/current.sha
11|1791487774|regular file|/data/.last-backup
114253757|1791487774|regular file|/root/pre-wipe-20261009-113438.tgz
4096|1791487774|directory|/var/lib/containers/storage/overlay
`

// gcHostState answers the vm gc probes like the real VM: the sweep fixture,
// one in-use path (the live fc2.cjs triage file), one live build context
// (the soak container's build dir), and the podman estate.
const gcHostState = `case "$cmd" in
  "find /tmp /var/tmp /opt /home -maxdepth "*)
    echo "/tmp/walbuild/Dockerfile"
    exit 0 ;;
  "find /tmp "*)
    cat <<'SWEEPEOF'
` + vmGCSweepFixture + `
SWEEPEOF
    exit 0 ;;
  "( cat /proc/"*)
    echo "/tmp/fc2.cjs"
    exit 0 ;;
  "podman ps -a "*)
    printf 'esellar-api|127.0.0.1:5000/esellar-api:latest|Up 2 hours\nkamal-proxy|docker.io/basecamp/kamal-proxy:latest|Up 8 days\nesellar-walshipper|localhost/esellar-walshipper:soak2|Up 13 hours\nyouthful_elbakyan|127.0.0.1:5000/esellar-api:latest|Up 13 hours\n'
    exit 0 ;;
  "podman images "*)
    printf '127.0.0.1:5000/esellar-api:latest|3450e1f6cbf3|40.5 MB|6 days ago\nlocalhost/esellar-walshipper:soak2|df278e580fb9|11.9 MB|13 hours ago\n'
    exit 0 ;;
esac`

func setupGC(t *testing.T, hostState string) *wipeHarness {
	t.Helper()
	h := setupWipe(t, hostState)
	// A repo manifest naming today's estate, so Protect routes the real
	// dataDir/containers/services (the generic defaults would fail-closed
	// all of /data to tenant-data — correct for production, wrong fixture).
	dir := t.TempDir()
	manifest := `{
  "container": "esellar-api",
  "shadowSuffix": "-shadow",
  "dataDir": "/data/tenants",
  "services": ["esellar-api", "esellar-backup", "esellar-walshipper", "beszel", "beszel-agent", "kamal-proxy"]
}`
	if err := os.WriteFile(filepath.Join(dir, "kampodra.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	h.deps.Dir = dir
	return h
}

func TestVMGCRequiresHost(t *testing.T) {
	h := setupGC(t, gcHostState)
	before := len(h.calls(t))
	code := command.Execute("test", h.deps, []string{"vm", "gc"})
	if code == 0 {
		t.Fatal("vm gc without a resolvable host must fail")
	}
	if after := len(h.calls(t)); after != before {
		t.Errorf("unresolved host must cost zero round-trips (%d calls)", after-before)
	}
}

func TestVMGCDryRunNeverMutates(t *testing.T) {
	h := setupGC(t, gcHostState)
	code := command.Execute("test", h.deps, []string{"vm", "gc", "--host", "root@203.0.113.9"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, h.stderr.String())
	}
	calls := h.calls(t)
	for _, c := range calls {
		if strings.Contains(c, " rm ") || strings.Contains(c, "find /data/app/releases") ||
			strings.Contains(c, "podman rm") || strings.Contains(c, "podman rmi") {
			t.Errorf("dry run executed a mutation command: %s", c)
		}
	}
	joined := strings.Join(calls, "\n")
	for _, probe := range []string{"find /tmp ", "podman ps -a ", "podman images "} {
		if !strings.Contains(joined, probe) {
			t.Errorf("dry run inventory missing probe %q", probe)
		}
	}
	if out := h.stdout.String(); !strings.Contains(out, "would remove") {
		t.Error("dry run must list the exact commands --apply would run")
	}
	out := h.stdout.String()
	// The typed classification table must be visible.
	for _, want := range []string{
		"/tmp/zatcaretrigger",
		"/tmp/live-exit28-report.txt",
		"/data/tenants",
		"/tmp/walbuild",
		"/tmp/fc2.cjs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("inventory table missing %q", want)
		}
	}
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("dry run must say it is a dry run; stdout:\n%s", out)
	}
}

func TestVMGCApplyDeletesOnlyStale(t *testing.T) {
	h := setupGC(t, gcHostState)
	code := command.Execute("test", h.deps, []string{"vm", "gc", "--host", "root@203.0.113.9", "--apply"})
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, h.stderr.String())
	}
	var mutations []string
	for _, c := range h.calls(t) {
		if strings.Contains(c, " rm ") || strings.Contains(c, "find /data/app/releases") {
			mutations = append(mutations, c)
		}
		if strings.Contains(c, "podman rm") || strings.Contains(c, "podman rmi") {
			t.Errorf("vm gc must NEVER mutate podman: %s", c)
		}
	}
	want := map[string]bool{
		"rm -f /tmp/zatcaretrigger":                                  true,
		"rm -f /tmp/migrater":                                        true,
		"rm -f /tmp/dbquery":                                         true,
		"rm -rf /tmp/migrations":                                     true,
		"rm -f /tmp/live-exit28-report.txt":                          true,
		"rm -f /tmp/pre-rebase-2229.tgz":                             true,
		"rm -rf /tmp/green-rebase":                                   true,
		"find /data/app/releases -maxdepth 0 -type d -empty -delete": true,
	}
	if len(mutations) != len(want) {
		t.Fatalf("mutation commands = %v, want exactly %d stale deletions; stdout:\n%s", mutations, len(want), h.stdout.String())
	}
	for _, m := range mutations {
		matched := false
		for w := range want {
			if strings.Contains(m, w) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("unexpected mutation %q", m)
		}
	}
	// The allowlisted estate is never touched, and the in-use file is
	// blocked (reported, not deleted).
	out := h.stdout.String()
	if strings.Contains(out, "would remove") {
		t.Error("apply must not print dry-run 'would remove' lines")
	}
	if !strings.Contains(out, "DONE") {
		t.Error("apply must print its DONE verdict")
	}
	for _, keep := range []string{"/data/tenants", "/tmp/walbuild", "/tmp/fc2.cjs"} {
		if !strings.Contains(out, keep) {
			t.Errorf("report must mention kept/blocked path %q", keep)
		}
	}
}
