package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// vm-wipe (kampodra-native): stops + disables the project's services,
// removes the project containers, prunes images, and removes the env file /
// deployed-sha stamp / state dirs on the VM. --yes gates every mutation;
// --keep-data preserves the data dir; a host whose services don't match the
// profile is refused unless --force.

type wipeHarness struct {
	deps   command.Deps
	stub   string
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	calls  func(t *testing.T) []string
}

func setupWipe(t *testing.T, hostState string) *wipeHarness {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	inv := filepath.Join(stub, "invocations")
	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"cmd=\"${*: -1}\"\n" +
		hostState +
		"\nexit 0\n"
	os.WriteFile(filepath.Join(stub, "ssh"), []byte(shim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE"} {
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
	calls := func(t *testing.T) []string {
		t.Helper()
		data, err := os.ReadFile(inv)
		if err != nil {
			return nil
		}
		var out []string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) != "" {
				out = append(out, line)
			}
		}
		return out
	}
	return &wipeHarness{deps: deps, stub: stub, stdout: stdout, stderr: stderr, calls: calls}
}

// healthyHostState answers the detection probes like a prepared Alpine VM
// running the project stack.
const healthyHostState = `case "$cmd" in
  "if command -v rc-service"*) echo openrc; echo httpc=wget; exit 0 ;;
  "podman ps -a --format '{{.Names}}'") printf 'app\nkamal-proxy\n'; exit 0 ;;
  "for s in"*) echo "app: running"; echo "kamal-proxy: running"; exit 0 ;;
esac`

func TestVMWipeRequiresYes(t *testing.T) {
	h := setupWipe(t, healthyHostState)
	before := len(h.calls(t))
	if code := command.Execute("test", h.deps, []string{"vm-wipe", "--host", "root@203.0.113.9"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "--yes") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if after := len(h.calls(t)); after != before {
		t.Errorf("vm-wipe without --yes touched the host (%d calls)", after-before)
	}
}

func TestVMWipeFullSequence(t *testing.T) {
	h := setupWipe(t, healthyHostState)
	if code := command.Execute("test", h.deps, []string{"vm-wipe", "--host", "root@203.0.113.9", "--yes"}); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.calls(t)
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"rc-service app stop",
		"rc-service kamal-proxy stop",
		"rc-update del app default",
		"rc-update del kamal-proxy default",
		"podman rm -f app",
		"podman rm -f kamal-proxy",
		"podman image prune -a -f",
		"rm -f /etc/kampodra/env",
		"rm -f /etc/kampodra/deployed-sha",
		"rm -rf /data",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("wipe sequence missing %q:\n%s", want, joined)
		}
	}
	// The env dir itself is NOT wiped: it may hold other services' files
	// (monitoring agents) — managed files go individually, the dir stays.
	if strings.Contains(joined, "rm -rf /etc/kampodra") {
		t.Errorf("wipe must not rm -rf the env dir (collateral lives there):\n%s", joined)
	}
	if !strings.Contains(h.stdout.String(), "left in place (not kampodra-managed): /etc/kampodra/") {
		t.Errorf("wipe must name the left-in-place env dir:\n%s", h.stdout.String())
	}
	// Unprofiled runs print <none>, never an empty paren.
	if !strings.Contains(h.stdout.String(), "(profile: <none>)") {
		t.Errorf("unprofiled wipe must print (profile: <none>):\n%s", h.stdout.String())
	}
	// No legacy third service: the roll call is exactly the project
	// default's services (app + kamal-proxy).
	if strings.Contains(joined, "walshipper") {
		t.Errorf("wipe touched a service outside the project services:\n%s", joined)
	}
	// Safety ordering: services stop BEFORE containers are removed; files
	// and the data dir go LAST (after the containers are down).
	idx := func(substr string) int {
		for i, c := range calls {
			if strings.Contains(c, substr) {
				return i
			}
		}
		return -1
	}
	if idx("rc-service app stop") > idx("podman rm -f app") {
		t.Error("service stop must precede container removal")
	}
	if idx("podman image prune -a -f") < idx("podman rm -f kamal-proxy") {
		t.Error("image prune must come after container removal")
	}
	if idx("rm -rf /data") < idx("podman rm -f app") {
		t.Error("data dir removal must come after containers are down")
	}
	// The summary names everything removed.
	out := h.stdout.String()
	for _, want := range []string{"removed:", "app", "/etc/kampodra/env", "/data"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestVMWipeKeepDataPreservesDataDir(t *testing.T) {
	h := setupWipe(t, healthyHostState)
	if code := command.Execute("test", h.deps, []string{"vm-wipe", "--host", "root@203.0.113.9", "--yes", "--keep-data"}); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.calls(t), "\n")
	if strings.Contains(joined, "rm -rf /data") {
		t.Errorf("--keep-data must not touch the data dir:\n%s", joined)
	}
	if !strings.Contains(h.stdout.String(), "kept: /data") {
		t.Errorf("summary must record the kept data dir:\n%s", h.stdout.String())
	}
}

func TestVMWipeRefusesUnrelatedHost(t *testing.T) {
	unrelatedState := `case "$cmd" in
  "if command -v rc-service"*) echo openrc; echo httpc=wget; exit 0 ;;
  "podman ps -a --format '{{.Names}}'") printf 'unrelated-thing\n'; exit 0 ;;
  "for s in"*) echo "nginx: running"; exit 0 ;;
esac`
	h := setupWipe(t, unrelatedState)
	if code := command.Execute("test", h.deps, []string{"vm-wipe", "--host", "root@203.0.113.9", "--yes"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "no kampodra services found") || !strings.Contains(h.stderr.String(), "--force") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	for _, c := range h.calls(t) {
		for _, destructive := range []string{"stop", "disable", "rm -f", "rm -rf", "prune"} {
			if strings.Contains(c, destructive) {
				t.Errorf("refused wipe ran a destructive command (%q): %s", destructive, c)
			}
		}
	}

	// --force overrides the refusal.
	h2 := setupWipe(t, unrelatedState)
	if code := command.Execute("test", h2.deps, []string{"vm-wipe", "--host", "root@203.0.113.9", "--yes", "--force"}); code != 0 {
		t.Fatalf("forced exit = %d, stderr: %s", code, h2.stderr.String())
	}
	if !strings.Contains(strings.Join(h2.calls(t), "\n"), "podman image prune -a -f") {
		t.Error("forced wipe must proceed with the full sequence")
	}
}

func TestVMWipeRequiresHost(t *testing.T) {
	h := setupWipe(t, healthyHostState)
	if code := command.Execute("test", h.deps, []string{"vm-wipe", "--yes"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "target required") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}
