// deploy_binary.go — the image deploy's small sibling: the repo's declared
// artifact (kampodra.json "binary") crosses on its own — a local
// cross-build, an atomic remote swap, a restart — with the SAME identity
// gates (served-sha health gate, public smoke, stamp, ledger). Safety
// rails, in order of the failure they answer to:
//
//	wrong manifest/selector  → dies at the banner (ChooseBinaryBlock)
//	wrong deps (node)        → drift guard refuses before the push
//	build failure            → nothing on the VM is touched
//	upload/swap failure      → staging sits BESIDE the run path; the
//	                           running artifact never sees a partial file
//	health-gate failure      → the previous artifact (.prev) is auto-
//	                           restored, restarted and re-verified
//	public-smoke failure     → same auto-restore (the edge disagrees
//	                           with VM-local health = stale mount/proxy)
//	lost marker              → binary rollback refuses with the exact path

package command

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// The image deploy's small sibling: the repo's declared artifact
// (kampodra.json "binary") crosses on its own — a local cross-build, an
// atomic remote swap, a restart — with the SAME identity gates
// (served-sha health gate, public smoke, stamp, ledger). Safety rails,
// in order of the failure they answer to:
//
//	wrong manifest/selector  → dies at the banner (ChooseBinaryBlock)
//	wrong deps (node)        → drift guard refuses before the push
//	build failure            → nothing on the VM is touched
//	upload/swap failure      → staging sits BESIDE the run path; the
//	                           running artifact never sees a partial file
//	health-gate failure      → the previous artifact (.prev) is auto-
//	                           restored, restarted and re-verified
//	public-smoke failure     → same auto-restore (the edge disagrees
//	                           with VM-local health = stale mount/proxy)
//	lost marker              → binary rollback refuses with the exact path

func (r *deployRun) binarySwitch() error {
	// The static kind shares the machinery (staging swap, marker chain,
	// .prev rollback) but restarts nothing — its whole flow lives in
	// deploy_static.go.
	if r.bin.Kind == project.BinaryKindStatic {
		if r.mode == "rollback" {
			return r.staticRollback()
		}
		return r.staticSwitch()
	}
	if r.mode == "rollback" {
		return r.binaryRollback()
	}
	bin := r.bin
	pj := r.target.Project

	if err := r.buildArtifact(); err != nil {
		return err
	}
	if err := r.guardDepsDrift(); err != nil {
		return err
	}
	// The pre-swap served sha — the auto-restore's verification target.
	prevSha := strings.TrimSpace(r.vmTolerant("cat " + pj.DeployedShaFile + " 2>/dev/null"))

	pushed := r.step(fmt.Sprintf("push %s artifact → %s (atomic swap)", bin.Kind, bin.Dir))
	err := r.pushArtifact()
	pushed(err)
	if err != nil {
		r.cleanupStaging()
		return err
	}

	restarted := r.step("restart " + pj.Container + " (" + string(r.initSys) + ")")
	rcmd, err := initadapter.ActionCommand(r.initSys, pj.Container, "restart")
	if err != nil {
		restarted(err)
		return err
	}
	if _, err := r.vm(rcmd); err != nil {
		restarted(err)
		r.say("✗ restart failed — auto-restoring the previous artifact")
		if restoreErr := r.restorePreviousArtifact(); restoreErr != nil {
			return fmt.Errorf("restart failed: %w — AND the auto-restore failed: %v — INSPECT THE VM", err, restoreErr)
		}
		return fmt.Errorf("restart failed (previous artifact restored): %w", err)
	}
	restarted(nil)

	if err := r.healthGate(pj.Port, r.ver); err != nil {
		r.say("✗ health gate failed — auto-restoring the previous artifact (%s.prev)", bin.Entry)
		if restoreErr := r.restorePreviousArtifact(); restoreErr != nil {
			return fmt.Errorf("%w — AND the auto-restore failed: %v — INSPECT THE VM", err, restoreErr)
		}
		if verr := r.healthGate(pj.Port, prevSha); verr != nil && prevSha != "" {
			return fmt.Errorf("%w — auto-restore did not recover either (%v) — INSPECT THE VM", err, verr)
		}
		r.say("✓ previous artifact restored and healthy (serving %s)", prevSha)
		return err
	}

	smoked := r.step("public smoke through the proxy")
	err = r.publicSmoke()
	smoked(err)
	if err != nil {
		r.say("✗ public smoke failed — auto-restoring the previous artifact")
		if restoreErr := r.restorePreviousArtifact(); restoreErr != nil {
			return fmt.Errorf("%w — AND the auto-restore failed: %v — INSPECT THE VM", err, restoreErr)
		}
		return err
	}
	// Success: NOW the identity bookkeeping advances.
	r.writeMarkers(r.runPath())
	return nil
}

