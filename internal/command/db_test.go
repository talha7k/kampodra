package command_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/dbq"
	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/command"
)

// kampodra db — read-only sqlite access over SSH (kampodra-native): ONE
// SELECT/WITH statement through a helper binary streamed to the VM's /tmp,
// JSON rows to stdout (data ONLY — human logs ride stderr); snapshot
// streams a consistent copy back at 0600. The ssh and go binaries are
// shimmed on PATH (the exec-shim fixture pattern — no test touches a real
// VM or compiles anything).

const dbFixtureRows = `[{"n":1}]` + "\n"
const dbFixtureSnapBytes = "SNAPBYTES"
const dbFixtureCpBytes = "CPBYTES"
const dbFixtureHelperBytes = "fake-sqlq-helper-bytes"

type dbHarness struct {
	deps     command.Deps
	stubDir  string
	fixtures string
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	sshLog   string
	stdinLog string
	goLog    string
}

func setupDb(t *testing.T) *dbHarness {
	t.Helper()
	home := t.TempDir()
	stubDir := t.TempDir()
	fixtures := t.TempDir()
	sshLog := filepath.Join(stubDir, "ssh-calls.log")
	stdinLog := filepath.Join(stubDir, "ssh-stdin.log")
	goLog := filepath.Join(stubDir, "go-calls.log")
	scratchDir := filepath.Join(stubDir, "scratch")

	sshShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + sshLog + "'\n" +
		"cmd=\"${*: -1}\"\n" +
		"case \"$cmd\" in\n" +
		// helper presence check: exits 0 only when the fixture says present
		"  \"test -x /tmp/kampodra-sqlq-\"*) [ -f \"" + fixtures + "/helper-present\" ] && exit 0 || exit 1 ;;\n" +
		// helper upload: capture the streamed bytes
		"  \"umask 077; cat > /tmp/kampodra-sqlq-\"*) cat >> '" + stdinLog + "'; exit 0 ;;\n" +
		// sqlite3 probe: echo yes/no per fixture
		"  *\"command -v sqlite3\"*) [ -f \"" + fixtures + "/has-sqlite3\" ] && echo yes || echo no ;;\n" +
		// sqlite3 .backup: write fixture bytes to the scratch path the
		// command composed (kept under the stub dir — no real /tmp litter)
		"  *\".backup \"*) f=${cmd#*\\\"}; f=${f%\\\"}; f=${f#* }; mkdir -p \"" + scratchDir + "\"; printf '%s' '" + dbFixtureSnapBytes + "' > \"" + scratchDir + "/$(basename \"$f\")\"; exit 0 ;;\n" +
		// cp fallback: same discipline (the scratch is the LAST token —
		// the copy destination)
		"  \"cp /data/\"*) f=${cmd##* }; mkdir -p \"" + scratchDir + "\"; printf '%s' '" + dbFixtureCpBytes + "' > \"" + scratchDir + "/$(basename \"$f\")\"; exit 0 ;;\n" +
		// scratch download: cat back the bytes the backup/cp leg wrote
		"  \"cat /tmp/kampodra-snapshot-\"*) f=${cmd#cat }; cat \"" + scratchDir + "/$(basename \"$f\")\" ;;\n" +
		// scratch cleanup
		"  \"rm -f /tmp/kampodra-snapshot-\"*) exit 0 ;;\n" +
		// the query itself: capture the statement stdin, emit fixture rows
		"  *\"-db \"*) cat >> '" + stdinLog + "'; printf '%s\\n' '" + strings.TrimSpace(dbFixtureRows) + "' ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(sshShim), 0o755); err != nil {
		t.Fatal(err)
	}

	// go shim: "build" the helper by writing fake bytes to the -o target,
	// and log the cross-build env (GOOS/GOARCH ride the env, not argv).
	goShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + goLog + "'\n" +
		"printf 'GOOS=%s GOARCH=%s CGO_ENABLED=%s\\n' \"$GOOS\" \"$GOARCH\" \"$CGO_ENABLED\" >> '" + goLog + "'\n" +
		"prev=\"\"; for a in \"$@\"; do if [ \"$prev\" = \"-o\" ]; then printf '%s' '" + dbFixtureHelperBytes + "' > \"$a\"; fi; prev=\"$a\"; done\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "go"), []byte(goShim), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_PROXY_HOST"} {
		t.Setenv(k, "")
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &dbHarness{
		deps: command.Deps{
			Home:     home,
			Env:      systemLookup,
			Stdout:   stdout,
			Stderr:   stderr,
			Stdin:    strings.NewReader(""),
			Prober:   &probe.Prober{HTTPClient: &http.Client{Transport: deadTransport{}}},
			CacheDir: filepath.Join(stubDir, "cache"),
		},
		stubDir:  stubDir,
		fixtures: fixtures,
		stdout:   stdout,
		stderr:   stderr,
		sshLog:   sshLog,
		stdinLog: stdinLog,
		goLog:    goLog,
	}
}

