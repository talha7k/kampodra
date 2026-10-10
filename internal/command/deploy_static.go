// The static artifact kind: the frontend SPA fast-push. A local build
// command produces the dist, the dist crosses as a tarball, the mount dir
// swaps atomically — and NOTHING restarts (the app serves the mount
// per-request, so the next request already sees the new SPA). The
// freshness signal is /build-id.txt: it must serve the pushed sha, which
// is exactly the signal the app's BuildVersionWatcher consumes.
//
// Safety rails, in order of the failure they answer to:
//
//	wrong manifest/selector  → dies at the banner (ChooseBinaryBlock)
//	build failure            → nothing on the VM is touched
//	upload/swap failure      → staging sits BESIDE the run path; the
//	                           running dist never sees a partial tree
//	freshness-gate failure   → the previous dist (.prev) is auto-
//	                           restored (no restart — the next request
//	                           serves it) and re-verified
//	lost marker              → static rollback refuses with the exact path

package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/talha7k/kampodra/internal/adapter/project"
)

// staticBuildIDPath is the freshness signal inside the mount: the file
// the app's static server serves at this path must carry the deployed
// sha (the app's BuildVersionWatcher reads the same signal).
const staticBuildIDPath = "/build-id.txt"

// staticSwitch is the static kind's forward push: build → push (dir swap
// with the build-id stamped into staging) → freshness gate → markers. No
// restart, no health gate, no smoke — the app and the edge are untouched
// by an asset swap.
func (r *deployRun) staticSwitch() error {
	bin := r.bin
	pj := r.target.Project

	if err := r.buildArtifact(); err != nil {
		return err
	}
	// The pre-swap served sha — the auto-restore's verification target.
	prevSha := strings.TrimSpace(r.vmTolerant("cat " + pj.DeployedShaFile + " 2>/dev/null"))

	pushed := r.step(fmt.Sprintf("push %s artifact → %s (atomic swap, no restart)", bin.Kind, bin.Dir))
	err := r.pushArtifact()
	pushed(err)
	if err != nil {
		r.cleanupStaging()
		return err
	}

	if err := r.staticFreshnessGate(r.ver); err != nil {
		r.say("✗ freshness gate failed — auto-restoring the previous artifact (%s.prev)", bin.Dir)
		if restoreErr := r.restorePreviousArtifact(); restoreErr != nil {
			return fmt.Errorf("%w — AND the auto-restore failed: %v — INSPECT THE VM", err, restoreErr)
		}
		if verr := r.staticFreshnessGate(prevSha); verr != nil && prevSha != "" {
			return fmt.Errorf("%w — auto-restore did not recover either (%v) — INSPECT THE VM", err, verr)
		}
		r.say("✓ previous artifact restored (serving %s)", prevSha)
		return err
	}
	// Success: NOW the identity bookkeeping advances.
	r.writeMarkers(r.runPath())
	return nil
}

// buildStaticDist runs the block's declared build command with cwd =
// buildDir and verifies the artifact dir exists. The command is the
// repo's own (e.g. "pnpm build --filter web") — kampodra knows how to
// BUILD a static artifact, not how every repo's frontend builds.
func (r *deployRun) buildStaticDist() error {
	bin := r.bin
	buildDir := filepath.Join(r.repoRoot, bin.BuildDir)
	done := r.step("build static dist (" + bin.Build + ")")
	cmd := exec.CommandContext(r.ctx, "sh", "-c", bin.Build)
	cmd.Dir = buildDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		err = fmt.Errorf("static build failed (%s): %w", bin.Build, err)
		done(err)
		return err
	}
	dist := filepath.Join(buildDir, bin.LocalArtifactDir())
	if st, err := os.Stat(dist); err != nil || !st.IsDir() {
		err = fmt.Errorf("static build produced no dist at %s — the \"artifact\" dir under buildDir (got: %v)", dist, err)
		done(err)
		return err
	}
	done(nil)
	r.localArtifactDir = dist
	return nil
}

// staticFreshnessGate verifies the mount serves the pushed sha: a VM-side
// fetch of /build-id.txt compared for equality. The swap is atomic and
// the static server reads per-request, so the first attempt normally
// passes — the retries absorb a server-side cache edge.
func (r *deployRun) staticFreshnessGate(sha string) error {
	url := fmt.Sprintf("http://127.0.0.1:%s%s", r.target.Project.Port, staticBuildIDPath)
	fetch := fmt.Sprintf("wget -qO- -T 3 %s 2>/dev/null || curl -s -m 3 %s 2>/dev/null", url, url)
	r.say("freshness gate (VM-side fetch of %s — must serve %s)…", staticBuildIDPath, sha)
	for attempt := 0; attempt < DeployGateAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(DeployGateSleep)
		}
		out, err := r.vm(fetch)
		if err == nil && strings.TrimSpace(out) == sha {
			r.say("✓ freshness gate passed on attempt %d (%s serves %s)", attempt+1, staticBuildIDPath, sha)
			return nil
		}
		if err != nil {
			r.say("… freshness gate attempt %d/%d — endpoint unreachable", attempt+1, DeployGateAttempts)
			continue
		}
		r.say("… freshness gate attempt %d/%d — serving %q (want %s)",
			attempt+1, DeployGateAttempts, strings.TrimSpace(out), sha)
	}
	return fmt.Errorf("%s still does not serve %s — the SPA mount did not take (is the unit's -v mount live? a freshly vm-prepared VM needs one image deploy/restart before the first static push)", staticBuildIDPath, sha)
}

// staticRollback restores the recorded previous dist. Its identity comes
// from the <run>.prev.sha marker (NOT git HEAD, NOT the image stamp) —
// only the last static deploy is undoable. No restart: the restored tree
// is served on the next request, re-verified through the same gate.
func (r *deployRun) staticRollback() error {
	pj := r.target.Project
	prevSha := strings.TrimSpace(r.vmTolerant("cat " + r.runPath() + ".prev.sha 2>/dev/null"))
	if prevSha == "" {
		return fmt.Errorf("no %s.prev.sha — only the last static deploy can be rolled back (image rollback: kampodra deploy --rollback)", r.runPath())
	}
	restored := r.step("restore previous static artifact from " + r.runPath() + ".prev")
	err := r.restorePreviousArtifact()
	restored(err)
	if err != nil {
		return err
	}
	if err := r.staticFreshnessGate(prevSha); err != nil {
		return err
	}
	// The stamp tracks WHAT IS SERVING (the restored sha).
	if _, err := r.vm(fmt.Sprintf("printf '%%s' %s > %s", prevSha, pj.DeployedShaFile)); err != nil {
		r.say("WARNING: could not rewrite %s — a bare image rollback will target the wrong sha", pj.DeployedShaFile)
	}
	// The marker chain re-aligns with the restored artifact.
	r.vmTolerant(fmt.Sprintf("{ [ -e %[1]s.prev.sha ] && mv %[1]s.prev.sha %[1]s.sha || true; }", r.runPath()))
	return nil
}

// declaredBinaries returns every declared binary block in a stable order
// (name-sorted): an image deploy mirrors ALL of them into their mounts —
// neither the backend binary nor the frontend dist may drift from a
// release (2026-10-11: the static kind made multi-block repos the norm,
// so the old lone-block sync with its skip-warning is gone).
func declaredBinaries(blocks map[string]project.ManifestBinary) []*project.ManifestBinary {
	names := sortedBinaryNames(blocks)
	out := make([]*project.ManifestBinary, 0, len(names))
	for _, n := range names {
		b := blocks[n]
		out = append(out, &b)
	}
	return out
}
