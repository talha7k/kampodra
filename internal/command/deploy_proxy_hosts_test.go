package command_test

import (
	"testing"
)

// The kamal-proxy re-point must carry the FULL effective host list: the
// production edge serves several hostnames, and re-pointing with a single
// scalar silently DROPS the rest (the esellar incident — the list was
// hand-extended on the VM via ssh because the CLI had no surface). The
// single-host byte-compat is pinned by the existing pipeline tests
// (--host app.example.com with no list configured).

const deployHostList = "edge.example.com,www.example.com,api.example.com"

func TestDeployInPlaceRepointCarriesTheFullHostList(t *testing.T) {
	deps, stubDir, stdout, _, _ := setupDeployPipeline(t)
	t.Setenv("KAMPODRA_PROXY_HOSTS", deployHostList)
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, stdout.String())
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, pipelineProxyCmd+" --host "+deployHostList+" --target "+deployContainer+":8080 --tls --health-check-path "+pipelineHealthPath)

	// The public smoke probes the FIRST host (the list's head is the edge).
	mustContain(t, stdout.String(), "── public edge.example.com")
}

func TestDeployRollingRepointCarriesTheFullHostList(t *testing.T) {
	deps, stubDir, stdout, _ := setupPipeline(t)
	t.Setenv("KAMPODRA_PROXY_HOSTS", deployHostList)
	if code := runDeploy(t, deps, "--host", statusHost); code != 0 {
		t.Fatalf("rolling exit = %d, stderr: %s", code, stderrString(t, deps))
	}
	inv := invocations(t, stubDir)
	// The rolling default path goes through proxyRepointForced (the forward
	// switch to the shadow) — the SAME full-list --host value.
	mustContain(t, inv, pipelineProxyCmd+" --host "+deployHostList+" --target "+rollingShadow+":8080 --tls --health-check-path /up")
	mustContain(t, stdout.String(), "── public edge.example.com")
}
