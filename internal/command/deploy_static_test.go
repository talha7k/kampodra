// The static artifact kind (frontend SPA fast-push): local build → tar the
// dist → stage a RELEASE inside the mount (releases/<sha>) → ATOMIC FLIP
// of the `current` symlink → NO restart → the freshness gate
// (/build-id.txt must serve the pushed sha) → the marker chain + flip-back
// rollback. Renames INSIDE the mounted inode are container-visible; a
// rename AT the mount root is not (the bind pins the inode — 2026-10-11
// live fire). The exec-kind backend block coexists in the same manifest;
// the image deploy syncs BOTH mounts.

package command_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const (
	staticRunPath   = "/data/web"
	staticReleases  = staticRunPath + "/releases"
	staticStagingID = ".staging-%d" // os.Getpid() at push time
	staticCurrent   = staticRunPath + "/current"
)

// staticStaging renders the per-run staging dir (the pid varies).
func staticStagingAt(pid int) string { return staticReleases + "/" + fmt.Sprintf(staticStagingID, pid) }

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
	// The build identity rides in as KAMPODRA_SHA (the SPA's
	// VITE_BUILD_ID stamps from it — the freshness gate's ground truth).
	mustContain(t, inv, "pnpmenv KAMPODRA_SHA="+pipelineVer)
	// No image machinery, no local podman gate (code+assets only).
	for _, forbidden := range []string{"podman build", "podman save", "podman load", "podman info"} {
		if strings.Contains(inv, forbidden) {
			t.Errorf("static mode must not touch the image path (%q):\n%s", forbidden, inv)
		}
	}
	// The release stages INSIDE the mount (releases/), the build-id is
	// stamped into the release BEFORE the flip, and the flip is the
	// atomic step (rename over the current symlink).
	staging := staticStagingAt(os.Getpid())
	mustContain(t, inv, "mkdir "+staticRunPath+"/.kamdeploy.lock")
	mustContain(t, inv, "mkdir -p "+staticReleases+" && rm -rf "+staticReleases+"/.staging-*")
	mustContain(t, inv, "umask 077; cat > "+staging+".tgz")
	mustContain(t, inv, "tar -xzf "+staging+".tgz -C "+staging)
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > "+staging+"/build-id.txt")
	mustContain(t, inv, "mv "+staging+" "+staticReleases+"/"+pipelineVer)
	mustContain(t, inv, "ln -sfn releases/"+pipelineVer+" "+staticCurrent)
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
	mustContain(t, inv, "mv "+staticCurrent+".sha "+staticCurrent+".prev.sha")
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > "+staticCurrent+".sha")
	mustContain(t, inv, "echo "+pipelineVer+" > "+pipelineStamp)
	// The gate precedes the markers (identity advances only on success).
	gate := indexOf(inv, "http://127.0.0.1:8080/build-id.txt")
	marker := indexOf(inv, "printf '%s' "+pipelineVer+" > "+staticCurrent+".sha")
	flip := indexOf(inv, "ln -sfn releases/"+pipelineVer+" "+staticCurrent)
	if flip < 0 || gate < 0 || marker < 0 {
		t.Fatalf("sequence incomplete: flip=%d gate=%d marker=%d", flip, gate, marker)
	}
	if flip > gate || gate > marker {
		t.Errorf("order violated: flip=%d gate=%d marker=%d", flip, gate, marker)
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
	// The readlink fixture names the previously-served release; the
	// buildid fixture serves old0001, so the gate fails ONCE (failonce
	// on the first fetch), the flip-back restores it, and the re-verify
	// passes.
	writeFixture(t, stubDir, "currentlink", "releases/old0001\n")
	writeFixture(t, stubDir, "buildid", "old0001\n")
	writeFixture(t, stubDir, "failonce", "http://127.0.0.1:8080/build-id.txt")

	if code := runDeploy(t, deps, "--host", statusHost, "--binary", "web-dist"); code == 0 {
		t.Fatal("exit = 0, want the freshness gate to fail")
	}
	inv := invocations(t, stubDir)
	out := stdout.String()

	// The restore sequence: symlink flipped BACK to the previous
	// release, NO restart, markers untouched (the failed push never
	// advanced them).
	mustContain(t, inv, "test -d "+staticReleases+"/old0001")
	mustContain(t, inv, "ln -sfn releases/old0001 "+staticCurrent)
	if strings.Contains(inv, "rc-service "+deployContainer+" restart") {
		t.Errorf("the static restore must not restart the container:\n%s", inv)
	}
	if strings.Contains(inv, "mv "+staticCurrent+".sha "+staticCurrent+".prev.sha") {
		t.Error("the failed push must not rotate the marker chain")
	}
	mustContain(t, out, "flipping back to the previous release")
	mustContain(t, out, "previous release restored")
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

	// The flip-back: current -> releases/ccc3333 (atomic, no restart).
	mustContain(t, inv, "ln -sfn releases/ccc3333 "+staticCurrent)
	mustContain(t, inv, "http://127.0.0.1:8080/build-id.txt")
	// The stamp names what is NOW serving (the restored sha).
	mustContain(t, inv, "printf '%s' ccc3333 > "+pipelineStamp)
	// The marker chain re-aligns with the restored release.
	mustContain(t, inv, "mv "+staticCurrent+".prev.sha "+staticCurrent+".sha")
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

	// One sync container; each block cp's its imagePath (the dist is a
	// tree — the /. form copies its contents) into its own layout.
	mustContain(t, inv, "podman cp kamdeploy-mount-sync:/usr/local/bin/esellar-api-go /data/app/.esellar-api-go.new")
	staging := staticReleases + "/.staging-sync"
	mustContain(t, inv, "podman cp kamdeploy-mount-sync:/app/apps/web/dist/. "+staging)
	mustContain(t, inv, "chmod -R a-s,u+rwX,go+rX "+staging)
	mustContain(t, inv, "mv "+staging+" "+staticReleases+"/"+pipelineVer)
	mustContain(t, inv, "ln -sfn releases/"+pipelineVer+" "+staticCurrent)
	// Both marker chains advanced.
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > /data/app/esellar-api-go.sha")
	mustContain(t, inv, "printf '%s' "+pipelineVer+" > "+staticCurrent+".sha")
	// No restart from the sync itself, and both cp legs ride the image ref.
	if strings.Contains(stdout.String(), "WARNING: 2 \"binary\" blocks") {
		t.Errorf("multi-block repos must not warn/skip the sync:\n%s", stdout.String())
	}
}
