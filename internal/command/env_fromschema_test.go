package command_test

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// env from-schema (kampodra-native): generate the env file from the repo's
// COMMITTED .env.schema via the repo's varlock (pass() refs resolve from
// the pass store — values NEVER echo; fingerprints only), then either write
// it locally (--out, 0600) or push it to the VM through the env family's
// 0600-from-creation + atomic-mv flow.

func setupFromSchema(t *testing.T, varlockOutput string) (command.Deps, *bytes.Buffer, *bytes.Buffer, string, string) {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	repo := t.TempDir()

	// fake repo toolchain: node_modules/.bin/varlock
	binDir := filepath.Join(repo, "node_modules", ".bin")
	os.MkdirAll(binDir, 0o755)
	varlockOut := filepath.Join(stub, "varlock-output")
	os.WriteFile(varlockOut, []byte(varlockOutput), 0o600)
	varlockShim := "#!/bin/sh\ncat " + quote(varlockOut) + "\n"
	os.WriteFile(filepath.Join(binDir, "varlock"), []byte(varlockShim), 0o755)

	// git shim: rev-parse answers the repo root; archive serves a fixture
	// tar containing apps/api/.env.schema
	gitLog := filepath.Join(stub, "git.log")
	tarPath := filepath.Join(stub, "schema.tar")
	writeTarFixture(t, tarPath, "apps/api/.env.schema", "FOO=schema\n")
	gitShim := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + quote(gitLog) + "\n" +
		"if [ \"$3\" = \"rev-parse\" ]; then echo " + quote(repo) + "; exit 0; fi\n" +
		"if [ \"$3\" = \"archive\" ]; then cat " + quote(tarPath) + "; exit 0; fi\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "git"), []byte(gitShim), 0o755)

	// ssh shim: capture the env upload stream
	uploaded := filepath.Join(stub, "uploaded")
	sshShim := "#!/bin/bash\n" +
		"cmd=\"${*: -1}\"\n" +
		"case \"$cmd\" in\n" +
		"  \"umask 077; cat >\"*) cat > " + quote(uploaded) + "; exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	os.WriteFile(filepath.Join(stub, "ssh"), []byte(sshShim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_ENV_CLEAR_KEYS"} {
		t.Setenv(k, "")
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	deps := command.Deps{
		Home:   home,
		Env:    systemLookup,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
	}
	return deps, stdout, stderr, repo, uploaded
}

const fromSchemaVarlockFixture = `# varlock env output
API_SESSION_SECRET="s3cret-value"
LIBSQL_URL="file:/data/tenants/x.db"
NODE_ENV=production
PORT=8080
LIBSQL_TENANT_DIR=/data/tenants
LIBSQL_API_MOUNT=1
STATIC_SPA_MOUNT=1
`

func wantFromSchemaEnv() string {
	return "API_SESSION_SECRET=s3cret-value\nLIBSQL_URL=file:/data/tenants/x.db\n"
}

func TestEnvFromSchemaWritesLocalOut(t *testing.T) {
	deps, stdout, stderr, repo, _ := setupFromSchema(t, fromSchemaVarlockFixture)
	out := filepath.Join(t.TempDir(), "generated.env")

	if code := command.Execute("test", deps, []string{"env", "from-schema", "--repo", repo, "--out", out}); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("out file: %v", err)
	}
	if string(data) != wantFromSchemaEnv() {
		t.Errorf("generated env = %q, want %q", data, wantFromSchemaEnv())
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Errorf("out perm = %o, want 600", fi.Mode().Perm())
	}
	// fingerprints only — the secret VALUE never prints
	if strings.Contains(stdout.String(), "s3cret-value") {
		t.Errorf("stdout leaked a secret value:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "API_SESSION_SECRET") {
		t.Errorf("fingerprint table missing:\n%s", stdout.String())
	}
}

func TestEnvFromSchemaPushesThroughEnvFamily(t *testing.T) {
	deps, stdout, stderr, repo, uploaded := setupFromSchema(t, fromSchemaVarlockFixture)

	if code := command.Execute("test", deps, []string{"env", "from-schema", "--repo", repo,
		"--host", "root@203.0.113.9"}); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	data, err := os.ReadFile(uploaded)
	if err != nil {
		t.Fatalf("upload captured nothing: %v", err)
	}
	if string(data) != wantFromSchemaEnv() {
		t.Errorf("pushed env = %q, want %q", data, wantFromSchemaEnv())
	}
	if strings.Contains(stdout.String(), "s3cret-value") {
		t.Errorf("push leaked a secret value:\n%s", stdout.String())
	}
}

func TestEnvFromSchemaVarlockFailureIsFatal(t *testing.T) {
	deps, _, stderr, repo, _ := setupFromSchema(t, "")
	// no varlock output → no usable lines → the documented failure
	if code := command.Execute("test", deps, []string{"env", "from-schema", "--repo", repo, "--out", filepath.Join(t.TempDir(), "x.env")}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "varlock env generation failed") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestEnvFromSchemaNeedsRepo(t *testing.T) {
	home := t.TempDir()
	stub := t.TempDir()
	// git shim that FAILS rev-parse — not a repo
	gitShim := "#!/bin/sh\nif [ \"$3\" = \"rev-parse\" ]; then echo 'fatal: not a git repository' >&2; exit 128; fi\nexit 0\n"
	os.WriteFile(filepath.Join(stub, "git"), []byte(gitShim), 0o755)
	t.Setenv("PATH", stub+":/usr/bin:/bin")

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	deps := command.Deps{Home: home, Env: systemLookup, Stdout: stdout, Stderr: stderr, Stdin: strings.NewReader("")}
	if code := command.Execute("test", deps, []string{"env", "from-schema", "--out", filepath.Join(t.TempDir(), "x.env")}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func quote(s string) string { return "'" + s + "'" }

func writeTarFixture(t *testing.T, path, name, content string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, buf.Bytes(), 0o600)
}
