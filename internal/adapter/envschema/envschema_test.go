package envschema

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The deploy.sh varlock block, as testable pieces: the committed schema is
// extracted from git archive output into a scratch dir INSIDE the repo
// (plugin resolution needs node_modules; local .env/.env.local never enter
// the pipeline), varlock resolves it, and the raw output is filtered into a
// podman --env-file (values unquoted, rc-script-owned keys dropped).

func TestBuildEnvFileFiltersAndUnquotes(t *testing.T) {
	varlockOut := strings.Join([]string{
		"# varlock env output",
		"export LINE_IGNORED=1",
		"API_SESSION_SECRET=\"hunter2-do-not-echo\"",
		"LIBSQL_URL=\"file:/data/tenants/x.db\"",
		"NODE_ENV=production",
		"PORT=8080",
		"LIBSQL_TENANT_DIR=/data/tenants",
		"LIBSQL_API_MOUNT=1",
		"STATIC_SPA_MOUNT=1",
		"PLAIN=123",
		"garbage line without equals",
		"",
	}, "\n")
	clearKeys := []string{"NODE_ENV", "PORT", "LIBSQL_TENANT_DIR", "LIBSQL_API_MOUNT", "STATIC_SPA_MOUNT"}
	got := BuildEnvFile(varlockOut, clearKeys)
	want := strings.Join([]string{
		"API_SESSION_SECRET=hunter2-do-not-echo",
		"LIBSQL_URL=file:/data/tenants/x.db",
		"PLAIN=123",
		"",
	}, "\n")
	if got != want {
		t.Errorf("BuildEnvFile() =\n%q\nwant\n%q", got, want)
	}
	// The rc-script-owned keys are gone entirely; secrets never double-print.
	if strings.Contains(got, "NODE_ENV") || strings.Contains(got, `"`) {
		t.Errorf("clear keys or quotes leaked:\n%s", got)
	}
}

func TestBuildEnvFileEmptyOutputIsErrorCondition(t *testing.T) {
	if got := BuildEnvFile("# nothing\n\n", nil); got != "" {
		t.Errorf("BuildEnvFile() = %q, want empty", got)
	}
}

func buildSchemaTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := "FOO=bar\n"
	if err := tw.WriteHeader(&tar.Header{Name: "apps/api/.env.schema", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarIntoScratch(t *testing.T) {
	dir := t.TempDir()
	if err := ExtractTar(buildSchemaTar(t), dir); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "apps/api/.env.schema"))
	if err != nil || string(data) != "FOO=bar\n" {
		t.Fatalf("extracted file = %q, %v", data, err)
	}
}

func TestExtractTarRefusesTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "../../../etc/escape", Mode: 0o600, Size: 2})
	_, _ = tw.Write([]byte("no"))
	_ = tw.Close()
	dir := t.TempDir()
	if err := ExtractTar(buf.Bytes(), dir); err == nil {
		t.Fatal("traversal member must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "etc", "escape")); err == nil {
		t.Error("traversal member escaped the scratch dir")
	}
}

func TestGitArchiveRecordsAndServesFixture(t *testing.T) {
	stub := t.TempDir()
	fixture := buildSchemaTar(t)
	fixturePath := filepath.Join(stub, "fixture.tar")
	os.WriteFile(fixturePath, fixture, 0o600)
	gitLog := filepath.Join(stub, "git.log")
	shim := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + quoteGit(gitLog) + "\n" +
		"cat " + quoteGit(fixturePath) + "\n"
	os.WriteFile(filepath.Join(stub, "git"), []byte(shim), 0o755)
	t.Setenv("PATH", stub+":/usr/bin:/bin")

	repo := t.TempDir()
	data, err := GitArchive(t.Context(), repo, "apps/api/.env.schema")
	if err != nil {
		t.Fatalf("GitArchive: %v", err)
	}
	if !bytes.Equal(data, fixture) {
		t.Fatal("GitArchive did not return the fixture bytes")
	}
	logged, _ := os.ReadFile(gitLog)
	if !strings.Contains(string(logged), "archive HEAD apps/api/.env.schema") {
		t.Errorf("git invocation wrong: %s", logged)
	}
}

func TestRunVarlockExecutesRepoBinaryInScratch(t *testing.T) {
	repo := t.TempDir()
	scratch := t.TempDir()
	binDir := filepath.Join(repo, "node_modules", ".bin")
	os.MkdirAll(binDir, 0o755)
	pwdLog := filepath.Join(scratch, "pwd.log")
	shim := "#!/bin/sh\npwd > " + quoteGit(pwdLog) + "\necho 'SECRET_A=\"one\"'\necho 'SECRET_B=\"two\"'\n"
	os.WriteFile(filepath.Join(binDir, "varlock"), []byte(shim), 0o755)

	out, err := RunVarlock(repo, scratch, "apps/api")
	if err != nil {
		t.Fatalf("RunVarlock: %v", err)
	}
	if !strings.Contains(out, "SECRET_A") {
		t.Errorf("varlock output = %q", out)
	}
	pwd, _ := os.ReadFile(pwdLog)
	if got := strings.TrimSpace(string(pwd)); got != scratch {
		t.Errorf("varlock ran in %q, want scratch %q", got, scratch)
	}
}

func TestRunVarlockFailsWhenBinaryMissing(t *testing.T) {
	repo := t.TempDir()
	if _, err := RunVarlock(repo, t.TempDir(), "apps/api"); err == nil {
		t.Fatal("missing repo varlock binary must fail (schema-only deploys need the repo toolchain)")
	}
}

func quoteGit(s string) string { return "'" + s + "'" }
