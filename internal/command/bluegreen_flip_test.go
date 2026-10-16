// bluegreen flip + rollback: unhealthy refusal, the happy path, ACME
// failure auto-rollback, dormant/rollback-holder cleanup, and the
// compartment flag contract.

package command_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBluegreenFlipRefusesUnhealthy(t *testing.T) {
	h := setupBG(t, map[string]string{"instances-green": greenRow()}, map[string]string{"HEALTHY": "1"})
	if code := h.run(t, "flip", "--to", "green"); code == 0 {
		t.Error("unhealthy target must refuse without --force")
	}
	if !strings.Contains(h.stderr.String(), "UNHEALTHY") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "public-ip update") {
			t.Errorf("refused flip must not touch the reserved IP: %s", call)
		}
	}
}

func TestBluegreenFlipBadColorUsage(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	if code := h.run(t, "flip", "--to", "purple"); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

func TestBluegreenFlipHappyPath(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 -",
		"instances-green": greenRow(),
	}, map[string]string{"HEALTHY": "0", "ADDR_READY": "0", "ACME": "0", "SHA": "abc1234"})
	if code := h.run(t, "flip", "--to", "green"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{"flip[1/4]", "flip[2/4]", "flip[3/4]", "flip[4/4]", "FLIPPED:"} {
		if !strings.Contains(out, want) {
			t.Errorf("flip sequence missing %q:\n%s", want, out)
		}
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "public-ip update") || !strings.Contains(joined, "--wait-for-state ASSIGNED") {
		t.Errorf("assign must wait ASSIGNED:\n%s", joined)
	}
	sshJoined := strings.Join(h.sshCalls(t), "\n")
	if !strings.Contains(sshJoined, "anchor.conf") || !strings.Contains(sshJoined, "kamal-proxy deploy app --host=") {
		t.Errorf("ACME-first sequence missing guest steps:\n%s", sshJoined)
	}
	// Healthy pair: no fallback warning on either stream.
	if strings.Contains(out+h.stderr.String(), "NO healthy fallback") {
		t.Errorf("healthy holder must not warn:\nstdout: %s\nstderr: %s", out, h.stderr.String())
	}
}

// failNthHealthProbe rewrites the harness ssh shim so the Nth app health
// probe (`rc-service app status …`) FAILS while the rest answer the
// HEALTHY knob: the flip gates the TARGET first, then the holder, so
// N=2 pins target-healthy + holder-unhealthy through the single knob.
func failNthHealthProbe(t *testing.T, h *bgHarness, n int) {
	t.Helper()
	shimPath := filepath.Join(filepath.Dir(h.ociLog), "ssh")
	raw, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatal(err)
	}
	count := filepath.Join(filepath.Dir(h.ociLog), "health-probes")
	guard := "  *\"rc-service app status\"*)\n" +
		"    c=$(( $(cat '" + count + "' 2>/dev/null || echo 0) + 1 ))\n" +
		"    printf '%s' \"$c\" > '" + count + "'\n" +
		"    if [ \"$c\" = \"" + strconv.Itoa(n) + "\" ]; then exit 1; fi\n" +
		"    exit 0 ;;\n"
	marker := "case \"$remote\" in\n"
	patched := strings.Replace(string(raw), marker, marker+guard, 1)
	if patched == string(raw) {
		t.Fatal("ssh shim marker not found — the harness shim changed")
	}
	if err := os.WriteFile(shimPath, []byte(patched), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestBluegreenFlipWarnsWhenHolderUnhealthy: flipping to a HEALTHY target
// whose HOLDER (the other color — the post-flip rollback target) is dead
// must warn that there is NO healthy fallback, and still proceed —
// warn-only, the operator may be rescuing a dead pair on purpose.
func TestBluegreenFlipWarnsWhenHolderUnhealthy(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 -",
		"instances-green": greenRow(),
		"instances-blue":  blueRow(),
	}, map[string]string{"HEALTHY": "0", "ADDR_READY": "0", "ACME": "0", "SHA": "abc1234"})
	failNthHealthProbe(t, h, 2) // probe 1 = target (healthy), probe 2 = holder (dead)
	if code := h.run(t, "flip", "--to", "green"); code != 0 {
		t.Fatalf("warn-only path must still flip: exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "target health: app-green app HEALTHY") {
		t.Errorf("target must gate HEALTHY before the holder check:\n%s", h.stdout.String())
	}
	warn := "WARN: app-blue is UNHEALTHY — after this flip there is NO healthy fallback (rollback would restore a dead instance)"
	if !strings.Contains(h.stderr.String(), warn) {
		t.Errorf("dead holder must warn:\nstderr: %s", h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "FLIPPED:") {
		t.Errorf("warning must never block the flip:\n%s", h.stdout.String())
	}
}

func TestBluegreenFlipACMEFailureAutoRollsBack(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 -",
		"instances-green": greenRow(),
	}, map[string]string{"HEALTHY": "0", "ADDR_READY": "0", "ACME": "1", "SHA": "abc1234"})
	if code := h.run(t, "flip", "--to", "green"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "public-ip update") {
		t.Errorf("auto-rollback must reassign or unassign:\n%s", joined)
	}
	if !strings.Contains(h.stderr.String(), "auto-rollback") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestBluegreenRollbackDormant(t *testing.T) {
	h := setupBG(t, map[string]string{"reserved": "ocid1.publicip.r1 203.0.113.50 -"}, nil)
	if code := h.run(t, "rollback"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.stdout.String(), "already UNASSIGNED (dormant)") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "public-ip update") {
			t.Errorf("dormant rollback must not call update: %s", call)
		}
	}
}

func TestBluegreenRollbackHolderCleanup(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 ocid1.privateip.anchor",
		"instances-green": greenRow(),
	}, map[string]string{"CONFADDR": "10.0.0.7/24"})
	if code := h.run(t, "rollback"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "current holder: app-green") || !strings.Contains(out, "ROLLED BACK") {
		t.Errorf("stdout = %q", out)
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "--wait-for-state AVAILABLE") {
		t.Errorf("unassign must wait AVAILABLE:\n%s", joined)
	}
}

func TestBluegreenRequiresCompartment(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	t.Setenv("OCI_COMPARTMENT", "")
	if code := h.run(t, "status"); code != 1 || !strings.Contains(h.stderr.String(), "OCI_COMPARTMENT") {
		t.Errorf("missing compartment must fail naming the fix: exit=%d stderr=%q", code, h.stderr.String())
	}
}

// The renamed kampodra-namespaced compartment env satisfies the same gate
// the deprecated OCI_COMPARTMENT alias does — the rename must not close
// the door on either spelling.
func TestBluegreenCloudEnvCompartmentSatisfiesGate(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	t.Setenv("OCI_COMPARTMENT", "")
	t.Setenv("KAMPODRA_CLOUD_COMPARTMENT", "test-compartment")
	if code := h.run(t, "status"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
}

func TestBluegreenSubcommandHelpHermetic(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	t.Setenv("OCI_COMPARTMENT", "")
	if code := h.run(t, "flip", "--help"); code != 0 {
		t.Errorf("help must work without config: exit=%d", code)
	}
}

// TestBluegreenGuestLegsCarryResolvedKey: every bluegreen ssh leg at the
