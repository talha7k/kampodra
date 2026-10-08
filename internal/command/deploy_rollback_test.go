package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --rollback [<sha>] resolution matrix — THE shell latent-bug fix, pinned:
// the target resolves from the EXPLICIT argument, else the VM's
// deployed-sha stamp file, else it dies. It NEVER silently falls back to
// git HEAD (the shell resolved `git rev-parse --short HEAD` for a bare
// --rollback, silently redeploying the broken build it was asked to roll
// back FROM).

func TestDeployRollbackExplicitShaSkipsBuildAndStreamWhenTagOnVM(t *testing.T) {
	deps, stubDir, stdout, stderr := setupPipeline(t)
	// vmimageexists=0 (the default fixture): the tag is already on the VM.
	// Target ccc3333 — the ledger holds its ORIGINAL deploy's subject, which
	// a rollback must re-attach (ledger_subject_for_tag). The edge must
	// serve the ROLLBACK sha for the smoke to pass.
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"ccc3333"}`)
	writeFixture(t, stubDir, "served-sha", "ccc3333")
	if code := runDeploy(t, deps, "--host", statusHost, "--rollback=ccc3333"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, "podman image exists "+deployImageRepo+":ccc3333")
	if strings.Contains(inv, "podman build") {
		t.Error("rollback must never build")
	}
	if strings.Contains(inv, "podman save") || strings.Contains(inv, "podman load") {
		t.Errorf("rollback must skip the stream when the tag exists on the VM:\n%s", inv)
	}
	mustContain(t, inv, "podman tag "+deployImageRepo+":ccc3333 "+deployImageRepo+":latest")
	mustContain(t, inv, "rc-service "+deployContainer+" restart")
	// Rollback never rewrites the deployed-sha stamp (a stamp round-trip
	// would send the NEXT bare --rollback to the same place).
	if strings.Contains(inv, "> "+pipelineStamp) {
		t.Errorf("rollback must not rewrite the deployed-sha stamp:\n%s", inv)
	}
	// Ledger: result=rollback, subject = the ORIGINAL deploy's subject
	// (ledger_subject_for_tag — git HEAD would name the wrong commit).
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"result":"rollback"`) || !strings.Contains(last, `"sha":"ccc3333"`) {
		t.Errorf("rollback ledger line = %s", last)
	}
	if !strings.Contains(last, `"subject":"fix: tls edge"`) {
		t.Errorf("rollback subject must come from the ledger's original deploy, got: %s", last)
	}
	mustContain(t, stdout.String(), "Total deployments: 4 · current tag: ccc3333")
	mustContain(t, stdout.String(), "already on the VM — skipping build/stream")
}

func TestDeployRollbackSpaceFormTakesTheNextArgument(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"ccc3333"}`)
	writeFixture(t, stubDir, "served-sha", "ccc3333")
	if code := runDeploy(t, deps, "--host", statusHost, "--rollback", "ccc3333"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	mustContain(t, invocations(t, stubDir), "podman tag "+deployImageRepo+":ccc3333 "+deployImageRepo+":latest")
}

func TestDeployRollbackResolvesFromStampFile(t *testing.T) {
	deps, stubDir, stdout, stderr := setupPipeline(t)
	// The stamp fixture serves ccc3333 (the harness default); the edge must
	// serve it too for the smoke.
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"ccc3333"}`)
	writeFixture(t, stubDir, "served-sha", "ccc3333")
	if code := runDeploy(t, deps, "--host", statusHost, "--rollback"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, "podman tag "+deployImageRepo+":ccc3333 "+deployImageRepo+":latest")
	mustContain(t, stdout.String(), "deployed-sha stamp")
	// NEVER HEAD: not a single git invocation in the whole rollback path.
	if strings.Contains(inv, "git ") {
		t.Errorf("rollback fell back to git (the shell latent bug):\n%s", inv)
	}
}

func TestDeployRollbackWithoutShaAndStampDies(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	if err := os.Remove(filepath.Join(stubDir, "stamp")); err != nil {
		t.Fatal(err)
	}
	if code := runDeploy(t, deps, "--host", statusHost, "--rollback"); code != 1 {
		t.Fatalf("exit = %d, want 1 (die — never fall back to HEAD)", code)
	}
	inv := invocations(t, stubDir)
	for _, mutation := range []string{"podman tag", "podman save", "podman build", "rc-service", "git "} {
		if strings.Contains(inv, mutation) {
			t.Errorf("dying rollback must not %s — invocations:\n%s", mutation, inv)
		}
	}
	mustContain(t, stderr.String(), "--rollback <sha7>")
	mustContain(t, stderr.String(), pipelineStamp)
}

func TestDeployRollbackStreamsWhenTagOnlyExistsLocally(t *testing.T) {
	deps, stubDir, stdout, stderr := setupPipeline(t)
	writeFixture(t, stubDir, "vmimageexists", "1") // NOT on the VM
	writeFixture(t, stubDir, "localimageexists", "0")
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"bbb2222"}`)
	writeFixture(t, stubDir, "served-sha", "bbb2222")
	if code := runDeploy(t, deps, "--host", statusHost, "--rollback=bbb2222"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	inv := invocations(t, stubDir)
	mustContain(t, inv, "podman save --format docker-archive "+deployImageRepo+":bbb2222")
	mustContain(t, inv, "podman load")
	mustContain(t, stdout.String(), "streaming image over SSH")
}

func TestDeployRollbackNeitherVMNorLocalDies(t *testing.T) {
	deps, stubDir, _, stderr := setupPipeline(t)
	// Both fixtures to "absent" (exit 1): the tag exists nowhere.
	writeFixture(t, stubDir, "vmimageexists", "1")
	writeFixture(t, stubDir, "localimageexists", "1")
	if code := runDeploy(t, deps, "--host", statusHost, "--rollback=bbb2222"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	mustContain(t, stderr.String(), "neither the VM nor this machine has")
	mustContain(t, stderr.String(), "deploy list")
}
