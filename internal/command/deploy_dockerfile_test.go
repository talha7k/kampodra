package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The deploy --dockerfile ladder: flag > project config (env > profile
// block > kampodra.json > default "Dockerfile" — the layers already merged
// into target.Project.Dockerfile).

func TestDeployDockerfileDefaultsThroughProjectConfig(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	// No manifest, no profile block, no env — the project default
	// "Dockerfile" feeds the build (the historical flag default, now
	// resolved through the ladder).
	if code := runDeploy(t, deps, "--host", statusHost); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	mustContain(t, invocations(t, stubDir), "-f Dockerfile")
	_ = reporoot
}

func TestDeployDockerfileFromRepoManifest(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	if err := os.WriteFile(filepath.Join(reporoot, "kampodra.json"),
		[]byte(`{"dockerfile": "deploy/Containerfile"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Dir = reporoot // run from inside the repo (discovery upward)

	if code := runDeploy(t, deps, "--host", statusHost); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, "-f deploy/Containerfile")
	if strings.Contains(inv, "-f Dockerfile ") || strings.Contains(inv, "-f Dockerfile"+reporoot) {
		t.Errorf("manifest dockerfile must replace the default build file:\n%s", inv)
	}
}

func TestDeployDockerfileFlagBeatsManifest(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	if err := os.WriteFile(filepath.Join(reporoot, "kampodra.json"),
		[]byte(`{"dockerfile": "deploy/Containerfile"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Dir = reporoot

	if code := runDeploy(t, deps, "--host", statusHost, "--dockerfile", "Containerfile.custom"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	mustContain(t, invocations(t, stubDir), "-f Containerfile.custom")
}

func TestDeployDockerfileMalformedManifestFailsClosed(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	if err := os.WriteFile(filepath.Join(reporoot, "kampodra.json"),
		[]byte(`{"dockerfile": "deploy/Containerfile"`), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Dir = reporoot

	if code := runDeploy(t, deps, "--host", statusHost); code != 1 {
		t.Fatalf("exit = %d, want 1 (fail closed before any build)", code)
	}
	if !strings.Contains(stderr.String(), "kampodra.json") {
		t.Errorf("stderr = %q, want it to name the malformed manifest", stderr.String())
	}
	// Missing/unbuilt invocations file = nothing ran; any content must not
	// include the build step.
	if data, err := os.ReadFile(filepath.Join(stubDir, "invocations")); err == nil {
		if strings.Contains(string(data), "podman build") {
			t.Errorf("a malformed manifest must abort BEFORE the build:\n%s", data)
		}
	}
	_ = stdout
}
