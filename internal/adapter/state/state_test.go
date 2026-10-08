package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var ledgerFixture = strings.Join([]string{
	`{"ts":"2026-10-08T10:05:00Z","host":"root@203.0.113.9","sha":"` + sha40("aaa1111") + `","tag":"aaa1111","result":"success","duration_ms":182000,"subject":"feat: ledger merge"}`,
	`{"ts":"2026-10-08T11:00:00Z","host":"root@203.0.113.9","sha":"` + sha40("ccc3333") + `","tag":"ccc3333","result":"rollback","duration_ms":42000,"subject":"fix: tls edge"}`,
	`{"ts":"2026-10-05T08:00:00Z","host":"root@203.0.113.9","sha":"` + sha40("feed432") + `","tag":"feed432","result":"failed","duration_ms":64000,"subject":"fix: half-shipped"}`,
	`{"ts":"2026-10-06T08:00:00Z","host":"root@10.0.0.1","sha":"` + sha40("beef888") + `","tag":"beef888","result":"success","duration_ms":120000,"subject":"chore: other host"}`,
	`{"ts":"2026-10-06T08:00:00Z","host":"root@203.0.113.90","sha":"` + sha40("cafe111") + `","tag":"cafe111","result":"success","duration_ms":100000,"subject":"chore: sibling-prefix host"}`,
}, "\n")

func sha40(tag string) string { return (tag + strings.Repeat("0", 40))[:40] }

func writeLedger(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".kampodra")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deployments.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestLedgerPath(t *testing.T) {
	home := t.TempDir()
	want := filepath.Join(home, ".kampodra", "deployments.jsonl")
	if got := LedgerPath(home); got != want {
		t.Errorf("LedgerPath() = %q, want %q", got, want)
	}
}

func TestLedgerCount(t *testing.T) {
	home := writeLedger(t, ledgerFixture+"\n")
	path := LedgerPath(home)

	if got := LedgerCount(path, ""); got != 5 {
		t.Errorf("LedgerCount(all) = %d, want 5", got)
	}
	if got := LedgerCount(path, "root@203.0.113.9"); got != 3 {
		t.Errorf("LedgerCount(host) = %d, want 3", got)
	}
	// The seam match is exact: root@203.0.113.9 must not swallow …90.
	if got := LedgerCount(path, "root@203.0.113.90"); got != 1 {
		t.Errorf("LedgerCount(sibling-prefix host) = %d, want 1", got)
	}
	if got := LedgerCount(path, "root@elsewhere"); got != 0 {
		t.Errorf("LedgerCount(unknown host) = %d, want 0", got)
	}
}

func TestLedgerCountMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.jsonl")
	if got := LedgerCount(path, ""); got != 0 {
		t.Errorf("LedgerCount(missing) = %d, want 0 (a ledger-less machine is not an error)", got)
	}
}

func TestLedgerCountMalformedLinesStillCount(t *testing.T) {
	// Shell parity: `grep -c .` counts every non-empty line — a malformed
	// line must not silently deflate the owner's deployment count.
	home := writeLedger(t, "{broken json\n"+ledgerFixture+"\n")
	if got := LedgerCount(LedgerPath(home), ""); got != 6 {
		t.Errorf("LedgerCount(with malformed line) = %d, want 6", got)
	}
}

func TestLoadConfig(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".kampodra")
	os.MkdirAll(dir, 0o700)
	cfgJSON := `{
	  "defaultProfile": "prod",
	  "profiles": {
	    "prod": {"host": "root@203.0.113.9", "sshKey": "/ops/id_ed25519", "proxyHost": "app.example.com", "group": "edge", "init": "openrc"},
	    "staging": {"host": "root@10.0.0.1"}
	  }
	}`
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600)

	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.DefaultProfile != "prod" {
		t.Errorf("DefaultProfile = %q, want prod", cfg.DefaultProfile)
	}
	prod, err := cfg.Profile("prod")
	if err != nil {
		t.Fatalf("Profile(prod) error = %v", err)
	}
	if prod.Host != "root@203.0.113.9" || prod.SSHKey != "/ops/id_ed25519" || prod.ProxyHost != "app.example.com" || prod.Group != "edge" || prod.Init != "openrc" {
		t.Errorf("Profile(prod) = %+v", prod)
	}
}

func TestLoadConfigMissingFileIsEmpty(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("LoadConfig(missing) error = %v (a config-less machine is fine)", err)
	}
	if cfg.DefaultProfile != "" || len(cfg.Profiles) != 0 {
		t.Errorf("LoadConfig(missing) = %+v, want empty", cfg)
	}
}

func TestLoadConfigBadJSONFailsClosed(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".kampodra")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{nope"), 0o600)
	if _, err := LoadConfig(home); err == nil {
		t.Fatal("LoadConfig(corrupt) must fail closed")
	}
}

func TestSelectProfileName(t *testing.T) {
	cfg := &Config{DefaultProfile: "def", Profiles: map[string]Profile{"def": {}, "prod": {}}}
	emptyCfg := &Config{Profiles: map[string]Profile{}}
	tests := []struct {
		name     string
		cfg      *Config
		explicit string
		env      string
		want     string
		wantErr  bool
	}{
		{name: "explicit flag first", cfg: cfg, explicit: "prod", env: "", want: "prod"},
		{name: "env second", cfg: cfg, explicit: "", env: "prod", want: "prod"},
		{name: "config default third", cfg: cfg, explicit: "", env: "", want: "def"},
		{name: "nothing configured resolves empty", cfg: emptyCfg, explicit: "", env: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(string) (string, bool) { return tt.env, tt.env != "" }
			got, err := SelectProfileName(tt.cfg, tt.explicit, lookup)
			if tt.wantErr && err == nil {
				t.Fatal("want error")
			}
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("SelectProfileName() error = %v", err)
				}
				if got != tt.want {
					t.Errorf("SelectProfileName() = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func TestSelectProfileNameUnknownFailsWithKnownSet(t *testing.T) {
	cfg := &Config{Profiles: map[string]Profile{"prod": {}, "staging": {}}}
	_, err := SelectProfileName(cfg, "nope", func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("unknown profile must fail")
	}
	for _, known := range []string{"prod", "staging"} {
		if !strings.Contains(err.Error(), known) {
			t.Errorf("error %q must name known profile %q", err, known)
		}
	}
}
