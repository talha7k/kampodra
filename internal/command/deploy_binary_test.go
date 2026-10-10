package command_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The deploy --binary fast-path tests: go (single binary) and node
// (source-tree dir) artifacts, the dependency drift guard, the fail-safe
// auto-restore, rollback, exclusive flags, and the image-deploy's
// mount-sync tail. Same shim harness as the pipeline tests, plus go/cargo
// shims that materialize the built artifact.

const (
	binaryGoManifest = `{"binary":{"api-go":{"kind":"go","dir":"/data/app","buildDir":"apps/api-go","target":"./cmd/server","entry":"esellar-api-go","imagePath":"/usr/local/bin/esellar-api-go"}}}`
	binaryRunPath    = "/data/app/esellar-api-go"
	binarySwap       = binaryRunPath + ".prev"
	binaryMarkerPrev = binaryRunPath + ".prev.sha"
)

// writeBinaryManifest drops the manifest into the harness's repo root and
// points the run at it.
func writeBinaryManifest(t *testing.T, reporoot, manifest string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(reporoot, "kampodra.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// depsHashOf replicates hashDepManifest (sha256 over each file's path +
// bytes) so the drift fixtures can carry the real value.
func depsHashOf(t *testing.T, repoRoot string, files ...string) string {
	t.Helper()
	h := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\n", rel)
		h.Write(data)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestDeployBinaryGoHappySequencePinned(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryGoManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	if code := runDeploy(t, deps, "--host", statusHost, "--binary"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)

	// The clean-tree gate stays (the fast path ships what HEAD names);
	// the podman gate does not (no image build). No image build/stream.
	mustContain(t, inv, "git status --porcelain")
	if strings.Contains(inv, "podman build") || strings.Contains(inv, "podman save") || strings.Contains(inv, "podman load") {
		t.Errorf("binary mode must not build/stream an image:\n%s", inv)
	}
	if strings.Contains(inv, "podman info") {
		t.Error("binary mode must not gate on the local podman machine")
	}

	// The cross-compile: sha-stamped ldflags, the right argv, and the
	// pinned cross env (CGO off, linux/arm64).
	mustContain(t, inv, "go build -ldflags -s -w -X main.buildSha="+pipelineVer+" -X main.gitSha="+pipelineVer+" -o /")
	mustContain(t, inv, "./cmd/server")
	mustContain(t, inv, "goenv CGO_ENABLED=0")
	mustContain(t, inv, "goenv GOOS=linux")
	mustContain(t, inv, "goenv GOARCH=arm64")

	// The upload lands as a dotfile BESIDE the run path, then the atomic
	// swap: lock, previous kept, mv, chmod, markers.
	mustContain(t, inv, "umask 077; cat > /data/app/.esellar-api-go.new")
	mustContain(t, inv, "mkdir /data/app/.kamdeploy.lock")
	mustContain(t, inv, "mv "+binaryRunPath+" "+binarySwap)
	mustContain(t, inv, "mv /data/app/.esellar-api-go.new "+binaryRunPath+" && chmod 755 "+binaryRunPath)
	mustContain(t, inv, "mv "+binaryRunPath+".sha "+binaryMarkerPrev)
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > "+binaryRunPath+".sha")

	// The SAME identity gates as an image deploy: restart, served-sha
	// health gate, public smoke, stamp, ledger.
	mustContain(t, inv, "rc-service "+deployContainer+" restart")
	mustContain(t, inv, "http://127.0.0.1:8080"+pipelineHealthPath)
	mustContain(t, inv, "echo "+pipelineVer+" > "+pipelineStamp)
	restart := indexOf(inv, "rc-service "+deployContainer+" restart")
	health := indexOf(inv, "http://127.0.0.1:8080")
	stamp := indexOf(inv, "echo "+pipelineVer+" > "+pipelineStamp)
	if restart < 0 || health < 0 || stamp < 0 {
		t.Fatalf("sequence incomplete: restart=%d health=%d stamp=%d", restart, health, stamp)
	}
	if restart > health || health > stamp {
		t.Errorf("order violated: restart=%d health=%d stamp=%d", restart, health, stamp)
	}
	if swapAt := indexOf(inv, "mv /data/app/.esellar-api-go.new"); swapAt < 0 || swapAt > restart {
		t.Error("the atomic swap must precede the restart")
	}

	lines := ledgerLines(readLedgerFile(t, deps.Home))
	last := lines[len(lines)-1]
	for _, want := range []string{`"sha":"` + pipelineVer + `"`, `"result":"success"`} {
		if !strings.Contains(last, want) {
			t.Errorf("ledger line %s missing %s", last, want)
		}
	}
	// The operator-facing header and verdict (the Railway-style log
	// contract): target, mode, artifact, verified, rollback command.
	out := stdout.String()
	mustContain(t, out, "mode   binary fast-push")
	mustContain(t, out, "artifact api-go (go) → /data/app")
	mustContain(t, out, "✓ push go artifact")
	mustContain(t, out, "✓ restart "+deployContainer+" (openrc)")
	mustContain(t, out, "SUCCESS ─")
	mustContain(t, out, "rollback kampodra deploy --binary api-go")
}

func TestDeployBinaryNodeDirSwapPinned(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	manifest := `{"binary":{"api-ts":{"kind":"node","dir":"/data/app","buildDir":"apps/api","entry":"src/serve-node.ts","exec":"tsx","imagePath":"/app/apps/api","depsPath":"/app/node_modules"}}}`
	writeBinaryManifest(t, reporoot, manifest)
	deps.Dir = reporoot
	// The source tree + the dependency manifest the drift guard hashes.
	writeTree(t, reporoot, "apps/api/src/serve-node.ts", "// entry\n")
	writeTree(t, reporoot, "apps/api/package.json", `{"name":"api"}`)
	writeTree(t, reporoot, "pnpm-lock.yaml", "lockfile: v9\n")
	hash := depsHashOf(t, reporoot, "apps/api/package.json", "pnpm-lock.yaml")
	writeFixture(t, stubDir, "depssha", hash+"\n")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)

	// The drift guard passed (no go/cargo build for node).
	mustContain(t, stdout.String(), "✓ dependency manifest unchanged ("+hash[:12]+")")
	if strings.Contains(inv, "go build") || strings.Contains(inv, "cargo") {
		t.Error("node kind must not compile")
	}

	// The dir-shape transfer: staging sibling, tarball upload, extract,
	// whole-dir swap, markers under the dir path.
	mustContain(t, inv, "rm -rf /data/app.staging && mkdir -p /data/app.staging")
	mustContain(t, inv, "umask 077; cat > /data/app.staging/app.tgz")
	mustContain(t, inv, "tar -xzf /data/app.staging/app.tgz -C /data/app.staging")
	mustContain(t, inv, "mkdir /data/app/.kamdeploy.lock")
	mustContain(t, inv, "mv /data/app /data/app.prev")
	mustContain(t, inv, "mv /data/app.staging /data/app && true")
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > /data/app.sha")
	mustContain(t, inv, "rc-service "+deployContainer+" restart")

	lines := ledgerLines(readLedgerFile(t, deps.Home))
	if last := lines[len(lines)-1]; !strings.Contains(last, `"result":"success"`) {
		t.Errorf("ledger missing success: %s", last)
	}
}

func TestDeployBinaryDepsDriftRefusesBeforeAnyPush(t *testing.T) {
	deps, stubDir, _, _, reporoot := setupDeployPipeline(t)
	manifest := `{"binary":{"api-ts":{"kind":"node","dir":"/data/app","buildDir":"apps/api","entry":"src/serve-node.ts","exec":"tsx","imagePath":"/app/apps/api","depsPath":"/app/node_modules"}}}`
	writeBinaryManifest(t, reporoot, manifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/api/src/serve-node.ts", "// entry\n")
	writeTree(t, reporoot, "apps/api/package.json", `{"name":"api"}`)
	writeTree(t, reporoot, "pnpm-lock.yaml", "lockfile: v9\n")
	writeFixture(t, stubDir, "depssha", "stale-deps-hash\n")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary"); code == 0 {
		t.Fatal("exit = 0, want the drift guard to refuse")
	}
	errOut := stderrString(t, deps)
	if !strings.Contains(errOut, "dependency manifest changed") || !strings.Contains(errOut, "redeploy the image") {
		t.Errorf("drift error must name the cause and the fix, got: %s", errOut)
	}
	inv := invocations(t, stubDir)
	// The refusal must not MUTATE: the init probe (a read) may have run,
	// but no restart, no upload, no lock.
	for _, forbidden := range []string{"rc-service " + deployContainer + " restart", "cat > /data/app", "kamdeploy.lock"} {
		if strings.Contains(inv, forbidden) {
			t.Errorf("drift refusal must not touch the VM (%q present):\n%s", forbidden, inv)
		}
	}
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	if last := lines[len(lines)-1]; !strings.Contains(last, `"result":"failed"`) {
		t.Errorf("a refused push must record a failed ledger line: %s", last)
	}
}

func TestDeployBinaryHealthFailureAutoRestoresPrevious(t *testing.T) {
	deps, stubDir, stdout, _, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryGoManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	// The running version is old0001 (stamp + health endpoint serve it);
	// this deploy pushes pipelineVer, so the gate must fail — and the
	// restore verification passes against the previous sha.
	writeFixture(t, stubDir, "stamp", "old0001\n")
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"old0001"}`)
	writeFixture(t, stubDir, "served-sha", "old0001")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary"); code == 0 {
		t.Fatal("exit = 0, want the health gate to fail")
	}
	inv := invocations(t, stubDir)
	out := stdout.String()

	// The restore sequence: .prev back into place, markers swapped, VM
	// restarted, health re-verified against the previous sha.
	mustContain(t, inv, "test -e "+binarySwap)
	mustContain(t, inv, "rm -rf "+binaryRunPath+" && mv "+binarySwap+" "+binaryRunPath)
	mustContain(t, inv, "mv "+binaryMarkerPrev+" "+binaryRunPath+".sha")
	restarts := strings.Count(inv, "rc-service "+deployContainer+" restart")
	if restarts < 2 {
		t.Errorf("want the deploy restart AND the restore restart, got %d:\n%s", restarts, inv)
	}
	mustContain(t, out, "auto-restoring the previous artifact")
	mustContain(t, out, "previous artifact restored and healthy")
	if !strings.Contains(out, "✗") {
		t.Errorf("the failed legs must print the ✗ marker:\n%s", out)
	}
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	if last := lines[len(lines)-1]; !strings.Contains(last, `"result":"failed"`) {
		t.Errorf("failed deploy must record a failed ledger line: %s", last)
	}
}

func TestDeployBinaryRollbackRestoresRecordedPrevious(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryGoManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	// The marker chain names the previous artifact; health serves it.
	writeFixture(t, stubDir, "prevsha", "ccc3333\n")
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"ccc3333"}`)
	writeFixture(t, stubDir, "served-sha", "ccc3333")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary", "--rollback"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)

	mustContain(t, inv, "rm -rf "+binaryRunPath+" && mv "+binarySwap+" "+binaryRunPath)
	mustContain(t, inv, "rc-service "+deployContainer+" restart")
	// The stamp must name what is NOW serving (the restored sha), not the
	// one the rolled-back deploy wrote.
	mustContain(t, inv, "printf '%s' ccc3333 > "+pipelineStamp)
	// No image rollback machinery may run.
	if strings.Contains(inv, "podman tag") || strings.Contains(inv, "podman save") {
		t.Errorf("binary rollback must not touch images:\n%s", inv)
	}
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	if last := lines[len(lines)-1]; !strings.Contains(last, `"result":"rollback"`) {
		t.Errorf("rollback must record a rollback ledger line: %s", last)
	}
	mustContain(t, stdout.String(), "rollback kampodra deploy --binary api-go --rollback")
}

