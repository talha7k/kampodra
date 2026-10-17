// vm.go — kampodra's host-lifecycle command group beyond the project stack.
// `vm gc` exists because agents kept hand-rolling find|rm garbage sweeps
// over raw ssh — the surface belongs HERE: a typed, allowlisted inventory
// (dry run by default) whose --apply deletes ONLY the stale-artifact
// classes. The allowlists live in internal/adapter/vmgc and are enforced
// twice (classification + per-command validation); podman is report-only.
package command

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/vmgc"
)

const vmGcHelp = `Usage:
  kampodra vm gc [--apply] [--host root@<ip>] [--profile <name>] [--ssh-key <path>]

Dry-run-by-default VM garbage sweep. The inventory phase classifies every
host artifact (scratch / stale-script / residue / kampodra-helper /
os-session / os / infra / tenant-data / live / owner-decision / unknown)
into a typed table; --apply deletes ONLY the stale classes:

  scratch        /tmp,/var/tmp junk (stray binaries, copies, scratch dirs)
  stale-script   .sh files outside OS/infra paths
  residue        legacy build residues (e.g. the empty pre-binary releases dir)

NEVER touched (hard-coded allowlists): OS paths and podman storage, the
live artifact/web/backup chains, kampodra's own version-stamped helpers
(/tmp/kampodra-*), OS session dirs (/tmp/.X11-*), live build contexts
(scratch dirs holding a Dockerfile), and owner-decision items (cutover db
snapshots beside the project data dir, /root pre-op safety snapshots).
Tenant data routes from the project's dataDir — zero knowledge of it
fails closed to report-only. Podman containers/images are inventoried
and cross-checked against the estate naming — strictly report-only.
Candidates in use by a live process or a container bind mount are
BLOCKED from deletion.

Host/key resolution is the shared ladder: --host | --profile <name> |
KAMPODRA_PROFILE | config defaultProfile | KAMPODRA_HOST; --ssh-key |
profile sshKey | KAMPODRA_SSH_KEY | agent / ssh config.

Examples:
  kampodra vm gc --profile prod              # typed inventory, nothing deleted
  kampodra vm gc --profile prod --apply      # delete ONLY the stale classes
`

func newVMCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm",
		Short: "host lifecycle beyond the project stack: gc (dry-run-by-default garbage sweep; --apply deletes ONLY stale classes)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), vmGcHelp)
			return nil
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), vmGcHelp)
	})
	cmd.AddCommand(newVMGCSub(d))
	return cmd
}

func newVMGCSub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gc [--apply]",
		Short: "dry-run-by-default VM garbage sweep: typed inventory; --apply deletes ONLY stale scratch/scripts/residues (never-touch allowlists)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runVMGC(d, c)
		},
	}
	remoteTargetFlags(cmd)
	cmd.Flags().Bool("apply", false, "delete the stale classes found by the sweep (default: dry run)")
	return cmd
}

