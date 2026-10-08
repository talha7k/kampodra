// Package state owns local (deploy-machine) state: the append-only
// deployment ledger (~/.kampodine/deployments.jsonl — the deployment
// HISTORY) and the per-instance profile config (~/.kampodine/config.json).
// The file never holds secrets: sshKey is a PATH.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LedgerDirName/LedgerFileName — the shell's fixed ledger layout.
const (
	LedgerDirName  = ".kampodine"
	LedgerFileName = "deployments.jsonl"
	ConfigFileName = "config.json"
)

// LedgerPath returns <home>/.kampodine/deployments.jsonl.
func LedgerPath(home string) string {
	return filepath.Join(home, LedgerDirName, LedgerFileName)
}

// hostSeam is the fixed key-order seam the shell matches on: the JSONL
// contract (fixed key order, escaped values) guarantees this byte sequence
// can only be the host field itself, never a forged occurrence inside a
// value.
const hostSeam = `","sha":"`

// LedgerCount ports ledger_count: the number of non-empty ledger lines,
// optionally fixed-string-filtered to one host. A missing ledger is 0 (not
// an error); malformed lines still count (shell `grep -c .` parity — a
// broken line must not silently deflate the owner's deployment count).
func LedgerCount(path, host string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	needle := ""
	if host != "" {
		needle = `"host":"` + host + hostSeam
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if needle != "" && !strings.Contains(line, needle) {
			continue
		}
		n++
	}
	return n
}

// Profile is one flat per-instance config entry — no inheritance (owner
// decision): host/sshKey/proxyHost/group + the cached init verdict.
type Profile struct {
	Host      string `json:"host"`
	SSHKey    string `json:"sshKey"`
	ProxyHost string `json:"proxyHost"`
	Group     string `json:"group"`
	Init      string `json:"init"`
}

// Config is ~/.kampodine/config.json.
type Config struct {
	DefaultProfile string             `json:"defaultProfile"`
	Profiles       map[string]Profile `json:"profiles"`
}

// ConfigPath returns <home>/.kampodine/config.json.
func ConfigPath(home string) string {
	return filepath.Join(home, LedgerDirName, ConfigFileName)
}

// LoadConfig reads the profile config; a MISSING file is an empty config
// (config-less machines are fine), a corrupt one fails closed.
func LoadConfig(home string) (*Config, error) {
	data, err := os.ReadFile(ConfigPath(home))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{Profiles: map[string]Profile{}}, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath(home), err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	return &cfg, nil
}

// Profile returns the named profile, failing with the known set when
// unknown (shell profile_field parity).
func (c *Config) Profile(name string) (Profile, error) {
	p, ok := c.Profiles[name]
	if !ok {
		known := make([]string, 0, len(c.Profiles))
		for k := range c.Profiles {
			known = append(known, k)
		}
		sort.Strings(known)
		return Profile{}, fmt.Errorf("unknown profile %q — known: %s", name, strings.Join(known, " "))
	}
	return p, nil
}

// SelectProfileName ports profile_resolve's selection: explicit flag >
// KAMPODINE_PROFILE > config defaultProfile > "" (nothing configured).
// A selected name that does not exist is an error naming the known set.
func SelectProfileName(cfg *Config, explicit string, lookup func(string) (string, bool)) (string, error) {
	name := explicit
	if name == "" {
		if v, ok := lookup("KAMPODINE_PROFILE"); ok {
			name = v
		}
	}
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name == "" {
		return "", nil
	}
	if _, err := cfg.Profile(name); err != nil {
		return "", err
	}
	return name, nil
}