func TestDeployBinaryRollbackWithoutMarkerDies(t *testing.T) {
	deps, stubDir, _, _, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryGoManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	writeFixture(t, stubDir, "prevsha", "")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary", "--rollback"); code == 0 {
		t.Fatal("exit = 0, want a missing marker to refuse")
	}
	inv := invocations(t, stubDir)
	if strings.Contains(inv, "mv /data/app") {
		t.Errorf("a marker-less rollback must not swap anything:\n%s", inv)
	}
}

func TestDeployBinarySelectorErrors(t *testing.T) {
	cases := []struct {
		name, manifest string
		args, errWant  []string
	}{
		{
			"unknown block", binaryGoManifest,
			[]string{"--binary", "nope"},
			[]string{`no "binary.nope" block in kampodra.json (declared: api-go)`},
		},
		{
			"two blocks unnamed",
			`{"binary":{"api-go":` + goBlockBody + `,"api-ts":` + nodeBlockBody + `}}`,
			[]string{"--binary"},
			[]string{"pick one: kampodra deploy --binary <name>"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, _, _, reporoot := setupDeployPipeline(t)
			writeBinaryManifest(t, reporoot, tc.manifest)
			deps.Dir = reporoot
			args := append([]string{"--host", statusHost}, tc.args...)
			if code := runDeploy(t, deps, args...); code == 0 {
				t.Fatal("exit = 0, want a selector error")
			}
			errOut := stderrString(t, deps)
			for _, want := range tc.errWant {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr = %q, want %q", errOut, want)
				}
			}
		})
	}
}

