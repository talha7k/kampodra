package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// `config print` — the debugging tool for the resolution ladder: the fully
// resolved effective project config with per-field provenance
// (flag/env/profile/repo-file/default).

func setupPrint(t *testing.T, profileJSON string, manifestJSON string) (command.Deps, *strings.Builder, *strings.Builder, string) {
	t.Helper()
	home := t.TempDir()
	repo := t.TempDir()
	if profileJSON != "" {
		dir := filepath.Join(home, ".kampodra")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(profileJSON), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if manifestJSON != "" {
		if err := os.WriteFile(filepath.Join(repo, "kampodra.json"), []byte(manifestJSON), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	deps := command.Deps{
		Home:   home,
		Dir:    repo,
		Env:    systemLookup,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
	}
	for _, k := range []string{"KAMPODRA_PROFILE", "KAMPODRA_HOST", "KAMPODRA_SSH_KEY"} {
		t.Setenv(k, "")
	}
	return deps, stdout, stderr, repo
}

func runConfigPrint(t *testing.T, deps command.Deps, args ...string) int {
	t.Helper()
	return command.Execute("test", deps, append([]string{"config", "print"}, args...))
}

func TestConfigPrintRendersEveryFieldWithProvenance(t *testing.T) {
	deps, stdout, stderr, repo := setupPrint(t,
		`{"defaultProfile": "prod", "profiles": {"prod": {"host": "root@203.0.113.9", "project": {"container": "pf-container", "dataDir": "/pf/data"}}}}`,
		`{"dataDir": "/repo/data", "dockerfile": "deploy/Containerfile", "services": ["svc-a", "svc-b"]}`)

	if code := runConfigPrint(t, deps); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	out := stdout.String()

	// Every Config field renders (the print is the WHOLE effective config).
	for _, field := range []string{
		"container", "shadowSuffix", "envFile", "dataDir", "bucket", "objectPrefix",
		"healthPath", "proxyHost", "services", "imagePrefix", "port", "network",
		"shadowProbePort", "deployedShaFile", "envClearKeys", "dockerfile",
		"migrateScript",
	} {
		if !strings.Contains(out, field) {
			t.Errorf("print is missing field %q:\n%s", field, out)
		}
	}

	// Provenance per winning layer.
	for _, want := range []string{
		"default",                            // untouched fields
		"profile",                            // container/dataDir from the profile block
		"repo-file",                          // dockerfile/services from kampodra.json
		filepath.Join(repo, "kampodra.json"), // the manifest named as origin
		"pf-container",                       // profile wins over manifest
		"/pf/data",                           // profile wins over manifest
		"deploy/Containerfile",               // manifest wins where the profile is silent
	} {
		if !strings.Contains(out, want) {
			t.Errorf("print is missing %q:\n%s", want, out)
		}
	}
}

func TestConfigPrintEnvLayer(t *testing.T) {
	deps, stdout, stderr, _ := setupPrint(t,
		`{"defaultProfile": "prod", "profiles": {"prod": {}}}`, "")
	t.Setenv("KAMPODRA_CONTAINER", "env-container")
	defer os.Unsetenv("KAMPODRA_CONTAINER")

	if code := runConfigPrint(t, deps); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "env-container") || !strings.Contains(out, "KAMPODRA_CONTAINER") {
		t.Errorf("print must show the env value AND its env origin:\n%s", out)
	}
}

func TestConfigPrintNoManifestNoProfileStillRenders(t *testing.T) {
	// config-less, manifest-less machines are fine: the whole default
	// config renders with default provenance.
	deps, stdout, stderr, _ := setupPrint(t, ``, "")
	if code := runConfigPrint(t, deps); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "app") || !strings.Contains(out, "default") {
		t.Errorf("print must render the full default config:\n%s", out)
	}
	if strings.Contains(out, "repo-file") {
		t.Errorf("no manifest present — no repo-file provenance may appear:\n%s", out)
	}
}

func TestConfigPrintMalformedManifestFailsClosed(t *testing.T) {
	deps, _, stderr, _ := setupPrint(t,
		`{"defaultProfile": "prod", "profiles": {"prod": {}}}`, `{"container": "oops",}`)
	if code := runConfigPrint(t, deps); code != 1 {
		t.Fatalf("exit = %d, want 1 (fail closed)", code)
	}
	if !strings.Contains(stderr.String(), "kampodra.json") {
		t.Errorf("stderr = %q, want it to name the malformed manifest", stderr.String())
	}
}

func TestConfigPrintUnknownProfileFailsClosed(t *testing.T) {
	deps, _, stderr, _ := setupPrint(t, `{"defaultProfile": "prod", "profiles": {"prod": {}}}`, "")
	if code := runConfigPrint(t, deps, "--profile", "nope"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "nope") {
		t.Errorf("stderr = %q, want it to name the unknown profile", stderr.String())
	}
}

func TestConfigHelpDocumentsManifestAndSecretsRule(t *testing.T) {
	deps, stdout, stderr, _ := setupPrint(t, ``, "")
	if code := command.Execute("test", deps, []string{"config", "--help"}); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	help := stdout.String()
	if !strings.Contains(help, "kampodra.json") {
		t.Errorf("config --help must document the repo manifest:\n%s", help)
	}
	if !strings.Contains(strings.ToLower(help), "never") || !strings.Contains(strings.ToLower(help), "secret") {
		t.Errorf("config --help must carry the never-secrets rule:\n%s", help)
	}
}
