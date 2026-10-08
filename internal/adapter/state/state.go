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
	"regexp"
	"sort"
	"strings"
	"time"
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

// LedgerSubjectForTag ports ledger_subject_for_tag: the NEWEST recorded
// subject for host+tag (the last non-empty matching line in append order),
// "" when none. A rollback's ledger subject is the ORIGINAL deploy's
// subject — git HEAD would name the wrong commit.
func LedgerSubjectForTag(path, host, tag string) string {
	subject := ""
	for _, e := range LedgerEntries(path, host) {
		if e.Tag == tag && e.Subject != "" {
			subject = e.Subject
		}
	}
	return subject
}

// ledgerResultRe / ledgerShaRe are ledger_append's field validators: the
// ledger is append-only with NO repair, so every field is validated BEFORE
// the write — a malformed line would poison every reader.
var (
	ledgerShaRe    = regexp.MustCompile(`^[0-9a-f]{4,40}$`)
	ledgerHostRe   = regexp.MustCompile(`^[^[:space:]]+$`)
	ledgerValidRes = map[string]bool{"success": true, "failed": true, "rollback": true}
)

// AppendEntry ports ledger_append: ONE validated JSONL line appended to the
// ledger (0600 from creation, the directory created on demand). The line
// shape is byte-compatible with the shell printf — fixed key order, JSON
// escaped strings — which is the seam contract every reader matches on.
// Invalid input returns an error and writes NOTHING.
func AppendEntry(path string, e LedgerEntry) error {
	if !ledgerHostRe.MatchString(e.Host) {
		return fmt.Errorf("ledger: refusing to append — invalid host %q", e.Host)
	}
	if !ledgerShaRe.MatchString(e.Sha) || !ledgerShaRe.MatchString(e.Tag) {
		return fmt.Errorf("ledger: refusing to append — sha/tag must be a 4-40 char hex fragment (sha=%q tag=%q)", e.Sha, e.Tag)
	}
	if !ledgerValidRes[e.Result] {
		return fmt.Errorf("ledger: refusing to append — result must be success|failed|rollback (got %q)", e.Result)
	}
	if e.DurationMs < 0 {
		return fmt.Errorf("ledger: refusing to append — negative duration_ms %d", e.DurationMs)
	}
	ts := e.Ts
	if ts == "" {
		ts = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}
	line := fmt.Sprintf(`{"ts":"%s","host":"%s","sha":"%s","tag":"%s","result":"%s","duration_ms":%d,"subject":"%s"}`+"\n",
		ts, jsonEscapeStr(e.Host), e.Sha, e.Tag, e.Result, e.DurationMs, jsonEscapeStr(e.Subject))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return fmt.Errorf("ledger: %w", err)
	}
	_ = os.Chmod(path, 0o600) // best-effort (the create mode already says 0600)
	return nil
}

// jsonEscapeStr ports json_escape_str: backslash and double quote escaped,
// tab/newline/CR flattened to spaces (subjects are single-line by
// construction — the flattening is defense, not an invitation).
func jsonEscapeStr(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\t", " ",
		"\n", " ",
		"\r", " ",
	)
	return r.Replace(s)
}

// Project is the raw "project" block — its shape belongs to the project
// adapter (state never interprets it, keeping adapters sibling-clean).
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
