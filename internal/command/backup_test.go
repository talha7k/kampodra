package command_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/command"
)

// The backup.sh port: OCI object-storage list/download/verify/restore-plan.
// The oci CLI is shimmed on PATH (the exec-shim fixture pattern — no test
// touches real OCI or a real VM); verify's ssh leg rides the ssh shim.
// Verify-flow tests live in backup_verify_test.go, the namespace ladder in
// backup_namespace_test.go.

const backupFixtureBody = "kampodra backup download fixture payload\n"

type backupHarness struct {
	deps     command.Deps
	fixtures string
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	ociLog   string
	sshLog   string
	stdinLog string
}

func setupBackup(t *testing.T) *backupHarness {
	t.Helper()
	home := t.TempDir()
	stubDir := t.TempDir()
	fixtures := t.TempDir()
	ociLog := filepath.Join(stubDir, "oci-calls.log")
	sshLog := filepath.Join(stubDir, "ssh-calls.log")
	stdinLog := filepath.Join(stubDir, "ssh-stdin.log")
	contentPath := filepath.Join(fixtures, "body")
	os.WriteFile(contentPath, []byte(backupFixtureBody), 0o600)

	ociShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + ociLog + "'\n" +
		"if [ \"$1 $2\" = \"os ns\" ]; then\n" +
		"  if [ -f \"" + fixtures + "/deny-ns\" ]; then echo 'ServiceError: NotAuthorizedOrNotFound' >&2; exit 1; fi\n" +
		"  printf '{\"data\":\"test-ns\"}'; exit 0\n" +
		"fi\n" +
		"if [ -f \"" + fixtures + "/deny\" ]; then echo 'ServiceError: NotAuthorizedOrNotFound' >&2; exit 1; fi\n" +
		"if [ \"$1 $2\" != \"os object\" ]; then exit 0; fi\n" +
		"case \"$3\" in\n" +
		"  list) [ -f \"" + fixtures + "/list.json\" ] && cat \"" + fixtures + "/list.json\" || echo '{\"data\":{}}'; exit 0 ;;\n" +
		"  head) [ -f \"" + fixtures + "/head.json\" ] && cat \"" + fixtures + "/head.json\" || echo '{}'; exit 0 ;;\n" +
		"  get)\n" +
		"    prev=\"\"; for a in \"$@\"; do if [ \"$prev\" = \"--file\" ]; then cp \"" + contentPath + "\" \"$a\"; fi; prev=\"$a\"; done\n" +
		"    exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stubDir, "oci"), []byte(ociShim), 0o755)

	sshShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + sshLog + "'\n" +
		"cmd=\"${*: -1}\"\n" +
		"case \"$cmd\" in\n" +
		"  *\"cat > \"/tmp/kampodra-verify-*) cat >> '" + stdinLog + "'; exit 0 ;;\n" +
		"  *restore-verify*) exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(sshShim), 0o755)

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_BUCKET", "OCI_PROFILE"} {
		t.Setenv(k, "")
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &backupHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
		},
		fixtures: fixtures,
		stdout:   stdout,
		stderr:   stderr,
		ociLog:   ociLog,
		sshLog:   sshLog,
		stdinLog: stdinLog,
	}
}

func (h *backupHarness) run(t *testing.T, args ...string) int {
	t.Helper()
	return command.Execute("test", h.deps, append([]string{"backup"}, args...))
}

func (h *backupHarness) ociInvocations(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.ociLog)
	if err != nil {
		return ""
	}
	return string(data)
}

func (h *backupHarness) sshInvocations(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.sshLog)
	if err != nil {
		return ""
	}
	return string(data)
}

const backupListFixture = `{"data":{"objects":[
  {"name":"db/tenants/acme/20261008T050000Z.db","size":"1048576","timeCreated":"2026-10-08T05:00:05.000Z"},
  {"name":"db/tenants/beta/latest.db","size":"42","timeCreated":null}
]}}`

func fixtureSHA256() string {
	sum := sha256.Sum256([]byte(backupFixtureBody))
	return hex.EncodeToString(sum[:])
}

