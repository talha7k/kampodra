package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/talha7k/kampodra/internal/adapter/envfile"
	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/osfacts"
	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/runtime"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// The deploy build/stream pipeline — the port of the retired shell
// scripts/deploy.sh (behavior, not shell): clean-tree gate → podman build
// (GIT_SHA stamp) → podman save | ssh podman load → env push (the env
// family's 0600/atomic-mv path) → init restart → VM-side health gate with
// served-sha verify → kamal-proxy re-point → public smoke → tolerant image
// cleanup (keep-set) → deployed-sha stamp → disk report → ledger append +
// footer. Every failure after the sha is resolved records a `failed` ledger
// line — the EXIT-trap contract, ported as a deferred finalizer.

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
	d          Deps
	ctx        context.Context
	target     Target
	ver        string
	mode       string // deploy | version | rollback
	opts       deployOpts
	started    time.Time
	recording  bool
	appended   bool
	skipStream bool // rollback fast path: the tag is already on the VM
	initSys    initadapter.System
	repoRoot   string
	bin        *project.ManifestBinary // the fast-path artifact (binary mode only)
	binName    string                  // the winning "binary" block name (banner/rollback text)
	// local build outputs (binary mode): a single file (go/rust) or the
	// source tree (node).
	localArtifact    string
	localArtifactDir string
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
	} else if !r.opts.rolling {
		mode = "in-place image deploy (restart)"
	}
	if r.mode == "rollback" {
		mode = "rollback — restoring the previous artifact"
	}
	r.say("── target %s · profile %s · container %s · port %s",
		r.target.HostSpec.Host, displayProfile(r.target.ProfileName), pj.Container, pj.Port)
	r.say("── mode   %s", mode)
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

// primaryImage carries the primary app image through build → stream →
// retag → sidecars (the ROLLING path retags later, inside the switch —
// see the retag guard below).
func (r *deployRun) primaryImage() error {
	pj := r.target.Project
	// --- build (deploy mode always builds; version/rollback never) --------
	if r.mode == "deploy" {
		r.say("building linux/arm64 (GIT_SHA=%s, -f %s) — build identity, never remove…", r.ver, r.opts.dockerfile)
		buildArgs := []string{"build", "--platform", "linux/arm64",
			"-f", r.opts.dockerfile,
			"--build-arg", "GIT_SHA=" + r.ver,
			"-t", pj.ImagePrefix + ":" + r.ver,
			r.repoRoot}
		if err := localPassthrough(r.ctx, "podman", buildArgs...); err != nil {
			return fmt.Errorf("podman build failed: %w", err)
		}
	}

	// --- stream (skipped when the VM already has the tag: rollback to an
	// image that never left the host is instant and works even when the
	// local build is long gone; an explicit --sha deploy of a sha that
	// already lives on the VM is the same case — verified live 2026-10-08
	// when re-deploying an existing sha tried to stream from a machine
	// that never had it) ------------------------------------------
	if (r.mode == "rollback" || r.mode == "sha") && !r.skipStream {
		if err := r.vmTolerantErr(fmt.Sprintf("podman image exists %s:%s", pj.ImagePrefix, r.ver)); err == nil {
			r.say("%s:%s already on the VM — skipping build/stream", pj.ImagePrefix, r.ver)
			r.skipStream = true
		}
	}
	if !r.skipStream {
		if r.mode == "rollback" {
			// The stream source is this machine's image — it must exist
			// locally (its podman info is checked implicitly by the lookup).
			if _, err := localOutput(r.ctx, "", "podman", "image", "exists", pj.ImagePrefix+":"+r.ver); err != nil {
				return fmt.Errorf("neither the VM nor this machine has %s:%s — kampodra deploy list shows what is available", pj.ImagePrefix, r.ver)
			}
		}
		r.say("streaming image over SSH (podman save | ssh podman load)…")
		if err := r.streamImage(); err != nil {
			return err
		}
	}

	// The retag runs on every NON-rolling path (streamed or already-on-VM):
	// the restart re-runs podman run on :latest, which must be the target
	// version regardless of how the image got there. The ROLLING path
	// retags LATER (inside the switch: only after the proxy moved and the
	// old container stopped) — retagging :latest early would leave the old
	// container serving an image that any crash-respawn would re-run as the
	// NEW version, on a deploy that never converged.
	if !r.opts.rolling {
		if out, err := r.vm(fmt.Sprintf("podman tag %s:%s %s:latest", pj.ImagePrefix, r.ver, pj.ImagePrefix)); err != nil {
			return fmt.Errorf("stream retag failed (%s): %w", strings.TrimSpace(out), err)
		}
	}

	// --- sidecars (kampodra.json images.sidecars; deploy mode ONLY) ------
	// Rollback and --sha never touch sidecars: rollback is instant BECAUSE
	// it only re-points the primary; sidecars are :latest-rolling by
	// contract (an old sidecar is never needed).
	if r.mode == "deploy" {
		return r.streamSidecars()
	}
	return nil
}

