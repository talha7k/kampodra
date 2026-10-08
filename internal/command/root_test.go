package command_test

import (
	"bytes"
	"os"
	"path/filepath"
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
	if !strings.Contains(out, "kampodra — kamal-alternative CLI for Alpine + Podman deploys, built on kamal-proxy") {
		t.Errorf("index lost the mission line:\n%s", out)
	}
	// Groups in the frozen-spec order, with the kampodra-native HOST group.
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
	// Every frozen-spec command appears, plus the kampodra-native ssh.
	for _, c := range []string{"deploy", "bluegreen", "migrate", "vm-prepare", "image-import", "status", "dns", "env", "config", "metrics", "backup", "ssh"} {
		if !strings.Contains(out, c) {
			t.Errorf("index missing command %q:\n%s", c, out)
		}
	}
	if !strings.Contains(out, "Every command supports --help with usage + examples.") {
		t.Errorf("index lost its footer:\n%s", out)
	}
	// Unported spec surface is honestly marked in the index.
	if !strings.Contains(out, "NOT_YET_PORTED") {
		t.Errorf("index must mark unported commands:\n%s", out)
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
	// are pinned together.
	data, err := os.ReadFile(filepath.Join("..", "..", "npm", "package.json"))
	if err != nil {
		t.Skipf("npm packaging not present yet: %v", err)
	}
	if !strings.Contains(string(data), `"version": "0.7.0-alpha.3"`) {
		t.Errorf("npm package.json version drifted from the binary version")
	}
}
