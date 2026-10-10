// deploy_pipeline_sidecar_test.go — sidecar streaming (build/stream/verify
// every manifest-declared sidecar) and rollback's sidecar isolation.

package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeployPipelineSidecarsStreamed(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	manifest := `{"images":{"sidecars":[{"name":"backup","dockerfile":"deploy/backup.Dockerfile"}]}}`
	if err := os.WriteFile(filepath.Join(reporoot, "kampodra.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Dir = reporoot
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)
	sidecarTag := deployImageRepo + "-backup:latest"

	// build: same platform + identity stamp as the primary, own dockerfile,
	// :latest tag (sidecars are not sha-versioned — rollback never needs
	// an old sidecar). CONTEXT = the sidecar dockerfile's own directory —
	// the RUNBOOK contract (`podman build -f apps/api-go/Dockerfile.backup
	// apps/api-go`); the 2026-10-09 live fire caught repo-root context
	// failing on the first real sidecar Dockerfile (COPY go.mod go.sum).
	sidecarCtx := filepath.Dir(filepath.Join(reporoot, "deploy/backup.Dockerfile"))
	mustContain(t, inv, "podman build --platform linux/arm64 -f deploy/backup.Dockerfile --build-arg GIT_SHA="+pipelineVer+" -t "+sidecarTag+" "+sidecarCtx)
	// stream: the same save|load pipe as the primary…
	mustContain(t, inv, "podman save --format docker-archive "+sidecarTag)
	// …and a fail-closed existence check on the VM after the load.
	mustContain(t, inv, "podman image exists "+sidecarTag)

	// ORDER: the sidecar pass runs after the primary retag (the primary's
	// contract is settled before sidecars start) and before the restart.
	primary := indexOf(inv, "podman tag "+pipelineImageTag+" "+deployImageRepo+":latest")
	sidecar := indexOf(inv, "podman build --platform linux/arm64 -f deploy/backup.Dockerfile")
	restart := indexOf(inv, "rc-service "+deployContainer+" restart")
	if primary < 0 || sidecar < 0 || restart < 0 {
		t.Fatalf("sequence incomplete: primary=%d sidecar=%d restart=%d", primary, sidecar, restart)
	}
	if primary > sidecar {
		t.Error("sidecar build must follow the primary retag")
	}
	if sidecar > restart {
		t.Error("sidecar pass must precede the restart")
	}
}

func TestDeployPipelineRollbackNeverTouchesSidecars(t *testing.T) {
	deps, stubDir, _, _, reporoot := setupDeployPipeline(t)
	manifest := `{"images":{"sidecars":[{"name":"backup","dockerfile":"deploy/backup.Dockerfile"}]}}`
	if err := os.WriteFile(filepath.Join(reporoot, "kampodra.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Dir = reporoot
	// The harness's rollback contract ends at the health gate (exit 1 —
	// served-sha != rolled-back sha), which is PAST the point where sidecar
	// invocations would have been recorded. Purity is the assertion, not
	// the exit code.
	runDeploy(t, deps, "--host", statusHost, "--rollback")
	inv := invocations(t, stubDir)
	if strings.Contains(inv, "-backup:latest") {
		t.Errorf("rollback must not build/stream/verify sidecars:\n%s", inv)
	}
}
