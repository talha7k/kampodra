package command_test

import (
	"strings"
	"testing"
)

// Rolling promotion (2026-10-10): rolling is the DEFAULT — promoted after
// the OCI live-fire drill (zero failed probes, safe-abort + converge proven
// live) plus two clean prod rolling deploys. --in-place is the escape
// hatch (the pre-promotion default) and beats an explicit --rolling.
// Rollback is inherently in-place: a bare --rollback stays valid under the
// rolling default and never boots a shadow.

func TestDeployRollingIsTheDefault(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, rollingShadowRun)
	if strings.Contains(inv, "rc-service "+deployContainer+" restart") {
		t.Errorf("a bare deploy must take the rolling path, not the restart-in-place tail:\n%s", inv)
	}
}

func TestDeployInPlaceEscapeHatch(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)
	if strings.Contains(inv, rollingShadowRun) {
		t.Errorf("--in-place must not boot a shadow:\n%s", inv)
	}
	mustContain(t, inv, "rc-service "+deployContainer+" restart")
}

func TestDeployInPlaceBeatsExplicitRolling(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "--rolling", "--in-place"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)
	if strings.Contains(inv, rollingShadowRun) {
		t.Errorf("--in-place beats --rolling:\n%s", inv)
	}
	mustContain(t, inv, "rc-service "+deployContainer+" restart")
}

func TestDeployRollbackImpliesInPlaceUnderRollingDefault(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	// Explicit sha: independent of the stamp fixture.
	if code := runDeploy(t, deps, "--host", statusHost, "--rollback", "abc1234"); code != 0 {
		t.Fatalf("--rollback must stay valid under the rolling default:\nstderr: %s", stderr.String())
	}
	inv := invocations(t, stubDir)
	if strings.Contains(inv, rollingShadowRun) {
		t.Errorf("rollback is instant and in-place — never boots a shadow:\n%s", inv)
	}
}

func TestDeployDrainTimeoutWorksUnderRollingDefault(t *testing.T) {
	deps, _, _, stderr := setupPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "--drain-timeout", "5"); code != 0 {
		t.Fatalf("--drain-timeout is valid on the default rolling path:\nstderr: %s", stderr.String())
	}
}
