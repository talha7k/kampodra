// The deploy build/stream pipeline — the port of the retired shell
// scripts/deploy.sh (behavior, not shell): clean-tree gate → podman build
// (GIT_SHA stamp) → podman save | ssh podman load → env push (the env
// family's 0600/atomic-mv path) → init restart → VM-side health gate with
// served-sha verify → kamal-proxy re-point → public smoke → tolerant image
// cleanup (keep-set) → deployed-sha stamp → disk report → ledger append +
// footer. Every failure after the sha is resolved records a `failed` ledger
// line — the EXIT-trap contract, ported as a deferred finalizer.

// Split: the skeleton (types, finalizer, execute/preflight, shared tail)
// lives here; the image-mode legs in deploy_image.go; the fast-push legs
// in deploy_binary.go.

package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/osfacts"
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/runtime"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// Health-gate and drain budgets. Exported ONLY so tests can shrink them to
// test scale; production callers use the defaults (30 × 3s of health
// patience, a 1s drain poll).
var (
	DeployGateAttempts      = 30
	DeployGateSleep         = 3 * time.Second
	DeployDrainPollInterval = time.Second
)

// deployCleanupKeepN is the post-deploy cleanup keep-set: the running image
// + ts-rollback + the newest 3 sha tags survive (the shell's tail -n +4).
const deployCleanupKeepN = 3

var shaFragmentRe = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

// deployOpts is the parsed pipeline invocation.
type deployOpts struct {
	dockerfile    string
	envFile       string
	diskThreshold string
	skipSmoke     bool
	rolling       bool
	drainTimeout  int
	sidecars      []project.ManifestSidecar
	binary        string // the kampodra.json "binary" artifact name ("" = image deploy)
}

// deployRun carries one pipeline execution: the resolved target, the
// resolved sha, and the mode. The finalizer implements the ledger EXIT
// trap: once recording, any exit without a success append records failed.
type deployRun struct {
	d              Deps
	ctx            context.Context
	target         Target
	ver            string
	mode           string // deploy | version | rollback
	opts           deployOpts
	started        time.Time
	recording      bool
	appended       bool
	skipStream     bool // rollback fast path: the tag is already on the VM
	initSys        initadapter.System
	repoRoot       string
	bin            *project.ManifestBinary // the fast-path artifact (binary mode only)
	binName        string                  // the winning "binary" block name (banner/rollback text)
	artifactSHA256 string                  // binary fast-push: the pushed artifact's sha256 (ledger + rebuild-didnt-happen warn)
	// local build outputs (binary mode): a single file (go/rust) or the
	// source tree (node).
	localArtifact    string
	localArtifactDir string
	staticTarball    string // the packed dist tarball (static mode)
}

func (r *deployRun) say(format string, args ...any) {
	fmt.Fprintf(r.d.Stdout, "[deploy] "+format+"\n", args...)
}

// step times one pipeline leg and prints the Railway-style ✓/✗ line with
// its duration (the deploy log answers "what ran, how long, what next"
// without an operator re-deriving it from wall-clock gaps):
//
//	done := r.step("build"); err := ...; done(err)
func (r *deployRun) step(name string) func(error) {
	start := time.Now()
	r.say("%s…", name)
	return func(err error) {
		d := time.Since(start).Round(10 * time.Millisecond)
		if err != nil {
			r.say("✗ %s failed after %s: %v", name, d, err)
			return
		}
		r.say("✓ %s (%s)", name, d)
	}
}

// banner prints the run's identity header before any mutation: where,
// what, and which strategy — the first thing an operator reads when
// triaging a deploy log tail.
func (r *deployRun) banner() {
	pj := r.target.Project
	mode := "rolling image deploy (zero-downtime shadow + proxy switch)"
	if r.opts.binary != "" {
		mode = "binary fast-push — code only; dependency changes redeploy the image"
		if r.bin != nil && r.bin.Kind == project.BinaryKindStatic {
			mode = "static asset push — no restart; the app serves the mount per-request"
		}
	} else if !r.opts.rolling {
		mode = "in-place image deploy (restart)"
	}
	if r.mode == "rollback" {
		mode = "rollback — restoring the previous artifact"
	}
	r.say("── target %s · profile %s · container %s · port %s",
		r.target.HostSpec.Host, displayProfile(r.target.ProfileName), pj.Container, pj.Port)
	r.say("── mode   %s", mode)
	// WHICH tree is shipping: a wrong-checkout deploy (worktree vs shared
	// checkout) otherwise only surfaces as a confusing health-gate failure
	// long after the fact (2026-10-16 complaint).
	if r.repoRoot != "" {
		r.say("── build root %s @ %s (clean-tree gate passed)", r.repoRoot, r.ver)
	}
	if r.opts.binary != "" && r.bin != nil && r.repoRoot != "" {
		// Staleness heuristic: an artifact older than HEAD was probably not
		// built from this tree (the other-checkout mistake). mtime lies
		// across copies — WARN, never fail; the served-sha gate stays the
		// identity check.
		art := filepath.Join(r.repoRoot, r.bin.BuildDir, r.bin.Artifact)
		if fi, err := os.Stat(art); err == nil {
			if out, err := localOutput(r.ctx, r.repoRoot, "git", "log", "-1", "--format=%ct"); err == nil {
				if commitAt, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil && fi.ModTime().Unix() < commitAt {
					r.say("WARN: artifact %s (mtime %s) predates HEAD %s — was it built from THIS tree? the served-sha gate will catch a stale build, after the push",
						art, fi.ModTime().Format(time.RFC3339), r.ver)
				}
			}
		}
	}
	if r.opts.binary != "" {
		r.say("── artifact %s (%s) → %s", r.binName, r.bin.Kind, r.bin.Dir)
	} else {
		r.say("── image  %s:%s (build identity: GIT_SHA=%s)", pj.ImagePrefix, r.ver, r.ver)
	}
	r.say("── public %s", r.target.ProxyHost)
}

