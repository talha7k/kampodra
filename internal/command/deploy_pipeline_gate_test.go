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
