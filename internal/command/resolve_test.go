package command

import (
	"os"
	"path/filepath"
	"testing"

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
			got, err := ResolveTarget(cfg, tt.flagHost, tt.flagKey, tt.flagProfile, lookup)
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