func displayProfile(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

func (r *deployRun) vm(remote string) (string, error) {
	return r.d.Runner.Run(r.ctx, r.target.HostSpec, remote)
}

func (r *deployRun) vmTolerant(remote string) string {
	out, _ := r.vm(remote)
	return out
}

// --- local (deploy-machine) exec ------------------------------------------

// localOutput runs a command on the deploy machine and captures stdout
// (git reads; `podman image exists`). dir="" means the process cwd.
func localOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, err)
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// localPassthrough runs a long local command with stdio inherited (podman
// build progress belongs on the operator's terminal, not in a buffer).
func localPassthrough(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// --- entry -----------------------------------------------------------------

// runDeployPipeline executes the deploy/version pipeline (rolling when
// opted in). Returns the process error; the ledger finalizer has already
// recorded the outcome when it returns.
func runDeployPipeline(d Deps, ctx context.Context, target Target, ver string, mode string, opts deployOpts) error {
	r := &deployRun{d: d, ctx: ctx, target: target, ver: ver, mode: mode, opts: opts, started: time.Now()}
	err := r.execute()
	return r.finalize(err)
}

// finalize is the EXIT-trap port: after recording starts, any error exit
// appends a failed ledger line and prints the FAILED footer (once — a
// successful pipeline already appended its own line).
func (r *deployRun) finalize(err error) error {
	if !r.recording || r.appended {
		return err
	}
	duration := time.Since(r.started).Milliseconds()
	_ = state.AppendEntry(state.LedgerPath(r.d.Home), state.LedgerEntry{
		Host: r.target.HostSpec.Host, Sha: r.ver, Tag: r.ver,
		Result: "failed", DurationMs: int(duration),
	})
	if err != nil {
		r.say("Total deployments: %d · FAILED — attempted tag: %s (not activated)",
			state.LedgerCount(state.LedgerPath(r.d.Home), r.target.HostSpec.Host), r.ver)
	}
	return err
}

func (r *deployRun) appendLedger(result, subject string) error {
	duration := time.Since(r.started).Milliseconds()
	err := state.AppendEntry(state.LedgerPath(r.d.Home), state.LedgerEntry{
		Host: r.target.HostSpec.Host, Sha: r.ver, Tag: r.ver,
		Result: result, DurationMs: int(duration), Subject: subject,
		ArtifactSHA256: r.artifactSHA256,
	})
	r.appended = true
	return err
}

// execute runs the shared prelude, then branches: rolling (shadow double
// re-point) or the standard restart path. Rollback reuses execute with
// mode=rollback (no build, no clean gate, stream only when the VM lacks
// the tag, no stamp write).
func (r *deployRun) execute() error {
	pj := r.target.Project

	if err := r.preflight(); err != nil {
		return err
	}
	// The fast path resolves its artifact block before any mutation: a
	// wrong selector must fail here, at the banner, not at the swap.
	if r.opts.binary != "" {
		name, bin, err := ChooseBinaryBlockNamed(r.target.Binary, r.opts.binary)
		if err != nil {
			return err
		}
		r.bin = bin
		r.binName = name
		if r.mode == "rollback" {
			// The restore target is the recorded previous artifact's
			// marker — resolve it before the banner prints.
			if err := r.resolveBinaryRollbackSha(); err != nil {
				return err
			}
		}
	}
	r.banner()

	if r.opts.binary == "" {
		done := r.step("build + stream image " + pj.ImagePrefix + ":" + r.ver)
		err := r.primaryImage()
		done(err)
		if err != nil {
			return err
		}
	} else {
		r.say("--binary: skipping the image build/stream — the artifact crosses on its own")
	}

	// --- env file (the env family: 0600 temp + atomic mv, fingerprints
	// only on stdout; the deploy stamps API_GIT_SHA into the payload) -----
	if r.opts.envFile != "" {
		done := r.step("push env file (fingerprints only, values never printed)")
		err := r.pushEnvFile()
		done(err)
		if err != nil {
			return err
		}
	} else {
		r.say("no --env-file — leaving %s unchanged", pj.EnvFile)
	}

	// --- init detection (one round-trip; cached in the profile) ----------
	initSys, _, _ := initadapter.Detect(r.ctx, func(_ context.Context, remote string) (string, error) {
		return r.vm(remote)
	}, r.target.ProfileInit)
	r.initSys = initSys

	if r.opts.binary != "" {
		if err := r.binarySwitch(); err != nil {
			return err
		}
		return r.epilogue()
	}

	if r.opts.rolling {
		if err := r.executeRolling(); err != nil {
			return err
		}
		return r.epilogue()
	}

	if err := r.inPlaceSwitch(); err != nil {
		return err
	}
	return r.epilogue()
}

// preflight runs the fail-before-mutating checks in order: repo identity
// + clean-tree gate (deploy mode only — the build stamps the git sha, so
// the tree must BE what HEAD names), podman reachability (deploy/version
// always build or stream), the VM disk gate — then flips the run into the
// EXIT-trap recording window.
func (r *deployRun) preflight() error {
	if r.mode == "deploy" {
		done := r.step("gate: git repo + clean tree (the build ships what HEAD names)")
		root, err := localOutput(r.ctx, "", "git", "rev-parse", "--show-toplevel")
		if err != nil {
			err = fmt.Errorf("not a git repository — deploy stamps the git sha of HEAD; from a non-repo directory use --sha <sha7> to stream an existing build")
			done(err)
			return err
		}
		r.repoRoot = strings.TrimSpace(root)
		dirty, err := localOutput(r.ctx, r.repoRoot, "git", "status", "--porcelain")
		if err != nil {
			err = fmt.Errorf("git status failed — cannot verify the tree is clean: %w", err)
			done(err)
			return err
		}
		if strings.TrimSpace(dirty) != "" {
			err = fmt.Errorf("dirty tree — deploys must ship COMMITTED files (build identity stamps the git sha); commit first, or stream an existing build with --sha <sha7>")
			done(err)
			return err
		}
		// The Dockerfile ladder resolves relative to the PROCESS CWD, but
		// the build context is repoRoot — a CWD != repoRoot (subdir, or a
		// sibling checkout) would build THIS tree's files with ANOTHER
		// tree's recipe. Anchor -f to repoRoot; refuse anything that
		// escapes it: the build must ship what HEAD names, recipe included
		// (live-fire 2026-10-16 complaint: a stale shared checkout fed the
		// build while the worktree's sha got stamped).
		if !filepath.IsAbs(r.opts.dockerfile) {
			r.opts.dockerfile = filepath.Join(r.repoRoot, r.opts.dockerfile)
		}
		rel, err := filepath.Rel(r.repoRoot, r.opts.dockerfile)
		if err != nil || strings.HasPrefix(rel, "..") {
			err = fmt.Errorf("--dockerfile %s lives OUTSIDE the deploy root %s — the stamped sha would not describe the recipe that built the image; run from the tree you mean to ship", r.opts.dockerfile, r.repoRoot)
			done(err)
			return err
		}
		done(nil)
	}
	if r.mode != "rollback" && r.opts.binary == "" {
		done := r.step("gate: local podman machine reachable")
		if _, err := localOutput(r.ctx, "", "podman", "info"); err != nil {
			err = fmt.Errorf("podman machine not reachable (podman machine start): %w", err)
			done(err)
			return err
		}
		done(nil)
	}
	{
		done := r.step("gate: VM disk (df " + deployDiskPath + ")")
		err := r.vmDiskCheck("gate")
		done(err)
		if err != nil {
			return err
		}
	}
	// from here on, every outcome is recorded (the EXIT-trap contract)
	r.recording = true
	return nil
}

// cleanup (dangling + sha tags beyond the keep-set), the deployed-sha
// stamp (rollback never rewrites it), the post-deploy disk report, the
// ledger append and the verdict.
func (r *deployRun) epilogue() error {
	pj := r.target.Project
	r.say("cleanup (dangling + sha tags beyond the newest %d — tolerant, never fatal)…", deployCleanupKeepN)
	r.vmTolerant("podman image prune -f >/dev/null 2>&1 || true")
	images := runtime.ParseImages(r.vmTolerant(fmt.Sprintf("podman images --format '{{.Tag}}|{{.CreatedAt}}|{{.Size}}' %s 2>/dev/null", pj.ImagePrefix)))
	ps := r.vmTolerant("podman ps --format '{{.Image}}' 2>/dev/null")
	if removals, err := runtime.PruneSelect(runtime.ShaTagged(images), ps, deployCleanupKeepN); err == nil {
		for _, rem := range removals {
			r.vmTolerant(fmt.Sprintf("podman rmi %s:%s >/dev/null 2>&1 || true", pj.ImagePrefix, rem.Tag))
		}
	}

	if r.mode != "rollback" {
		if _, err := r.vm(fmt.Sprintf("echo %s > %s", r.ver, pj.DeployedShaFile)); err != nil {
			r.say("WARNING: could not update %s — a bare --rollback will need the explicit sha", pj.DeployedShaFile)
		}
	}

	// post-deploy disk report (warn-only — the deploy already succeeded)
	_ = r.vmDiskCheck("report")

	result := "success"
	subject := ""
	if r.mode == "rollback" {
		// A rollback's subject is the ORIGINAL deploy's subject — the ledger
		// knows it; git HEAD would name the wrong commit.
		result = "rollback"
		subject = state.LedgerSubjectForTag(state.LedgerPath(r.d.Home), r.target.HostSpec.Host, r.ver)
	} else {
		subject = strings.TrimSpace(localOutputOrEmpty(r.ctx, r.repoRoot, "git", "log", "-1", "--pretty=%s"))
	}
	if err := r.appendLedger(result, subject); err != nil {
		r.say("WARNING: ledger append failed: %v", err)
	}
	r.say("Total deployments: %d · current tag: %s",
		state.LedgerCount(state.LedgerPath(r.d.Home), r.target.HostSpec.Host), r.ver)
	r.say("done (version: %s). instant rollback: %s", r.ver, rollbackCommand(r.binName))
	r.verdict(result, subject)
	return nil
}

// rollbackCommand names the exact undo for the mode that just ran.
func rollbackCommand(binaryName string) string {
	if binaryName != "" {
		return "kampodra deploy --binary " + binaryName + " --rollback"
	}
	return "kampodra deploy --rollback"
}

// verdict is the closing block — the deploy log's answer to "what is now
// serving, and how do I undo it": identity, how it was verified, and the
// exact rollback command.
func (r *deployRun) verdict(result, subject string) {
	r.say("── %s ──────────────────────────────", strings.ToUpper(result))
	r.say("host     %s (%s)", r.target.HostSpec.Host, r.target.ProxyHost)
	r.say("version  %s %q", r.ver, subject)
	if r.bin != nil {
		r.say("artifact %s (block %s, %s; previous kept at .prev)", r.bin.Dir, r.binName, r.bin.Kind)
	} else {
		r.say("image    %s:%s", r.target.Project.ImagePrefix, r.ver)
	}
	if r.bin != nil && r.bin.Kind == project.BinaryKindStatic {
		r.say("verified %s serves the pushed sha (no restart — the next request sees the new dist)", staticBuildIDPath)
	} else {
		r.say("verified served git sha via %s + public smoke", r.target.Project.HealthPath)
	}
	r.say("rollback %s", rollbackCommand(r.binName))
	r.say("─────────────────────────────────────")
}

// vmDiskCheck ports vm_disk_check: read-only VM df; the --disk-threshold
// fail-closed gate (phase=gate); the always-on loud warning above 90%
// (report phase is warn-only by definition).
func (r *deployRun) vmDiskCheck(phase string) error {
	out := r.vmTolerant(fmt.Sprintf("df -P %s 2>/dev/null", deployDiskPath))
	pct, ok := osfacts.DiskUsedPct(out)
	verdict := osfacts.DiskVerdict(pctString(pct, ok), r.opts.diskThreshold)
	switch verdict {
	case osfacts.VerdictFail:
		return fmt.Errorf("VM disk at %d%% (>= --disk-threshold %s%%) — free space first: kampodra deploy prune --dry-run", pct, r.opts.diskThreshold)
	case osfacts.VerdictWarn:
		r.say("WARNING: VM disk %s at %d%% — old sha-tagged deploy images pile up (~1GB each)", deployDiskPath, pct)
		r.say("WARNING: reclaim space: kampodra deploy prune --dry-run (this deploy continues)")
	case osfacts.VerdictUnknown:
		if phase == "gate" && r.opts.diskThreshold != "" {
			return fmt.Errorf("cannot read VM disk usage (df %s) — --disk-threshold is fail-closed", deployDiskPath)
		}
		if phase == "gate" {
			r.say("disk usage unknown (df unreadable) — continuing")
		}
	}
	return nil
}

func hasExactLine(text, line string) bool {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

func localOutputOrEmpty(ctx context.Context, dir, name string, args ...string) string {
	out, err := localOutput(ctx, dir, name, args...)
	if err != nil {
		return ""
	}
	return out
}
