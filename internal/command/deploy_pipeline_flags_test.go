// deploy_pipeline_flags_test.go — deploy's flag-grammar exclusivity matrix
// and the help surface (the usage doc pins rolling + converge).

package command_test

import (
	"strings"
	"testing"
)

func TestDeployPipelineFlagValidation(t *testing.T) {
	t.Run("--rollback and --sha are exclusive", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--rollback", "--sha", "abc1234"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "exclusive")
	})
	t.Run("--sha requires a sha fragment", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--sha", "release-42"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "git sha fragment")
	})
	t.Run("--rolling and --rollback are exclusive", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--rolling", "--rollback"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "exclusive")
	})
	t.Run("--drain-timeout requires rolling (dies with --in-place)", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--drain-timeout", "5", "--in-place"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "--drain-timeout requires the rolling mode")
	})
	t.Run("unknown positional argument dies", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "wat"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "unknown argument")
	})
	t.Run("retired --refresh-config is gone", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--refresh-config"); code != 1 {
			t.Fatalf("exit = %d, want 1 (unknown flag — no legacy aliases)", code)
		}
		mustContain(t, stderr.String(), "unknown flag")
	})
}

func TestDeployHelpDocumentsPipelineRollingAndConverge(t *testing.T) {
	deps, _, stdout, _, _ := setupDeployPipeline(t)
	if code := runDeploy(t, deps, "--help"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"--rolling", "--drain-timeout", "--env-file", "converge",
		"deployed-sha stamp", "NEVER", "two-writer", "shadow",
		"podman save | ssh podman load", "--disk-threshold", "--skip-smoke",
		"keep-set image cleanup",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy --help missing %q", want)
		}
	}
	if strings.Contains(out, "--refresh-config") {
		t.Error("deploy --help still documents the retired --refresh-config no-op")
	}
	if strings.Contains(out, "newest 3") {
		t.Error("deploy --help hardcodes the pipeline keep count — reference the keep-set, not a number that contradicts prune's --keep")
	}
}