func TestBackupListRendersTable(t *testing.T) {
	h := setupBackup(t)
	os.WriteFile(filepath.Join(h.fixtures, "list.json"), []byte(backupListFixture), 0o600)

	if code := h.run(t, "list", "--bucket", "my-bkt", "--prefix", "db/tenants/"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"[backup] objects in bucket my-bkt (prefix: db/tenants/) (profile: oci-cli default):",
		"NAME", "SIZE", "UPDATED",
		"db/tenants/acme/20261008T050000Z.db", "1048576", "2026-10-08T05:00:05.000Z",
		"db/tenants/beta/latest.db", "42", "-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	logged := h.ociInvocations(t)
	if strings.Contains(logged, "--profile") || !strings.Contains(logged, "--bucket-name my-bkt") {
		t.Errorf("native resolution must pass no auth flags (bucket must reach): %s", logged)
	}
}

func TestBackupListAuthDeniedPrintsPolicyGuidance(t *testing.T) {
	h := setupBackup(t)
	os.WriteFile(filepath.Join(h.fixtures, "deny"), []byte("denied"), 0o600)

	if code := h.run(t, "list", "--bucket", "my-bkt"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	errOut := h.stderr.String()
	for _, want := range []string{
		"LIST denied",
		"lacks INSPECT+READ on bucket my-bkt",
		"Allow dynamic-group",
		"GET works without LIST once the object name is known",
		"kampodra backup download <object> --bucket my-bkt",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("policy guidance missing %q:\n%s", want, errOut)
		}
	}
}

func TestBackupListEmpty(t *testing.T) {
	h := setupBackup(t)
	if code := h.run(t, "list"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "[backup] (no objects)") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestBackupDownloadVerifiesDigest(t *testing.T) {
	h := setupBackup(t)
	out := filepath.Join(t.TempDir(), "restore.db")
	os.WriteFile(filepath.Join(h.fixtures, "head.json"), []byte(
		`{"data":{"opc-meta-sha256":"`+fixtureSHA256()+`"}}`), 0o600)

	if code := h.run(t, "download", "db/tenants/acme/x.db", "--out", out, "--bucket", "my-bkt"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != backupFixtureBody {
		t.Fatalf("downloaded file wrong: %v %q", err, data)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Errorf("downloaded perm = %o, want 600", fi.Mode().Perm())
	}
	outStr := h.stdout.String()
	if !strings.Contains(outStr, fmt.Sprintf("downloaded db/tenants/acme/x.db -> %s (0600, %d bytes)", out, len(backupFixtureBody))) {
		t.Errorf("download line missing:\n%s", outStr)
	}
	if !strings.Contains(outStr, "integrity: verified (sha256)") {
		t.Errorf("integrity line missing:\n%s", outStr)
	}
	if !strings.Contains(outStr, "sha256: "+fixtureSHA256()[:16]+"…") {
		t.Errorf("fingerprint line missing:\n%s", outStr)
	}
}

func TestBackupDownloadDigestMismatchFailsClosed(t *testing.T) {
	h := setupBackup(t)
	out := filepath.Join(t.TempDir(), "restore.db")
	os.WriteFile(filepath.Join(h.fixtures, "head.json"), []byte(
		`{"data":{"opc-meta-sha256":"deadbeef"}}`), 0o600)

	if code := h.run(t, "download", "db/x.db", "--out", out); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	errOut := h.stderr.String()
	if !strings.Contains(errOut, "digest MISMATCH (sha256)") ||
		!strings.Contains(errOut, "expected: deadbeef") ||
		!strings.Contains(errOut, "got     : "+fixtureSHA256()) {
		t.Errorf("mismatch report wrong:\n%s", errOut)
	}
}

func TestBackupDownloadSkipsIntegrityWithoutMetadata(t *testing.T) {
	h := setupBackup(t)
	out := filepath.Join(t.TempDir(), "restore.db")
	if code := h.run(t, "download", "db/x.db", "--out", out); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "integrity: skipped (no digest metadata on the object)") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestBackupRestorePlanPrintsOnly(t *testing.T) {
	h := setupBackup(t)
	if code := h.run(t, "restore-plan", "db/tenants/acme/x.db"); code != 0 {
		t.Fatal("restore-plan must succeed")
	}
	if logged := h.ociInvocations(t); strings.TrimSpace(logged) != "" {
		t.Errorf("restore-plan invoked oci: %s", logged)
	}
	// The plan renders the project config's own naming — derived from the
	// project defaults, never hardcoded here (project.go is the ONE source).
	def := project.LoadDefault()
	out := h.stdout.String()
	for _, want := range []string{
		"== restore plan (print-only — kampodra NEVER executes these steps) ==",
		"object : db/tenants/acme/x.db",
		"bucket : " + def.Bucket,
		"kampodra backup download 'db/tenants/acme/x.db' --out /tmp/restore.db",
		"rc-service " + def.Container + " stop",
		def.DataDir + "/<tenant-dir>",
		"kampodra status",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q:\n%s", want, out)
		}
	}
}

// The bucket ladder: --bucket > KAMPODRA_BUCKET (via the project ladder) >
// the project config's bucket. The retired backup-specific env name is gone:
// setting it selects nothing.
func TestBackupListBucketFromProjectLadderEnv(t *testing.T) {
	h := setupBackup(t)
	t.Setenv("KAMPODRA_BUCKET", "env-bkt")
	if code := h.run(t, "list"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "[backup] objects in bucket env-bkt") {
		t.Errorf("KAMPODRA_BUCKET did not reach the bucket resolution:\n%s", h.stdout.String())
	}

	// The duplicate env name is DEAD — it must not select the bucket anymore.
	h = setupBackup(t)
	t.Setenv("KAMPODRA_BACKUP_BUCKET", "legacy-bkt")
	if code := h.run(t, "list"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	def := project.LoadDefault()
	if !strings.Contains(h.stdout.String(), "[backup] objects in bucket "+def.Bucket) {
		t.Errorf("KAMPODRA_BACKUP_BUCKET removal changed the default resolution:\n%s", h.stdout.String())
	}
	if strings.Contains(h.stdout.String(), "legacy-bkt") {
		t.Errorf("the retired KAMPODRA_BACKUP_BUCKET env name still selects the bucket:\n%s", h.stdout.String())
	}
}

// The instance profile's `cloud` block selects the OCI CONFIG profile and
// drives instance-principal auth; the instance `--profile` itself never
// reaches the oci CLI; without any of it, resolution is native.
func TestBackupCloudBlockSelectsConfigProfile(t *testing.T) {
	h := setupBackup(t)
	os.MkdirAll(filepath.Join(h.deps.Home, ".kampodra"), 0o700)
	cfg := `{"defaultProfile":"","profiles":{"inst-prof":{"host":"root@203.0.113.9","sshKey":"/keys/p"}}}`
	os.WriteFile(filepath.Join(h.deps.Home, ".kampodra", "config.json"), []byte(cfg), 0o600)
	if code := h.run(t, "list", "--bucket", "b", "--profile", "inst-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if strings.Contains(h.ociInvocations(t), "--profile inst-prof") {
		t.Errorf("--profile (instance profile) leaked into the oci CLI call:\n%s", h.ociInvocations(t))
	}

	h = setupBackup(t)
	os.MkdirAll(filepath.Join(h.deps.Home, ".kampodra"), 0o700)
	cfg = `{"defaultProfile":"","profiles":{"inst-prof":{"host":"root@203.0.113.9","sshKey":"/keys/p","cloud":{"profile":"myprof"}}}}`
	os.WriteFile(filepath.Join(h.deps.Home, ".kampodra", "config.json"), []byte(cfg), 0o600)
	if code := h.run(t, "list", "--bucket", "b", "--profile", "inst-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.ociInvocations(t), "--profile myprof") {
		t.Errorf("cloud-block profile did not select the OCI config profile:\n%s", h.ociInvocations(t))
	}
}

func TestBackupDownloadRequiresObject(t *testing.T) {
	h := setupBackup(t)
	if code := h.run(t, "download"); code != 1 || !strings.Contains(h.stderr.String(), "usage: kampodra backup download <object>") {
		t.Errorf("exit=%d stderr=%q", code, h.stderr.String())
	}
}
