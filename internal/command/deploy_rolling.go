package command

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/vmbootstrap"
)

// --rolling: the zero-downtime shadow-container double re-point (opt-in).
//
// Instead of retag-then-restart (a brief outage window), the new version
// boots ALONGSIDE the old one as <container>-shadow — created from the SHA
// tag (never :latest — a retag race would promote whatever :latest pointed
// at when the shadow started), on the kamal network, with a loopback-only
// probe port so the health gate can reach it before any traffic can. The
// switch happens twice through kamal-proxy (the two-proxy-switch pattern):
//
//	1. shadow up (sha tag, loopback probe port)     — main still serving
//	2. health-gate the shadow                        — main still serving
//	3. kamal-proxy deploy → shadow target            — shadow takes traffic
//	4. drain wait (bounded by --drain-timeout)       — in-flight finishes
//	5. init-system stop of the old container         — RESPAWN DISCIPLINE:
//	                                                   the supervisor stops
//	                                                   (never `podman stop`,
//	                                                   which would fight
//	                                                   the respawn)
//	6. retag :latest; 7. init start (new main); 8. health-gate main
//	9. kamal-proxy deploy → main target; 10. rm the shadow (LAST)
//
// Failure keeps the safer state, always:
//   - a bad shadow never took traffic: rm the shadow, die (main untouched);
//   - a switch already made: the shadow is SERVING — leave it serving and
//     exit LOUD with `kampodra deploy converge` (rm-ing a serving shadow
//     would manufacture an outage to tidy up a mess).
//
// `deploy converge` idempotently completes steps 6-10 after an interrupted
// rolling deploy.

// deployRollingDrainTimeout is the default drain budget (seconds): how long
// the switch waits for kamal-proxy to confirm the shadow as the target
// before continuing anyway (the init stop terminates any stragglers).
const deployRollingDrainTimeout = 10

var convergeShaRe = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

// shadowName is the rolling shadow container's name.
func (r *deployRun) shadowName() string {
	return r.target.Project.Container + r.target.Project.ShadowSuffix
}

// leftoverShadow detects an interrupted rolling deploy: the shadow
// container existing in ANY state means the last rolling deploy never
// converged — building on top of it would stack two shadow schemes.
func (r *deployRun) leftoverShadow() bool {
	names := r.vmTolerant("podman ps -a --format '{{.Names}}'")
	return hasExactLine(names, r.shadowName())
}

// executeRolling is the rolling branch of execute(): the shared prelude
// (build/stream/env/detect) has already run.
func (r *deployRun) executeRolling() error {
	pj := r.target.Project
	shadow := r.shadowName()

	// --- preflight convergence: never stack two interrupted deploys ------
	if r.leftoverShadow() {
		return fmt.Errorf("leftover shadow container %s from an interrupted deploy — converge first: kampodra deploy converge", shadow)
	}

	// --- 1. shadow up: SHA tag (never :latest), kamal network, loopback
	// probe port. The run replicates the init unit's CLEAR env
	// (vmbootstrap.ClearEnvArgs) — the env file deliberately lacks
	// envClearKeys keys, so a shadow without them boots the app's compiled
	// default port and the probe targets a dead endpoint (2026-10-09 live
	// fire: safe-abort at the shadow health gate).
	r.say("[rolling] shadow %s from %s:%s (network %s, loopback probe :%s)…",
		shadow, pj.ImagePrefix, r.ver, pj.Network, pj.ShadowProbePort)
	runCmd := fmt.Sprintf("podman run -d --name %s --network %s %s --env-file %s -p 127.0.0.1:%s:%s %s:%s",
		shadow, pj.Network, vmbootstrap.ClearEnvArgs(pj.Port), pj.EnvFile, pj.ShadowProbePort, pj.Port, pj.ImagePrefix, r.ver)
	if _, err := r.vm(runCmd); err != nil {
		return fmt.Errorf("shadow container failed to start: %w", err)
	}

	// --- 2. gate the shadow (before it can possibly take traffic) ---------
	if err := r.healthGate(pj.ShadowProbePort, r.ver); err != nil {
		r.vmTolerant("podman rm -f " + shadow)
		return fmt.Errorf("shadow container never became healthy — nothing took traffic; the main service is untouched (removed %s): %w", shadow, err)
	}

	// --- 3. FIRST switch: the proxy moves to the shadow -------------------
	if err := r.proxyRepointForced(shadow + ":" + pj.Port); err != nil {
		// The switch failed BEFORE landing: no traffic moved — the shadow is
		// healthy but idle; removing it restores the pre-deploy world.
		r.vmTolerant("podman rm -f " + shadow)
		return err
	}

	// --- 4. drain: bounded wait for the switch to be confirmed ------------
	if err := r.drainWait(); err != nil {
		// Drain timeout is a CONTINUE branch, not a failure: the switch
		// already landed; the init stop terminates any in-flight stragglers.
		r.say("drain wait timed out after %ds — continuing (the init stop terminates any in-flight stragglers)", r.opts.drainTimeout)
	}

	// --- 5-10. stop old (init), retag, start new, gate, switch back, rm ---
	return r.convergeSteps(shadow, r.ver, true)
}

