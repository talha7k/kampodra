package command_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/state"
	"github.com/talha7k/kampodra/internal/command"
)

// The config.sh port: per-instance profiles in ~/.kampodra/config.json
// (0700/0600), FLAT — no inheritance. First profile auto-becomes the
// default; removing the LAST profile requires --force; the cached init
// verdict and the project block survive re-init (tooling-owned fields).

func setupConfig(t *testing.T) (command.Deps, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	deps := command.Deps{
		Home:   home,
		Env:    systemLookup,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
	}
	for _, k := range []string{"KAMPODRA_PROFILE"} {
		t.Setenv(k, "")
	}
	return deps, home, stdout, stderr
}

func runConfig(t *testing.T, deps command.Deps, args ...string) int {
	t.Helper()
	return command.Execute("test", deps, append([]string{"config"}, args...))
}

func loadConfigFile(t *testing.T, home string) *state.Config {
	t.Helper()
	cfg, err := state.LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func TestConfigInitCreatesFirstProfileAsDefault(t *testing.T) {
	deps, home, stdout, stderr := setupConfig(t)
	key := filepath.Join(home, "id_ed25519")
	os.WriteFile(key, []byte("not-a-real-key"), 0o600)

	code := runConfig(t, deps, "init",
		"--name", "prod",
		"--host", "root@203.0.113.9",
		"--ssh-key", key,
		"--proxy-host", "app.example.com",
		"--group", "main")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	cfg := loadConfigFile(t, home)
	if cfg.DefaultProfile != "prod" {
		t.Errorf("defaultProfile = %q, want prod (first profile auto-becomes default)", cfg.DefaultProfile)
	}
	p := cfg.Profiles["prod"]
	if p.Host != "root@203.0.113.9" || p.SSHKey != key || p.ProxyHost != "app.example.com" || p.Group != "main" {
		t.Errorf("profile = %+v", p)
	}
	if !strings.Contains(stdout.String(), "saved profile 'prod'") || !strings.Contains(stdout.String(), "— default") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestConfigInitSecondProfileDoesNotStealDefault(t *testing.T) {
	deps, home, stdout, stderr := setupConfig(t)
	runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9", "--ssh-key", "/tmp/k")
	stdout.Reset()
	code := runConfig(t, deps, "init", "--name", "staging", "--host", "root@203.0.113.8", "--ssh-key", "/tmp/k2")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	cfg := loadConfigFile(t, home)
	if cfg.DefaultProfile != "prod" {
		t.Errorf("defaultProfile = %q, want prod (later profiles need --set-default)", cfg.DefaultProfile)
	}
	if !strings.Contains(stdout.String(), "make default later: kampodra config set-default staging") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestConfigInitSetDefaultFlagTakesDefault(t *testing.T) {
	deps, home, _, _ := setupConfig(t)
	runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9", "--ssh-key", "/tmp/k")
	runConfig(t, deps, "init", "--name", "staging", "--host", "root@203.0.113.8", "--ssh-key", "/tmp/k2", "--set-default")
	if cfg := loadConfigFile(t, home); cfg.DefaultProfile != "staging" {
		t.Errorf("defaultProfile = %q, want staging", cfg.DefaultProfile)
	}
}

func TestConfigInitRefusesExistingWithoutForce(t *testing.T) {
	deps, _, _, stderr := setupConfig(t)
	runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9", "--ssh-key", "/tmp/k")
	code := runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.8", "--ssh-key", "/tmp/k2")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already exists") || !strings.Contains(stderr.String(), "--force") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestConfigInitForceKeepsToolingOwnedFields(t *testing.T) {
	deps, home, _, stderr := setupConfig(t)
	runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9", "--ssh-key", "/tmp/k")

	seed := loadConfigFile(t, home)
	p := seed.Profiles["prod"]
	p.Init = "openrc"
	p.Project = json.RawMessage(`{"container":"my-api"}`)
	seed.Profiles["prod"] = p
	if err := state.SaveConfig(home, seed); err != nil {
		t.Fatal(err)
	}

	// Same host+key: cached init + project block survive.
	if code := runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9",
		"--ssh-key", "/tmp/k", "--proxy-host", "new.example.com", "--force"); code != 0 {
		t.Fatalf("re-init exit = %d, stderr: %s", code, stderr.String())
	}
	got := loadConfigFile(t, home).Profiles["prod"]
	if got.Init != "openrc" {
		t.Errorf("init = %q, want openrc (cached verdict kept while host/key unchanged)", got.Init)
	}
	if string(got.Project) == "" || !strings.Contains(string(got.Project), "my-api") {
		t.Errorf("project block lost: %q", string(got.Project))
	}
	if got.ProxyHost != "new.example.com" {
		t.Errorf("proxyHost = %q, want refreshed", got.ProxyHost)
	}

	// Host changed: the cached init verdict names the OLD host — drop it.
	if code := runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.7",
		"--ssh-key", "/tmp/k", "--force"); code != 0 {
		t.Fatalf("host-change re-init exit = %d", code)
	}
	if got := loadConfigFile(t, home).Profiles["prod"]; got.Init != "" {
		t.Errorf("init = %q, want dropped after host change", got.Init)
	}
}

func TestConfigInitExpandsTildeAndNotesMissingKey(t *testing.T) {
	deps, home, stdout, _ := setupConfig(t)
	code := runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9", "--ssh-key", "~/.ssh/id_missing")
	if code != 0 {
		t.Fatalf("exit = %d (a missing key file is a note, not an error)", code)
	}
	if got := loadConfigFile(t, home).Profiles["prod"].SSHKey; got != filepath.Join(home, ".ssh/id_missing") {
		t.Errorf("sshKey = %q, want tilde expanded against home", got)
	}
	if !strings.Contains(stdout.String(), "does not exist (yet)") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestConfigInitValidationFailures(t *testing.T) {
	deps, _, _, stderr := setupConfig(t)
	if code := runConfig(t, deps, "init", "--host", "root@h", "--ssh-key", "/k"); code != 1 || !strings.Contains(stderr.String(), "--name is required") {
		t.Errorf("missing name: exit=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := runConfig(t, deps, "init", "--name", "-bad", "--host", "root@h", "--ssh-key", "/k"); code != 1 || !strings.Contains(stderr.String(), "invalid --name") {
		t.Errorf("bad name: exit=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := runConfig(t, deps, "init", "--name", "prod", "--host", "root with space", "--ssh-key", "/k"); code != 1 || !strings.Contains(stderr.String(), "must not contain whitespace") {
		t.Errorf("bad host: exit=%d stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "root with space") {
		t.Errorf("stderr must not echo the rejected value: %q", stderr.String())
	}
}

func writeTwoProfiles(t *testing.T, home string) {
	t.Helper()
	cfg := &state.Config{
		DefaultProfile: "prod",
		Profiles: map[string]state.Profile{
			"prod":    {Host: "root@203.0.113.9", SSHKey: "/keys/prod", ProxyHost: "app.example.com", Group: "main", Init: "openrc"},
			"staging": {Host: "root@203.0.113.8", SSHKey: "/keys/staging", Group: "preview"},
		},
	}
	if err := state.SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestConfigListRendersTable(t *testing.T) {
	deps, home, stdout, _ := setupConfig(t)
	writeTwoProfiles(t, home)
	if code := runConfig(t, deps, "list"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"NAME", "GROUP", "INIT", "HOST", "SSH-KEY (path)", "PROXY-HOST",
		"prod", "main", "openrc", "root@203.0.113.9", "/keys/prod", "app.example.com",
		"staging", "preview", "-",
		"* default",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestConfigListGroupFilter(t *testing.T) {
	deps, home, stdout, _ := setupConfig(t)
	writeTwoProfiles(t, home)
	if code := runConfig(t, deps, "list", "--group", "preview"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "staging") || strings.Contains(out, "prod ") {
		t.Errorf("--group filter leaked other profiles:\n%s", out)
	}
	stdout.Reset()
	if code := runConfig(t, deps, "list", "--group", "nope"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "(no profiles in group 'nope')") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestConfigListWithoutConfigFails(t *testing.T) {
	deps, _, _, stderr := setupConfig(t)
	if code := runConfig(t, deps, "list"); code != 1 || !strings.Contains(stderr.String(), "no kampodra config yet") {
		t.Errorf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestConfigShowRendersFields(t *testing.T) {
	deps, home, stdout, _ := setupConfig(t)
	writeTwoProfiles(t, home)
	if code := runConfig(t, deps, "show"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"profile: prod (default)",
		"host      : root@203.0.113.9",
		"sshKey    : /keys/prod",
		"proxyHost : app.example.com",
		"group     : main",
		"init      : openrc",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("show output missing %q:\n%s", want, out)
		}
	}
}

func TestConfigShowExplicitProfileAndUnknown(t *testing.T) {
	deps, home, stdout, stderr := setupConfig(t)
	writeTwoProfiles(t, home)
	stdout.Reset()
	if code := runConfig(t, deps, "show", "--profile", "staging"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "profile: staging\n") || strings.Contains(stdout.String(), "(default)") {
		t.Errorf("stdout = %q", stdout.String())
	}
	stderr.Reset()
	if code := runConfig(t, deps, "show", "--profile", "ghost"); code != 1 || !strings.Contains(stderr.String(), "unknown profile") {
		t.Errorf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "prod") || !strings.Contains(stderr.String(), "staging") {
		t.Errorf("unknown-profile error must name the known set: %q", stderr.String())
	}
}

// config show resolves the profile through the SAME ladder as print/deploy:
// explicit --profile > KAMPODRA_PROFILE env > config defaultProfile.
func TestConfigShowHonorsProfileEnv(t *testing.T) {
	deps, home, stdout, stderr := setupConfig(t)
	writeTwoProfiles(t, home)
	t.Setenv("KAMPODRA_PROFILE", "staging")
	if code := runConfig(t, deps, "show"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "profile: staging\n") {
		t.Errorf("config show ignored KAMPODRA_PROFILE:\n%s", stdout.String())
	}
}

func TestConfigSetDefault(t *testing.T) {
	deps, home, stdout, stderr := setupConfig(t)
	writeTwoProfiles(t, home)
	if code := runConfig(t, deps, "set-default", "staging"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if cfg := loadConfigFile(t, home); cfg.DefaultProfile != "staging" {
		t.Errorf("defaultProfile = %q", cfg.DefaultProfile)
	}
	if !strings.Contains(stdout.String(), "default profile: staging") {
		t.Errorf("stdout = %q", stdout.String())
	}
	stderr.Reset()
	if code := runConfig(t, deps, "set-default", "ghost"); code != 1 || !strings.Contains(stderr.String(), "unknown profile") {
		t.Errorf("exit=%d stderr=%q", code, stderr.String())
	}
	stderr.Reset()
	if code := runConfig(t, deps, "set-default"); code != 1 || !strings.Contains(stderr.String(), "usage: kampodra config set-default <name>") {
		t.Errorf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestConfigRemove(t *testing.T) {
	deps, home, stdout, stderr := setupConfig(t)
	writeTwoProfiles(t, home)

	// Removing the DEFAULT (two profiles remain) reassigns to the
	// sorted-first remaining profile.
	if code := runConfig(t, deps, "remove", "prod"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	cfg := loadConfigFile(t, home)
	if cfg.DefaultProfile != "staging" {
		t.Errorf("defaultProfile = %q, want reassigned to staging", cfg.DefaultProfile)
	}
	if _, ok := cfg.Profiles["prod"]; ok {
		t.Error("prod still present after remove")
	}
	if !strings.Contains(stdout.String(), "removed profile 'prod' (1 remaining)") ||
		!strings.Contains(stdout.String(), "default profile is now: staging") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestConfigRemoveRefusesLastWithoutForce(t *testing.T) {
	deps, home, _, stderr := setupConfig(t)
	writeTwoProfiles(t, home)
	// Shrink to one profile first.
	if code := runConfig(t, deps, "remove", "staging"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	stderr.Reset()
	if code := runConfig(t, deps, "remove", "prod"); code != 1 || !strings.Contains(stderr.String(), "refusing to remove the last profile") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if _, err := os.Stat(state.ConfigPath(home)); err != nil {
		t.Fatalf("refused remove must not touch the config: %v", err)
	}
	if cfg := loadConfigFile(t, home); len(cfg.Profiles) != 1 {
		t.Errorf("profiles = %d, want untouched", len(cfg.Profiles))
	}
}

func TestConfigRemoveLastWithForce(t *testing.T) {
	deps, home, _, stderr := setupConfig(t)
	writeTwoProfiles(t, home)
	if code := runConfig(t, deps, "remove", "staging"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	stderr.Reset()
	if code := runConfig(t, deps, "remove", "prod", "--force"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	cfg := loadConfigFile(t, home)
	if len(cfg.Profiles) != 0 || cfg.DefaultProfile != "" {
		t.Errorf("final config = %+v (want empty profiles, no default)", cfg)
	}
}

func TestConfigRemoveRequiresName(t *testing.T) {
	deps, _, _, stderr := setupConfig(t)
	if code := runConfig(t, deps, "remove"); code != 1 || !strings.Contains(stderr.String(), "usage: kampodra config remove <name>") {
		t.Errorf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestConfigCloneCopiesAndOverrides(t *testing.T) {
	deps, home, stdout, stderr := setupConfig(t)
	key := filepath.Join(home, "id_ed25519")
	os.WriteFile(key, []byte("k"), 0o600)
	runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9",
		"--ssh-key", key, "--proxy-host", "app.example.com", "--group", "web")

	code := runConfig(t, deps, "clone", "prod", "prod-2", "--host", "root@203.0.113.10")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	cfg := loadConfigFile(t, home)
	p := cfg.Profiles["prod-2"]
	if p.Host != "root@203.0.113.10" {
		t.Errorf("clone host = %q, want the --host override", p.Host)
	}
	// everything not overridden copies explicitly — no live link to src
	if p.SSHKey != key || p.ProxyHost != "app.example.com" || p.Group != "web" {
		t.Errorf("clone = %+v, want copied ssh-key/proxy-host/group", p)
	}
	if cfg.DefaultProfile != "prod" {
		t.Errorf("clone must not steal the default (got %q)", cfg.DefaultProfile)
	}
	if !strings.Contains(stdout.String(), "cloned 'prod' -> 'prod-2'") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestConfigCloneRequiresHost(t *testing.T) {
	deps, _, _, stderr := setupConfig(t)
	runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9", "--ssh-key", "/tmp/k")
	code := runConfig(t, deps, "clone", "prod", "prod-2")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (--host required)", code)
	}
	if !strings.Contains(stderr.String(), "--host is required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestConfigCloneUnknownSrcAndExistingTarget(t *testing.T) {
	deps, _, _, stderr := setupConfig(t)
	runConfig(t, deps, "init", "--name", "prod", "--host", "root@203.0.113.9", "--ssh-key", "/tmp/k")
	if code := runConfig(t, deps, "clone", "nope", "prod-2", "--host", "root@203.0.113.10"); code != 1 {
		t.Fatalf("unknown src: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown profile 'nope'") {
		t.Errorf("stderr = %q", stderr.String())
	}
	stderr.Reset()
	// first clone creates prod-2 …
	if code := runConfig(t, deps, "clone", "prod", "prod-2", "--host", "root@203.0.113.10"); code != 0 {
		t.Fatalf("first clone: exit = %d, stderr: %s", code, stderr.String())
	}
	stderr.Reset()
	// … so a second clone onto the same name refuses without --force
	if code := runConfig(t, deps, "clone", "prod", "prod-2", "--host", "root@203.0.113.11"); code != 1 {
		t.Fatalf("existing target: exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already exists — pass --force") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if code := runConfig(t, deps, "clone", "prod", "prod-2", "--host", "root@203.0.113.10", "--force"); code != 0 {
		t.Fatalf("--force overwrite: exit = %d", code)
	}
	if code := runConfig(t, deps, "clone", "prod", "prod-3", "--host", "root@203.0.113.10", "--set-default"); code != 0 {
		t.Fatalf("--set-default clone: exit = %d", code)
	}
	if cfg := loadConfigFile(t, deps.Home); cfg.DefaultProfile != "prod-3" {
		t.Errorf("defaultProfile = %q, want prod-3", cfg.DefaultProfile)
	}
}
