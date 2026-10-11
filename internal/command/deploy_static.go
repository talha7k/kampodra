// The static artifact kind: the frontend SPA fast-push. A local build
// command produces the dist; the dist crosses as a tarball and lands as
// a RELEASE under the mount (releases/<sha>/), and the app reads it
// through the `current` symlink — flipped ATOMICALLY (rename(2) over
// the symlink, busybox-safe). Nothing restarts: the app resolves
// WEB_DIST_DIR per-request, so the next request already sees the new
// SPA.
//
// WHY a symlink flip and not a directory swap: the unit's bind mount
// pins the MOUNTED DIR'S INODE — renaming the host directory is
// invisible inside the container (2026-10-11 live fire: the swap "took"
// on the host and the app served the old tree forever). Renames INSIDE
// the mounted dir are visible; the symlink lives inside it.
//
// Layout under bin.Dir (the stable bind):
//
//	releases/<sha>/   — one dist per push (the seed release ships with
//	                    the first image deploy / vm-prepare pre-seed)
//	current           — symlink -> releases/<served sha> (the flip target)
//	current.sha       — the served sha (advanced only on success)
//	current.prev.sha  — the sha current pointed at before the flip
//	.kamdeploy.lock/  — the concurrency lock
//
// Safety rails, in order of the failure they answer to:
//
//	wrong manifest/selector  → dies at the banner (ChooseBinaryBlock)
//	build failure            → nothing on the VM is touched
//	upload/flip failure      → staging sits under releases/; the served
//	                           symlink never points at a partial tree
//	freshness-gate failure   → the symlink flips BACK to the previous
//	                           release and is re-verified (no restart)
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

// staticCurrent / staticReleases name the layout inside the stable mount.
func (r *deployRun) staticCurrent() string  { return filepath.Join(r.bin.Dir, "current") }
func (r *deployRun) staticReleases() string { return filepath.Join(r.bin.Dir, "releases") }

// readlinkCurrent returns the symlink's release name (e.g. "95d1bd848")
// — the currently SERVED release, and the auto-restore's flip-back
// target. Empty = no current yet (first push).
func (r *deployRun) readlinkCurrent() string {
	out := strings.TrimSpace(r.vmTolerant("readlink " + r.staticCurrent() + " 2>/dev/null"))
	return strings.TrimPrefix(out, "releases/")
}

// staticFlip points current at releases/<release>. BUSYBOX REALITY
// (2026-10-11 live fire): `mv -f new current` FOLLOWS the destination
// symlink into the directory it points at — the new symlink lands
// INSIDE the old release and the flip silently never happens. `ln -sfn`
// replaces the symlink in place (unlink + create); the microseconds
// window is absorbed by the app's shell fallback (an in-flight request
// gets the previous shell — never a 404, never a partial tree).
func (r *deployRun) staticFlip(release string) error {
	cur := r.staticCurrent()
	if _, err := r.vm(fmt.Sprintf("ln -sfn releases/%s %s", release, cur)); err != nil {
		return fmt.Errorf("flip of %s -> releases/%s failed: %w", cur, release, err)
	}
	return nil
}

// staticSwitch is the static kind's forward push: build → stage the
// release inside the mount → atomic flip → freshness gate → markers. No
// restart, no health gate, no smoke — the app and the edge are untouched
// by an asset swap.
func (r *deployRun) staticSwitch() error {
	bin := r.bin

	if err := r.buildArtifact(); err != nil {
		return err
	}
	prevRelease := r.readlinkCurrent()

	pushed := r.step(fmt.Sprintf("push %s artifact → %s (release + atomic flip, no restart)", bin.Kind, bin.Dir))
	if err := r.packStaticTarball(); err != nil {
		pushed(err)
		return err
	}
	err := r.pushStaticArtifact(prevRelease)
	pushed(err)
	if err != nil {
		r.cleanupStaging()
		return err
	}

	if err := r.staticFreshnessGate(r.ver); err != nil {
		r.say("✗ freshness gate failed — flipping back to the previous release (%s)", prevRelease)
		if restoreErr := r.staticRestore(prevRelease); restoreErr != nil {
			return fmt.Errorf("%w — AND the auto-restore failed: %v — INSPECT THE VM", err, restoreErr)
		}
		if verr := r.staticFreshnessGate(prevRelease); verr != nil && prevRelease != "" {
			return fmt.Errorf("%w — auto-restore did not recover either (%v) — INSPECT THE VM", err, verr)
		}
		r.say("✓ previous release restored (serving %s)", prevRelease)
		return err
	}
	// Success: NOW the identity bookkeeping advances.
	r.writeMarkers(r.staticCurrent())
	return nil
}

