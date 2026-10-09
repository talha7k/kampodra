package command_test

import (
	"strings"
	"testing"
)

// --rolling: the zero-downtime shadow-container double re-point. Sequence
// pins and the failure-path matrix — on EVERY failure path the safer state
// survives: a bad shadow never took traffic (rm + die), a switch already
// made keeps the shadow serving (LOUD exit + converge).

var (
	rollingShadow     = deployContainer + "-shadow"
	// The shadow replicates the init unit's CLEAR env (NODE_ENV=production,
	// PORT) — the app defaults to its own port without PORT (3095 for the
	// esellar api), and the env file deliberately lacks envClearKeys keys,
	// so a shadow without them probes the wrong port (2026-10-09 live
	// fire: shadow health gate failed, safe-abort).
	rollingShadowRun  = "podman run -d --name " + rollingShadow + " --network kamal -e NODE_ENV=production -e PORT=8080 --env-file " + pipelineEnvFile + " -p 127.0.0.1:18080:8080 " + pipelineImageTag
	rollingSwitchFwd  = pipelineProxyCmd + " --host app.example.com --target " + rollingShadow + ":8080 --tls --health-check-path /up"
	rollingSwitchBack = pipelineProxyCmd + " --host app.example.com --target " + deployContainer + ":8080 --tls --health-check-path /up"
	rollingRmShadow   = "podman rm -f " + rollingShadow
)

func TestDeployRollingHappySequencePinned(t *testing.T) {
	deps, stubDir, stdout, stderr := setupPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "--rolling"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)

	// Shadow from the SHA tag — NEVER :latest (a retag race would promote
	// whatever :latest pointed at when the shadow started).
	mustContain(t, inv, rollingShadowRun)
	if strings.Contains(inv, "podman run -d --name "+rollingShadow+" ") &&
		strings.Contains(inv, "podman run -d --name "+rollingShadow+" --network kamal --env-file "+pipelineEnvFile+" -p 127.0.0.1:18080:8080 "+deployImageRepo+":latest") {
		t.Error("shadow must come from the sha tag, never :latest")
	}
	// Loopback-only probe port: the shadow is reachable for the health gate
	// but invisible to anything but localhost.
	mustContain(t, inv, "-p 127.0.0.1:18080:8080")

	// THE two-switch pattern: proxy → shadow, then (only after the new main
	// is healthy) proxy → main. Stop MUST be the init-system stop (a podman
	// stop would fight the supervisor's respawn), and it must precede the
	// retag; the retag precedes the start.
	order := [][]string{
		{rollingShadowRun, "http://127.0.0.1:18080"},                  // shadow gated before it can take traffic
		{"http://127.0.0.1:18080", rollingSwitchFwd},                  // gate precedes the first switch
		{rollingSwitchFwd, "rc-service " + deployContainer + " stop"}, // drain/switch before touching the old
		{"rc-service " + deployContainer + " stop", "podman tag " + pipelineImageTag + " " + deployImageRepo + ":latest"},
		{"podman tag " + pipelineImageTag + " " + deployImageRepo + ":latest", "rc-service " + deployContainer + " start"},
		{"rc-service " + deployContainer + " start", "http://127.0.0.1:8080"}, // new main gated
		{"http://127.0.0.1:8080", rollingSwitchBack},                          // second switch back to main
		{rollingSwitchBack, rollingRmShadow},                                  // shadow rm LAST
	}
	for _, pair := range order {
		a, b := indexOf(inv, pair[0]), indexOf(inv, pair[1])
		if a < 0 || b < 0 || a > b {
			t.Errorf("rolling order violated: %q must precede %q\ninvocations:\n%s", pair[0], pair[1], inv)
		}
	}

	// Exactly ONE init stop (the old container) and it is rc-service, not
	// podman stop (respawn discipline).
	if strings.Count(inv, "rc-service "+deployContainer+" stop") != 1 {
		t.Errorf("rolling must init-stop the old container exactly once:\n%s", inv)
	}
	if strings.Contains(inv, "podman stop") {
		t.Errorf("rolling must never podman-stop a supervised container:\n%s", inv)
	}

	// Stamp written (a completed rolling deploy IS the deployed sha), ledger
	// success, footer.
	mustContain(t, inv, "echo "+pipelineVer+" > "+pipelineStamp)
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"result":"success"`) {
		t.Errorf("rolling ledger line = %s", last)
	}
	mustContain(t, stdout.String(), "Total deployments: 4 · current tag: "+pipelineVer)
}

func TestDeployRollingShadowUnhealthyNeverTakesTraffic(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"0000000"}`)
	if code := runDeploy(t, deps, "--host", statusHost, "--rolling"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	inv := invocations(t, stubDir)
	t.Logf("failure-path invocations (shadow-unhealthy):\n%s", inv)
	if !strings.Contains(inv, rollingRmShadow) {
		t.Errorf("the bad shadow was never removed:\n%s", inv)
	}
	if strings.Contains(inv, "kamal-proxy deploy") {
		t.Errorf("an unhealthy shadow was re-pointed — it must NEVER take traffic:\n%s", inv)
	}
	if strings.Contains(inv, "rc-service "+deployContainer+" stop") {
		t.Errorf("the serving main was stopped for a bad shadow:\n%s", inv)
	}
	mustContain(t, stderr.String(), "never became healthy")
	mustContain(t, stderr.String(), "untouched")
}

