package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

func runRoot(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	deps := command.Deps{Home: t.TempDir(), Env: systemLookup, Stdout: &stdout, Stderr: &stderr}
	code := command.Execute("0.7.0-alpha.1", deps, args)
	return code, stdout.String(), stderr.String()
}

func TestRootNoArgsPrintsIndex(t *testing.T) {
	code, out, _ := runRoot(t)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	assertIndexShape(t, out)
}

func TestRootHelpFlagPrintsIndex(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		code, out, _ := runRoot(t, flag)
		if code != 0 {
			t.Fatalf("%s: exit = %d, want 0", flag, code)
		}
		assertIndexShape(t, out)
	}
}

func assertIndexShape(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "kampodra — kamal-alternative CLI for Alpine/Ubuntu + Podman deploys, built on kamal-proxy") {
		t.Errorf("index lost the mission line:\n%s", out)
	}
	// Groups in index order.
	wantGroups := []string{"DEPLOY\n", "DEPLOY LIFECYCLE\n", "INFRA\n", "DNS\n", "ENV\n", "CONFIG\n", "HOST\n", "METRICS\n", "BACKUP\n"}
	last := -1
	for _, g := range wantGroups {
		i := strings.Index(out, g)
		if i < 0 {
			t.Errorf("index missing group %q:\n%s", g, out)
			continue
		}
		if i < last {
			t.Errorf("group %q out of order:\n%s", g, out)
		}
		last = i
	}
	// Every implemented command appears in the index — unimplemented
	// commands are unlisted, so the index lists exactly the real surface.
	for _, c := range []string{"deploy", "migrate", "vm-prepare", "status", "dns", "env", "config", "metrics", "backup", "ssh", "vm-wipe"} {
		if !strings.Contains(out, c) {
			t.Errorf("index missing command %q:\n%s", c, out)
		}
	}
	if !strings.Contains(out, "Every command supports --help with usage + examples.") {
		t.Errorf("index lost its footer:\n%s", out)
	}
	// Unimplemented commands are never advertised.
	for _, dead := range []string{"bluegreen", "image-import", "NOT_YET_PORTED"} {
		if strings.Contains(out, dead) {
			t.Errorf("index must not list unimplemented %q:\n%s", dead, out)
		}
	}
}

func TestRootVersion(t *testing.T) {
	code, out, _ := runRoot(t, "--version")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if out != "0.7.0-alpha.1\n" {
		t.Errorf("--version = %q, want %q (shell prints the bare version)", out, "0.7.0-alpha.1\n")
	}
}

func TestRootUnknownCommandExits2(t *testing.T) {
	code, _, stderr := runRoot(t, "frobnicate")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (cli.js parity)", code)
	}
	if !strings.Contains(stderr, "kampodra: unknown command: frobnicate") {
		t.Errorf("stderr = %q, want the unknown-command shape", stderr)
	}
	assertIndexShape(t, stderr) // the index prints after the error
}

func TestRootNoHelpSubcommand(t *testing.T) {
	// The shell has no `help` subcommand — it is an unknown command.
	code, _, _ := runRoot(t, "help")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (no invented surface)", code)
	}
}

func TestRootNoCompletionSubcommand(t *testing.T) {
	code, _, _ := runRoot(t, "completion")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (cobra's completion command is invented surface)", code)
	}
}

func TestNpmPackageVersionMatchesBinary(t *testing.T) {
	// The npm shim and the binary ship from the same release; their versions
	// are pinned together. The expected version derives from main.go (the
	// binary's source of truth) so the pin itself never drifts per release.
	mainSrc, err := os.ReadFile(filepath.Join("..", "..", "cmd", "kampodra", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	version := mainVersionRe.FindSubmatch(mainSrc)
	if version == nil {
		t.Fatalf("cannot find the version var in cmd/kampodra/main.go")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "npm", "package.json"))
	if err != nil {
		t.Skipf("npm packaging not present yet: %v", err)
	}
	want := `"version": "` + string(version[1]) + `"`
	if !strings.Contains(string(data), want) {
		t.Errorf("npm package.json version drifted from the binary version — want %s", want)
	}
}

var mainVersionRe = regexp.MustCompile(`var version = "([^"]+)"`)