// pushStaticArtifact stages the dist INSIDE the mount (releases/), then
// flips current atomically. Every step operates on paths under the
// mounted inode — visible to the app immediately.
func (r *deployRun) pushStaticArtifact(prevRelease string) error {
	bin := r.bin
	rel := r.staticReleases()
	lock := bin.Dir + "/.kamdeploy.lock"
	stagingID := fmt.Sprintf(".staging-%d", os.Getpid())
	staging := rel + "/" + stagingID
	if _, err := r.vm(fmt.Sprintf("mkdir %s 2>/dev/null || { echo 'another binary deploy holds %s (stale? rm it)' >&2; exit 1; }", lock, lock)); err != nil {
		return fmt.Errorf("could not acquire the binary deploy lock: %w", err)
	}
	defer r.vmTolerant("rmdir " + lock)
	if _, err := r.vm(fmt.Sprintf("mkdir -p %[1]s && rm -rf %[1]s/.staging-*", rel)); err != nil {
		return fmt.Errorf("releases dir create failed: %w", err)
	}

	f, err := os.Open(r.staticTarball)
	if err != nil {
		return fmt.Errorf("cannot read the packed dist: %w", err)
	}
	defer f.Close()
	if _, err := r.d.Runner.RunWithStdin(r.ctx, r.target.HostSpec, "umask 077; cat > "+rel+"/"+stagingID+".tgz", f); err != nil {
		return fmt.Errorf("dist upload failed: %w", err)
	}
	if _, err := r.vm(fmt.Sprintf("mkdir -p %[1]s && tar -xzf %[1]s.tgz -C %[1]s && rm -f %[1]s.tgz && chmod -R a-s,u+rwX,go+rX %[1]s", staging)); err != nil {
		return fmt.Errorf("dist extract failed: %w", err)
	}
	// The build-id rides in the dist (VITE_BUILD_ID) — but stamp it
	// authoritatively at the release root too, so the gate verifies what
	// THIS deploy pushed even if the build script's resolver drifted.
	if _, err := r.vm(fmt.Sprintf("printf '%%s' %s > %s/build-id.txt", r.ver, staging)); err != nil {
		r.say("WARNING: could not stamp build-id.txt into the release")
	}
	if _, err := r.vm(fmt.Sprintf("rm -rf %[1]s/%[2]s && mv %[1]s/%[3]s %[1]s/%[2]s", rel, r.ver, stagingID)); err != nil {
		return fmt.Errorf("release promote failed: %w", err)
	}
	if err := r.staticFlip(r.ver); err != nil {
		return err
	}
	// Prune: keep the served release and the flip-back target.
	r.vmTolerant(fmt.Sprintf("find %[1]s -mindepth 1 -maxdepth 1 -type d ! -name '%[2]s' ! -name '%[3]s' ! -name '.staging*' -exec rm -rf {} +", rel, r.ver, prevRelease))
	return nil
}

