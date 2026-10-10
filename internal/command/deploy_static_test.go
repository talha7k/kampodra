// The static artifact kind (frontend SPA fast-push): local build → tar the
// dist → staging swap of the mount dir → NO restart → the freshness gate
// (/build-id.txt must serve the pushed sha) → the same marker chain and
// .prev rollback. The exec-kind backend block coexists in the same
// manifest; the image deploy syncs BOTH mounts.

package command_test

import (
	"strings"
	"testing"
)

const (
	staticRunPath = "/data/web"
	staticStaging = staticRunPath + ".staging"
	staticBuildID = staticRunPath + ".staging/build-id.txt"
)

// binaryStaticManifest: the backend (go) AND the frontend (static) blocks
// side by side — the shape the fast path must serve from one manifest.
const binaryStaticManifest = `{"binary":{"api-go":{"kind":"go","dir":"/data/app","buildDir":"apps/api-go","target":"./cmd/server","entry":"esellar-api-go","imagePath":"/usr/local/bin/esellar-api-go"},` +
	`"web-dist":{"kind":"static","dir":"/data/web","buildDir":"apps/web","artifact":"dist","imagePath":"/app/apps/web/dist","build":"pnpm build --filter web","env":"WEB_DIST_DIR"}}}`

func TestDeployStaticHappyPathNoRestart(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryStaticManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/web/index.html", "<html>app</html>\n")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary", "web-dist"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)

	// The local build ran the declared command (the pnpm shim records it;
	// its cwd is buildDir — the shim materializes <buildDir>/dist).
	mustContain(t, inv, "pnpm build --filter web")
	// No image machinery, no local podman gate (code+assets only).
	for _, forbidden := range []string{"podman build", "podman save", "podman load", "podman info"} {
		if strings.Contains(inv, forbidden) {
			t.Errorf("static mode must not touch the image path (%q):\n%s", forbidden, inv)
		}
	}
	// The upload lands in the staging sibling as a tarball, the build-id
	// is stamped into staging BEFORE the swap (atomic with the tree).
	mustContain(t, inv, "umask 077; cat > "+staticStaging+"/app.tgz")
	mustContain(t, inv, "rm -rf "+staticStaging+" && mkdir -p "+staticStaging)
	mustContain(t, inv, "tar -xzf "+staticStaging+"/app.tgz -C "+staticStaging)
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > "+staticBuildID)
	// The atomic swap: lock, previous kept, staging into place.
	mustContain(t, inv, "mkdir "+staticRunPath+"/.kamdeploy.lock")
	mustContain(t, inv, "mv "+staticRunPath+" "+staticRunPath+".prev")
	mustContain(t, inv, "mv "+staticStaging+" "+staticRunPath+" && true")
	// NO RESTART: the app serves the mount per-request — this is the whole
	// point of the static kind (a restart would be needless downtime).
	if strings.Contains(inv, "rc-service "+deployContainer+" restart") {
		t.Errorf("a static push must not restart the container:\n%s", inv)
	}
	// The freshness gate: VM-side fetch of /build-id.txt == the pushed sha.
	mustContain(t, inv, "http://127.0.0.1:8080/build-id.txt")
	// NO health gate / public smoke: the app is untouched, the edge is
	// untouched — the freshness gate IS the contract.
	if strings.Contains(inv, "http://127.0.0.1:8080"+pipelineHealthPath) {
		t.Errorf("a static push must not run the app health gate:\n%s", inv)
	}
	if strings.Contains(stdout.String(), "smoke") {
		t.Errorf("a static push must not run the public smoke:\n%s", stdout.String())
	}
	// The SAME identity bookkeeping as every other mode: markers, stamp,
	// ledger.
	mustContain(t, inv, "mv "+staticRunPath+".sha "+staticRunPath+".prev.sha")
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > "+staticRunPath+".sha")
	mustContain(t, inv, "echo "+pipelineVer+" > "+pipelineStamp)
	// The gate precedes the markers (identity advances only on success).
	gate := indexOf(inv, "http://127.0.0.1:8080/build-id.txt")
	marker := indexOf(inv, "printf '%s' "+pipelineVer+" > "+staticRunPath+".sha")
	swap := indexOf(inv, "mv "+staticStaging+" "+staticRunPath)
	if swap < 0 || gate < 0 || marker < 0 {
		t.Fatalf("sequence incomplete: swap=%d gate=%d marker=%d", swap, gate, marker)
	}
	if swap > gate || gate > marker {
		t.Errorf("order violated: swap=%d gate=%d marker=%d", swap, gate, marker)
	}

	lines := ledgerLines(readLedgerFile(t, deps.Home))
	if last := lines[len(lines)-1]; !strings.Contains(last, `"result":"success"`) {
		t.Errorf("ledger line %s missing success", last)
	}
	out := stdout.String()
	mustContain(t, out, "artifact web-dist (static) → /data/web")
	mustContain(t, out, "SUCCESS ─")
	mustContain(t, out, "rollback kampodra deploy --binary web-dist")
}

