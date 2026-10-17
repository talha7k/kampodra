package vmgc

import (
	"strings"
	"testing"
)

// vmgc is kampodra's VM garbage-collection adapter. The classification and
// allowlist logic is the safety core: `vm gc --apply` deletes ONLY entries
// whose class is scratch/stale-script/residue, and the allowlists below are
// hard-coded as never-touch (mandatory estate, owner-decision items,
// kampodra's own helpers, OS paths, tenant data).
//
// The fixtures mirror the REAL esellar-green inventory (2026-10-17) so the
// tool is pinned to the estate it will actually run against.

func entry(path string, size int64, kind string) Entry {
	return Entry{Path: path, Size: size, MTime: 1792230323, Kind: kind}
}

// testProtect mirrors the esellar profile's ProjectConfig block (test
// fixtures may name today's values — production code may not).
func testProtect() Protect {
	return Protect{
		Container:    "esellar-api",
		ShadowSuffix: "-shadow",
		Services:     []string{"esellar-api", "esellar-backup", "esellar-walshipper", "beszel", "beszel-agent", "kamal-proxy"},
		DataDir:      "/data/tenants",
	}
}

// sweepFixture is the real sweep listing shape (busybox stat output).
const sweepFixture = `16678342|1792230323|regular file|/tmp/zatcaretrigger
674|1792190726|regular file|/tmp/fc2.cjs
120|1792190235|directory|/tmp/migrations
63931|1792190373|regular file|/tmp/drizzle.tgz
7274656|1792190373|regular file|/tmp/migrater
7209120|1792224198|regular file|/tmp/dbquery
100|1792189042|directory|/tmp/walbuild
60|1792143692|directory|/tmp/dbscratch
380928|1792143692|regular file|/tmp/r-scratch-marker
163|1792143750|regular file|/tmp/._who2.mjs
40|1791186792|directory|/tmp/.X11-unix
40|1791186792|directory|/tmp/.ICE-unix
1776148292|1791534755|regular file|/tmp/live-exit28-report.txt
114254646|1791487774|regular file|/tmp/pre-rebase-2229.tgz
120|1791487651|directory|/tmp/green-rebase
289357824|1791487774|regular file|/tmp/green-rebase/tenant_indian-valley-restaurant-2.db
553828352|1791487774|regular file|/tmp/green-rebase/tenant_indian-valley-restaurant-exit-28.db
97|1742908603|regular file|/etc/profile.d/20locale.sh
2128|1791344013|regular file|/etc/periodic/daily/walshipper-soak-sample.sh
2548|1791142146|regular file|/boot/grub/arm64-efi/modinfo.sh
97|1742908603|regular file|/usr/local/sbin/esellar-anchor.sh
4096|1792224080|regular file|/data/app/esellar-api-go.prev
4096|1792230269|regular file|/data/app/esellar-api-go
9|1792230272|regular file|/data/app/esellar-api-go.sha
9|1792224083|regular file|/data/app/esellar-api-go.prev.sha
4096|1791677601|directory|/data/app/releases
4096|1791677601|directory|/data/tenants
4096|1791487774|directory|/data/tenants-preflip-snap-20261016
4096|1791487774|directory|/data/tenants-presync-snap-20261016
9|1791487774|regular file|/data/web/current.sha
9|1791487774|regular file|/data/web/current.prev.sha
11|1791487774|regular file|/data/.last-backup
6042|1791487774|regular file|/data/esellar-backup.log
114253757|1791487774|regular file|/root/pre-wipe-20261009-113438.tgz
4096|1791487774|directory|/root/pre-second-go-20261010-1209
4096|1791487774|directory|/var/lib/containers/storage/overlay
`

func TestParseSweepListing(t *testing.T) {
	entries := ParseSweepListing(sweepFixture)
	if len(entries) == 0 {
		t.Fatal("ParseSweepListing returned no entries")
	}
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	zat, ok := byPath["/tmp/zatcaretrigger"]
	if !ok {
		t.Fatal("missing /tmp/zatcaretrigger")
	}
	if zat.Size != 16678342 {
		t.Errorf("size = %d, want 16678342", zat.Size)
	}
	if zat.Kind != "file" {
		t.Errorf("kind = %q, want file", zat.Kind)
	}
	if zat.MTime != 1792230323 {
		t.Errorf("mtime = %d, want 1792230323", zat.MTime)
	}
	if d := byPath["/tmp/green-rebase"]; d.Kind != "dir" {
		t.Errorf("green-rebase kind = %q, want dir", d.Kind)
	}
}