// packStaticTarball packs the built dist (deps-free — a dist carries no
// node_modules) into a unique local staging tarball.
func (r *deployRun) packStaticTarball() error {
	tmpTar, err := stagingPath("kamdeploy-" + filepath.Base(r.bin.Dir) + ".tgz")
	if err != nil {
		return err
	}
	tar := exec.CommandContext(r.ctx, "tar", "-czf", tmpTar, "-C", r.localArtifactDir, ".")
	tar.Stdout, tar.Stderr = os.Stdout, os.Stderr
	if err := tar.Run(); err != nil {
		return fmt.Errorf("tar of the dist failed: %w", err)
	}
	r.staticTarball = tmpTar
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
	// The build identity rides in as KAMPODRA_SHA — the SPA's
	// VITE_BUILD_ID (build-id.txt + the client bundle) stamps from it so
	// the freshness gate has something true to verify.
	cmd.Env = append(os.Environ(), "KAMPODRA_SHA="+r.ver)
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

// staticRestore flips current back to a previous release — the static
// kind's auto-restore. No restart, no marker moves: the failed push
// never advanced the chain.
func (r *deployRun) staticRestore(prevRelease string) error {
	if prevRelease == "" {
		return fmt.Errorf("no previous release recorded — the FIRST static push cannot be auto-restored (inspect the VM)")
	}
	if r.vmTolerantErr(fmt.Sprintf("test -d %s/releases/%s", r.bin.Dir, prevRelease)) != nil {
		return fmt.Errorf("previous release %s/releases/%s is gone — cannot auto-restore (inspect the VM)", r.bin.Dir, prevRelease)
	}
	if err := r.staticFlip(prevRelease); err != nil {
		return err
	}
	return nil
}

// staticRollback restores the recorded previous release. Its identity
// comes from the current.prev.sha marker (NOT git HEAD, NOT the image
// stamp) — only the last static deploy is undoable. No restart: the
// restored tree is served on the next request, re-verified through the
// same gate.
func (r *deployRun) staticRollback() error {
	pj := r.target.Project
	prevSha := strings.TrimSpace(r.vmTolerant("cat " + r.staticCurrent() + ".prev.sha 2>/dev/null"))
	if prevSha == "" {
		return fmt.Errorf("no %s.prev.sha — only the last static deploy can be rolled back (image rollback: kampodra deploy --rollback)", r.staticCurrent())
	}
	restored := r.step("flip back to the previous release (releases/" + prevSha + ")")
	err := r.staticRestore(prevSha)
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
	// The marker chain re-aligns with the restored release.
	r.vmTolerant(fmt.Sprintf("{ [ -e %[1]s.prev.sha ] && mv %[1]s.prev.sha %[1]s.sha || true; }", r.staticCurrent()))
	return nil
}

// syncStaticFromImage mirrors the image's dist into the release layout
// (a new release + flip) — the image-side half of the static parity
// contract. The image IS the dependency snapshot (it was built from this
// same clean tree); its build-id stamps into the release so the next
// code push's gate has a coherent baseline.
func (r *deployRun) syncStaticFromImage(ctr string) error {
	bin := r.bin
	rel := r.staticReleases()
	lock := bin.Dir + "/.kamdeploy.lock"
	stagingID := ".staging-sync"
	staging := rel + "/" + stagingID
	if _, err := r.vm(fmt.Sprintf("mkdir %s 2>/dev/null || { echo 'another binary deploy holds %s (stale? rm it)' >&2; exit 1; }", lock, lock)); err != nil {
		return fmt.Errorf("could not acquire the binary deploy lock: %w", err)
	}
	defer r.vmTolerant("rmdir " + lock)
	if _, err := r.vm(fmt.Sprintf("mkdir -p %[1]s && rm -rf %[1]s/.staging-* && mkdir -p %[2]s", rel, staging)); err != nil {
		return fmt.Errorf("sync staging create failed: %w", err)
	}
	if _, err := r.vm(fmt.Sprintf("podman cp %s:%s/. %s", ctr, bin.ImagePath, staging)); err != nil {
		return fmt.Errorf("podman cp of the image dist failed: %w", err)
	}
	if _, err := r.vm(fmt.Sprintf("chmod -R a-s,u+rwX,go+rX %s && printf '%%s' %s > %s/build-id.txt", staging, r.ver, staging)); err != nil {
		return fmt.Errorf("sync stamp failed: %w", err)
	}
	if _, err := r.vm(fmt.Sprintf("rm -rf %[1]s/%[2]s && mv %[1]s/%[3]s %[1]s/%[2]s", rel, r.ver, stagingID)); err != nil {
		return fmt.Errorf("sync release promote failed: %w", err)
	}
	prevRelease := r.readlinkCurrent()
	if err := r.staticFlip(r.ver); err != nil {
		return err
	}
	r.writeMarkers(r.staticCurrent())
	r.vmTolerant(fmt.Sprintf("find %[1]s -mindepth 1 -maxdepth 1 -type d ! -name '%[2]s' ! -name '%[3]s' ! -name '.staging*' -exec rm -rf {} +", rel, r.ver, prevRelease))
	r.say("static release synced from the image (releases/%s served via the flipped symlink)", r.ver)
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