const (
	goBlockBody   = `{"kind":"go","dir":"/data/app","buildDir":"apps/api-go","target":"./cmd/server","entry":"esellar-api-go","imagePath":"/usr/local/bin/esellar-api-go"}`
	nodeBlockBody = `{"kind":"node","dir":"/data/app","buildDir":"apps/api","entry":"src/serve-node.ts","exec":"tsx","imagePath":"/app/apps/api","depsPath":"/app/node_modules"}`
)

func TestDeployBinaryExclusiveFlags(t *testing.T) {
	deps, _, _, _, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryGoManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	cases := [][]string{
		{"--host", statusHost, "--binary", "--rolling"},
		{"--host", statusHost, "--binary", "--sha", "abc1234"},
		{"--host", statusHost, "--binary", "--rollback", "abc1234"},
	}
	for _, args := range cases {
		if code := runDeploy(t, deps, args...); code == 0 {
			t.Errorf("%v: exit = 0, want the exclusivity refusal", args)
		}
	}
}

// TestDeployImageSyncsMountFromImage pins the identity-preservation tail:
// an image deploy mirrors the artifact into the mount (the unit's exec
// override must never serve the last fast-pushed version).
func TestDeployImageSyncsMountFromImage(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryGoManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)

	mustContain(t, inv, "podman create --name kamdeploy-mount-sync "+pipelineImageTag)
	mustContain(t, inv, "podman cp kamdeploy-mount-sync:/usr/local/bin/esellar-api-go /data/app/.esellar-api-go.new")
	mustContain(t, inv, "mv /data/app/.esellar-api-go.new "+binaryRunPath+" && chmod 755 "+binaryRunPath)
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > "+binaryRunPath+".sha")
	mustContain(t, inv, "podman rm -f kamdeploy-mount-sync")

	// ORDER: the sync runs after the switch (restart) but before the
	// stamp — the stamped sha and the mounted artifact are one identity.
	restart := indexOf(inv, "rc-service "+deployContainer+" restart")
	sync := indexOf(inv, "podman create --name kamdeploy-mount-sync")
	stamp := indexOf(inv, "echo "+pipelineVer+" > "+pipelineStamp)
	if restart < 0 || sync < 0 || stamp < 0 {
		t.Fatalf("sequence incomplete: restart=%d sync=%d stamp=%d", restart, sync, stamp)
	}
	if restart > sync || sync > stamp {
		t.Errorf("order violated: restart=%d sync=%d stamp=%d", restart, sync, stamp)
	}
}

// writeTree materializes a file (and its parents) under the harness's
// repo root — the node artifact's source tree.
func writeTree(t *testing.T, repoRoot, rel, content string) {
	t.Helper()
	p := filepath.Join(repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