// buildArtifact produces the artifact locally: a sha-stamped cross-build
// for go/rust; the declared build command for static (its dist dir); the
// source tree as-is for node (its runtime compiles nothing — tsx serves
// the source).
func (r *deployRun) buildArtifact() error {
	bin := r.bin
	if bin.Kind == project.BinaryKindStatic {
		return r.buildStaticDist()
	}
	buildDir := filepath.Join(r.repoRoot, bin.BuildDir)
	switch bin.Kind {
	case project.BinaryKindGo:
		// Unique staging per run: a fixed temp name collides with any
		// stale leftover (go build refuses to overwrite a non-object
		// file) — a failed run must never poison the next one.
		out, err := stagingPath("kamdeploy-" + bin.Entry)
		if err != nil {
			return err
		}
		done := r.step("build go " + bin.Target + " (linux/arm64, CGO off, sha-stamped)")
		cmd := exec.CommandContext(r.ctx, "go", "build",
			"-ldflags", "-s -w -X main.buildSha="+r.ver+" -X main.gitSha="+r.ver,
			"-o", out, bin.Target)
		cmd.Dir = buildDir
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			err = fmt.Errorf("go build failed: %w", err)
			done(err)
			return err
		}
		done(nil)
		r.localArtifact = out
		return nil
	case project.BinaryKindRust:
		done := r.step("build cargo --release --target " + bin.RustTriple() + " --bin " + bin.Target)
		cmd := exec.CommandContext(r.ctx, "cargo", "build", "--release", "--target", bin.RustTriple(), "--bin", bin.Target)
		cmd.Dir = buildDir
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			err = fmt.Errorf("cargo build failed: %w", err)
			done(err)
			return err
		}
		done(nil)
		art := filepath.Join(buildDir, bin.LocalArtifactDir())
		if _, err := os.Stat(art); err != nil {
			return fmt.Errorf("cargo build produced no artifact at %s: %w", art, err)
		}
		r.localArtifact = art
		return nil
	case project.BinaryKindNode:
		dir := filepath.Join(buildDir, bin.LocalArtifactDir())
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return fmt.Errorf("node artifact dir %s is not a directory: %v", dir, err)
		}
		r.localArtifactDir = dir
		r.say("node artifact: source tree %s (no compile — %s serves it)", dir, bin.Exec)
		return nil
	}
	return fmt.Errorf("unknown binary kind %q (go, rust, node)", bin.Kind)
}

// guardDepsDrift is the node kind's dependency contract: the mount
// carries SOURCE, the image carries DEPS, and the marker hashed at the
// last image deploy must still match the local dependency manifest —
// otherwise the fast push refuses (code-only fast path).
func (r *deployRun) guardDepsDrift() error {
	if r.bin.Kind != project.BinaryKindNode {
		return nil
	}
	localHash, err := hashDepManifest(r.repoRoot, r.bin.DepsManifestFiles())
	if err != nil {
		return fmt.Errorf("cannot hash the dependency manifest (%v): %w", r.bin.DepsManifestFiles(), err)
	}
	remote := strings.TrimSpace(r.vmTolerant("cat " + r.bin.Dir + "/.deps-sha 2>/dev/null"))
	if remote == "" {
		return fmt.Errorf("no %s/.deps-sha marker — this mount's deps come from the image; redeploy the image once (kampodra deploy) so it stamps the marker, then code pushes may proceed", r.bin.Dir)
	}
	if remote != localHash {
		return fmt.Errorf("dependency manifest changed since the image stamped this mount (%s → %s) — the fast push is CODE-only: redeploy the image (kampodra deploy) to refresh deps, then push code", shortSha(remote), shortSha(localHash))
	}
	r.say("✓ dependency manifest unchanged (%s) — deps still come from the image", shortSha(localHash))
	return nil
}