// inPlaceSwitch is the non-rolling tail of the pipeline: restart (the
// init system re-runs podman run on the retagged :latest) → VM-side
// health gate → kamal-proxy re-point → public smoke.
func (r *deployRun) inPlaceSwitch() error {
	pj := r.target.Project
	restartCmd, err := initadapter.ActionCommand(r.initSys, pj.Container, "restart")
	if err != nil {
		return err
	}
	r.say("%s (supervise-daemon on Alpine re-runs podman run on the retagged :latest)…", restartCmd)
	if _, err := r.vm(restartCmd); err != nil {
		return fmt.Errorf("%s failed: %w", restartCmd, err)
	}
	if err := r.healthGate(pj.Port, r.ver); err != nil {
		return err
	}
	if err := r.proxyRepoint(pj.Container + ":" + pj.Port); err != nil {
		return err
	}
	return r.publicSmoke()
}

// skipStream marks the rollback fast path (tag already on the VM); the
// field lives on deployRun.

func (r *deployRun) vmTolerantErr(remote string) error {
	_, err := r.vm(remote)
	return err
}

// streamImage pipes a local `podman save` into the ssh `podman load` —
// no registry, no tunnel; the image crosses on the ssh the deploy already
// uses.
func (r *deployRun) streamImage() error {
	return r.streamImageRef(r.target.Project.ImagePrefix + ":" + r.ver)
}

// streamImageRef is streamImage for any local image ref (the primary's
// sha tag, a sidecar's :latest) — one save|load pipe implementation.
func (r *deployRun) streamImageRef(image string) error {
	save := exec.CommandContext(r.ctx, "podman", "save", "--format", "docker-archive", image)
	stdout, err := save.StdoutPipe()
	if err != nil {
		return fmt.Errorf("podman save failed: %w", err)
	}
	save.Stderr = os.Stderr
	if err := save.Start(); err != nil {
		return fmt.Errorf("podman save failed: %w", err)
	}
	if _, err := r.d.Runner.RunWithStdin(r.ctx, r.target.HostSpec, "podman load", stdout); err != nil {
		_ = save.Wait()
		return fmt.Errorf("image stream failed (ssh podman load): %w", err)
	}
	if err := save.Wait(); err != nil {
		return fmt.Errorf("podman save failed: %w", err)
	}
	return nil
}

