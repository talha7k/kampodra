package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// env set: the no-local-file update path. The remote content is pulled,
// the pairs replace-in-place (or append), the merge installs atomically,
// and values NEVER print (the fingerprint table masks them).
func TestEnvSetUpsertsIntoRemoteWithoutLocalFile(t *testing.T) {
	deps, stubDir, stdout, stderr := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "set", "PORT=9090", "NEW_KEY=fresh"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	// The upload stdin-capture carries the MERGED file: existing keys keep
	// their order with PORT replaced in place, NEW_KEY appends.
	capture := filepath.Join(stubDir, "uploaded")
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("no upload captured: %v", err)
	}
	merged := string(data)
	for _, want := range []string{"APP_MODE=production", "LEGACY_KEY=ol Drupal 8", "PORT=9090", "SAME_KEY=identical", "NEW_KEY=fresh"} {
		if !strings.Contains(merged, want) {
			t.Errorf("merged env missing %q:\n%s", want, merged)
		}
	}
	if strings.Contains(merged, "PORT=8080") {
		t.Errorf("PORT must be replaced, not duplicated:\n%s", merged)
	}
	// Values never print: the fingerprint table masks, no raw values.
	out := stdout.String()
	if strings.Contains(out, "fresh") || strings.Contains(out, "9090") {
		t.Errorf("stdout leaked values:\n%s", out)
	}
	_ = stubDir
	if !strings.Contains(out, "restart to apply") {
		t.Errorf("the restart hint is missing:\n%s", out)
	}
}

func TestEnvSetRestartAppliesInitAware(t *testing.T) {
	deps, stubDir, stdout, _ := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "set", "PORT=9090", "--restart"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, stdout.String())
	}
	if !strings.Contains(invocations(t, stubDir), "rc-service "+deployContainer+" restart") {
		t.Errorf("--restart must run the init-aware restart:\n%s", invocations(t, stubDir))
	}
}

func TestEnvSetRejectsNonPairsAndInvalidKeys(t *testing.T) {
	deps, _, _, _ := setupEnv(t)
	for _, arg := range []string{"NOEQUALS", "1BAD=x"} {
		if code := runEnv(t, deps, "--host", envHost, "set", arg); code == 0 {
			t.Errorf("env set %q must fail", arg)
		}
	}
}

// The incident pattern (2026-10-16): append an origin to TWO comma-list
// keys in ONE command — no pull, no local file, no python.
func TestEnvSetAppendToCommaLists(t *testing.T) {
	deps, stubDir, stdout, _ := setupEnv(t)
	if code := runEnv(t, deps, "--host", envHost, "set",
		"PORT+=9090",
		"NEW_KEY+=fresh",
		"--restart"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, stdout.String())
	}
	capture := filepath.Join(stubDir, "uploaded")
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("no upload captured: %v", err)
	}
	merged := string(data)
	// PORT exists: append to its comma-list. NEW_KEY is new: the value alone.
	for _, want := range []string{"PORT=8080,9090", "NEW_KEY=fresh", "APP_MODE=production", "SAME_KEY=identical"} {
		if !strings.Contains(merged, want) {
			t.Errorf("merged env missing %q:\n%s", want, merged)
		}
	}
	// Idempotent: a second append of the same value must not duplicate.
	if code := runEnv(t, deps, "--host", envHost, "set", "PORT+=9090", "--restart"); code != 0 {
		t.Fatalf("second exit = %d", code)
	}
	data, _ = os.ReadFile(capture)
	if strings.Contains(string(data), "9090,9090") {
		t.Errorf("append must be idempotent:\n%s", string(data))
	}
}
