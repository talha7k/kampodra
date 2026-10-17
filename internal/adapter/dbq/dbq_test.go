package dbq_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/dbq"
)

// The read-only gate: exactly ONE statement, first keyword SELECT or WITH.
// This is the client-side line of defense (exit 2, no ssh round-trip); the
// sqlq helper enforces the same contract again VM-side, and mode=ro is the
// driver-level backstop.

func TestValidateReadOnlyAcceptsReads(t *testing.T) {
	ok := []string{
		"SELECT 1",
		"select * from users",
		"  SELECT 1",
		"WITH c AS (SELECT 1) SELECT * FROM c",
		"with recursive t(n) AS (VALUES (1)) SELECT n FROM t",
		"-- peek\nSELECT 1",
		"/* block */ SELECT 1",
		"/* nested /* deep */ block */ SELECT 1",
		"-- c1\n-- c2\nSELECT 1",
		"SELECT 1;",
		"SELECT 1 ;",
		"SELECT 1;\n",
		"SELECT 'a;b' FROM t", // quoted semicolon inside ONE statement is fine
		"\t\n SELECT count(*) FROM sqlite_master",
	}
	for _, stmt := range ok {
		if err := dbq.ValidateReadOnly(stmt); err != nil {
			t.Errorf("ValidateReadOnly(%q) = %v, want nil", stmt, err)
		}
	}
}

func TestValidateReadOnlyRejectsWritesAndEverythingElse(t *testing.T) {
	reject := []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET x = 1",
		"DELETE FROM t",
		"ATTACH DATABASE '/tmp/evil.db' AS evil",
		"PRAGMA wal_checkpoint",
		"DROP TABLE t",
		"CREATE TABLE t (x)",
		"VACUUM",
		"REPLACE INTO t VALUES (1)",
		"insert into t values (1)",
		"",
		"   ",
		"; DROP TABLE t",
		"(SELECT 1)",             // not a bare keyword — reject, normalize first
		`"SELECT" * FROM t`,      // quoted identifier is NOT the keyword
		"EXPLAIN SELECT 1",       // read-only tool: data statements only
		"SELECT 1; DROP TABLE t", // multi-statement smuggling
		"WITH c AS (SELECT 1) SELECT 1; ATTACH '/x' AS y",
	}
	for _, stmt := range reject {
		err := dbq.ValidateReadOnly(stmt)
		if err == nil {
			t.Errorf("ValidateReadOnly(%q) = nil, want an error", stmt)
			continue
		}
		if !strings.Contains(err.Error(), "read-only gate") {
			t.Errorf("ValidateReadOnly(%q) error %q must name the read-only gate", stmt, err)
		}
	}
}

// ValidateDbPath guards remote-command composition: the db path rides into
// shell commands (sqlite3 .backup, the helper's argv) — shell metacharacters,
// option-leading, and URI-special characters must fail closed.
func TestValidateDbPath(t *testing.T) {
	ok := []string{
		"/data/tenants/acme/tenant_acme.db",
		"/data/root.db",
		"tenant_x.db",
	}
	for _, p := range ok {
		if err := dbq.ValidateDbPath(p); err != nil {
			t.Errorf("ValidateDbPath(%q) = %v, want nil", p, err)
		}
	}
	reject := []string{
		"",
		"-db-flag.db",
		"/data/x.db; rm -rf /data",
		"/data/x.db && echo pwned",
		"/data/$(whoami).db",
		"/data/x`id`.db",
		"/data/x | cat.db",
		"/data/x>y.db",
		"/data/x<y.db",
		"/data/x.db?mode=rwc", // URI-special — the DSN builder would parse it
		"/data/x.db#frag",
		"/data/x 'quoted'.db",
		"/data/x \"dq\".db",
		"/data/x\nrm -rf /data",
		"/data/with space.db",
	}
	for _, p := range reject {
		if err := dbq.ValidateDbPath(p); err == nil {
			t.Errorf("ValidateDbPath(%q) = nil, want an error", p)
		}
	}
}

// ReadOnlyDSN is the driver-level backstop: mode=ro&immutable=0 — writes
// fail at the driver even if the gate were bypassed.
func TestReadOnlyDSN(t *testing.T) {
	got := dbq.ReadOnlyDSN("/data/tenants/x.db")
	want := "file:/data/tenants/x.db?mode=ro&immutable=0"
	if got != want {
		t.Errorf("ReadOnlyDSN = %q, want %q", got, want)
	}
}

// The on-VM helper artifact lives under /tmp with the kampodra scratch
// naming discipline: generated name only (version + arch stamped, never
// operator-supplied), so the remote command needs no quoting.
func TestHelperRemotePath(t *testing.T) {
	got := dbq.HelperRemotePath("arm64")
	if !strings.HasPrefix(got, "/tmp/kampodra-sqlq-v"+dbq.Version+"-") {
		t.Errorf("HelperRemotePath = %q — must carry the /tmp/kampodra-sqlq prefix and the version stamp", got)
	}
	if !strings.HasSuffix(got, "-arm64") {
		t.Errorf("HelperRemotePath = %q — must carry the arch suffix", got)
	}
	if got != dbq.HelperRemotePath("arm64") {
		t.Errorf("HelperRemotePath must be deterministic (the VM copy is reused), got %q twice-differs", got)
	}
}