// streamSidecars builds+streams+verifies every sidecar declared in the
// repo manifest (kampodra.json images.sidecars): same platform and
// GIT_SHA identity as the primary, tagged <ImagePrefix>-<name>:latest
// (sidecars are not sha-versioned — rollback never needs an old sidecar),
// streamed over the same ssh pipe, then fail-closed verified on the VM.
func (r *deployRun) streamSidecars() error {
	for _, sc := range r.opts.sidecars {
		ref := r.target.Project.SidecarImageRef(sc.Name) + ":latest"
		r.say("sidecar %s: building (%s, -f %s)…", sc.Name, ref, sc.Dockerfile)
		// CONTEXT = the sidecar dockerfile's own directory (the RUNBOOK
		// convention — e.g. `podman build -f apps/api-go/Dockerfile.backup
		// apps/api-go`). Repo-root context failed on the first real sidecar
		// Dockerfile (2026-10-09 live fire: COPY go.mod go.sum); the primary
		// keeps the repo-root context its Dockerfile is written for.
		sidecarCtx := filepath.Join(r.repoRoot, filepath.Dir(sc.Dockerfile))
		buildArgs := []string{"build", "--platform", "linux/arm64",
			"-f", sc.Dockerfile,
			"--build-arg", "GIT_SHA=" + r.ver,
			"-t", ref,
			sidecarCtx}
		if err := localPassthrough(r.ctx, "podman", buildArgs...); err != nil {
			return fmt.Errorf("sidecar %s build failed: %w", sc.Name, err)
		}
		r.say("sidecar %s: streaming (podman save | ssh podman load)…", sc.Name)
		if err := r.streamImageRef(ref); err != nil {
			return fmt.Errorf("sidecar %s: %w", sc.Name, err)
		}
		if out, err := r.vm("podman image exists " + ref); err != nil {
			return fmt.Errorf("sidecar %s missing on the VM after load (%s): %w", sc.Name, strings.TrimSpace(out), err)
		}
	}
	return nil
}

// pushEnvFile pushes the operator's env file through the env family's
// exact remote flow (umask 077 temp + chmod 600 + atomic mv), appending
// the deploy's API_GIT_SHA stamp. Values NEVER appear on stdout — the
// fingerprint table does.
func (r *deployRun) pushEnvFile() error {
	data, err := os.ReadFile(r.opts.envFile)
	if err != nil {
		return fmt.Errorf("no such env file: %s", r.opts.envFile)
	}
	content := strings.TrimRight(string(data), "\n") + fmt.Sprintf("\nAPI_GIT_SHA=%s\n", r.ver)
	remote := r.target.Project.EnvFile
	r.say("pushing env file -> %s:%s (fingerprint summary below; values are NEVER printed)", r.target.HostSpec.Host, remote)
	fmt.Fprint(r.d.Stdout, envfile.Table(content))

	tmp := envfile.RemoteTmpPath(remote, os.Getpid())
	if _, err := r.d.Runner.RunWithStdin(r.ctx, r.target.HostSpec, "umask 077; cat > "+tmp, strings.NewReader(content)); err != nil {
		return fmt.Errorf("env upload failed")
	}
	if _, err := r.vm(fmt.Sprintf("chmod 600 %s && mv -f %s %s", tmp, tmp, remote)); err != nil {
		return fmt.Errorf("env atomic install failed (remote temp left at: %s)", tmp)
	}
	r.say("env installed %s (0600)", remote)
	return nil
}

// healthGate polls the VM-side fetch of the health endpoint until the body
// serves the deployed sha (the shell's 30 × 3s wget/curl + client-side
// grep — remote grep quoting was fragile, the verdict stays client-side).
func (r *deployRun) healthGate(port, sha string) error {
	pj := r.target.Project
	url := fmt.Sprintf("http://127.0.0.1:%s%s", port, pj.HealthPath)
	fetch := fmt.Sprintf("wget -qO- -T 3 %s 2>/dev/null || curl -s -m 3 %s 2>/dev/null", url, url)
	r.say("health gate (VM-side fetch of %s + served-sha verify against %s)…", pj.HealthPath, sha)
	for attempt := 0; attempt < DeployGateAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(DeployGateSleep)
		}
		if out, err := r.vm(fetch); err == nil && probe.BodyServesSha(out, sha) {
			r.say("✓ health gate passed on attempt %d (served %s)", attempt+1, sha)
			return nil
		} else if err == nil {
			r.say("… health gate attempt %d/%d — endpoint up, serving %q (want %s)",
				attempt+1, DeployGateAttempts, strings.TrimSpace(out), sha)
		} else {
			r.say("… health gate attempt %d/%d — endpoint unreachable", attempt+1, DeployGateAttempts)
		}
	}
	// Diagnostics before dying (best-effort): the container's last words
	// and the service status, then the rollback hint with the stamp.
	r.say("container never became healthy — last logs:")
	fmt.Fprint(r.d.Stdout, r.vmTolerant(fmt.Sprintf("podman logs --tail 30 %s 2>&1", pj.Container)))
	hint := r.vmTolerant(fmt.Sprintf("cat %s 2>/dev/null", pj.DeployedShaFile))
	hint = strings.TrimSpace(hint)
	if hint != "" && shaFragmentRe.MatchString(hint) && hint != sha {
		return fmt.Errorf("container never became healthy — rollback: kampodra deploy --rollback %s", hint)
	}
	return fmt.Errorf("container never became healthy — rollback: kampodra deploy --rollback <sha7> (see: kampodra deploy list)")
}

