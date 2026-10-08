package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/talha7k/kampodra/internal/adapter/envfile"
	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/osfacts"
	"github.com/talha7k/kampodra/internal/adapter/probe"
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
	dockerfile   string
	envFile      string
	requireDisk  string
	skipSmoke    bool
	refreshCfg   bool
	rolling      bool
	drainTimeout int
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
}

func (r *deployRun) say(format string, args ...any) {
	fmt.Fprintf(r.d.Stdout, "[deploy] "+format+"\n", args...)
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
	target := r.target
	pj := target.Project

	// --- preflight: repo identity + clean-tree gate (deploy mode only) ----
	if r.mode == "deploy" {
		root, err := localOutput(r.ctx, "", "git", "rev-parse", "--show-toplevel")
		if err != nil {
			return fmt.Errorf("not a git repository — deploy stamps the git sha of HEAD; from a non-repo directory use --version <sha7> to stream an existing build")
		}
		r.repoRoot = strings.TrimSpace(root)
		dirty, err := localOutput(r.ctx, r.repoRoot, "git", "status", "--porcelain")
		if err != nil {
			return fmt.Errorf("git status failed — cannot verify the tree is clean: %w", err)
		}
		if strings.TrimSpace(dirty) != "" {
			return fmt.Errorf("dirty tree — deploys must ship COMMITTED files (build identity stamps the git sha); commit first, or stream an existing build with --version <sha7>")
		}
	}

	// --- preflight: podman (deploy/version always build or stream) --------
	if r.mode != "rollback" {
		if _, err := localOutput(r.ctx, "", "podman", "info"); err != nil {
			return fmt.Errorf("podman machine not reachable (podman machine start): %w", err)
		}
	}

	// --- disk gate (pre-deploy, may fail) ---------------------------------
	if err := r.vmDiskCheck("gate"); err != nil {
		return err
	}

	// from here on, every outcome is recorded (the EXIT-trap contract)
	r.recording = true

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
	// local build is long gone; an explicit --version deploy of a sha that
	// already lives on the VM is the same case — verified live 2026-10-08
	// when re-deploying an existing sha tried to stream from a machine
	// that never had it) ------------------------------------------
	if (r.mode == "rollback" || r.mode == "version") && !r.skipStream {
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

	// --- env file (the env family: 0600 temp + atomic mv, fingerprints
	// only on stdout; the deploy stamps API_GIT_SHA into the payload) -----
	if r.opts.envFile != "" {
		if err := r.pushEnvFile(); err != nil {
			return err
		}
	} else {
		r.say("no --env-file — leaving %s unchanged", pj.EnvFilePath)
	}
	if r.opts.refreshCfg {
		r.say("WARNING: --refresh-config was the retired shell's repo-local ansible hook — kampodra keeps the flag for script compatibility; rc-script drift repair is manual (kampodra deploy shell exec -- <cmd>)")
	}

	// --- init detection (one round-trip; cached in the profile) ----------
	initSys, _, _ := initadapter.Detect(r.ctx, func(_ context.Context, remote string) (string, error) {
		return r.vm(remote)
	}, target.ProfileInit)
	r.initSys = initSys

	if r.opts.rolling {
		if err := r.executeRolling(); err != nil {
			return err
		}
		return r.epilogue()
	}

	// --- restart (the init system re-runs podman run on the retagged :latest)
	restartCmd, err := initadapter.ActionCommand(initSys, pj.Container, "restart")
	if err != nil {
		return err
	}
	r.say("%s (supervise-daemon on Alpine re-runs podman run on the retagged :latest)…", restartCmd)
	if _, err := r.vm(restartCmd); err != nil {
		return fmt.Errorf("%s failed: %w", restartCmd, err)
	}

	// --- VM-side health gate (served-sha verify against the deployed tag)
	if err := r.healthGate(pj.Port, r.ver); err != nil {
		return err
	}

	// --- kamal-proxy re-point --------------------------------------------
	if err := r.proxyRepoint(pj.Container + ":" + pj.Port); err != nil {
		return err
	}

	// --- public smoke ------------------------------------------------------
	if err := r.publicSmoke(); err != nil {
		return err
	}
	return r.epilogue()
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
	image := r.target.Project.ImagePrefix + ":" + r.ver
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
	remote := r.target.Project.EnvFilePath
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
			return nil
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
		return fmt.Errorf("build-id.txt stale (got %q, want %s)", id, r.ver)
	}
	r.say("LIVE: git=%s, /up ok, build-id fresh", r.ver)
	return nil
}

// epilogue is the shared tail: tolerant cleanup (dangling + sha tags beyond
// the keep-set), the deployed-sha stamp (rollback never rewrites it), the
// post-deploy disk report, the ledger append and the footer.
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
	r.say("done (version: %s). instant rollback: kampodra deploy --rollback", r.ver)
	return nil
}

// vmDiskCheck ports vm_disk_check: read-only VM df; the --require-disk
// fail-closed gate (phase=gate); the always-on loud warning above 90%
// (report phase is warn-only by definition).
func (r *deployRun) vmDiskCheck(phase string) error {
	out := r.vmTolerant(fmt.Sprintf("df -P %s 2>/dev/null", deployDiskPath))
	pct, ok := osfacts.DiskUsedPct(out)
	verdict := osfacts.DiskVerdict(pctString(pct, ok), r.opts.requireDisk)
	switch verdict {
	case osfacts.VerdictFail:
		return fmt.Errorf("VM disk at %d%% (>= --require-disk %s%%) — free space first: kampodra deploy prune --dry-run", pct, r.opts.requireDisk)
	case osfacts.VerdictWarn:
		r.say("WARNING: VM disk %s at %d%% — old sha-tagged deploy images pile up (~1GB each)", deployDiskPath, pct)
		r.say("WARNING: reclaim space: kampodra deploy prune --dry-run (this deploy continues)")
	case osfacts.VerdictUnknown:
		if phase == "gate" && r.opts.requireDisk != "" {
			return fmt.Errorf("cannot read VM disk usage (df %s) — --require-disk is fail-closed", deployDiskPath)
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