func TestParseSweepListingToleratesGarbage(t *testing.T) {
	entries := ParseSweepListing("find: bad line\n\nnot|a-stat-line\n" + sweepFixture)
	if len(entries) == 0 {
		t.Fatal("garbage lines must be skipped, not fatal")
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, "/") {
			t.Errorf("parsed non-absolute path %q from garbage", e.Path)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		path string
		kind string
		want Class
	}{
		// --- never-touch: OS / infra -----------------------------------
		{"/etc/profile.d/20locale.sh", "file", ClassOS},
		{"/etc/init.d/esellar-api", "file", ClassOS},
		{"/etc/periodic/daily/walshipper-soak-sample.sh", "file", ClassOS},
		{"/boot/grub/arm64-efi/modinfo.sh", "file", ClassOS},
		{"/usr/local/sbin/esellar-anchor.sh", "file", ClassInfra},
		{"/usr/local/sbin/kampodra-anchor.sh", "file", ClassInfra},
		{"/bin/sh", "file", ClassOS},
		{"/sbin/rc", "file", ClassOS},
		{"/var/lib/containers/storage/overlay", "dir", ClassOS},
		// --- never-touch: tenant data + cutover snapshots ---------------
		{"/data/tenants", "dir", ClassTenantData},
		{"/data/tenants/root.db", "file", ClassTenantData},
		{"/data/tenants/tenant_acme.db", "file", ClassTenantData},
		{"/data/tenants-preflip-snap-20261016", "dir", ClassOwnerDecision},
		{"/data/tenants-presync-snap-20261016", "dir", ClassOwnerDecision},
		// --- never-touch: live artifact + web chain + backup telemetry --
		{"/data/app/esellar-api-go", "file", ClassLiveArtifact},
		{"/data/app/esellar-api-go.prev", "file", ClassLiveArtifact},
		{"/data/app/esellar-api-go.sha", "file", ClassLiveArtifact},
		{"/data/app/esellar-api-go.prev.sha", "file", ClassLiveArtifact},
		{"/data/web/current.sha", "file", ClassLiveArtifact},
		{"/data/web/current.prev.sha", "file", ClassLiveArtifact},
		{"/data/.last-backup", "file", ClassLiveArtifact},
		{"/data/esellar-backup.log", "file", ClassLiveArtifact},
		// --- never-touch: owner decisions -------------------------------
		// NOTE: build contexts (e.g. a soak container's build dir) are
		// protected at PLAN level via the Dockerfile probe — the pure
		// classifier cannot see inside dirs (pinned by
		// TestPlanBuildContextOwner / TestPlanNeverTouchesProtected).
		{"/root/pre-wipe-20261009-113438.tgz", "file", ClassOwnerDecision},
		{"/root/pre-second-go-20261010-1209", "dir", ClassOwnerDecision},
		// --- never-touch: kampodra's own helpers + OS session dirs ------
		{"/tmp/kampodra-sqlq-v1-arm64", "file", ClassKampodra},
		{"/tmp/kampodra-snapshot-abc123.db", "file", ClassKampodra},
		{"/tmp/.X11-unix", "dir", ClassOSSession},
		{"/tmp/.ICE-unix", "dir", ClassOSSession},
		// --- deletable: stale scripts -----------------------------------
		{"/tmp/old-triage.sh", "file", ClassStaleScript},
		{"/root/legacy-op.sh", "file", ClassStaleScript},
		// --- deletable: residues ----------------------------------------
		{"/data/app/releases", "dir", ClassResidue},
		// --- deletable: scratch (binaries, copies, AppleDouble, dirs) ---
		{"/tmp/zatcaretrigger", "file", ClassScratch},
		{"/tmp/dbquery", "file", ClassScratch},
		{"/tmp/migrater", "file", ClassScratch},
		{"/tmp/fc2.cjs", "file", ClassScratch},
		{"/tmp/drizzle.tgz", "file", ClassScratch},
		{"/tmp/migrations", "dir", ClassScratch},
		{"/tmp/._who2.mjs", "file", ClassScratch},
		{"/tmp/live-exit28-report.txt", "file", ClassScratch},
		{"/tmp/pre-rebase-2229.tgz", "file", ClassScratch},
		{"/tmp/green-rebase", "dir", ClassScratch},
		{"/tmp/green-rebase/tenant_x.db", "file", ClassScratch},
		{"/tmp/dbscratch", "dir", ClassScratch},
		{"/tmp/r-scratch-marker", "file", ClassScratch},
		{"/var/tmp/leftover.tgz", "file", ClassScratch},
		// --- unknown (report, never delete) ------------------------------
		{"/data/something-new", "dir", ClassUnknown},
		{"/opt/mystery", "file", ClassUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyWith(testProtect(), entry(tc.path, 1, tc.kind)); got != tc.want {
			t.Errorf("ClassifyWith(%q, %s) = %q, want %q", tc.path, tc.kind, got, tc.want)
		}
	}
}

// TestClassifyFailsClosedWithoutEstateNaming: with zero Protect (no data
// dir known), tenant paths must classify report-only — never deletable.
func TestClassifyFailsClosedWithoutEstateNaming(t *testing.T) {
	for _, p := range []string{"/data/tenants", "/data/tenants/root.db", "/data/tenants-preflip-snap-x"} {
		got := Classify(entry(p, 1, "file"))
		if Deletable(got) {
			t.Errorf("zero-Protect Classify(%q) = %q — must fail closed to a non-deletable class", p, got)
		}
	}
}

// TestPlanBuildContextOwner: a scratch-zone dir holding a Dockerfile is a
// live build context — owner-decision, never in the removal set.
func TestPlanBuildContextOwner(t *testing.T) {
	entries := []Entry{
		{Path: "/tmp/somebuild", Size: 100, Kind: "dir"},
		{Path: "/tmp/somebuild/Dockerfile", Size: 158, Kind: "file"},
		{Path: "/tmp/somebuild/app", Size: 4096, Kind: "file"},
		{Path: "/tmp/plainjunk", Size: 5, Kind: "file"},
	}
	ownerDirs := DirOfBuildContext([]string{"/tmp/somebuild/Dockerfile"})
	if len(ownerDirs) != 1 || ownerDirs[0] != "/tmp/somebuild" {
		t.Fatalf("DirOfBuildContext = %v, want [/tmp/somebuild]", ownerDirs)
	}
	plan := BuildPlan(entries, nil, ownerDirs, testProtect())
	for _, r := range plan.Removes {
		if r.Entry.Path == "/tmp/somebuild" || strings.HasPrefix(r.Entry.Path, "/tmp/somebuild/") {
			t.Errorf("live build context %q must never be auto-removed", r.Entry.Path)
		}
	}
	found := false
	for _, row := range plan.Rows {
		if row.Entry.Path == "/tmp/somebuild" {
			found = true
			if row.Class != ClassOwnerDecision {
				t.Errorf("build context class = %q, want owner-decision", row.Class)
			}
		}
	}
	if !found {
		t.Fatal("build context row missing from plan")
	}
	// Untouched junk right next to it stays deletable.
	removed := map[string]bool{}
	for _, r := range plan.Removes {
		removed[r.Entry.Path] = true
	}
	if !removed["/tmp/plainjunk"] {
		t.Error("plain scratch next to a build context must stay deletable")
	}
}

// TestPlanNeverTouchesProtected is the allowlist gate: with the FULL real
// inventory as input, the plan's removal set is EXACTLY the stale set and
// never contains any mandatory/owner/OS/tenant/live path.
func TestPlanNeverTouchesProtected(t *testing.T) {
	entries := ParseSweepListing(sweepFixture)
	plan := BuildPlan(entries, nil, []string{"/tmp/walbuild"}, testProtect())

	wantRemoves := map[string]bool{
		"/tmp/zatcaretrigger":         true,
		"/tmp/migrater":               true,
		"/tmp/dbquery":                true,
		"/tmp/fc2.cjs":                true, // deletable when nothing holds it
		"/tmp/drizzle.tgz":            true,
		"/tmp/migrations":             true,
		"/tmp/dbscratch":              true,
		"/tmp/r-scratch-marker":       true,
		"/tmp/._who2.mjs":             true,
		"/tmp/live-exit28-report.txt": true,
		"/tmp/pre-rebase-2229.tgz":    true,
		"/tmp/green-rebase":           true, // children fold into the parent rm -rf
		"/data/app/releases":          true,
	}
	got := map[string]bool{}
	for _, r := range plan.Removes {
		got[r.Entry.Path] = true
	}
	if len(got) != len(plan.Removes) {
		t.Fatal("duplicate paths in plan.Removes")
	}
	for path := range wantRemoves {
		if !got[path] {
			t.Errorf("plan must remove stale path %q — it does not", path)
		}
	}
	for path := range got {
		if !wantRemoves[path] {
			t.Errorf("plan removes NON-stale path %q — allowlist leak", path)
		}
	}
	// green-rebase's children are covered by the parent's rm -rf — they
	// must not be separate removals (and their size must fold into Freed).
	for _, r := range plan.Removes {
		if strings.HasPrefix(r.Entry.Path, "/tmp/green-rebase/") {
			t.Errorf("child %q must fold into its parent's removal", r.Entry.Path)
		}
	}
	// The never-touch estate must not appear anywhere in the removal set.
	never := []string{
		"/data/tenants", "/data/tenants/root.db",
		"/data/tenants-preflip-snap-20261016", "/data/tenants-presync-snap-20261016",
		"/data/app/esellar-api-go", "/data/app/esellar-api-go.prev",
		"/data/web/current.sha", "/data/.last-backup",
		"/tmp/walbuild", "/tmp/fc2.cjs-is-not-here-but-walbuild-is",
		"/root/pre-wipe-20261009-113438.tgz",
		"/tmp/kampodra-sqlq-v1-arm64", "/tmp/.X11-unix",
		"/etc/profile.d/20locale.sh", "/usr/local/sbin/esellar-anchor.sh",
		"/var/lib/containers/storage/overlay",
	}
	for _, n := range never {
		if got[n] {
			t.Errorf("NEVER-TOUCH path %q is in the removal set", n)
		}
	}
	// Freed must include the folded green-rebase subtree (children sizes),
	// not just the 4096-byte dir inode.
	var wantFreed int64
	for _, e := range entries {
		if wantRemoves[e.Path] || strings.HasPrefix(e.Path, "/tmp/green-rebase/") {
			wantFreed += e.Size
		}
	}
	if plan.Freed != wantFreed {
		t.Errorf("Freed = %d, want %d (dir subtrees must be accounted once)", plan.Freed, wantFreed)
	}
}

func TestPlanBlocksInUse(t *testing.T) {
	entries := ParseSweepListing("/tmp/fc2.cjs\n674|1792190726|regular file|/tmp/fc2.cjs")
	plan := BuildPlan(entries, []string{"/tmp/fc2.cjs"}, nil, testProtect())
	if len(plan.Removes) != 0 {
		t.Errorf("in-use path must be blocked, got %d removes", len(plan.Removes))
	}
	if len(plan.Blocked) != 1 || plan.Blocked[0].Entry.Path != "/tmp/fc2.cjs" {
		t.Errorf("blocked = %+v, want exactly /tmp/fc2.cjs", plan.Blocked)
	}
}

// TestPlanRemovalReclaims: each removal carries its full reclaimed-bytes
// figure (row + folded dir subtree), the per-removal figures sum to
// plan.Freed, and the folded subtree lands on the parent's row.
func TestPlanRemovalReclaims(t *testing.T) {
	entries := ParseSweepListing(sweepFixture)
	plan := BuildPlan(entries, nil, []string{"/tmp/walbuild"}, testProtect())
	var sum int64
	var parentReclaims int64
	for _, r := range plan.Removes {
		if r.Reclaims < r.Entry.Size {
			t.Fatalf("removal %q reclaims %d < its own size %d", r.Entry.Path, r.Reclaims, r.Entry.Size)
		}
		sum += r.Reclaims
		if r.Entry.Path == "/tmp/green-rebase" {
			parentReclaims = r.Reclaims
		}
	}
	if sum != plan.Freed {
		t.Errorf("sum(Reclaims) = %d, want plan.Freed = %d", sum, plan.Freed)
	}
	// green-rebase dir inode is 120B; its children are ~1.02G.
	if parentReclaims <= 120 {
		t.Errorf("green-rebase Reclaims = %d, want the folded subtree total", parentReclaims)
	}
}

func TestValidateDeletablePath(t *testing.T) {
	for _, ok := range []string{
		"/tmp/zatcaretrigger",
		"/tmp/green-rebase",
		"/tmp/live-exit28-report.txt",
		"/data/app/releases",
		"/root/legacy-op.sh",
	} {
		if err := ValidateDeletablePath(ok); err != nil {
			t.Errorf("ValidateDeletablePath(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"tmp/relative",
		"/data/tenants",               // tenant data — never
		"/data/tenants/root.db",       // tenant data — never
		"/tmp/kampodra-sqlq-v1-arm64", // kampodra helper — never
		"/tmp/.X11-unix",              // os session — never
		"/data/app/esellar-api-go",    // live artifact — never
		"/root/pre-wipe-x.tgz",        // owner decision — never
		"/etc/profile.d/x.sh",         // os — never
		"/tmp/a b",                    // whitespace
		"/tmp/a;b",                    // shell metachar
		"/tmp/$(reboot)",              // shell metachar
		"/tmp/`id`",                   // shell metachar
		"/tmp/a|b",                    // shell metachar
		"/tmp/../etc/passwd",          // traversal
		"/etc/../data/tenants",        // traversal
	} {
		if err := ValidateDeletablePath(bad); err == nil {
			t.Errorf("ValidateDeletablePath(%q) = nil, want error", bad)
		}
	}
}

func TestRemoveCommand(t *testing.T) {
	cases := []struct {
		path string
		kind string
		want string
	}{
		{"/tmp/zatcaretrigger", "file", "rm -f /tmp/zatcaretrigger"},
		{"/tmp/green-rebase", "dir", "rm -rf /tmp/green-rebase"},
		{"/data/app/releases", "dir", "find /data/app/releases -maxdepth 0 -type d -empty -delete"},
	}
	for _, tc := range cases {
		got, err := RemoveCommand(entry(tc.path, 1, tc.kind))
		if err != nil {
			t.Fatalf("RemoveCommand(%q): %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("RemoveCommand(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	if _, err := RemoveCommand(entry("/data/tenants", 1, "dir")); err == nil {
		t.Error("RemoveCommand must refuse non-deletable classes")
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:          "0B",
		512:        "512B",
		1024:       "1.0K",
		16384:      "16.0K",
		1 << 20:    "1.0M",
		16678342:   "15.9M",
		1776148292: "1.7G",
		114254646:  "109.0M",
	}
	for in, want := range cases {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyEstate(t *testing.T) {
	pr := testProtect()
	containers := ParsePodmanPS(`beszel|docker.io/henrygd/beszel:0.21.0|Up 11 days
beszel-agent|docker.io/henrygd/beszel-agent:0.21.0|Up 11 days
kamal-proxy|docker.io/basecamp/kamal-proxy:latest|Up 8 days
esellar-backup|localhost/esellar-backup:daily|Up 8 days
esellar-walshipper|localhost/esellar-walshipper:soak2|Up 13 hours
youthful_elbakyan|127.0.0.1:5000/esellar-api:latest|Up 13 hours
esellar-api|127.0.0.1:5000/esellar-api:latest|Up 2 hours
esellar-api-shadow|127.0.0.1:5000/esellar-api:latest|Up 3 minutes
`)
	want := map[string]Class{
		"esellar-api":        ClassMandatory, // the project stack
		"esellar-api-shadow": ClassMandatory, // its rolling shadow
		"kamal-proxy":        ClassMandatory, // the TLS edge
		"beszel":             ClassOwnerDecision,
		"beszel-agent":       ClassOwnerDecision,
		"esellar-backup":     ClassOwnerDecision,
		"esellar-walshipper": ClassOwnerDecision,
		"youthful_elbakyan":  ClassUnknown, // orphaned probe (report-only)
	}
	ccs, cims := ClassifyEstate(pr, containers, ParsePodmanImages(`localhost/esellar-walshipper:soak2|df278e580fb9|11.9 MB|13 hours ago
127.0.0.1:5000/esellar-api:latest|3450e1f6cbf3|40.5 MB|6 days ago
localhost/esellar-backup:daily|842369fd0330|21 MB|8 days ago
`))
	if len(ccs) != len(want) {
		t.Fatalf("classified %d containers, want %d", len(ccs), len(want))
	}
	for _, c := range ccs {
		if got := c.Class; got != want[c.Name] {
			t.Errorf("ClassifyEstate container %q = %q, want %q", c.Name, got, want[c.Name])
		}
	}
	// Images ride the containers that use them; shared images take the
	// strongest verdict.
	wantImg := map[string]Class{
		"localhost/esellar-walshipper:soak2": ClassOwnerDecision,
		"127.0.0.1:5000/esellar-api:latest":  ClassMandatory, // used by the stack + an unknown probe
		"localhost/esellar-backup:daily":     ClassOwnerDecision,
	}
	if len(cims) != len(wantImg) {
		t.Fatalf("classified %d images, want %d", len(cims), len(wantImg))
	}
	for _, im := range cims {
		if got := im.Class; got != wantImg[im.Ref] {
			t.Errorf("ClassifyEstate image %q = %q, want %q", im.Ref, got, wantImg[im.Ref])
		}
	}
	// podman inventory is report-only: the adapter must expose NO delete
	// command builder for containers/images. (Compile-time pinned: any
	// future DeleteContainerCommand/DeleteImageCommand must come with the
	// owner's explicit sign-off.)
}