// The local cache path rides the caller's cache root (injectable — tests
// never touch the real user cache).
func TestHelperLocalPath(t *testing.T) {
	got := dbq.HelperLocalPath("/cache", "linux", "arm64")
	if !strings.HasPrefix(got, filepath.Join("/cache", "dbq")+"/") {
		t.Errorf("HelperLocalPath = %q — must live under <cacheRoot>/dbq/", got)
	}
	if !strings.Contains(got, "linux-arm64-v"+dbq.Version) {
		t.Errorf("HelperLocalPath = %q — must carry the triple + version stamp", got)
	}
}

// The snapshot scratch path follows the verify-scratch discipline: fixed
// /tmp prefix, crypto/rand suffix, never reused.
func TestSnapshotScratchPath(t *testing.T) {
	a, b := dbq.SnapshotScratchPath(), dbq.SnapshotScratchPath()
	if !strings.HasPrefix(a, "/tmp/kampodra-snapshot-") || !strings.HasSuffix(a, ".db") {
		t.Errorf("SnapshotScratchPath = %q — must be /tmp/kampodra-snapshot-*.db", a)
	}
	if a == b {
		t.Errorf("SnapshotScratchPath reused %q across calls — the suffix must be random", a)
	}
}

// MaterializeHelperModule writes the helper's standalone build unit
// (main.go, go.mod, go.sum) into dir — what the on-demand `go build`
// compiles for the VM.
func TestMaterializeHelperModule(t *testing.T) {
	dir := t.TempDir()
	if err := dbq.MaterializeHelperModule(dir); err != nil {
		t.Fatalf("MaterializeHelperModule: %v", err)
	}
	for _, name := range []string{"main.go", "go.mod", "go.sum"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Size() == 0 {
			t.Errorf("materialized %s missing or empty: %v", name, err)
		}
	}

	// Version pin: the materialized go.mod must require the SAME
	// modernc.org/sqlite version the repo builds the helper against — a
	// drifted template compiles a helper that no longer matches the
	// tested driver behavior.
	mainGoMod, err := os.ReadFile(filepath.Join("..", "..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read repo go.mod: %v", err)
	}
	repoRe := regexp.MustCompile(`modernc\.org/sqlite (v[0-9][^\s]+)`)
	m := repoRe.FindSubmatch(mainGoMod)
	if m == nil {
		t.Fatalf("repo go.mod carries no modernc.org/sqlite requirement")
	}
	tmpl, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatalf("read materialized go.mod: %v", err)
	}
	if !strings.Contains(string(tmpl), "modernc.org/sqlite "+string(m[1])) {
		t.Errorf("materialized go.mod drifted from the repo requirement (%s):\n%s", m[1], tmpl)
	}

	// The embedded helper source carries its own version stamp — it must
	// equal dbq.Version (the VM cache path's invalidator).
	srcRe := regexp.MustCompile(`sqlqVersion = "([^"]+)"`)
	sm := srcRe.FindSubmatch([]byte(dbq.SQLQSource))
	if sm == nil {
		t.Fatalf("sqlq source carries no sqlqVersion stamp")
	}
	if string(sm[1]) != dbq.Version {
		t.Errorf("sqlq source stamp %q != dbq.Version %q — bump both together (the VM cache path keys on it)", sm[1], dbq.Version)
	}

	// The rewritten main.go must import the helper module's own dbq
	// package — the live internal import cannot resolve in a standalone
	// module (and internal/ is unimportable cross-module anyway).
	mainSrc, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(mainSrc), "github.com/talha7k/kampodra/internal") {
		t.Errorf("materialized main.go still imports the kampodra module:\n%s", mainSrc)
	}
	if !strings.Contains(string(mainSrc), `"kampodrasqlq/dbq"`) {
		t.Errorf("materialized main.go lost the rewritten dbq import:\n%s", mainSrc)
	}
}

// TestBuildLinuxHelperRealFire is the REAL build: materialize + cross-compile
// with the actual go toolchain and prove the artifact is a static
// linux/arm64 ELF. Skipped unless KAMPODRA_SQLQ_E2E=1 (it takes seconds and
// wants the module cache) — run it before touching the helper's build:
//
//	KAMPODRA_SQLQ_E2E=1 go test ./internal/adapter/dbq/ -run RealFire -count=1
func TestBuildLinuxHelperRealFire(t *testing.T) {
	if os.Getenv("KAMPODRA_SQLQ_E2E") != "1" {
		t.Skip("real toolchain build — set KAMPODRA_SQLQ_E2E=1")
	}
	cacheRoot := t.TempDir()
	out, err := dbq.BuildLinuxHelper(context.Background(), cacheRoot, os.Stderr)
	if err != nil {
		t.Fatalf("BuildLinuxHelper: %v", err)
	}
	if want := dbq.HelperLocalPath(cacheRoot, "linux", "arm64"); out != want {
		t.Errorf("artifact = %q, want the version-stamped cache path %q", out, want)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// ELF magic + EM_AARCH64 (183 little-endian at bytes 18-19)
	if !bytes.HasPrefix(raw, []byte("\x7fELF")) {
		t.Fatalf("artifact is not an ELF binary (first bytes: %x)", raw[:4])
	}
	if raw[18] != 0xb7 || raw[19] != 0x00 {
		t.Errorf("ELF machine = %x %x, want b7 00 (aarch64)", raw[18], raw[19])
	}
	if len(raw) < 1<<20 {
		t.Errorf("artifact is suspiciously small (%d bytes) for the sqlite driver build", len(raw))
	}
	// Cache hit: a second call must not rebuild (same path, no error).
	again, err := dbq.BuildLinuxHelper(context.Background(), cacheRoot, os.Stderr)
	if err != nil || again != out {
		t.Errorf("cache-hit rebuild: got %q, %v — want %q, nil", again, err, out)
	}
}
