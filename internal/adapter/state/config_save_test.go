package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The config-file write contract (config.sh's config_atomic_write): dir 0700,
// file 0600, atomic same-dir tmp + mv — a reader never sees a partial file.

func TestSaveConfigCreatesDirAndFileWithTightPerms(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		DefaultProfile: "prod",
		Profiles: map[string]Profile{
			"prod": {Host: "root@203.0.113.9", SSHKey: "/tmp/id", ProxyHost: "app.example.com", Group: "main"},
		},
	}
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	path := ConfigPath(home)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("config file missing: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file perm = %o, want 600", perm)
	}
	dirFi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("config dir missing: %v", err)
	}
	if perm := dirFi.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir perm = %o, want 700", perm)
	}
	loaded, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if loaded.DefaultProfile != "prod" {
		t.Errorf("DefaultProfile = %q, want prod", loaded.DefaultProfile)
	}
	if got := loaded.Profiles["prod"].Host; got != "root@203.0.113.9" {
		t.Errorf("profiles[prod].host = %q", got)
	}
}

func TestSaveConfigLeavesNoTmpBehind(t *testing.T) {
	home := t.TempDir()
	if err := SaveConfig(home, &Config{Profiles: map[string]Profile{}}); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(ConfigPath(home)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("atomic write left temp file behind: %s", e.Name())
		}
	}
}

func TestSaveConfigRoundTripsProjectBlockAndInit(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		DefaultProfile: "prod",
		Profiles: map[string]Profile{
			"prod": {
				Host:    "root@203.0.113.9",
				Init:    "openrc",
				Project: []byte(`{"container":"my-api"}`),
			},
		},
	}
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	loaded, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	p := loaded.Profiles["prod"]
	if p.Init != "openrc" {
		t.Errorf("init = %q, want openrc", p.Init)
	}
	// MarshalIndent re-indents the embedded RawMessage — compare compacted.
	var compact any
	if err := json.Unmarshal(p.Project, &compact); err != nil {
		t.Fatalf("project block unparseable: %v", err)
	}
	compacted, _ := json.Marshal(compact)
	if string(compacted) != `{"container":"my-api"}` {
		t.Errorf("project block = %s", compacted)
	}
}

// is_profile_name: ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$
func TestValidProfileName(t *testing.T) {
	valid := []string{"a", "prod", "Prod1", "my-host", "my_host2", strings.Repeat("a", 64)}
	for _, n := range valid {
		if !ValidProfileName(n) {
			t.Errorf("ValidProfileName(%q) = false, want true", n)
		}
	}
	invalid := []string{"", "-lead", "_lead", "has space", "a/b", "quote\"", "back\\slash", strings.Repeat("a", 65)}
	for _, n := range invalid {
		if ValidProfileName(n) {
			t.Errorf("ValidProfileName(%q) = true, want false", n)
		}
	}
}

// reject_config_value: non-empty, no whitespace/quote/backslash (the config
// is hand-greppable and jq-round-trippable).
func TestRejectConfigValue(t *testing.T) {
	if err := RejectConfigValue("", "--host"); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Errorf("empty value: err = %v, want 'must not be empty'", err)
	}
	for _, bad := range []string{"has space", "tab\there", "quote\"", "back\\slash", "new\nline"} {
		err := RejectConfigValue(bad, "--host")
		if err == nil {
			t.Errorf("RejectConfigValue(%q) = nil, want error", bad)
			continue
		}
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error must not echo the rejected value: %v", err)
		}
		if !strings.Contains(err.Error(), "--host") {
			t.Errorf("error must name the field: %v", err)
		}
	}
	if err := RejectConfigValue("root@203.0.113.9", "--host"); err != nil {
		t.Errorf("plain value rejected: %v", err)
	}
}

// expand_tilde: config stores absolute paths.
func TestExpandTilde(t *testing.T) {
	home := "/Users/op"
	cases := []struct{ in, want string }{
		{"~", home},
		{"~/.ssh/id_ed25519", home + "/.ssh/id_ed25519"},
		{"/abs/path", "/abs/path"},
		{"relative", "relative"},
		{"~user/x", "~user/x"}, // only bare ~ expands
	}
	for _, c := range cases {
		if got := ExpandTilde(c.in, home); got != c.want {
			t.Errorf("ExpandTilde(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
