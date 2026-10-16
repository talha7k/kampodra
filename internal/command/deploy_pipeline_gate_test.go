// deploy_pipeline_gate_test.go — the pipeline's failure paths: the failed
// ledger line (the EXIT-trap contract), the unhealthy health gate's
// rollback hints, smoke mismatch, and --skip-smoke.

package command_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/command"
)

func TestDeployPipelineBuildFailureRecordsFailedLedger(t *testing.T) {
	deps, stubDir, stdout, _, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "failon", "podman build")
	if code := runDeploy(t, deps, "--host", statusHost); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	ledger := readLedgerFile(t, deps.Home)
	lines := ledgerLines(ledger)
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"result":"failed"`) || !strings.Contains(last, `"sha":"`+pipelineVer+`"`) {
		t.Errorf("failed deploy never reached the ledger: %s", last)
	}
	mustContain(t, stdout.String(), fmt.Sprintf("Total deployments: 4 · FAILED — attempted tag: %s (not activated)", pipelineVer))
}

func TestDeployPipelineUnhealthyGateDiesWithRollbackHint(t *testing.T) {
	deps, stubDir, _, stderr, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"0000000"}`)
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	errStr := stderr.String()
	mustContain(t, errStr, "never became healthy")
	mustContain(t, errStr, "--rollback")
	// The hint resolves the stamp best-effort: the fixture serves ccc3333.
	mustContain(t, errStr, "--rollback ccc3333")
	// Diagnostics before dying: container logs were pulled.
	mustContain(t, invocations(t, stubDir), "podman logs --tail 30 "+deployContainer)
}

func TestDeployPipelineSmokeMismatchDies(t *testing.T) {
	deps, stubDir, _, stderr, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "served-sha", "bbb2222") // edge still serving the OLD build
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	mustContain(t, stderr.String(), "public smoke: served git sha")
	// The failure must name WHERE proxyHost came from (the 2026-10-16
	// incident: a stale profile project block shadowed the fixed manifest)
	// and reassure that the VM-side app is fine.
	mustContain(t, stderr.String(), "proxyHost source:")
	mustContain(t, stderr.String(), "the VM health gate PASSED")
}

func TestDeployPipelineSkipSmoke(t *testing.T) {
	deps, _, stdout, _, _ := setupDeployPipeline(t)
	// Dead prober: with --skip-smoke the deploy must still succeed.
	deps.Prober = &probe.Prober{HTTPClient: &http.Client{Transport: deadTransport{}}}
	if code := runDeploy(t, deps, "--host", statusHost, "--skip-smoke", "--in-place"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	mustContain(t, stdout.String(), "--skip-smoke")
}

// --- multi-path health gate (healthPaths) ----------------------------------

// gateSecondPath is the second gate route in the multi-path tests (test
// fixture only — production code carries no literal routes).
const gateSecondPath = "/api/catalog"

// gatePathsWith routes the run through TWO health paths: the harness mux
// serves the sha body on the second route too.
func gatePathsWith(t *testing.T, deps command.Deps, stubDir string) {
	t.Helper()
	t.Setenv("KAMPODRA_HEALTH_PATHS", pipelineHealthPath+" "+gateSecondPath)
	writeFixture(t, stubDir, "extra-health-paths", gateSecondPath)
}

// TestDeployPipelineHealthPathsBothPass: with healthPaths set, the VM-side
// gate probes EVERY path in order (each logged with its position) and the
// public smoke checks them all — both green = the deploy succeeds.
func TestDeployPipelineHealthPathsBothPass(t *testing.T) {
	deps, stubDir, stdout, stderr, _ := setupDeployPipeline(t)
	gatePathsWith(t, deps, stubDir)
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	mustContain(t, out, "health gate (path 1/2: "+pipelineHealthPath+")…")
	mustContain(t, out, "health gate (path 2/2: "+gateSecondPath+")…")
	if i1, i2 := indexOf(out, "health gate (path 1/2:"), indexOf(out, "health gate (path 2/2:"); i1 < 0 || i1 > i2 {
		t.Errorf("health paths must gate in order:\n%s", out)
	}
	// The VM fetches hit BOTH paths, first path before second.
	inv := invocations(t, stubDir)
	u1, u2 := "http://127.0.0.1:8080"+pipelineHealthPath, "http://127.0.0.1:8080"+gateSecondPath
	if indexOf(inv, u1) < 0 || indexOf(inv, u1) > indexOf(inv, u2) {
		t.Errorf("gate must fetch path 1 before path 2:\n%s", inv)
	}
	mustContain(t, out, "LIVE:")
}

// TestDeployPipelineHealthPathsSecondPathFailDies: a second path that
// never serves the sha kills the deploy and NAMES the failed path (the
// 2026-10-16 incident: the primary route answered while every store
// route was gone).
func TestDeployPipelineHealthPathsSecondPathFailDies(t *testing.T) {
	deps, stubDir, stdout, stderr, _ := setupDeployPipeline(t)
	gatePathsWith(t, deps, stubDir)
	writeFixture(t, stubDir, "healthfail", gateSecondPath) // the second path's VM fetch dies
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	mustContain(t, stdout.String(), "health gate (path 1/2: "+pipelineHealthPath+")…")
	mustContain(t, stdout.String(), "health gate (path 2/2: "+gateSecondPath+")…")
	mustContain(t, stderr.String(), "health gate failed on "+gateSecondPath)
	mustContain(t, stderr.String(), "never became healthy")
	mustContain(t, stderr.String(), "--rollback ccc3333")
}
