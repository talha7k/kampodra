package command_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// The backup.sh port: OCI object-storage list/download/verify/restore-plan.
// The oci CLI is shimmed on PATH (the exec-shim fixture pattern — no test
// touches real OCI or a real VM); verify's ssh leg rides the ssh shim.

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
		"if [ \"$cmd\" = \"/usr/local/bin/restore-verify /dev/stdin\" ]; then cat >> '" + stdinLog + "'; exit 0; fi\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(sshShim), 0o755)

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_BACKUP_BUCKET", "OCI_PROFILE"} {
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

	if code := h.run(t, "list", "--bucket", "my-bkt", "--prefix", "db/tenants/", "--profile", "oci-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"[backup] objects in bucket my-bkt (prefix: db/tenants/) (profile: oci-prof):",
		"NAME", "SIZE", "UPDATED",
		"db/tenants/acme/20261008T050000Z.db", "1048576", "2026-10-08T05:00:05.000Z",
		"db/tenants/beta/latest.db", "42", "-",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	logged := h.ociInvocations(t)
	if !strings.Contains(logged, "--profile oci-prof") || !strings.Contains(logged, "--bucket-name my-bkt") {
		t.Errorf("oci invocation missing auth/bucket: %s", logged)
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
	out := h.stdout.String()
	for _, want := range []string{
		"== restore plan (print-only — kampodra NEVER executes these steps) ==",
		"object : db/tenants/acme/x.db",
		"bucket : esellar-libsql-backups",
		"kampodra backup download 'db/tenants/acme/x.db' --out /tmp/restore.db",
		"rc-service kampodine-api stop",
		"/data/tenants/<tenant-dir>",
		"kampodra status",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan missing %q:\n%s", want, out)
		}
	}
}

func TestBackupVerifyDbPipesOverSSHStdin(t *testing.T) {
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
	out := h.stdout.String()
	if !strings.Contains(out, "piping "+dbFile+" over ssh stdin to /usr/local/bin/restore-verify on root@203.0.113.9") ||
		!strings.Contains(out, "OK — restore-verify accepted the db image") {
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
		t.Errorf("piped members = %q, want a.db then sub/b.db in order", stdin)
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

func TestBackupVerifyResolvesKampodineProfile(t *testing.T) {
	h := setupBackup(t)
	os.MkdirAll(filepath.Join(h.deps.Home, ".kampodra"), 0o700)
	cfg := `{"defaultProfile":"","profiles":{"prod":{"host":"root@203.0.113.9","sshKey":"/keys/p"}}}`
	os.WriteFile(filepath.Join(h.deps.Home, ".kampodra", "config.json"), []byte(cfg), 0o600)
	dbFile := filepath.Join(t.TempDir(), "dump.db")
	os.WriteFile(dbFile, []byte("db"), 0o600)

	if code := h.run(t, "verify", dbFile, "--kampodine-profile", "prod"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "on root@203.0.113.9") {
		t.Errorf("--kampodine-profile did not resolve the VM target:\n%s", h.stdout.String())
	}
}

func TestBackupDownloadRequiresObject(t *testing.T) {
	h := setupBackup(t)
	if code := h.run(t, "download"); code != 1 || !strings.Contains(h.stderr.String(), "usage: kampodra backup download <object>") {
		t.Errorf("exit=%d stderr=%q", code, h.stderr.String())
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
