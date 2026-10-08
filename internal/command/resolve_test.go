package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

func mustCfg(t *testing.T, json string) *state.Config {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".kampodra")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := state.LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestResolveTarget(t *testing.T) {
	// The shell's profile_resolve ladder, as one pure decision:
	//   profile selection: --profile > KAMPODRA_PROFILE > config default
	//   host:  --host flag > profile host > KAMPODRA_HOST
	//   key:   --ssh-key flag > profile sshKey > KAMPODRA_SSH_KEY
	//   proxy: profile proxyHost > KAMPODRA_PROXY_HOST > app.example.com
	cfg := mustCfg(t, `{
	  "defaultProfile": "def",
	  "profiles": {
	    "def": {},
	    "prod": {"host": "root@203.0.113.9", "sshKey": "/ops/id_prod", "proxyHost": "prod.example.com"},
	    "staging": {"host": "root@10.0.0.1"}
	  }
	}`)

	tests := []struct {
		name          string
		flagHost      string
		flagKey       string
		flagProfile   string
		env           map[string]string
		wantHost      string
		wantKey       string
		wantProxyHost string
		wantErr       bool
	}{
		{
			name:          "explicit flags beat everything",
			flagHost:      "root@1.1.1.1",
			flagKey:       "/k1",
			env:           map[string]string{"KAMPODRA_HOST": "root@2.2.2.2", "KAMPODRA_SSH_KEY": "/k2"},
			wantHost:      "root@1.1.1.1",
			wantKey:       "/k1",
			wantProxyHost: "app.example.com",
		},
		{
			name:          "profile fills what flags leave open",
			flagProfile:   "prod",
			wantHost:      "root@203.0.113.9",
			wantKey:       "/ops/id_prod",
			wantProxyHost: "prod.example.com",
		},
		{
			name:          "env fills what the profile leaves open",
			flagProfile:   "staging",
			env:           map[string]string{"KAMPODRA_SSH_KEY": "/env-key", "KAMPODRA_PROXY_HOST": "staging.example.com"},
			wantHost:      "root@10.0.0.1",
			wantKey:       "/env-key",
			wantProxyHost: "staging.example.com",
		},
		{
			name:          "no profile no flags falls to env",
			env:           map[string]string{"KAMPODRA_HOST": "root@3.3.3.3", "KAMPODRA_SSH_KEY": "/k3"},
			wantHost:      "root@3.3.3.3",
			wantKey:       "/k3",
			wantProxyHost: "app.example.com",
		},
		{
			name:          "nothing set leaves host empty (hostless status is valid)",
			wantHost:      "",
			wantKey:       "",
			wantProxyHost: "app.example.com",
		},
		{
			name:        "unknown profile fails closed",
			flagProfile: "nope",
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				v, ok := tt.env[key]
				return v, ok
			}
			got, err := ResolveTarget(cfg, tt.flagHost, tt.flagKey, tt.flagProfile, nil, lookup)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTarget() error = %v", err)
			}
			if got.HostSpec.Host != tt.wantHost {
				t.Errorf("host = %q, want %q", got.HostSpec.Host, tt.wantHost)
			}
			if got.HostSpec.SSHKey != tt.wantKey {
				t.Errorf("key = %q, want %q", got.HostSpec.SSHKey, tt.wantKey)
			}
			if got.ProxyHost != tt.wantProxyHost {
				t.Errorf("proxyHost = %q, want %q", got.ProxyHost, tt.wantProxyHost)
			}
		})
	}
}

func TestResolveTargetManifestShapesProject(t *testing.T) {
	// The repo manifest slots into the ladder between the defaults and the
	// profile block: profile project block > manifest > defaults.
	cfg := mustCfg(t, `{
	  "defaultProfile": "def",
	  "profiles": {
	    "def": {"project": {"container": "pf-container"}},
	    "bare": {}
	  }
	}`)
	manifest := &project.Manifest{
		Path:   "/repo/kampodra.json",
		Fields: project.ManifestFields{Container: "mf-container", DataDir: "/mf/data", Dockerfile: "mf/Containerfile"},
	}

	t.Run("manifest beats defaults where the profile is silent", func(t *testing.T) {
		got, err := ResolveTarget(cfg, "", "", "bare", manifest, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		if got.Project.Container != "mf-container" {
			t.Errorf("container = %q, want the manifest value", got.Project.Container)
		}
		if got.Project.DataDir != "/mf/data" {
			t.Errorf("dataDir = %q, want the manifest value", got.Project.DataDir)
		}
	})
	t.Run("profile project block beats the manifest", func(t *testing.T) {
		got, err := ResolveTarget(cfg, "", "", "def", manifest, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		if got.Project.Container != "pf-container" {
			t.Errorf("container = %q, want the profile block value", got.Project.Container)
		}
		if got.Project.DataDir != "/mf/data" {
			t.Errorf("dataDir = %q, want the manifest value (profile silent)", got.Project.DataDir)
		}
	})
	t.Run("no manifest keeps defaults + profile", func(t *testing.T) {
		got, err := ResolveTarget(cfg, "", "", "def", nil, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		if got.Project.Container != "pf-container" {
			t.Errorf("container = %q, want the profile block value", got.Project.Container)
		}
		if got.Project.DataDir != project.LoadDefault().DataDir {
			t.Errorf("dataDir = %q, want the default", got.Project.DataDir)
		}
	})
}

func TestDepsManifestForDiscoversAndFailsClosed(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "kampodra.json"), []byte(`{"dataDir": "/repo/data"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "apps", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("discovers from a nested cwd", func(t *testing.T) {
		d := Deps{Dir: nested}
		mf, err := d.manifestFor()
		if err != nil {
			t.Fatal(err)
		}
		if mf == nil || mf.Fields.DataDir != "/repo/data" {
			t.Fatalf("manifestFor() = %+v, want the repo manifest", mf)
		}
	})
	t.Run("no manifest in tree is nil, not an error", func(t *testing.T) {
		d := Deps{Dir: t.TempDir()}
		mf, err := d.manifestFor()
		if err != nil || mf != nil {
			t.Fatalf("manifestFor() = (%v, %v), want (nil, nil)", mf, err)
		}
	})
	t.Run("malformed manifest fails closed", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(repo, "kampodra.json"), []byte(`{"dataDir": `), 0o644); err != nil {
			t.Fatal(err)
		}
		d := Deps{Dir: nested}
		_, err := d.manifestFor()
		if err == nil {
			t.Fatal("malformed manifest must fail closed")
		}
		if !strings.Contains(err.Error(), "kampodra.json") {
			t.Errorf("error = %v, want it to name the manifest", err)
		}
	})
}
