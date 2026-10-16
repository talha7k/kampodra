// The cloud-auth env ladder: the vendor-neutral KAMPODRA_CLOUD_* names are
// primary; the vendor-prefixed OCI_* names survive one release as read-
// but-deprecated aliases. One helper owns the resolution + the warn-once-
// per-process stderr line so no read site duplicates it.

package command

import (
	"bytes"
	"strings"
	"testing"
)

// envMap builds a Deps.Env over a fixed table (present-but-empty counts —
// the harness must distinguish empty from unset).
func envMap(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// cloudEnvTestDeps wires buffers + the table; withDefaults is NOT applied —
// the helper receives exactly what the call sites receive (a defaulted Deps).
func cloudEnvTestDeps(env map[string]string, stderr *bytes.Buffer) Deps {
	return Deps{Env: envMap(env), Stderr: stderr}
}

func TestCloudEnvNewNameResolvesWithoutWarning(t *testing.T) {
	resetCloudEnvDeprecationWarnings()
	t.Cleanup(resetCloudEnvDeprecationWarnings)
	stderr := &bytes.Buffer{}
	d := cloudEnvTestDeps(map[string]string{"KAMPODRA_CLOUD_COMPARTMENT": "new-c"}, stderr)

	v, deprecated := cloudEnvWithFallback(d, envCloudCompartment, envCloudCompartmentLegacy)

	if v != "new-c" || deprecated {
		t.Errorf("new name must resolve clean: value=%q deprecated=%v", v, deprecated)
	}
	if stderr.Len() != 0 {
		t.Errorf("the primary name must never warn:\n%s", stderr.String())
	}
}

func TestCloudEnvDeprecatedFallbackWarnsExactlyOnce(t *testing.T) {
	resetCloudEnvDeprecationWarnings()
	t.Cleanup(resetCloudEnvDeprecationWarnings)
	stderr := &bytes.Buffer{}
	d := cloudEnvTestDeps(map[string]string{"OCI_COMPARTMENT": "old-c"}, stderr)

	v, deprecated := cloudEnvWithFallback(d, envCloudCompartment, envCloudCompartmentLegacy)

	if v != "old-c" || !deprecated {
		t.Errorf("the alias must still resolve, flagged deprecated: value=%q deprecated=%v", v, deprecated)
	}
	want := "[config] OCI_COMPARTMENT is deprecated — use KAMPODRA_CLOUD_COMPARTMENT (the OCI_* names stop working in a future release)\n"
	if got := stderr.String(); got != want {
		t.Errorf("deprecation line = %q, want %q", got, want)
	}

	// The gate AND the resolver read the same key in one run: ONE line total.
	v, deprecated = cloudEnvWithFallback(d, envCloudCompartment, envCloudCompartmentLegacy)
	if v != "old-c" || !deprecated {
		t.Errorf("second read must resolve identically: value=%q deprecated=%v", v, deprecated)
	}
	if got := stderr.String(); got != want {
		t.Errorf("warn-once-per-process violated — got:\n%s", got)
	}
}

func TestCloudEnvNewNameWinsOverDeprecated(t *testing.T) {
	resetCloudEnvDeprecationWarnings()
	t.Cleanup(resetCloudEnvDeprecationWarnings)
	stderr := &bytes.Buffer{}
	d := cloudEnvTestDeps(map[string]string{
		"KAMPODRA_CLOUD_COMPARTMENT": "new-c",
		"OCI_COMPARTMENT":            "old-c",
	}, stderr)

	v, deprecated := cloudEnvWithFallback(d, envCloudCompartment, envCloudCompartmentLegacy)

	if v != "new-c" || deprecated {
		t.Errorf("the new name must win: value=%q deprecated=%v", v, deprecated)
	}
	if stderr.Len() != 0 {
		t.Errorf("no deprecation line when the primary name supplied the value:\n%s", stderr.String())
	}
}

func TestCloudEnvEmptyValuesCountAsUnset(t *testing.T) {
	resetCloudEnvDeprecationWarnings()
	t.Cleanup(resetCloudEnvDeprecationWarnings)
	stderr := &bytes.Buffer{}
	// `OCI_PROFILE= kampodra …` is no signal — never a deprecation warning.
	d := cloudEnvTestDeps(map[string]string{
		"KAMPODRA_CLOUD_PROFILE": "",
		"OCI_PROFILE":            "",
	}, stderr)

	v, deprecated := cloudEnvWithFallback(d, envCloudProfile, envCloudProfileLegacy)

	if v != "" || deprecated {
		t.Errorf("empty values must resolve to nothing: value=%q deprecated=%v", v, deprecated)
	}
	if stderr.Len() != 0 {
		t.Errorf("an empty deprecated value must stay silent:\n%s", stderr.String())
	}

	// Empty primary falls through to a non-empty alias — and warns.
	d = cloudEnvTestDeps(map[string]string{
		"KAMPODRA_CLOUD_PROFILE": "",
		"OCI_PROFILE":            "old-p",
	}, stderr)
	v, deprecated = cloudEnvWithFallback(d, envCloudProfile, envCloudProfileLegacy)
	if v != "old-p" || !deprecated {
		t.Errorf("empty primary must fall through to the alias: value=%q deprecated=%v", v, deprecated)
	}
	if !strings.Contains(stderr.String(), "[config] OCI_PROFILE is deprecated") {
		t.Errorf("fallthrough to the alias must warn:\n%s", stderr.String())
	}
}