// proxyRepoint points kamal-proxy at targetRef (container:port or
// shadow:port) — the same podman-exec invocation kamal used. A host without
// a running kamal-proxy skips the re-point (single-host dev shapes).
func (r *deployRun) proxyRepoint(targetRef string) error {
	pj := r.target.Project
	names := r.vmTolerant("podman ps --format '{{.Names}}'")
	if !hasExactLine(names, "kamal-proxy") {
		r.say("kamal-proxy not running — skipping re-point")
		return nil
	}
	r.say("kamal-proxy re-point (podman exec — the same invocation kamal used)…")
	cmd := fmt.Sprintf("podman exec kamal-proxy kamal-proxy deploy %s --host %s --target %s --tls --health-check-path %s",
		pj.Container, r.target.ProxyHost, targetRef, pj.HealthPath)
	if _, err := r.vm(cmd); err != nil {
		return fmt.Errorf("proxy re-point failed (podman logs kamal-proxy): %w", err)
	}
	return nil
}

// proxyRepointForced is the rolling variant: the proxy switch IS the
// mechanism, so a missing kamal-proxy is fatal there.
func (r *deployRun) proxyRepointForced(targetRef string) error {
	pj := r.target.Project
	names := r.vmTolerant("podman ps --format '{{.Names}}'")
	if !hasExactLine(names, "kamal-proxy") {
		return fmt.Errorf("kamal-proxy not running on %s — the rolling deploy's zero-downtime switch needs it (start kamal-proxy, or deploy without --rolling)", r.target.HostSpec.Host)
	}
	r.say("kamal-proxy re-point (podman exec)…")
	cmd := fmt.Sprintf("podman exec kamal-proxy kamal-proxy deploy %s --host %s --target %s --tls --health-check-path %s",
		pj.Container, r.target.ProxyHost, targetRef, pj.HealthPath)
	if _, err := r.vm(cmd); err != nil {
		return fmt.Errorf("proxy re-point failed (podman logs kamal-proxy): %w", err)
	}
	return nil
}

// publicSmoke verifies the PUBLIC edge serves the deployed sha — the
// deploy is not done when the world cannot see it.
func (r *deployRun) publicSmoke() error {
	if r.opts.skipSmoke {
		r.say("--skip-smoke; run the public smoke once the proxy serves this host")
		return nil
	}
	host := r.target.ProxyHost
	r.say("smoke (public, through the proxy)…")
	body, ok := r.d.Prober.LiveStatus(r.ctx, host, r.target.Project.HealthPath)
	if !ok || !probe.BodyServesSha(body, r.ver) {
		return fmt.Errorf("served git sha mismatch: %s (proxy still pointing at the old version?)", strings.TrimSpace(body))
	}
	if !r.d.Prober.Up(r.ctx, host) {
		return fmt.Errorf("/up failed on https://%s/up", host)
	}
	id, ok := r.d.Prober.BuildID(r.ctx, host)
	if !ok || !strings.Contains(id, r.ver) {
		// The SPA dist's build-id stamp is a freshness SIGNAL, not identity:
		// images built before the sha-stamped web pipeline serve "dev".
		// Identity is the served-sha gate above; warn loudly, never fail.
		r.say("WARN: /build-id.txt stale (got %q, want %s) — the web dist predates sha stamping", id, r.ver)
	} else {
		r.say("LIVE: git=%s, /up ok, build-id fresh", r.ver)
	}
	return nil
}

