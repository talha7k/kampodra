// Config-file WRITES (the config.sh port): atomic 0700/0600 saves, the
// profile-name and value validators, and tilde expansion. Reads live in
// state.go; this file only adds the mutation surface the `config` command
// family needs.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SaveConfig writes the profile config atomically: 0700 dir, 0600 file,
// same-dir tmp + mv (a reader never observes a partial file).
func SaveConfig(home string, cfg *Config) error {
	path := ConfigPath(home)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("config: cannot write %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("config: atomic install failed (tmp left at: %s): %w", tmp, err)
	}
	return nil
}

// ValidProfileName ports is_profile_name: config-key safe names only —
// ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$.
func ValidProfileName(name string) bool {
	return profileNameRe.MatchString(name)
}

var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// RejectConfigValue ports reject_config_value: nothing legit needs
// whitespace, quotes, or backslashes (the config is hand-greppable and
// round-trippable); the rejected value is NEVER echoed.
func RejectConfigValue(value, what string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", what)
	}
	if strings.ContainsAny(value, " \t\n\r\"\\") {
		return fmt.Errorf("%s must not contain whitespace, quotes, or backslashes (got it with — value not echoed)", what)
	}
	return nil
}

// ExpandTilde ports expand_tilde: config stores absolute paths. Only a bare
// leading ~ (or ~/-prefixed path) expands.
func ExpandTilde(path, home string) string {
	switch {
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return home + path[1:]
	default:
		return path
	}
}