func TestDeployRollingSwitchFailureLeavesShadowServing(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	// The old container refuses to stop AFTER the proxy already switched.
	writeFixture(t, stubDir, "failon", "rc-service "+deployContainer+" stop")
	if code := runDeploy(t, deps, "--host", statusHost, "--rolling"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	inv := invocations(t, stubDir)
	t.Logf("failure-path invocations (stop failed after the switch):\n%s", inv)
	if strings.Contains(inv, rollingRmShadow) {
		t.Errorf("the shadow was removed while it was SERVING — the safer state is to leave it:\n%s", inv)
	}
	mustContain(t, stderr.String(), "deploy converge")
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"result":"failed"`) {
		t.Errorf("interrupted rolling deploy must record failed, got: %s", last)
	}
}

func TestDeployRollingDrainTimeoutContinues(t *testing.T) {
	deps, stubDir, stdout, stderr := setupPipeline(t)
	// kamal-proxy never reports the shadow as the target → the drain wait
	// times out — and the deploy CONTINUES (the init stop terminates any
	// in-flight stragglers; the switch already landed).
	writeFixture(t, stubDir, "proxy-ls", deployContainer+":8080")
	if code := runDeploy(t, deps, "--host", statusHost, "--rolling", "--drain-timeout", "1"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)
	// The stop STILL happened, after the first switch.
	a, b := indexOf(inv, rollingSwitchFwd), indexOf(inv, "rc-service "+deployContainer+" stop")
	if a < 0 || b < 0 || a > b {
		t.Errorf("drain timeout must continue to the init stop:\n%s", inv)
	}
	mustContain(t, stdout.String(), "drain")
	mustContain(t, stdout.String(), "continuing")
}

func TestDeployRollingPreflightRefusesLeftoverShadow(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	writeFixture(t, stubDir, "psanames", "kamal-proxy\n"+rollingShadow+"\n")
	if code := runDeploy(t, deps, "--host", statusHost, "--rolling"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	inv := invocations(t, stubDir)
	if strings.Contains(inv, "podman run -d") {
		t.Errorf("a rolling deploy started on top of an interrupted one:\n%s", inv)
	}
	mustContain(t, stderr.String(), "converge")
}

func TestDeployConvergeCompletesAnInterruptedDeploy(t *testing.T) {
	deps, stubDir, stdout, stderr := setupPipeline(t)
	// psanames gains the shadow (the harness default has only kamal-proxy;
	// converge looks for the shadow via ps -a + inspect).
	writeFixture(t, stubDir, "psanames", "kamal-proxy\n"+rollingShadow+"\n")
	if code := runDeploy(t, deps, "--host", statusHost, "converge"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)
	// The sha came from the shadow's image config (never HEAD, never
	// inferred from git).
	mustContain(t, inv, "podman inspect --format '{{.Config.Image}}' "+rollingShadow)
	mustContain(t, inv, "podman tag "+pipelineImageTag+" "+deployImageRepo+":latest")
	// Converge re-runs steps 8-11: retag → restart → gate → re-point → rm.
	order := [][]string{
		{"podman tag " + pipelineImageTag, "rc-service " + deployContainer + " restart"},
		{"rc-service " + deployContainer + " restart", "http://127.0.0.1:8080"},
		{"http://127.0.0.1:8080", rollingSwitchBack},
		{rollingSwitchBack, rollingRmShadow},
	}
	for _, pair := range order {
		a, b := indexOf(inv, pair[0]), indexOf(inv, pair[1])
		if a < 0 || b < 0 || a > b {
			t.Errorf("converge order violated: %q must precede %q\ninvocations:\n%s", pair[0], pair[1], inv)
		}
	}
	// Converge repairs the interrupted deploy's stamp.
	mustContain(t, inv, "echo "+pipelineVer+" > "+pipelineStamp)
	mustContain(t, stdout.String(), "converged")
}

func TestDeployConvergeExplicitShaWins(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	writeFixture(t, stubDir, "psanames", "kamal-proxy\n"+rollingShadow+"\n")
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"ddd4444"}`)
	writeFixture(t, stubDir, "served-sha", "ddd4444")
	if code := runDeploy(t, deps, "--host", statusHost, "converge", "ddd4444"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	mustContain(t, invocations(t, stubDir), "podman tag "+deployImageRepo+":ddd4444 "+deployImageRepo+":latest")
}

func TestDeployConvergeNothingToDo(t *testing.T) {
	deps, _, stdout, stderr := setupPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "converge"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	mustContain(t, stdout.String(), "nothing to converge")
}

func TestDeployConvergeUnhealthyKeepsShadowServing(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	writeFixture(t, stubDir, "psanames", "kamal-proxy\n"+rollingShadow+"\n")
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"0000000"}`)
	if code := runDeploy(t, deps, "--host", statusHost, "converge"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	inv := invocations(t, stubDir)
	t.Logf("failure-path invocations (converge-unhealthy):\n%s", inv)
	if strings.Contains(inv, rollingRmShadow) {
		t.Errorf("converge removed the serving shadow on an unhealthy main:\n%s", inv)
	}
	mustContain(t, stderr.String(), "left the shadow serving")
	mustContain(t, stderr.String(), "deploy list")
}
