// Package state owns local (deploy-machine) state: the append-only
// deployment ledger (~/.kampodra/deployments.jsonl — the deployment
// HISTORY) and the per-instance profile config (~/.kampodra/config.json).
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

// LedgerDirName/LedgerFileName — the fixed ledger layout.
const (
	LedgerDirName  = ".kampodra"
	LedgerFileName = "deployments.jsonl"
	ConfigFileName = "config.json"
)

// LedgerPath returns <home>/.kampodra/deployments.jsonl.
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

// LedgerEntry is one parsed ledger line (the deployment-history contract in
// common.sh: fixed key order, escaped values).
type LedgerEntry struct {
	Ts         string `json:"ts"`
	Host       string `json:"host"`
	Sha        string `json:"sha"`
	Tag        string `json:"tag"`
	Result     string `json:"result"`
	DurationMs int    `json:"duration_ms"`
	Subject    string `json:"subject"`
}

// LedgerEntries parses the ledger into entries, optionally fixed-string
// filtered to one host via the same `"host":"…","sha":"` seam as
// LedgerCount. A missing ledger yields nil (rendering never fails on a
// fresh machine); malformed lines are SKIPPED here (LedgerCount counts them
// for the owner's total, but garbage must never crash a render). File order
// is preserved — the last entry for a tag is the newest.
func LedgerEntries(path, host string) []LedgerEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	needle := ""
	if host != "" {
		needle = `"host":"` + host + hostSeam
	}
	var out []LedgerEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if needle != "" && !strings.Contains(line, needle) {
			continue
		}
		var e LedgerEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Profile is one flat per-instance config entry — no inheritance (owner
// decision): host/sshKey/proxyHost/group + the cached init verdict.
// Project is the raw "project" block — its shape belongs to the project
// adapter (state never interprets it, keeping adapters sibling-clean).
type Profile struct {
	Host      string          `json:"host"`
	SSHKey    string          `json:"sshKey"`
	ProxyHost string          `json:"proxyHost"`
	Group     string          `json:"group"`
	Init      string          `json:"init"`
	Project   json.RawMessage `json:"project,omitempty"`
}

// Config is ~/.kampodra/config.json.
type Config struct {
	DefaultProfile string             `json:"defaultProfile"`
	Profiles       map[string]Profile `json:"profiles"`
}

// ConfigPath returns <home>/.kampodra/config.json.
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
// KAMPODRA_PROFILE > config defaultProfile > "" (nothing configured).
// A selected name that does not exist is an error naming the known set.
func SelectProfileName(cfg *Config, explicit string, lookup func(string) (string, bool)) (string, error) {
	name := explicit
	if name == "" {
		if v, ok := lookup("KAMPODRA_PROFILE"); ok {
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