// drainWait polls kamal-proxy until the shadow is the confirmed target, or
// the drain budget elapses (nil on confirm, nil on timeout — the caller
// continues either way; only the narration differs).
func (r *deployRun) drainWait() error {
	deadline := time.Now().Add(time.Duration(r.opts.drainTimeout) * time.Second)
	for {
		out := r.vmTolerant("podman exec kamal-proxy kamal-proxy list")
		if strings.Contains(out, r.shadowName()) {
			r.say("[rolling] proxy switched to the shadow — drained; stopping the old container")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("drain wait timed out")
		}
		time.Sleep(DeployDrainPollInterval)
	}
}

// convergeSteps runs steps 5-10 (or 6-10 when stopOld=false): the
// post-switch half of the rolling deploy. runningSwitch marks that traffic
// ALREADY moved to the shadow — every failure from here leaves the shadow
// serving and exits LOUD with the converge command.
func (r *deployRun) convergeSteps(shadow, sha string, stopOld bool) error {
	pj := r.target.Project
	loud := func(err error) error {
		return fmt.Errorf("%w — left the shadow serving (the safer state, left in place): finish with kampodra deploy converge", err)
	}

	if stopOld {
		stopCmd, err := initadapter.ActionCommand(r.initSys, pj.Container, "stop")
		if err != nil {
			return loud(err)
		}
		r.say("[rolling] %s (init-system stop — respawn discipline: never podman stop a supervised container)", stopCmd)
		if _, err := r.vm(stopCmd); err != nil {
			return loud(fmt.Errorf("%s failed: %w", stopCmd, err))
		}
	}

	// retag :latest — from the SHA tag, then bring the service up on it.
	// Rolling: the old container was init-stopped above, so `start` re-runs
	// podman run on the retagged :latest. Converge: the old container may be
	// stopped OR still running (an interrupted stop), so `restart` covers
	// both — the supervisor's restart always lands on the new :latest.
	if _, err := r.vm(fmt.Sprintf("podman tag %s:%s %s:latest", pj.ImagePrefix, sha, pj.ImagePrefix)); err != nil {
		return loud(fmt.Errorf("retag %s:latest failed: %w", pj.ImagePrefix, err))
	}
	upVerb := "start"
	if !stopOld {
		upVerb = "restart"
	}
	upCmd, err := initadapter.ActionCommand(r.initSys, pj.Container, upVerb)
	if err != nil {
		return loud(err)
	}
	if _, err := r.vm(upCmd); err != nil {
		return loud(fmt.Errorf("%s failed: %w", upCmd, err))
	}

	// gate the new main
	if err := r.healthGate(pj.Port, sha); err != nil {
		return loud(err)
	}

	// SECOND switch: back to the main container
	if err := r.proxyRepointForced(pj.Container + ":" + pj.Port); err != nil {
		return loud(err)
	}

	// shadow rm LAST — only after the main container is serving again.
	r.say("[rolling] converged — proxy back on %s:%s, removing %s", pj.Container, pj.Port, shadow)
	if _, err := r.vm("podman rm -f " + shadow); err != nil {
		r.say("WARNING: podman rm -f %s failed — remove it manually (the main container is serving)", shadow)
	}
	return nil
}

// runDeployConverge is the `deploy converge [<sha7>]` repair: complete an
// interrupted rolling deploy (steps 6-10). The sha resolves from the
// explicit argument, else the shadow's own image config — never HEAD.
func runDeployConverge(d Deps, ctx context.Context, target Target, shaArg string, drainTimeout int) error {
	r := &deployRun{d: d, ctx: ctx, target: target, ver: shaArg, mode: "converge", started: time.Now(),
		opts: deployOpts{drainTimeout: drainTimeout}}
	pj := target.Project
	shadow := r.shadowName()

	if !r.leftoverShadow() {
		r.say("no leftover shadow — nothing to converge")
		return nil
	}

	sha := strings.TrimSpace(shaArg)
	if sha == "" {
		img := strings.TrimSpace(r.vmTolerant(fmt.Sprintf("podman inspect --format '{{.Config.Image}}' %s", shadow)))
		if i := strings.LastIndex(img, ":"); i >= 0 {
			sha = img[i+1:]
		}
		if !convergeShaRe.MatchString(sha) {
			return fmt.Errorf("cannot determine the shadow's sha (image %q) — pass it: kampodra deploy converge <sha7>", img)
		}
		r.say("converge target: %s (from %s's image)", sha, shadow)
	} else {
		r.say("converge target: %s (explicit)", sha)
	}
	r.ver = sha

	// The old container may or may not still run — converge restarts it on
	// the retagged :latest either way (restart covers stopped and running).
	initSys, _, _ := initadapter.Detect(ctx, func(_ context.Context, remote string) (string, error) {
		return r.vm(remote)
	}, target.ProfileInit)
	r.initSys = initSys

	if err := r.convergeSteps(shadow, sha, false); err != nil {
		// converged-unhealthy: the shadow stays serving — LOUD exit with the
		// repair command.
		return fmt.Errorf("%w — repair path: kampodra deploy list, then kampodra deploy --rollback <previous-sha> (or fix + redeploy)", err)
	}

	// Converge completed the interrupted deploy: the stamp belongs to it.
	if _, err := r.vm(fmt.Sprintf("echo %s > %s", sha, pj.DeployedShaFile)); err != nil {
		r.say("WARNING: could not update %s — a bare --rollback will need the explicit sha", pj.DeployedShaFile)
	}
	if err := r.publicSmoke(); err != nil {
		return err
	}
	r.say("converged: proxy on %s:%s, shadow removed (version: %s)", pj.Container, pj.Port, sha)
	return nil
}
