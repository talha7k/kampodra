// bluegreen deploy — the standby-update composite: the one command behind
// "blue-green always up". The binary update runs on the STANDBY color (the
// pair's non-serving instance) on its own public ip, and only after the
// deploy pipeline's served-sha health gate passed there does the existing
// ACME-first flip move the reserved IP. Any pipeline failure returns
// before the flip: the pair keeps serving the holder untouched — the
// safety property that makes this composable.
//
// Reuse seam: runDeployPipeline takes a fully resolved Target, so the
// composite re-points ONLY HostSpec.Host at the standby's public ip
// (root@ — the same convention the pair's profile uses) and keeps the
// profile's project/cloud/sshKey wholesale. deployOpts mirrors
// assembleDeployRun minus deploy's own flags, with three v1 decisions:
//   - rolling is FORCED OFF — the flip is the cutover; a rolling shadow
//     double re-point on a standby buys nothing (state the boundary in the
//     parent help).
//   - the pipeline's public smoke is skipped — it probes ProxyHost, which
//     pre-flip is the OTHER color's edge; the flip's own ACME +
//     served-sha verify through the reserved IP is the post-cutover smoke
//     (with auto-rollback on mismatch).
//   - a declared kampodra.json "binary" block selects the fast-push (the
//     pipeline's native path; multiple blocks fail closed naming them).
//
// v1 boundaries: image deploys + binary fast-push compose; rolling deploys
// do NOT; no --env-file/--sha/--rollback surface (the standby keeps the
// env its provision/image deploys installed); the gcp guest anchor-watcher
// contract is unchanged (vmbootstrap follow-up, out of scope).

package command

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/project"
)

// runBluegreenDeploy executes the composite: resolve the pair → gate the
// standby → deploy onto it → health-gate it → flip. `bluegreen deploy --to
// <color> [--force-flip]`.
func runBluegreenDeploy(d Deps, c *cobra.Command) error {
	ctx := c.Context()
	to := flagString(c, "to")
	forceFlip := flagBool(c, "force-flip")
	if !bgColorRe.MatchString(to) {
		fmt.Fprintln(d.Stdout, "usage: kampodra bluegreen deploy --to <blue|green> [--force-flip]")
		fmt.Fprintln(d.Stdout, "(v1: image + binary fast-push deploys; the flip is the cutover — rolling is not composed)")
		return &exitError{code: 2}
	}
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "", surfaceBluegreen)
	if err != nil {
		return err
	}
	trow, err := bgInstance(ctx, pair, to)
	if err != nil {
		return err
	}
	if trow.OCID == "" {
		return fmt.Errorf("%s is not RUNNING — nothing to deploy to (not provisioned, or stopped — see: kampodra bluegreen status)", pair.colorName(to))
	}
	if trow.PublicIP == "" {
		return fmt.Errorf("%s is RUNNING but has no public ip yet — nothing to deploy to (check the VNIC in 'kampodra bluegreen status')", pair.colorName(to))
	}

	fmt.Fprintf(d.Stdout, "bg-deploy[1/3]: building + pushing to %s (%s)…\n", pair.colorName(to), trow.PublicIP)
	// The deploy Target rides the resolved profile (project/cloud/sshKey)
	// with ONLY the host re-pointed at the standby's own public ip.
	deployTarget := target
	deployTarget.HostSpec.Host = "root@" + trow.PublicIP
	// deployOpts: the assembleDeployRun ladder minus deploy's own flags
	// (see the file comment for the rolling/smoke/binary decisions).
	opts := deployOpts{
		dockerfile: target.Project.Dockerfile,
		skipSmoke:  true,
		sidecars:   []project.ManifestSidecar{},
	}
	if mf, err := d.manifestFor(); err != nil {
		return err
	} else if mf != nil && mf.Fields.Images != nil {
		opts.sidecars = mf.Fields.Images.Sidecars
	}
	if len(target.Binary) > 0 {
		name, _, err := ChooseBinaryBlockNamed(target.Binary, "")
		if err != nil {
			return err
		}
		opts.binary = name
	}
	// Deploy mode builds HEAD — the same clean-tree-gated identity the
	// root deploy stamps (the sha is required, never inferred).
	out, err := localOutput(ctx, "", "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("cannot resolve HEAD (not a git repository?) — bluegreen deploy stamps the git sha of HEAD")
	}
	ver := strings.TrimSpace(out)
	// Any pipeline failure = no flip: the pair keeps serving the holder.
	if err := runDeployPipeline(d, ctx, deployTarget, ver, "deploy", opts); err != nil {
		return err
	}

	fmt.Fprintln(d.Stdout, "bg-deploy[2/3]: health gate on the standby…")
	// The gate is kept even though the pipeline just passed its own: one
	// cheap service-up + loopback probe as the flip's explicit precondition.
	if !instanceHealthy(ctx, d, trow.PublicIP, pair.sshKey, pair.container, pair.port, pair.healthPath) {
		if !forceFlip {
			return fmt.Errorf("%s UNHEALTHY after the deploy — refusing flip (the pair keeps serving the holder; override: --force-flip)", pair.colorName(to))
		}
		fmt.Fprintf(d.Stdout, "bg-deploy[2/3]: standby UNHEALTHY — flipping anyway (--force-flip)\n")
	} else {
		fmt.Fprintf(d.Stdout, "bg-deploy[2/3]: %s HEALTHY on its own ip\n", pair.colorName(to))
	}

	fmt.Fprintf(d.Stdout, "bg-deploy[3/3]: flip → %s (health-gated)…\n", to)
	// --force-flip forwards to the flip's --force (the flip re-gates
	// internally and keeps its own override semantics).
	if forceFlip {
		if err := c.Flags().Set("force", "true"); err != nil {
			return err
		}
	}
	return runBluegreenFlip(d, c)
}
