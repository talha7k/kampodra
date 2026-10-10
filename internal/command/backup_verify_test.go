// The backup verify VM flow: file-based restore-verify on generated VM
// scratch paths (never the data dir), the .tgz member loop, scratch
// cleanup surviving cancellation, and the tgz fixture builder.

package command_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/transport"
	"github.com/talha7k/kampodra/internal/command"
)

// The 2026-10-09 live-fire contract: restore-verify is FILE-based
// (restore-verify -db <path>, RUNBOOK drill shape) — never stdin. verify
// uploads each member to a VM scratch file (`umask 077; cat > …`), runs
// restore-verify -db on it, then rm's the scratch — NEVER the data dir.
func TestBackupVerifyDbUploadsToScratchThenVerifies(t *testing.T) {
	h := setupBackup(t)
	dbFile := filepath.Join(t.TempDir(), "dump.db")
	os.WriteFile(dbFile, []byte("db-bytes"), 0o600)

	if code := h.run(t, "verify", dbFile, "--host", "root@203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	piped, err := os.ReadFile(h.stdinLog)
	if err != nil || string(piped) != "db-bytes" {
		t.Fatalf("ssh stdin = %q, %v (want the db image verbatim)", piped, err)
	}
	calls := h.sshInvocations(t)
	if !strings.Contains(calls, "/usr/local/bin/restore-verify -db /tmp/kampodra-verify") {
		t.Errorf("no restore-verify -db call on a scratch path:\n%s", calls)
	}
	if strings.Contains(calls, "/dev/stdin") {
		t.Errorf("stale /dev/stdin contract in:\n%s", calls)
	}
	if !strings.Contains(calls, "rm -f /tmp/kampodra-verify") {
		t.Errorf("scratch file never cleaned up:\n%s", calls)
	}
	if out := h.stdout.String(); !strings.Contains(out, "OK — restore-verify accepted the db image") {
		t.Errorf("verify output wrong:\n%s", out)
	}
}

func TestBackupVerifyTgzExtractsAndVerifiesEveryMember(t *testing.T) {
	h := setupBackup(t)
	tgz := filepath.Join(t.TempDir(), "bundle.tgz")
	buildFixtureTgz(t, tgz, map[string]string{
		"a.db":     "aaa",
		"sub/b.db": "bbb",
		"notes":    "not-a-db",
	})
	if code := h.run(t, "verify", tgz, "--host", "root@203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "verify member: a.db") || !strings.Contains(out, "verify member: sub/b.db") {
		t.Errorf("member lines missing:\n%s", out)
	}
	if !strings.Contains(out, "OK — every db member passed restore-verify") {
		t.Errorf("OK line missing:\n%s", out)
	}
	stdin, _ := os.ReadFile(h.stdinLog)
	if string(stdin) != "aaabbb" {
		t.Errorf("uploaded members = %q, want a.db then sub/b.db in order", stdin)
	}
	calls := h.sshInvocations(t)
	if got := strings.Count(calls, "/usr/local/bin/restore-verify -db /tmp/kampodra-verify"); got != 2 {
		t.Errorf("restore-verify -db calls = %d, want 2 (one per member):\n%s", got, calls)
	}
	if got := strings.Count(calls, "rm -f /tmp/kampodra-verify"); got != 2 {
		t.Errorf("scratch cleanup calls = %d, want 2:\n%s", got, calls)
	}
}

func TestBackupVerifyMigratedTopologyFlagReachesTheRemoteCall(t *testing.T) {
	h := setupBackup(t)
	dbFile := filepath.Join(t.TempDir(), "dump.db")
	os.WriteFile(dbFile, []byte("db"), 0o600)
	if code := h.run(t, "verify", dbFile, "--host", "root@203.0.113.9", "--migrated-topology"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.sshInvocations(t)
	if !strings.Contains(calls, "restore-verify -db /tmp/kampodra-verify") || !strings.Contains(calls, "--migrated-topology") {
		t.Errorf("--migrated-topology did not reach the remote command:\n%s", calls)
	}
}

// The root db is schema-only and NOT in restore-verify's tenant scope
// (esellar RUNBOOK: "root.db … not in restore-verify scope") — the tgz
// loop must skip it, not fail the whole verify on it.
func TestBackupVerifyTgzSkipsRootDb(t *testing.T) {
	h := setupBackup(t)
	tgz := filepath.Join(t.TempDir(), "bundle.tgz")
	buildFixtureTgz(t, tgz, map[string]string{
		"data/tenants/root/root.db":              "root-schema-bytes",
		"data/tenants/tenant_acme-restaurant.db": "tenant-bytes",
	})
	if code := h.run(t, "verify", tgz, "--host", "root@203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "skipping data/tenants/root/root.db") {
		t.Errorf("root-db skip line missing:\n%s", out)
	}
	calls := h.sshInvocations(t)
	if got := strings.Count(calls, "/usr/local/bin/restore-verify -db /tmp/kampodra-verify"); got != 1 {
		t.Errorf("restore-verify -db calls = %d, want 1 (root.db must NOT be fed to restore-verify):\n%s", got, calls)
	}
	stdin, _ := os.ReadFile(h.stdinLog)
	if string(stdin) != "tenant-bytes" {
		t.Errorf("uploaded members = %q, want ONLY the tenant db", stdin)
	}
	if !strings.Contains(out, "OK — every db member passed restore-verify") {
		t.Errorf("OK line missing:\n%s", out)
	}
}

// A bundle whose ONLY .db member is the schema-only root db verifies ZERO
// databases — the loop must fail closed instead of printing the
// all-members-passed OK line after zero restore-verify calls.
func TestBackupVerifyTgzRootOnlyBundleFailsClosed(t *testing.T) {
	h := setupBackup(t)
	tgz := filepath.Join(t.TempDir(), "bundle.tgz")
	buildFixtureTgz(t, tgz, map[string]string{
		"root/root.db": "root-schema-bytes",
	})
	if code := h.run(t, "verify", tgz, "--host", "root@203.0.113.9"); code != 1 {
		t.Fatalf("exit = %d, want 1 (nothing verified must fail closed)", code)
	}
	if !strings.Contains(h.stderr.String(), "nothing verified — bundle contains only the schema-only root db") {
		t.Errorf("nothing-verified error missing:\n%s", h.stderr.String())
	}
	if out := h.stdout.String(); strings.Contains(out, "OK —") {
		t.Errorf("OK line printed after zero verifications:\n%s", out)
	}
	if !strings.Contains(h.stdout.String(), "skipping root/root.db (root db is schema-only — not in restore-verify scope)") {
		t.Errorf("per-member skip messaging changed:\n%s", h.stdout.String())
	}
	if calls := h.sshInvocations(t); strings.Contains(calls, "restore-verify") {
		t.Errorf("restore-verify must not run for a root-only bundle:\n%s", calls)
	}
}

func TestBackupVerifyRequiresHost(t *testing.T) {
	h := setupBackup(t)
	dbFile := filepath.Join(t.TempDir(), "dump.db")
	os.WriteFile(dbFile, []byte("db"), 0o600)
	if code := h.run(t, "verify", dbFile); code != 1 || !strings.Contains(h.stderr.String(), "verify needs a VM target") {
		t.Errorf("exit=%d stderr=%q", code, h.stderr.String())
	}
}

func TestBackupVerifyRejectsUnknownExtension(t *testing.T) {
	h := setupBackup(t)
	f := filepath.Join(t.TempDir(), "dump.txt")
	os.WriteFile(f, []byte("x"), 0o600)
	if code := h.run(t, "verify", f, "--host", "root@h"); code != 1 || !strings.Contains(h.stderr.String(), "takes a .db (or .sqlite/.sqlite3) or .tgz") {
		t.Errorf("exit=%d stderr=%q", code, h.stderr.String())
	}
}

// verify's ssh leg resolves the INSTANCE profile through the standard
// ladder (--profile > KAMPODRA_PROFILE > config defaultProfile). The
// retired legacy alias flag is REMOVED — passing it is an unknown-flag
// error, never a silent accept.
func TestBackupVerifyResolvesInstanceProfileFromEnv(t *testing.T) {
	h := setupBackup(t)
	os.MkdirAll(filepath.Join(h.deps.Home, ".kampodra"), 0o700)
	cfg := `{"defaultProfile":"","profiles":{"prod":{"host":"root@203.0.113.9","sshKey":"/keys/p"}}}`
	os.WriteFile(filepath.Join(h.deps.Home, ".kampodra", "config.json"), []byte(cfg), 0o600)
	dbFile := filepath.Join(t.TempDir(), "dump.db")
	os.WriteFile(dbFile, []byte("db"), 0o600)

	t.Setenv("KAMPODRA_PROFILE", "prod")
	if code := h.run(t, "verify", dbFile); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "on root@203.0.113.9") {
		t.Errorf("KAMPODRA_PROFILE did not resolve the VM target:\n%s", h.stdout.String())
	}

	if code := h.run(t, "verify", dbFile, "--kampodine-profile", "prod"); code != 1 ||
		!strings.Contains(h.stderr.String(), "unknown flag") {
		t.Errorf("--kampodine-profile must be an unknown flag (exit 1): exit=%d stderr=%q", code, h.stderr.String())
	}
}

// cancelMidVerifyRunner simulates ctrl-C arriving mid-verify: it cancels
// the command context as the restore-verify leg starts, then delegates to
// the real SSHRunner (the PATH ssh shim stays the command log).
type cancelMidVerifyRunner struct {
	inner  transport.Runner
	cancel context.CancelFunc
}

func (r *cancelMidVerifyRunner) Run(ctx context.Context, host transport.HostSpec, remoteCmd string) (string, error) {
	if strings.Contains(remoteCmd, "restore-verify") {
		r.cancel()
	}
	return r.inner.Run(ctx, host, remoteCmd)
}

func (r *cancelMidVerifyRunner) RunWithStdin(ctx context.Context, host transport.HostSpec, remoteCmd string, stdin io.Reader) (string, error) {
	return r.inner.RunWithStdin(ctx, host, remoteCmd, stdin)
}

func (r *cancelMidVerifyRunner) Stream(ctx context.Context, sshArgs []string) (int, error) {
	return r.inner.Stream(ctx, sshArgs)
}

// The scratch rm must survive cancellation: it runs against
// context.Background(), so ctrl-C mid-verify still removes the tenant db
// bytes from the VM — and a cleanup failure must never mask the verdict.
func TestBackupVerifyScratchCleanupSurvivesCancellation(t *testing.T) {
	h := setupBackup(t)
	dbFile := filepath.Join(t.TempDir(), "dump.db")
	os.WriteFile(dbFile, []byte("db-bytes"), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	h.deps.Runner = &cancelMidVerifyRunner{inner: &transport.SSHRunner{}, cancel: cancel}

	root := command.NewRoot("test", h.deps)
	root.SetArgs([]string{"backup", "verify", dbFile, "--host", "root@203.0.113.9"})
	err := root.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "restore-verify FAILED for dump.db") {
		t.Fatalf("err = %v, want the restore-verify verdict (cleanup must not mask it)", err)
	}
	calls := h.sshInvocations(t)
	if !strings.Contains(calls, "rm -f /tmp/kampodra-verify-") {
		t.Errorf("scratch cleanup not attempted after cancellation:\n%s", calls)
	}
	if stderr := h.stderr.String(); strings.Contains(stderr, "WARNING: scratch cleanup failed") {
		t.Errorf("cleanup succeeded — no WARNING expected:\n%s", stderr)
	}
}

func buildFixtureTgz(t *testing.T, path string, members map[string]string) {
	t.Helper()
	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	sort.Strings(names)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for _, name := range names {
		content := members[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
}
