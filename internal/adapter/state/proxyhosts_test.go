package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// proxyHosts round-trips through config.json and is OMITTED for
// scalar-only profiles — existing config files stay byte-compatible.

func TestProfileProxyHostsRoundTrip(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		DefaultProfile: "prod",
		Profiles: map[string]Profile{
			"prod": {
				Host:       "root@203.0.113.9",
				ProxyHost:  "edge.example.com",
				ProxyHosts: []string{"edge.example.com", "www.example.com", "api.example.com"},
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
	got := loaded.Profiles["prod"].ProxyHosts
	if strings.Join(got, ",") != "edge.example.com,www.example.com,api.example.com" {
		t.Errorf("proxyHosts round-trip = %v, want the saved list", got)
	}
}

func TestProfileWithoutProxyHostsOmitsTheKey(t *testing.T) {
	home := t.TempDir()
	cfg := &Config{
		Profiles: map[string]Profile{
			"prod": {Host: "root@203.0.113.9", ProxyHost: "edge.example.com"},
		},
	}
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, LedgerDirName, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"proxyHosts"`) {
		t.Errorf("scalar-only profile must omit the proxyHosts key (byte-compatible config files), got:\n%s", raw)
	}
}
