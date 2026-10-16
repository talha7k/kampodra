// Config-file WRITES (the config.sh port): atomic 0700/0600 saves, the
// profile-name and value validators, and tilde expansion. Reads live in
// state.go; this file only adds the mutation surface the `config` command
// family needs.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// configSchemaVersion is the schema version SaveConfig stamps on every
// write; LoadConfig rejects anything newer (fail closed, never guess at
// unknown fields).
const configSchemaVersion = 1

// Advisory write-lock contract: <configdir>/config.lock taken with
// O_CREAT|O_EXCL so two concurrent `config` writers serialize instead of
// last-writer-wins clobbering each other's profiles. A lock older than
// configLockStaleAge is from a crashed process and is broken rather than
// wedging config writes forever.
const (
	configLockName     = "config.lock"
	configLockStaleAge = 30 * time.Second
	configLockRetries  = 10
	configLockRetryGap = 50 * time.Millisecond
)

// acquireConfigLock creates the advisory lock, retrying while another
// writer holds it (fresh locks are honored; stale ones are broken). It
// returns the unlock func — callers MUST defer it.
func acquireConfigLock(dir string) (func(), error) {
	lockPath := filepath.Join(dir, configLockName)
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid()) // best-effort debuggability
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("config: lock: %w", err)
		}
		if fi, statErr := os.Stat(lockPath); statErr == nil && time.Since(fi.ModTime()) > configLockStaleAge {
			os.Remove(lockPath) // crashed writer — break and re-contend
			continue
		}
		if attempt+1 >= configLockRetries {
			return nil, fmt.Errorf("config: another kampodra config write is in progress — retry")
		}
		time.Sleep(configLockRetryGap)
	}
}

// SaveConfig writes the profile config atomically: 0700 dir, 0600 file,
// same-dir tmp + mv (a reader never observes a partial file). The write is
// serialized across processes by the advisory config.lock, and the file is
// stamped with the current schema version. The caller's cfg is not mutated.
func SaveConfig(home string, cfg *Config) error {
	path := ConfigPath(home)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	unlock, err := acquireConfigLock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	stamped := *cfg
	stamped.Version = configSchemaVersion
	data, err := json.MarshalIndent(&stamped, "", "  ")
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