// --- the deploy --binary fast path -----------------------------------------
//
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
// for go/rust; the source tree as-is for node (its runtime compiles
// nothing — tsx serves the source).
func (r *deployRun) buildArtifact() error {
	bin := r.bin
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
		return r.pushArtifactDir()
	}
	f, err := os.Open(r.localArtifact)
	if err != nil {
		return fmt.Errorf("cannot read the built artifact: %w", err)
	}
	defer f.Close()
	tmp := filepath.Join(bin.Dir, "."+bin.Entry+".new")
	if _, err := r.d.Runner.RunWithStdin(r.ctx, r.target.HostSpec, "umask 077; cat > "+tmp, f); err != nil {
		return fmt.Errorf("artifact upload failed: %w", err)
	}
	return r.binarySwap(tmp, filepath.Join(bin.Dir, bin.Entry), "chmod 755 "+filepath.Join(bin.Dir, bin.Entry))
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
	if r.bin.IsDirShape() {
		r.vmTolerant("rm -rf " + r.bin.Dir + ".staging")
		return
	}
	r.vmTolerant("rm -f " + filepath.Join(r.bin.Dir, "."+r.bin.Entry+".new"))
}

// restorePreviousArtifact swaps <run>.prev back and restarts — the
// safety net behind a failed health gate or smoke.
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

// syncMountFromImage refreshes the mounted artifact FROM the image after
// every image-mode deploy. Without it the unit's exec override would
// serve the last fast-pushed artifact after an image deploy — the
// mounted copy and the image would disagree. Fail-closed: a deploy
// whose identity cannot be mirrored into the mount is not a deploy.
func (r *deployRun) syncMountFromImage() error {
	bin := r.bin
	pj := r.target.Project
	ctr := "kamdeploy-mount-sync"
	defer r.vmTolerant("podman rm -f " + ctr + " >/dev/null 2>&1 || true")
	if _, err := r.vm(fmt.Sprintf("podman create --name %s %s:%s >/dev/null", ctr, pj.ImagePrefix, r.ver)); err != nil {
		return fmt.Errorf("could not stage the image for the mount sync: %w", err)
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

// singleDeclaredBinary returns the repo's lone binary block (image
// deploys mirror it into the mount). Multiple blocks = the operator must
// pin one via vm-prepare --binary; nil = nothing to sync.
func (r *deployRun) singleDeclaredBinary() *project.ManifestBinary {
	switch len(r.target.Binary) {
	case 0:
		return nil
	case 1:
		for _, b := range r.target.Binary {
			return &b
		}
	}
	r.say("WARNING: %d \"binary\" blocks declared — the mount is pinned by vm-prepare --binary; skipping the image→mount sync", len(r.target.Binary))
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
// unit's exec override must serve the deployed version), tolerant
// cleanup (dangling + sha tags beyond the keep-set), the deployed-sha
// stamp (rollback never rewrites it), the post-deploy disk report, the
// ledger append and the verdict.
func (r *deployRun) epilogue() error {
	pj := r.target.Project
	// Image deploys mirror the artifact into the mount (the app unit's
	// command override execs the mounted copy on every respawn).
	if r.opts.binary == "" {
		if bin := r.singleDeclaredBinary(); bin != nil {
			r.bin = bin
			synced := r.step("sync mounted artifact from the image (" + bin.Dir + ")")
			err := r.syncMountFromImage()
			synced(err)
			if err != nil {
				return err
			}
		}
	}
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
	r.say("verified served git sha via %s + public smoke", r.target.Project.HealthPath)
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