func (h *dbHarness) run(t *testing.T, args ...string) int {
	t.Helper()
	return command.Execute("test", h.deps, append([]string{"db"}, args...))
}

func (h *dbHarness) invocations(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func TestDbBareInvocationPrintsHelp(t *testing.T) {
	h := setupDb(t)
	if code := h.run(t); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(h.stdout.String(), "Usage:") || !strings.Contains(h.stdout.String(), "db query") {
		t.Errorf("bare db must print usage with the subcommands:\n%s", h.stdout.String())
	}
}

// TestDbQueryHappyPath: helper already on the VM, one SELECT rides stdin,
// stdout carries ONLY the JSON rows.
func TestDbQueryHappyPath(t *testing.T) {
	h := setupDb(t)
	if err := os.WriteFile(filepath.Join(h.fixtures, "helper-present"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code := h.run(t, "query", "--host", statusHost, "--db", "/data/tenants/acme/tenant_acme.db", "--sql", "SELECT n FROM t WHERE n = 1")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if got := h.stdout.String(); got != dbFixtureRows {
		t.Errorf("stdout = %q, want exactly %q (stdout is DATA only)", got, dbFixtureRows)
	}
	inv := h.invocations(t, h.sshLog)
	// helper presence check + query leg, nothing else
	if !strings.Contains(inv, "test -x /tmp/kampodra-sqlq-") {
		t.Errorf("missing helper presence check:\n%s", inv)
	}
	if !strings.Contains(inv, " -db /data/tenants/acme/tenant_acme.db") {
		t.Errorf("query leg missing the -db argv:\n%s", inv)
	}
	if strings.Contains(inv, "cat > /tmp/kampodra-sqlq-") {
		t.Errorf("present helper must NOT be re-streamed:\n%s", inv)
	}
	// the statement crossed on STDIN, never argv
	stdin := h.invocations(t, h.stdinLog)
	if !strings.Contains(stdin, "SELECT n FROM t WHERE n = 1") {
		t.Errorf("statement must ride stdin, got:\n%s", stdin)
	}
	if strings.Contains(inv, "SELECT n FROM t") {
		t.Errorf("statement leaked into the remote argv:\n%s", inv)
	}
	// no local build happened (helper was present)
	if goInv := h.invocations(t, h.goLog); goInv != "" {
		t.Errorf("go build ran despite a present helper: %s", goInv)
	}
	// human progress on stderr
	if !strings.Contains(h.stderr.String(), "[db]") {
		t.Errorf("stderr = %q — want [db] progress lines", h.stderr.String())
	}
}

// TestDbQueryGateRejections: the SELECT/WITH gate rejects before ANY ssh
// round-trip and exits 2.
func TestDbQueryGateRejections(t *testing.T) {
	for _, stmt := range []string{
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET x = 1",
		"DELETE FROM t",
		"ATTACH DATABASE '/tmp/evil.db' AS evil",
		"PRAGMA wal_checkpoint",
		"DROP TABLE t",
		"SELECT 1; DROP TABLE t",
	} {
		h := setupDb(t)
		code := h.run(t, "query", "--host", statusHost, "--db", "/data/x.db", "--sql", stmt)
		if code != 2 {
			t.Errorf("query(%q) exit = %d, want 2", stmt, code)
		}
		if !strings.Contains(h.stderr.String(), "read-only gate") {
			t.Errorf("query(%q) stderr %q must name the read-only gate", stmt, h.stderr.String())
		}
		if inv := h.invocations(t, h.sshLog); inv != "" {
			t.Errorf("query(%q) ssh round-tripped before the gate verdict:\n%s", stmt, inv)
		}
	}
}

// TestDbQueryFileInput: --file is the alternative statement source; the
// --sql/--file pair is exactly-one.
func TestDbQueryFileInput(t *testing.T) {
	h := setupDb(t)
	if err := os.WriteFile(filepath.Join(h.fixtures, "helper-present"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sqlFile := filepath.Join(h.fixtures, "stmt.sql")
	if err := os.WriteFile(sqlFile, []byte("WITH c AS (SELECT 1 AS n) SELECT n FROM c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := h.run(t, "query", "--host", statusHost, "--db", "/data/x.db", "--file", sqlFile)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.invocations(t, h.stdinLog), "WITH c AS (SELECT 1 AS n) SELECT n FROM c") {
		t.Errorf("file statement must ride stdin:\n%s", h.invocations(t, h.stdinLog))
	}

	// both inputs → usage error, no ssh
	h2 := setupDb(t)
	code = h2.run(t, "query", "--host", statusHost, "--db", "/data/x.db", "--sql", "SELECT 1", "--file", sqlFile)
	if code != 1 {
		t.Errorf("--sql + --file exit = %d, want 1", code)
	}
	if inv := h2.invocations(t, h2.sshLog); inv != "" {
		t.Errorf("both-inputs must not ssh:\n%s", inv)
	}

	// no input → usage error
	h3 := setupDb(t)
	code = h3.run(t, "query", "--host", statusHost, "--db", "/data/x.db")
	if code != 1 {
		t.Errorf("no input exit = %d, want 1", code)
	}

	// --file that does not exist → error naming it
	h4 := setupDb(t)
	code = h4.run(t, "query", "--host", statusHost, "--db", "/data/x.db", "--file", filepath.Join(h4.fixtures, "nope.sql"))
	if code != 1 {
		t.Errorf("missing --file exit = %d, want 1", code)
	}
}

// TestDbQueryStreamsHelperWhenMissing: first run on a VM — presence check
// fails, the helper is built locally (go shim) and streamed with 0755
// perms, then the query runs.
func TestDbQueryStreamsHelperWhenMissing(t *testing.T) {
	h := setupDb(t)
	code := h.run(t, "query", "--host", statusHost, "--db", "/data/x.db", "--sql", "SELECT 1 AS n")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	inv := h.invocations(t, h.sshLog)
	if !strings.Contains(inv, "test -x /tmp/kampodra-sqlq-") {
		t.Errorf("missing presence check:\n%s", inv)
	}
	if !strings.Contains(inv, "umask 077; cat > /tmp/kampodra-sqlq-") || !strings.Contains(inv, "chmod 755 /tmp/kampodra-sqlq-") {
		t.Errorf("helper upload missing (umask 077 + chmod 755):\n%s", inv)
	}
	// the streamed bytes are the local artifact's bytes
	stdin := h.invocations(t, h.stdinLog)
	if !strings.Contains(stdin, dbFixtureHelperBytes) {
		t.Errorf("upload did not carry the local helper bytes:\n%s", stdin)
	}
	if !strings.Contains(stdin, "SELECT 1 AS n") {
		t.Errorf("query statement missing from stdin:\n%s", stdin)
	}
	goInv := h.invocations(t, h.goLog)
	if !strings.Contains(goInv, "build") {
		t.Errorf("go build shim not invoked: %s", goInv)
	}
	if !strings.Contains(goInv, "GOOS=linux GOARCH=arm64 CGO_ENABLED=0") {
		t.Errorf("go build must be the static linux/arm64 cross-build (CGO off): %s", goInv)
	}
	// The build must produce the version-stamped LOCAL cache artifact the
	// flow then streams (the on-VM name differs by design: /tmp/kampodra-sqlq-v<ver>-<arch>).
	wantArtifact := dbq.HelperLocalPath(h.deps.CacheDir, "linux", "arm64")
	if !strings.Contains(goInv, "-o "+wantArtifact+" ") {
		t.Errorf("the build must produce %s (the streamed artifact):\n%s", wantArtifact, goInv)
	}
}

// TestDbQueryUsageErrors: missing --db and an unsafe db path fail closed
// without any ssh round-trip.
func TestDbQueryUsageErrors(t *testing.T) {
	h := setupDb(t)
	if code := h.run(t, "query", "--host", statusHost, "--sql", "SELECT 1"); code != 1 {
		t.Errorf("missing --db exit = %d, want 1", code)
	}
	if inv := h.invocations(t, h.sshLog); inv != "" {
		t.Errorf("missing --db must not ssh:\n%s", inv)
	}

	h2 := setupDb(t)
	if code := h2.run(t, "query", "--host", statusHost, "--db", "/data/x.db; rm -rf /data", "--sql", "SELECT 1"); code != 1 {
		t.Errorf("unsafe --db exit = %d, want 1", code)
	}
	if inv := h2.invocations(t, h2.sshLog); inv != "" {
		t.Errorf("unsafe --db must not ssh:\n%s", inv)
	}
}

// TestDbSnapshotFlow: sqlite3 present → .backup scratch, stream back,
// 0600 local copy, scratch cleaned up, stdout stays data-free.
func TestDbSnapshotFlow(t *testing.T) {
	h := setupDb(t)
	if err := os.WriteFile(filepath.Join(h.fixtures, "has-sqlite3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(h.fixtures, "snapshot.db")
	code := h.run(t, "snapshot", "--host", statusHost, "--db", "/data/tenants/acme/tenant_acme.db", "--out", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	inv := h.invocations(t, h.sshLog)
	if !strings.Contains(inv, `sqlite3 /data/tenants/acme/tenant_acme.db ".backup /tmp/kampodra-snapshot-`) {
		t.Errorf("missing sqlite3 .backup leg:\n%s", inv)
	}
	if !strings.Contains(inv, "cat /tmp/kampodra-snapshot-") {
		t.Errorf("missing scratch download leg:\n%s", inv)
	}
	if !strings.Contains(inv, "rm -f /tmp/kampodra-snapshot-") {
		t.Errorf("missing scratch cleanup:\n%s", inv)
	}
	if strings.Contains(inv, "cp /data/") {
		t.Errorf("sqlite3 present — cp fallback must not run:\n%s", inv)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("snapshot file: %v", err)
	}
	if string(data) != dbFixtureSnapBytes {
		t.Errorf("snapshot content = %q, want %q", data, dbFixtureSnapBytes)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("snapshot perms = %o, want 600 (money-data discipline)", fi.Mode().Perm())
	}
	if strings.Contains(h.stdout.String(), dbFixtureSnapBytes) {
		t.Errorf("snapshot bytes leaked to stdout — data goes to --out only:\n%s", h.stdout.String())
	}
}

// TestDbSnapshotCpFallback: no sqlite3 on the VM → plain cp, noted on
// stderr.
func TestDbSnapshotCpFallback(t *testing.T) {
	h := setupDb(t)
	out := filepath.Join(h.fixtures, "snapshot.db")
	code := h.run(t, "snapshot", "--host", statusHost, "--db", "/data/root.db", "--out", out)
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	inv := h.invocations(t, h.sshLog)
	if !strings.Contains(inv, "cp /data/root.db /tmp/kampodra-snapshot-") {
		t.Errorf("missing cp fallback leg:\n%s", inv)
	}
	if strings.Contains(inv, ".backup") {
		t.Errorf("no sqlite3 on the VM — .backup must not run:\n%s", inv)
	}
	if !strings.Contains(h.stderr.String(), "cp") || !strings.Contains(h.stderr.String(), "sqlite3") {
		t.Errorf("stderr %q must note the cp fallback (no consistency guarantee)", h.stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("snapshot file: %v", err)
	}
	if string(data) != dbFixtureCpBytes {
		t.Errorf("cp snapshot content = %q, want %q", data, dbFixtureCpBytes)
	}
}

// TestDbSnapshotRefusesOverwrite: a silent clobber of a local file is not
// acceptable for a money-data tool — refuse before any round-trip.
func TestDbSnapshotRefusesOverwrite(t *testing.T) {
	h := setupDb(t)
	out := filepath.Join(h.fixtures, "exists.db")
	if err := os.WriteFile(out, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := h.run(t, "snapshot", "--host", statusHost, "--db", "/data/x.db", "--out", out)
	if code != 1 {
		t.Errorf("exit = %d, want 1 (refuse overwrite)", code)
	}
	if inv := h.invocations(t, h.sshLog); inv != "" {
		t.Errorf("refusal must happen before any ssh:\n%s", inv)
	}
}

// TestDbResolvesThroughProfile: the shared resolver feeds host/key — no
// forked resolution.
func TestDbResolvesThroughProfile(t *testing.T) {
	h := setupDb(t)
	cfgDir := filepath.Join(h.deps.Home, ".kampodra")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"defaultProfile": "prod", "profiles": {"prod": {"host": "` + statusHost + `", "sshKey": "/tmp/profile-key"}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.fixtures, "helper-present"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := h.run(t, "query", "--db", "/data/x.db", "--sql", "SELECT 1"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	inv := h.invocations(t, h.sshLog)
	if !strings.Contains(inv, "-i /tmp/profile-key "+statusHost) {
		t.Errorf("profile host/key must feed every ssh leg:\n%s", inv)
	}
}

// TestDbWithoutTargetFails: no host anywhere → the shared fail-closed
// message, exit 1.
func TestDbWithoutTargetFails(t *testing.T) {
	h := setupDb(t)
	if code := h.run(t, "query", "--db", "/data/x.db", "--sql", "SELECT 1"); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "target required") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}
