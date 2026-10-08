package command_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/command"
)

// Ports of the shell repo's env contract (scripts/env.sh): fingerprints
// NEVER leak values; push uploads verbatim (0600 temp + atomic mv); pull's
// payload goes to --out (0600) or stdout with the masked summary on stderr.
// The exec-shim ssh records invocations and serves the remote env fixture —
// no test touches a real VM.

const envHost = "root@203.0.113.9"

const remoteEnvFixture = "APP_MODE=production\nLEGACY_KEY=ol Drupal 8\nPORT=8080\nSAME_KEY=identical\n"

func setupEnv(t *testing.T) (deps command.Deps, stubDir string, stdout, stderr *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	stubDir = t.TempDir()
	inv := filepath.Join(stubDir, "invocations")
	fixture := filepath.Join(stubDir, "remote-env")
	if err := os.WriteFile(fixture, []byte(remoteEnvFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	uploaded := filepath.Join(stubDir, "uploaded")
	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"cmd=\"${*: -1}\"\n" +
		"case \"$cmd\" in\n" +
		"  \"cat /etc/kampodine/env\") cat '" + fixture + "'; exit 0 ;;\n" +
		"  \"umask 077; cat >\"*) cat > '" + uploaded + "'; exit 0 ;;\n" +
		"  \"chmod 600\"*) exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_PROXY_HOST", "KAMPODRA_ENV_FILE"} {
		t.Setenv(k, "")
	}

	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	return command.Deps{
		Home:   home,
		Env:    systemLookup,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
		Prober: &probe.Prober{HTTPClient: &http.Client{Transport: deadTransport{}}},
	}, stubDir, stdout, stderr
}

func runEnv(t *testing.T, deps command.Deps, args ...string) int {
	t.Helper()
	return command.Execute("test", deps, append([]string{"env"}, args...))
}

func writeLocalEnv(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "local.env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvBareSubcommandPrintsHelp(t *testing.T) {
	deps, _, stdout, _ := setupEnv(t)
	if code := runEnv(t, deps); code != 0 {
		t.Fatalf("exit = %d, want 0 (shell usage exits 0)", code)
	}
	for _, want := range []string{"env list", "env push", "env pull", "env fingerprint", "env diff <local-file>"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("env --help missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestEnvListPrintsFingerprintsNeverValues(t *testing.T) {
	deps, _, stdout, _ := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "list"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"APP_MODE", "len=10", "pr…", "LEGACY_KEY", "PORT", "SAME_KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("env list output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "production") || strings.Contains(out, "8080") {
		t.Errorf("env list leaked a raw value:\n%s", out)
	}
}

func TestEnvListWithoutTargetFailsClosed(t *testing.T) {
	deps, _, _, stderr := setupEnv(t)
	if code := runEnv(t, deps, "list"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "target required") {
		t.Errorf("stderr = %q, want the target-required guidance", stderr.String())
	}
}

func TestEnvListRemoteReadFailureNamesTheRemedy(t *testing.T) {
	deps, stubDir, _, stderr := setupEnv(t)
	// Remote env missing: the shim fails the cat.
	shim := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runEnv(t, deps, "--host", envHost, "list"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cannot read /etc/kampodine/env on "+envHost) {
		t.Errorf("stderr = %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "env push --file") {
		t.Errorf("stderr must name the remedy:\n%s", stderr.String())
	}
}

func TestEnvPushUploadsVerbatimWithAtomicInstall(t *testing.T) {
	deps, stubDir, stdout, _ := setupEnv(t)
	local := writeLocalEnv(t, t.TempDir(), "APP_MODE=staging\nSECRET_KEY=hushhush\n")
	if code := runEnv(t, deps, "--host", envHost, "push", "--file", local); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	inv, err := os.ReadFile(filepath.Join(stubDir, "invocations"))
	if err != nil {
		t.Fatal(err)
	}
	commands := string(inv)
	if !strings.Contains(commands, "umask 077; cat > /etc/kampodine/env.tmp.") {
		t.Errorf("push never created the 0600-from-creation remote temp:\n%s", commands)
	}
	if !strings.Contains(commands, "chmod 600 /etc/kampodine/env.tmp.") ||
		!strings.Contains(commands, "mv -f /etc/kampodine/env.tmp.") ||
		!strings.Contains(commands, "/etc/kampodine/env") {
		t.Errorf("push never performed the atomic install (chmod 600 + mv -f):\n%s", commands)
	}
	uploaded, err := os.ReadFile(filepath.Join(stubDir, "uploaded"))
	if err != nil || string(uploaded) != "APP_MODE=staging\nSECRET_KEY=hushhush\n" {
		t.Errorf("upload stream = %q (err=%v), want the local file verbatim", string(uploaded), err)
	}
	out := stdout.String()
	if !strings.Contains(out, "fingerprint summary") || !strings.Contains(out, "SE") || !strings.Contains(out, "len=8") {
		t.Errorf("push must print the fingerprint summary:\n%s", out)
	}
	if strings.Contains(out, "hushhush") || strings.Contains(out, "SECRET_KEY=hushhush") {
		t.Errorf("push leaked a raw value:\n%s", out)
	}
	if !strings.Contains(out, "installed /etc/kampodine/env (0600) on "+envHost) {
		t.Errorf("push must confirm the install:\n%s", out)
	}
	if !strings.Contains(out, "restart to apply") {
		t.Errorf("push must print the restart hint:\n%s", out)
	}
}

func TestEnvPushRequiresFile(t *testing.T) {
	deps, _, _, stderr := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "push"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "push requires --file") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestEnvPushRefusesNothingToPush(t *testing.T) {
	deps, _, _, stderr := setupEnv(t)
	local := writeLocalEnv(t, t.TempDir(), "# only comments\nno pairs here\n")
	if code := runEnv(t, deps, "--host", envHost, "push", "--file", local); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no KEY=VALUE lines") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestEnvPushMissingLocalFileFails(t *testing.T) {
	deps, _, _, stderr := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "push", "--file", "/nonexistent/env"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no such file: /nonexistent/env") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestEnvPullToOutIs0600AndNeverPrintsPayload(t *testing.T) {
	deps, _, stdout, _ := setupEnv(t)
	outPath := filepath.Join(t.TempDir(), "snapshot.env")
	if code := runEnv(t, deps, "--host", envHost, "pull", "--out", outPath); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	data, err := os.ReadFile(outPath)
	if err != nil || string(data) != remoteEnvFixture {
		t.Fatalf("--out payload = %q (err=%v), want the remote file verbatim", string(data), err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("--out perms = %o, want 600", perm)
	}
	out := stdout.String()
	if strings.Contains(out, "APP_MODE=production") {
		t.Errorf("pull --out leaked the payload on stdout:\n%s", out)
	}
	if !strings.Contains(out, "wrote "+outPath+" (0600)") || !strings.Contains(out, "len=10") {
		t.Errorf("pull --out must print the masked summary:\n%s", out)
	}
}

func TestEnvPullWithoutOutStreamsPayloadToStdout(t *testing.T) {
	deps, _, stdout, stderr := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "pull"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got := stdout.String(); got != remoteEnvFixture {
		t.Errorf("stdout = %q, want the raw payload (exactly one trailing newline)", got)
	}
	if !strings.Contains(stderr.String(), "fingerprint summary for /etc/kampodine/env on "+envHost) {
		t.Errorf("stderr = %q, want the masked summary", stderr.String())
	}
	if strings.Contains(stderr.String(), "APP_MODE=production") {
		t.Errorf("stderr leaked the payload:\n%s", stderr.String())
	}
}

func TestEnvFingerprintLocalFile(t *testing.T) {
	deps, _, stdout, _ := setupEnv(t)
	local := writeLocalEnv(t, t.TempDir(), "A=hello\n")
	if code := runEnv(t, deps, "fingerprint", "--file", local); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "A") || !strings.Contains(out, "len=5") || !strings.Contains(out, "he…") {
		t.Errorf("fingerprint output = %q", out)
	}
	if strings.Contains(out, "hello") {
		t.Errorf("fingerprint leaked the value:\n%s", out)
	}
}

func TestEnvDiffIdenticalExitsZero(t *testing.T) {
	deps, _, stdout, _ := setupEnv(t)
	local := writeLocalEnv(t, t.TempDir(), remoteEnvFixture)
	if code := runEnv(t, deps, "--host", envHost, "diff", local); code != 0 {
		t.Fatalf("exit = %d, want 0 for identical envs", code)
	}
	if !strings.Contains(stdout.String(), "== env diff: ") {
		t.Errorf("missing the diff header:\n%s", stdout.String())
	}
}

func TestEnvDiffClassifiesAndNeverLeaksValues(t *testing.T) {
	deps, _, stdout, _ := setupEnv(t)
	local := writeLocalEnv(t, t.TempDir(), "APP_MODE=staging\nNEW_KEY=brand new\nPORT=9090\nSAME_KEY=identical\n")
	if code := runEnv(t, deps, "--host", envHost, "diff", local); code != 1 {
		t.Fatalf("exit = %d, want 1 when the envs differ (GNU diff convention)", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"== env diff: " + local + " vs " + envHost + ":/etc/kampodine/env ==",
		"+ NEW_KEY",
		"- LEGACY_KEY",
		"~ APP_MODE",
		"~ PORT",
		"(local: len=7       st…)",
		"(remote: len=10      pr…)",
		"== env diff summary: 1 added, 1 removed, 2 changed, 1 unchanged ==",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff output missing %q:\n%s", want, out)
		}
	}
	for _, leaked := range []string{"staging", "9090", "brand new", "production", "ol Drupal"} {
		if strings.Contains(out, leaked) {
			t.Errorf("diff leaked the raw value %q:\n%s", leaked, out)
		}
	}
}

func TestEnvDiffRequiresExactlyOneArg(t *testing.T) {
	deps, _, _, stderr := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "diff"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "diff requires exactly one <local-file>") {
		t.Errorf("stderr = %q", stderr.String())
	}
	extra := writeLocalEnv(t, t.TempDir(), "A=1\n")
	if code := runEnv(t, deps, "--host", envHost, "diff", extra, extra); code != 1 {
		t.Fatalf("two-arg diff: exit = %d, want 1", code)
	}
}

func TestEnvDiffMissingLocalFileFails(t *testing.T) {
	deps, _, _, stderr := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "diff", "/nonexistent/env"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no such file: /nonexistent/env") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