func shortSha(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// pushArtifact moves the local artifact across the ssh and swaps it in:
// a single file (go/rust) or a directory tree (node, deps excluded).
func (r *deployRun) pushArtifact() error {
	bin := r.bin
	if bin.IsDirShape() {
		// Dir shapes stream a tar (non-deterministic bytes) — the
		// fingerprint gate below is single-file only for now.
		return r.pushArtifactDir()
	}
	f, err := os.Open(r.localArtifact)
	if err != nil {
		return fmt.Errorf("cannot read the built artifact: %w", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return fmt.Errorf("cannot hash the built artifact: %w", err)
	}
	localSum := hex.EncodeToString(h.Sum(nil))
	// The rebuild-didnt-happen catch (live-fire 2026-10-16: a stale
	// worktree binary shipped twice under two different sha labels —
	// identity rode the pushed API_GIT_SHA env, so nothing noticed): bytes
	// IDENTICAL to the last push while the stamped sha MOVED means the
	// build didn't pick up new code. WARN loudly; the deploy proceeds (a
	// deliberate re-push of the same bytes is legitimate).
	for _, e := range state.LedgerEntries(state.LedgerPath(r.d.Home), r.target.HostSpec.Host) {
		if e.Result != "success" && e.Result != "rollback" {
			continue
		}
		if e.ArtifactSHA256 != "" && e.ArtifactSHA256 == localSum && e.Sha != r.ver {
			r.say("WARN: artifact bytes are IDENTICAL to the %s push of %s — the build didn't change across a sha move to %s (stale build dir? wrong checkout?)",
				e.Result, e.Sha, r.ver)
			break
		}
	}
	r.artifactSHA256 = localSum
	tmp := filepath.Join(bin.Dir, "."+bin.Entry+".new")
	// Upload with ONE retry, then byte-verify: long stdin streams die
	// mid-transfer (2026-10-16: three truncated/corrupt 26.8MB pushes in a
	// row while small commands passed — a NAT drop the keepalives now
	// mitigate). Every failed attempt cleans its partial .new (no 11MB
	// litter beside the run path), and the fingerprint gate re-verifies
	// each attempt — a corrupt upload can never reach the swap.
	var lastErr error
	verified := false
	for attempt := 1; attempt <= 2 && !verified; attempt++ {
		if lastErr != nil {
			r.say("upload attempt %d failed — retrying: %v", attempt-1, lastErr)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return fmt.Errorf("cannot rewind the artifact: %w", err)
		}
		if _, err := r.d.Runner.RunWithStdin(r.ctx, r.target.HostSpec, "umask 077; cat > "+tmp, f); err != nil {
			lastErr = fmt.Errorf("artifact upload failed: %w", err)
			_, _ = r.d.Runner.Run(r.ctx, r.target.HostSpec, "rm -f "+tmp)
			continue
		}
		// Byte-verify the VM copy BEFORE the swap: what landed must equal
		// what was read — a drifting host artifact can never masquerade as
		// this push.
		sumOut, err := r.d.Runner.Run(r.ctx, r.target.HostSpec, "sha256sum "+tmp)
		if err != nil {
			lastErr = fmt.Errorf("cannot fingerprint the uploaded artifact: %w", err)
			_, _ = r.d.Runner.Run(r.ctx, r.target.HostSpec, "rm -f "+tmp)
			continue
		}
		remoteSum := strings.Fields(sumOut)
		if len(remoteSum) == 0 || remoteSum[0] != localSum {
			lastErr = fmt.Errorf("uploaded artifact DOES NOT match the local bytes (local sha256 %s, VM reports %s)", localSum, strings.TrimSpace(sumOut))
			_, _ = r.d.Runner.Run(r.ctx, r.target.HostSpec, "rm -f "+tmp)
			continue
		}
		verified = true
	}
	f.Close()
	if !verified {
		return fmt.Errorf("%w — refusing the swap; the host artifact is unreliable (one retry already attempted — check the VM's network/ssh)", lastErr)
	}
	r.say("artifact %d bytes, sha256 %s… verified on VM", r.artifactBytes(localSum), localSum[:12])
	return r.binarySwap(tmp, filepath.Join(bin.Dir, bin.Entry), "chmod 755 "+filepath.Join(bin.Dir, bin.Entry))
}

// artifactBytes reports the pushed size for the status line (0 = unknown).
func (r *deployRun) artifactBytes(_ string) int64 {
	if fi, err := os.Stat(r.localArtifact); err == nil {
		return fi.Size()
	}
	return 0
}

// pushArtifactDir tars the source tree (deps excluded — the image has
// them), extracts it into a staging sibling, then swaps the whole dir.
func (r *deployRun) pushArtifactDir() error {
	bin := r.bin
	tmpTar, err := stagingPath("kamdeploy-" + filepath.Base(bin.Dir) + ".tgz")
	if err != nil {
		return err
	}
	tar := exec.CommandContext(r.ctx, "tar", "-czf", tmpTar, "--exclude=node_modules", "-C", r.localArtifactDir, ".")
	tar.Stdout, tar.Stderr = os.Stdout, os.Stderr
	if err := tar.Run(); err != nil {
		return fmt.Errorf("tar of the source tree failed: %w", err)
	}
	f, err := os.Open(tmpTar)
	if err != nil {
		return fmt.Errorf("cannot read the packed tree: %w", err)
	}
	defer f.Close()
	staging := bin.Dir + ".staging"
	if _, err := r.vm(fmt.Sprintf("rm -rf %s && mkdir -p %s", staging, staging)); err != nil {
		return fmt.Errorf("staging dir create failed: %w", err)
	}
	if _, err := r.d.Runner.RunWithStdin(r.ctx, r.target.HostSpec, "umask 077; cat > "+staging+"/app.tgz", f); err != nil {
		return fmt.Errorf("tree upload failed: %w", err)
	}
	if _, err := r.vm(fmt.Sprintf("tar -xzf %s/app.tgz -C %s && rm -f %s/app.tgz && chmod -R a-s,u+rwX,go+rX %s", staging, staging, staging, staging)); err != nil {
		return fmt.Errorf("tree extract failed: %w", err)
	}
	// The static kind's freshness signal rides INTO the swap: the
	// build-id stamped now is served by the very next request.
	if r.bin.Kind == project.BinaryKindStatic {
		if _, err := r.vm(fmt.Sprintf("printf '%%s' %s > %s/build-id.txt", r.ver, staging)); err != nil {
			return fmt.Errorf("build-id stamp failed: %w", err)
		}
	}
	return r.binarySwap(staging, bin.Dir, "true")
}

// binarySwap is the atomic in-place swap the whole safety story rests on:
// one mkdir lock (a concurrent deploy dies naming it), the previous
// artifact kept at <run>.prev (the rollback target). Staging and run
// path share a filesystem, so the mv is atomic — the running process
// never observes a partial file. The SHA MARKER CHAIN is NOT written
// here: markers only advance on SUCCESS (writeMarkers, after the health
// gate) — a failed push must leave the identity bookkeeping exactly as
// it was (2026-10-10 live fire: a crashed restart left a marker naming
// an artifact the .prev file wasn't).
func (r *deployRun) binarySwap(staging, runPath, postCmd string) error {
	bin := r.bin
	lock := bin.Dir + "/.kamdeploy.lock"
	if _, err := r.vm(fmt.Sprintf("mkdir %s 2>/dev/null || { echo 'another binary deploy holds %s (stale? rm it)' >&2; exit 1; }", lock, lock)); err != nil {
		return fmt.Errorf("could not acquire the binary deploy lock: %w", err)
	}
	defer r.vmTolerant("rmdir " + lock)
	if _, err := r.vm(fmt.Sprintf("rm -rf %[1]s.prev && { [ -e %[1]s ] && mv %[1]s %[1]s.prev || true; } && mv %[2]s %[1]s && %[3]s", runPath, staging, postCmd)); err != nil {
		return fmt.Errorf("atomic swap into %s failed: %w", runPath, err)
	}
	return nil
}

// writeMarkers advances the sha marker chain AFTER the gates passed:
// <run>.sha names the artifact now serving, <run>.prev.sha the one it
// replaced (binary rollback's verification target).
func (r *deployRun) writeMarkers(runPath string) {
	if _, err := r.vm(fmt.Sprintf("{ [ -e %[1]s.sha ] && mv %[1]s.sha %[1]s.prev.sha || true; } && printf '%%s' %[2]s > %[1]s.sha", runPath, r.ver)); err != nil {
		r.say("WARNING: could not record %s.sha — a binary rollback will need the marker", runPath)
	}
}

func (r *deployRun) runPath() string {
	if r.bin.IsDirShape() {
		return r.bin.Dir
	}
	return filepath.Join(r.bin.Dir, r.bin.Entry)
}

// cleanupStaging removes a failed push's leftovers (the run path was
// never touched — staging sits beside it).
func (r *deployRun) cleanupStaging() {
	if r.bin == nil {
		return
	}
	switch {
	case r.bin.Kind == project.BinaryKindStatic:
		r.vmTolerant("rm -rf " + r.staticReleases() + "/.staging-*")
		if r.staticTarball != "" {
			_ = os.Remove(r.staticTarball)
		}
	case r.bin.IsDirShape():
		r.vmTolerant("rm -rf " + r.bin.Dir + ".staging")
	default:
		r.vmTolerant("rm -f " + filepath.Join(r.bin.Dir, "."+r.bin.Entry+".new"))
	}
}

// restorePreviousArtifact swaps <run>.prev back — the safety net behind
// a failed freshness gate (static) or health gate/smoke (exec kinds). The
// static kind restarts NOTHING: the app serves the restored mount on the
// next request.
func (r *deployRun) restorePreviousArtifact() error {
	prev := r.runPath() + ".prev"
	if r.vmTolerantErr("test -e "+prev) != nil {
		return fmt.Errorf("no previous artifact at %s — the FIRST binary push cannot be auto-restored (inspect the VM)", prev)
	}
	if _, err := r.vm(fmt.Sprintf("rm -rf %[1]s && mv %[1]s.prev %[1]s", r.runPath())); err != nil {
		return fmt.Errorf("restore swap failed: %w", err)
	}
	// Markers stay UNTOUCHED: the failed push never advanced them (the
	// success-path invariant), so .sha still names the artifact this
	// restore just brought back.
	if r.bin.Kind == project.BinaryKindStatic {
		r.say("static restore: %s serves the restored tree on the next request (no restart)", r.runPath())
		return nil
	}
	rcmd, err := initadapter.ActionCommand(r.initSys, r.target.Project.Container, "restart")
	if err != nil {
		return err
	}
	if _, err := r.vm(rcmd); err != nil {
		return fmt.Errorf("restart after restore failed: %w", err)
	}
	return nil
}

// binaryRollback restores the recorded previous artifact. Its identity
// comes from the <run>.prev.sha marker (NOT git HEAD, NOT the image
// stamp) — only the last binary deploy is undoable.
func (r *deployRun) binaryRollback() error {
	pj := r.target.Project
	runPath := r.runPath()
	prevSha := strings.TrimSpace(r.vmTolerant("cat " + runPath + ".prev.sha 2>/dev/null"))
	if prevSha == "" {
		return fmt.Errorf("no %s.prev.sha — only the last binary deploy can be rolled back (image rollback: kampodra deploy --rollback)", runPath)
	}
	restored := r.step("restore previous binary artifact from " + runPath + ".prev")
	err := r.restorePreviousArtifact()
	restored(err)
	if err != nil {
		return err
	}
	if err := r.healthGate(pj.Port, prevSha); err != nil {
		return err
	}
	smoked := r.step("public smoke through the proxy")
	err = r.publicSmoke()
	smoked(err)
	if err != nil {
		return err
	}
	// The stamp tracks WHAT IS SERVING: the restored sha, not the one the
	// failed deploy would have written (the image rollback's restore
	// verification reads this stamp).
	if _, err := r.vm(fmt.Sprintf("printf '%%s' %s > %s", prevSha, pj.DeployedShaFile)); err != nil {
		r.say("WARNING: could not rewrite %s — a bare image rollback will target the wrong sha", pj.DeployedShaFile)
	}
	// The marker chain re-aligns with the restored artifact: .sha names
	// what is serving again, and the chain stops here (the .prev file was
	// consumed by the restore).
	r.vmTolerant(fmt.Sprintf("{ [ -e %[1]s.prev.sha ] && mv %[1]s.prev.sha %[1]s.sha || true; }", runPath))
	return nil
}

// resolveBinaryRollbackSha reads the restore target BEFORE the banner
// prints (the version line is the restored sha).
func (r *deployRun) resolveBinaryRollbackSha() error {
	prev := strings.TrimSpace(r.vmTolerant("cat " + r.runPath() + ".prev.sha 2>/dev/null"))
	if prev == "" {
		return fmt.Errorf("no %s.prev.sha — only the last binary deploy can be rolled back (image rollback: kampodra deploy --rollback)", r.runPath())
	}
	r.ver = prev
	return nil
}

// syncAllMountsFromImage mirrors EVERY declared artifact from the image
// into its mount (pre-switch — see primaryImage). Fail-closed: a deploy
// whose identity cannot be mirrored into the mounts is not a deploy.
func (r *deployRun) syncAllMountsFromImage() error {
	blocks := declaredBinaries(r.target.Binary)
	if len(blocks) == 0 {
		return nil
	}
	pj := r.target.Project
	ctr := "kamdeploy-mount-sync"
	defer r.vmTolerant("podman rm -f " + ctr + " >/dev/null 2>&1 || true")
	if _, err := r.vm(fmt.Sprintf("podman create --name %s %s:%s >/dev/null", ctr, pj.ImagePrefix, r.ver)); err != nil {
		return fmt.Errorf("could not stage the image for the mount sync: %w", err)
	}
	for _, bin := range blocks {
		r.bin = bin
		synced := r.step("sync mounted artifact from the image (" + bin.Dir + ")")
		err := r.syncMountFromImageWith(ctr)
		synced(err)
		if err != nil {
			return err
		}
	}
	return nil
}

// syncMountFromImage refreshes the mounted artifact FROM the image after
// every image-mode deploy. Without it the unit's exec override would
// serve the last fast-pushed artifact after an image deploy — the
// mounted copy and the image would disagree. Fail-closed: a deploy
// whose identity cannot be mirrored into the mount is not a deploy.
func (r *deployRun) syncMountFromImageWith(ctr string) error {
	bin := r.bin
	if bin.Kind == project.BinaryKindStatic {
		return r.syncStaticFromImage(ctr)
	}
	if bin.IsDirShape() {
		staging := bin.Dir + ".staging"
		if _, err := r.vm(fmt.Sprintf("rm -rf %s && mkdir -p %s", staging, staging)); err != nil {
			return fmt.Errorf("sync staging create failed: %w", err)
		}
		if _, err := r.vm(fmt.Sprintf("podman cp %s:%s/. %s", ctr, bin.ImagePath, staging)); err != nil {
			return fmt.Errorf("podman cp of the image tree failed: %w", err)
		}
		if _, err := r.vm("chmod -R a-s,u+rwX,go+rX " + staging); err != nil {
			return fmt.Errorf("sync chmod failed: %w", err)
		}
		if err := r.binarySwap(staging, bin.Dir, "true"); err != nil {
			return err
		}
		r.writeMarkers(bin.Dir)
	} else {
		tmp := filepath.Join(bin.Dir, "."+bin.Entry+".new")
		if _, err := r.vm(fmt.Sprintf("podman cp %s:%s %s", ctr, bin.ImagePath, tmp)); err != nil {
			return fmt.Errorf("podman cp of the image binary failed: %w", err)
		}
		if err := r.binarySwap(tmp, filepath.Join(bin.Dir, bin.Entry), "chmod 755 "+filepath.Join(bin.Dir, bin.Entry)); err != nil {
			return err
		}
		r.writeMarkers(filepath.Join(bin.Dir, bin.Entry))
	}
	// The image IS the dependency snapshot (it was built from this same
	// clean tree): stamp the marker so node code pushes pass the guard.
	if bin.Kind == project.BinaryKindNode {
		hash, err := hashDepManifest(r.repoRoot, bin.DepsManifestFiles())
		if err != nil {
			return fmt.Errorf("cannot hash the dependency manifest for the marker: %w", err)
		}
		if _, err := r.vm(fmt.Sprintf("printf '%%s' %s > %s/.deps-sha", hash, bin.Dir)); err != nil {
			return fmt.Errorf("could not stamp the deps marker: %w", err)
		}
	}
	r.say("mounted artifact synced from the image (the unit's exec override serves the same version)")
	return nil
}

// stagingPath returns a FRESH local staging path (created-then-removed
// temp name — unique per run, so leftovers can never collide).
func stagingPath(base string) (string, error) {
	f, err := os.CreateTemp("", base)
	if err != nil {
		return "", fmt.Errorf("cannot create the staging path: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return name, nil
}

// hashDepManifest is the dependency closure's fingerprint: sha256 over
// each manifest file's path + bytes, in declared order.
func hashDepManifest(repoRoot string, files []string) (string, error) {
	h := sha256.New()
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n", rel)
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// epilogue is the shared tail: the image→mount sync (fail-closed — the
