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

// kampodra ssh — host-level passthrough through the profile's host/key
// (kampodra-native; no command = interactive login, otherwise the argv
// passes through verbatim and ssh's exit code propagates).

func setupSSH(t *testing.T) (deps command.Deps, stubDir string, stdout, stderr *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	stubDir = t.TempDir()
	inv := filepath.Join(stubDir, "invocations")
	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"exit 7\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_PROXY_HOST"} {
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

func TestSSHNoCommandIsInteractiveLogin(t *testing.T) {
	deps, stubDir, _, _ := setupSSH(t)
	if code := command.Execute("test", deps, []string{"ssh", "--host", statusHost}); code != 7 {
		t.Fatalf("exit = %d, want 7 (ssh's exit code propagates)", code)
	}
	inv := invocations(t, stubDir)
	// Login shell: forced tty, host, NO remote command vector.
	want := "-o ConnectTimeout=10 -o BatchMode=yes -t " + statusHost
	if !strings.Contains(inv, want) {
		t.Errorf("interactive login argv missing %q:\n%s", want, inv)
	}
	// The composed-command shapes must NOT appear (nothing after the host).
	if strings.Contains(inv, statusHost+" ") {
		t.Errorf("login must not carry a remote command:\n%s", inv)
	}
}

func TestSSHPassthroughArgvVerbatim(t *testing.T) {
	deps, stubDir, _, _ := setupSSH(t)
	code := command.Execute("test", deps, []string{"ssh", "--host", statusHost, "df", "-h", "/var/lib/containers"})
	if code != 7 {
		t.Fatalf("exit = %d, want 7 (ssh's exit code propagates)", code)
	}
	inv := invocations(t, stubDir)
	want := "-o ConnectTimeout=10 -o BatchMode=yes " + statusHost + " df -h /var/lib/containers"
	if !strings.Contains(inv, want) {
		t.Errorf("passthrough argv missing %q:\n%s", want, inv)
	}
	if strings.Contains(inv, "-t") {
		t.Errorf("one-shot passthrough must not force a tty:\n%s", inv)
	}
}

func TestSSHResolvesThroughProfile(t *testing.T) {
	deps, stubDir, _, _ := setupSSH(t)
	cfgDir := filepath.Join(deps.Home, ".kampodra")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"defaultProfile": "prod", "profiles": {"prod": {"host": "` + statusHost + `", "sshKey": "/tmp/profile-key"}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := command.Execute("test", deps, []string{"ssh", "uptime"}); code != 7 {
		t.Fatalf("exit = %d, want 7", code)
	}
	inv := invocations(t, stubDir)
	if !strings.Contains(inv, "-i /tmp/profile-key "+statusHost+" uptime") {
		t.Errorf("profile host/key must feed the passthrough:\n%s", inv)
	}
}

func TestSSHWithoutTargetFails(t *testing.T) {
	deps, _, _, stderr := setupSSH(t)
	if code := command.Execute("test", deps, []string{"ssh"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "target required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
