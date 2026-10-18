// deploy_image.go — the image-mode legs of the deploy pipeline: build →
// stream (podman save | ssh podman load) → retag → restart → VM-side
// health gate (served-sha verify) → kamal-proxy re-point → public smoke.

package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/talha7k/kampodra/internal/adapter/envfile"
	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/probe"
)

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
		if err := r.streamSidecars(); err != nil {
			return err
		}
	}

	// --- mount sync (BEFORE the switch) ----------------------------------
	// Every declared artifact mirrors from the just-streamed image BEFORE
	// the container restarts, so ONE restart picks up both the new image
	// AND the synced mounts. A post-restart directory swap would be
	// invisible: the fresh bind pins the pre-swap inode (2026-10-11).
	if r.opts.binary == "" {
		if err := r.syncAllMountsFromImage(); err != nil {
			return err
		}
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

// healthGate gates EVERY configured health path (kampodra.json
// healthPaths) against the deployed sha, VM-side: each path must serve
// the deployed sha. Unset paths = exactly [healthPath] — byte-identical
// to the single-path history; with several paths, each gets its own gate
// line (path i/n) and a failure NAMES the path.
func (r *deployRun) healthGate(port, sha string) error {
	paths := r.target.Project.HealthPathsEffective()
	for i, hp := range paths {
		if len(paths) > 1 {
			r.say("health gate (path %d/%d: %s)…", i+1, len(paths), hp)
		} else {
			r.say("health gate (VM-side fetch of %s + served-sha verify against %s)…", hp, sha)
		}
		if err := r.healthGateOne(port, sha, hp); err != nil {
			if len(paths) > 1 {
				return fmt.Errorf("health gate failed on %s: %w", hp, err)
			}
			return err
		}
	}
	return nil
}

// healthGateOne is the single-path poll loop (the historical healthGate
// body): VM-side wget/curl fetch until the body serves the deployed sha.
func (r *deployRun) healthGateOne(port, sha, healthPath string) error {
	pj := r.target.Project
	url := fmt.Sprintf("http://127.0.0.1:%s%s", port, healthPath)
	fetch := fmt.Sprintf("wget -qO- -T 3 %s 2>/dev/null || curl -s -m 3 %s 2>/dev/null", url, url)
	for attempt := 0; attempt < DeployGateAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(DeployGateSleep)
		}
		out, err := r.vm(fetch)
		if err == nil && probe.BodyServesSha(out, sha) {
			r.say("✓ health gate passed on attempt %d (served %s)", attempt+1, sha)
			return nil
		} else if err == nil || strings.TrimSpace(out) != "" {
			// output with a nonzero exit is an HTTP error (wget exit 8 on
			// 404/5xx) — the endpoint is UP and answering wrongly, which
			// is a different truth than unreachable (live-smoke finding:
			// a 404-ing app reported "unreachable" for all 30 attempts)
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

// proxyHostListArg renders the re-point's --host value: the comma-separated
// EFFECTIVE host list (kamal-proxy accepts a comma-separated list — the
// production edge serves several hostnames). Re-pointing with the scalar
// alone would silently DROP every other host the proxy serves.
func (r *deployRun) proxyHostListArg() string {
	return strings.Join(r.target.Project.EffectiveProxyHosts(), ",")
}

// proxyDeployCmd is the kamal-proxy re-point invocation (the same podman-exec
// form kamal used) — shared by the in-place and rolling variants so the
// --host list can never drift between them.
func (r *deployRun) proxyDeployCmd(targetRef string) string {
	pj := r.target.Project
	return fmt.Sprintf("podman exec kamal-proxy kamal-proxy deploy %s --host %s --target %s --tls --health-check-path %s",
		pj.Container, r.proxyHostListArg(), targetRef, pj.HealthPath)
}

// proxyRepoint points kamal-proxy at targetRef (container:port or
// shadow:port) — the same podman-exec invocation kamal used. A host without
// a running kamal-proxy skips the re-point (single-host dev shapes).
func (r *deployRun) proxyRepoint(targetRef string) error {
	names := r.vmTolerant("podman ps --format '{{.Names}}'")
	if !hasExactLine(names, "kamal-proxy") {
		r.say("kamal-proxy not running — skipping re-point")
		return nil
	}
	r.say("kamal-proxy re-point (podman exec — the same invocation kamal used)…")
	cmd := r.proxyDeployCmd(targetRef)
	if _, err := r.vm(cmd); err != nil {
		return fmt.Errorf("proxy re-point failed (podman logs kamal-proxy): %w", err)
	}
	return nil
}

// proxyRepointForced is the rolling variant: the proxy switch IS the
// mechanism, so a missing kamal-proxy is fatal there.
func (r *deployRun) proxyRepointForced(targetRef string) error {
	names := r.vmTolerant("podman ps --format '{{.Names}}'")
	if !hasExactLine(names, "kamal-proxy") {
		return fmt.Errorf("kamal-proxy not running on %s — the rolling deploy's zero-downtime switch needs it (start kamal-proxy, or deploy without --rolling)", r.target.HostSpec.Host)
	}
	r.say("kamal-proxy re-point (podman exec)…")
	cmd := r.proxyDeployCmd(targetRef)
	if _, err := r.vm(cmd); err != nil {
		return fmt.Errorf("proxy re-point failed (podman logs kamal-proxy): %w", err)
	}
	return nil
}

// publicSmoke verifies the PUBLIC edge serves the deployed sha — the
// deploy is not done when the world cannot see it. Every configured
// health path (healthPaths; unset = [healthPath]) is smoked through the
// edge, then the /up liveness leg and the build-id freshness signal.
func (r *deployRun) publicSmoke() error {
	if r.opts.skipSmoke {
		r.say("--skip-smoke; run the public smoke once the proxy serves this host")
		return nil
	}
	host := r.target.ProxyHost
	r.say("smoke (public, through the proxy)…")
	for _, hp := range r.target.Project.HealthPathsEffective() {
		if err := r.publicSmokePath(host, hp); err != nil {
			return err
		}
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

// publicSmokePath smokes ONE health path through the public edge: the
// sha-identity checks with errors that name the EXACT probed URL and
// WHERE proxyHost came from in the ladder — the smoke exercises the EDGE
// (proxyHost), not the VM the health gate just verified (a stale proxy
// project block SHADOWS kampodra.json — the 2026-10-16 incident).
func (r *deployRun) publicSmokePath(host, healthPath string) error {
	body, ok := r.d.Prober.LiveStatus(r.ctx, host, healthPath)
	src := r.target.ProxyHostSource
	if src == "" {
		src = "unknown"
	}
	if !ok {
		return fmt.Errorf("public smoke: https://%s%s is UNREACHABLE (proxyHost source: %s) — stale proxyHost or DNS not pointing at the serving instance? a bluegreen flip de-registers the pre-flip host: update the profile's project.proxyHost AND kampodra.json post-flip", host, healthPath, src)
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("public smoke: https://%s%s answered with an EMPTY body (proxyHost source: %s) — served no sha stamp at all: wrong host (stale proxyHost after a flip?) or the app's %s route serves nothing", host, healthPath, src, healthPath)
	}
	if !probe.BodyServesSha(body, r.ver) {
		return fmt.Errorf("public smoke: served git sha %q, want %s — probed https://%s%s (proxyHost source: %s); the VM health gate PASSED, so the app is fine: the edge is stale (update the profile's project.proxyHost AND kampodra.json post-flip)", strings.TrimSpace(body), r.ver, host, healthPath, src)
	}
	return nil
}

// --- the deploy --binary fast path -----------------------------------------
//