func runVMGC(d Deps, c *cobra.Command) error {
	apply := flagBool(c, "apply")
	target, err := resolveTargetFromFlags(d, c)
	if err != nil {
		return err
	}
	sayProvenance(Deps{Stdout: d.Stderr}, "vm-gc", target)
	if err := requireHost(target); err != nil {
		return err
	}

	ctx := c.Context()
	pj := target.Project
	pr := vmgc.Protect{
		Container:    pj.Container,
		ShadowSuffix: pj.ShadowSuffix,
		Services:     pj.Services,
		DataDir:      pj.DataDir,
	}
	run := func(remote string) (string, error) {
		return d.Runner.Run(ctx, target.HostSpec, remote)
	}
	runTolerant := func(remote string) string {
		out, err := run(remote)
		if err != nil {
			fmt.Fprintf(d.Stderr, "[vm-gc] (tolerated probe failure: %v)\n", err)
			return ""
		}
		return out
	}

	sweepOut, err := run(vmgc.SweepCommand(pr))
	if err != nil {
		return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to sweep", target.HostSpec.Host)
	}
	inUse := splitLines(runTolerant(vmgc.InUseProbeCommand()))
	buildDirs := vmgc.DirOfBuildContext(splitLines(runTolerant(vmgc.BuildContextProbeCommand())))
	containers, images := vmgc.ClassifyEstate(pr,
		vmgc.ParsePodmanPS(runTolerant(vmgc.PodmanPSCommand())),
		vmgc.ParsePodmanImages(runTolerant(vmgc.PodmanImagesCommand())))

	plan := vmgc.BuildPlan(vmgc.ParseSweepListing(sweepOut), inUse, buildDirs, pr)

	// --- the typed inventory table ------------------------------------------
	fmt.Fprintf(d.Stdout, "[vm-gc] inventory of %s (zones: /tmp /var/tmp /opt /home /data /root)\n", target.HostSpec.Host)
	fmt.Fprintf(d.Stdout, "%-16s %8s  %-10s  %s\n", "CLASS", "SIZE", "MODIFIED", "PATH")
	for _, row := range plan.Rows {
		fmt.Fprintf(d.Stdout, "%-16s %8s  %-10s  %s\n",
			string(row.Class), vmgc.HumanSize(row.Entry.Size),
			time.Unix(row.Entry.MTime, 0).UTC().Format("2006-01-02"), row.Entry.Path)
	}

	// --- podman: report-only, cross-checked against the estate naming ------
	if len(containers) > 0 || len(images) > 0 {
		fmt.Fprintf(d.Stdout, "[vm-gc] podman (report-only — never mutated by vm gc)\n")
		for _, ct := range containers {
			fmt.Fprintf(d.Stdout, "  container  %-16s %-14s %s (%s)\n",
				string(ct.Class), ct.Status, ct.Name, ct.Image)
		}
		for _, im := range images {
			fmt.Fprintf(d.Stdout, "  image      %-16s %-14s %s\n",
				string(im.Class), im.Size, im.Ref)
		}
	}

	// --- summary -------------------------------------------------------------
	owner, unknown := 0, 0
	for _, row := range plan.Rows {
		switch row.Class {
		case vmgc.ClassOwnerDecision:
			owner++
		case vmgc.ClassUnknown:
			unknown++
		}
	}
	fmt.Fprintf(d.Stdout, "[vm-gc] reclaimable: %s across %d path(s); blocked (in use): %d; owner-decision: %d; unknown: %d\n",
		vmgc.HumanSize(plan.Freed), len(plan.Removes), len(plan.Blocked), owner, unknown)
	for _, b := range plan.Blocked {
		fmt.Fprintf(d.Stdout, "[vm-gc] blocked (in use): %s\n", b.Entry.Path)
	}
	if !apply {
		for _, r := range plan.Removes {
			fmt.Fprintf(d.Stdout, "[vm-gc] would remove: %s\n", r.Cmd)
		}
		fmt.Fprintf(d.Stdout, "[vm-gc] DRY RUN — nothing deleted; pass --apply to delete ONLY the stale rows above\n")
		return nil
	}

	// --- apply: exact commands from the plan (each one already validated) ---
	var failed []string
	var freed int64
	sort.Slice(plan.Removes, func(i, j int) bool {
		return plan.Removes[i].Entry.Path < plan.Removes[j].Entry.Path
	})
	for _, r := range plan.Removes {
		if _, err := run(r.Cmd); err != nil {
			failed = append(failed, r.Entry.Path)
			fmt.Fprintf(d.Stdout, "[vm-gc] (failed: %s — %v)\n", r.Entry.Path, err)
			continue
		}
		freed += r.Reclaims
		fmt.Fprintf(d.Stdout, "[vm-gc] removed: %s (reclaims %s)\n", r.Entry.Path, vmgc.HumanSize(r.Reclaims))
	}
	fmt.Fprintf(d.Stdout, "[vm-gc] DONE — freed %s across %d path(s) on %s\n",
		vmgc.HumanSize(freed), len(plan.Removes)-len(failed), target.HostSpec.Host)
	if len(failed) > 0 {
		return fmt.Errorf("%d deletion(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}
