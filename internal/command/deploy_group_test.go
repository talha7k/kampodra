package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// The deploy --group fan-out (fleet UX): sequential deploys to every
// profile in a group, fail-fast with a verdict naming what deployed, what
// failed, and what was never attempted.
func setupDeployGroup(t *testing.T, failon string) (command.Deps, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	cfg := `{"defaultProfile":"web-a","profiles":{` +
		`"web-a":{"host":"root@203.0.113.9","group":"web"},` +
		`"web-b":{"host":"root@203.0.113.10","group":"web"},` +
		`"web-c":{"host":"root@203.0.113.11","group":"web"}}}`
	dir := filepath.Join(deps.Home, ".kampodra")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if failon != "" {
		if err := os.WriteFile(filepath.Join(stubDir, "failon"), []byte(failon), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	deps.Dir = reporoot
	return deps, stubDir, stdout, stderr
}

func TestDeployGroupHappyPath(t *testing.T) {
	deps, stubDir, stdout, _ := setupDeployGroup(t, "")
	if code := runDeploy(t, deps, "--group", "web"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, stdout.String())
	}
	inv := invocations(t, stubDir)
	if strings.Count(inv, "root@203.0.113.9 ") < 2 || strings.Count(inv, "root@203.0.113.10 ") < 2 {
		t.Errorf("both members must deploy:\n%s", inv)
	}
	out := stdout.String()
	for _, want := range []string{
		"[group web] deploying ",
		"[group web] 1/3 → web-a",
		"[group web] 2/3 → web-b",
		"[group web] 3/3 → web-c",
		"[group web] ✓ 3/3 deployed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestDeployGroupFailFastStopsAndReports(t *testing.T) {
	deps, stubDir, stdout, _ := setupDeployGroup(t, "")
	// Break the SECOND member at resolution (no host): the fan-out must
	// stop there — web-a deployed, web-b failed, web-c never attempted.
	cfg := `{"defaultProfile":"web-a","profiles":{` +
		`"web-a":{"host":"root@203.0.113.9","group":"web"},` +
		`"web-b":{"group":"web"},` +
		`"web-c":{"host":"root@203.0.113.11","group":"web"}}}`
	if err := os.WriteFile(filepath.Join(deps.Home, ".kampodra", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDeploy(t, deps, "--group", "web"); code == 0 {
		t.Fatalf("exit = 0, want non-zero (the second member fails)")
	}
	out := stdout.String()
	if !strings.Contains(out, "stopped at web-b") {
		t.Errorf("fail-fast verdict missing:\n%s", out)
	}
	if !strings.Contains(out, "not attempted: web-c") {
		t.Errorf("remaining list missing:\n%s", out)
	}
	if strings.Contains(invocations(t, stubDir), "root@203.0.113.11") {
		t.Errorf("web-c was attempted despite fail-fast:\n%s", invocations(t, stubDir))
	}
}

func TestDeployGroupMutualExclusionsAndEmptyGroup(t *testing.T) {
	deps, _, _, stderr := setupDeployGroup(t, "")
	if code := runDeploy(t, deps, "--group", "web", "--host", "root@203.0.113.9"); code == 0 {
		t.Errorf("--group + --host must be refused")
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("mutual-exclusion message missing:\n%s", stderr.String())
	}
	stderr.Reset()
	if code := runDeploy(t, deps, "--group", "nope"); code == 0 {
		t.Errorf("empty group must be refused")
	}
	if !strings.Contains(stderr.String(), `no profiles in group "nope"`) {
		t.Errorf("empty-group message missing:\n%s", stderr.String())
	}
}
