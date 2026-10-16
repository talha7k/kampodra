package command_test

import (
	"strings"
	"testing"
)

// deploy logs --since: verbatim pass-through of podman logs' --since (a Go
// duration like 2h or an RFC3339 timestamp — no validation beyond
// non-empty). Incident context: a startup WARN is the FIRST line of a
// boot; --tail N cuts it off, so post-restart checks look clean while the
// boot was dirty. --since pins the whole boot window into the podman logs
// argv instead. Harness: the lifecycle PATH-ssh-shim + invocations log
// (deploy_test.go's setupDeploy — the setupDeployPipeline pattern for the
// lifecycle subcommands).

func TestDeployLogsSinceDurationReachesPodmanArgvInTailMode(t *testing.T) {
	deps, stubDir, _, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "logs", "--lines", "30", "--since", "2h"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	mustContain(t, invocations(t, stubDir), "podman logs --since 2h --tail 30 "+deployContainer)
}

func TestDeployLogsSinceTimestampReachesPodmanArgvInFollowMode(t *testing.T) {
	deps, stubDir, _, _ := setupDeploy(t)
	// RFC3339 passes through verbatim (podman accepts both forms);
	// --follow keeps the clean-exit stream with the same window pinned.
	since := "2026-10-16T09:00:00Z"
	if code := runDeploy(t, deps, "--host", statusHost, "logs", "--lines", "20", "--since", since, "--follow"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	mustContain(t, invocations(t, stubDir), "podman logs --since "+since+" -f --tail 20 "+deployContainer)
}

func TestDeployLogsOmittedSinceLeavesArgvUnchanged(t *testing.T) {
	deps, stubDir, _, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "logs", "--lines", "30"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, "podman logs --tail 30 "+deployContainer)
	if strings.Contains(inv, "--since") {
		t.Errorf("omitting --since must leave the podman logs argv untouched (no --since at all):\n%s", inv)
	}
}

// The unit log is the DURABLE crash record (podman logs dies with the
// recreated container; the unit's persisted stderr survives). --unit-log
// must tail /var/log/kampodra-<container>.log via plain tail — NOT podman
// logs (live-fire 2026-10-16: a panic stack existed only in that file).
func TestDeployLogsUnitLogTailsThePersistedFile(t *testing.T) {
	deps, stubDir, stdout, _, _ := setupDeployPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "logs", "--lines", "50", "--unit-log"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, stdout.String())
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, "tail -n 50 /var/log/kampodra-"+deployContainer+".log")
	if strings.Contains(inv, "podman logs --tail") {
		t.Errorf("--unit-log must not read podman logs:\n%s", inv)
	}
}

func TestDeployLogsUnitLogFollowStreamsTheFile(t *testing.T) {
	deps, stubDir, stdout, _, _ := setupDeployPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "logs", "--unit-log", "--follow"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, stdout.String())
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, "tail -n 100 -f /var/log/kampodra-"+deployContainer+".log")
}