func TestDeployStaticGateFailureAutoRestoresNoRestart(t *testing.T) {
	deps, stubDir, stdout, _, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryStaticManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/web/index.html", "<html>app</html>\n")
	// The mount serves old0001; the gate fails ONCE (the first build-id
	// FETCH — the failonce needle names the fetch, not the stamp), the
	// auto-restore brings .prev back and the re-verify passes.
	writeFixture(t, stubDir, "stamp", "old0001\n")
	writeFixture(t, stubDir, "buildid", "old0001\n")
	writeFixture(t, stubDir, "failonce", "http://127.0.0.1:8080/build-id.txt")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary", "web-dist"); code == 0 {
		t.Fatal("exit = 0, want the freshness gate to fail")
	}
	inv := invocations(t, stubDir)
	out := stdout.String()

	// The restore sequence: .prev back into place, NO restart, markers
	// untouched (the failed push never advanced them).
	mustContain(t, inv, "test -e "+staticRunPath+".prev")
	mustContain(t, inv, "rm -rf "+staticRunPath+" && mv "+staticRunPath+".prev "+staticRunPath)
	if strings.Contains(inv, "rc-service "+deployContainer+" restart") {
		t.Errorf("the static restore must not restart the container:\n%s", inv)
	}
	if strings.Contains(inv, "mv "+staticRunPath+".sha "+staticRunPath+".prev.sha") {
		t.Error("the failed push must not rotate the marker chain")
	}
	mustContain(t, out, "auto-restoring the previous artifact")
	mustContain(t, out, "previous artifact restored")
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	if last := lines[len(lines)-1]; !strings.Contains(last, `"result":"failed"`) {
		t.Errorf("failed push must record a failed ledger line: %s", last)
	}
}

func TestDeployStaticRollbackNoRestart(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryStaticManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/web/index.html", "<html>app</html>\n")
	// The marker chain names the previous artifact; the mount serves it.
	writeFixture(t, stubDir, "prevsha", "ccc3333\n")
	writeFixture(t, stubDir, "buildid", "ccc3333\n")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary", "web-dist", "--rollback"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)

	mustContain(t, inv, "rm -rf "+staticRunPath+" && mv "+staticRunPath+".prev "+staticRunPath)
	mustContain(t, inv, "http://127.0.0.1:8080/build-id.txt")
	// The stamp names what is NOW serving (the restored sha).
	mustContain(t, inv, "printf '%s' ccc3333 > "+pipelineStamp)
	// The marker chain re-aligns with the restored artifact.
	mustContain(t, inv, "mv "+staticRunPath+".prev.sha "+staticRunPath+".sha")
	// No restart, no image machinery.
	if strings.Contains(inv, "rc-service "+deployContainer+" restart") {
		t.Errorf("a static rollback must not restart the container:\n%s", inv)
	}
	if strings.Contains(inv, "podman tag") || strings.Contains(inv, "podman save") {
		t.Errorf("a static rollback must not touch images:\n%s", inv)
	}
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	if last := lines[len(lines)-1]; !strings.Contains(last, `"result":"rollback"`) {
		t.Errorf("rollback must record a rollback ledger line: %s", last)
	}
}

// An image deploy syncs EVERY declared mount from the image — neither the
// backend binary nor the frontend dist may drift from the release.
func TestDeployImageSyncsBothMounts(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	writeBinaryManifest(t, reporoot, binaryStaticManifest)
	deps.Dir = reporoot
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	writeTree(t, reporoot, "apps/web/index.html", "<html>app</html>\n")
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)

	// Two sync containers (one per block), each cp'ing its imagePath (the
	// dist is a tree — the /. form copies its contents).
	mustContain(t, inv, "podman cp kamdeploy-mount-sync:/usr/local/bin/esellar-api-go /data/app/.esellar-api-go.new")
	mustContain(t, inv, "podman cp kamdeploy-mount-sync:/app/apps/web/dist/. "+staticStaging)
	mustContain(t, inv, "chmod -R a-s,u+rwX,go+rX "+staticStaging)
	mustContain(t, inv, "mv "+staticStaging+" "+staticRunPath+" && true")
	// Both marker chains advanced.
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > /data/app/esellar-api-go.sha")
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > "+staticRunPath+".sha")
	// No restart from the sync itself, and both cp legs ride the image ref.
	if strings.Contains(stdout.String(), "WARNING: 2 \"binary\" blocks") {
		t.Errorf("multi-block repos must not warn/skip the sync:\n%s", stdout.String())
	}
}
