// The deploy command family: wiring + help for the root pipeline and its
// lifecycle subcommands. Flag/argument grammar and target assembly live in
// deploy_args.go; the subcommand constructors and their RunE bodies live in
// deploy_subs.go; the pipeline itself in deploy_pipeline.go / deploy_image.go
// / deploy_binary.go; the rolling shadow switch in deploy_rolling.go.

package command

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/state"
)

// Ports of scripts/deploy.sh (surface + the full build/stream pipeline)
// and scripts/deploy-lifecycle.sh + scripts/common.sh (behavior): the
// deployment lifecycle over ssh. The pipeline: clean-tree gate → podman
// build (GIT_SHA) → podman save | ssh podman load → env push → init
// restart → VM-side health gate (served-sha verify) → kamal-proxy re-point
// → public smoke → ledger append → disk report → keep-set cleanup.
// --rollback [<sha>] resolves explicit arg > deployed-sha stamp > die
// (NEVER git HEAD). --rolling is the kampodra-native zero-downtime shadow
// double re-point (see deploy_rolling.go).
const (
	deployDiskPath = "/var/lib/containers"
	deployKeepN    = 2 // prune's --keep default
	deployLogLines = "100"
)

const deployHelp = `Usage:
  kampodra deploy [--host root@<ip>] [--profile <name>] [--sha <sha7>] [--rollback [<sha7>]]
                   [--rolling] [--drain-timeout <s>] [--dockerfile <path>] [--env-file <path>]
                   [--ssh-key <path>] [--skip-smoke] [--disk-threshold <pct>]
  kampodra deploy converge [--host root@<ip>] [--profile <name>] [<sha7>]   # finish an interrupted --rolling
  kampodra deploy list   [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--all-profiles] [--group <g>]
  kampodra deploy prune  [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--keep N] [--dry-run]
  kampodra deploy logs   [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--lines N] [--since <when>] [--follow]
  kampodra deploy restart [--host root@<ip>] [--profile <name>] [--ssh-key <path>]
  kampodra deploy shell  [--host root@<ip>] [--profile <name>] [exec -- <cmd>...]

Lifecycle subcommands are --host-aware with the SAME resolution as deploy:
--host | --profile <name> | KAMPODRA_PROFILE | config defaultProfile |
KAMPODRA_HOST; --ssh-key | profile sshKey | KAMPODRA_SSH_KEY | ssh-agent /
~/.ssh/config.

  (default) the full pipeline: clean-tree gate → podman build
            (--build-arg GIT_SHA=<sha>, sha tag from git rev-parse) →
            podman save | ssh podman load (registry-free stream) →
            env-file push (--env-file, 0600 + atomic mv, API_GIT_SHA
            stamped) → init restart → VM-side health gate (served-sha
            verified against the deployed tag) → kamal-proxy re-point →
            public smoke → ledger append → disk report →
            keep-set image cleanup (the running image, ts-rollback, and
            the newest keep-set tags stay — kampodra deploy prune reclaims
            the rest).
            --disk-threshold <pct> fails closed BEFORE the build when the VM
            disk is at/over pct; >90% always warns loudly.
  --sha <sha7>  stream an EXISTING local build (no rebuild; the
            clean-tree gate is skipped — the build happened when that sha
            was HEAD). The sha is required, never inferred.
  --rollback [<sha7>]  instant image-tag rollback. The target resolves:
            the EXPLICIT sha, else the VM's deployed-sha stamp file
            (written by every successful deploy), else it DIES — it NEVER
            falls back to git HEAD (rolling back must not redeploy the
            very build you are rolling back from). Skips build/stream when
            the tag still exists on the VM (podman image exists); streams
            from this machine when it does not. Never rewrites the stamp.
  converge  finish an interrupted --rolling deploy (retag → restart →
            health gate → re-point to the main container → remove the
            shadow). The sha comes from the argument, else from the
            shadow's own image — never HEAD. A leftover shadow from an
            interrupted deploy must be converged before the next --rolling.

  list     deployment history: the VM's sha-tagged images (running one marked)
           merged with the local ledger (~/.kampodra/deployments.jsonl) with a
           "Total deployments" footer. VM unreachable degrades to a
           ledger-only view. --all-profiles renders one section per profile;
           --group <g> renders one section per profile in that group.
  prune    reclaim VM disk: removes OLD sha-tagged deploy images. ALWAYS kept:
           the currently-running image, ts-rollback, and the newest --keep N
           (default 2). --dry-run prints the exact podman rmi commands.
   logs     tail the running api container's logs (--lines N; --since <when>:
            Go duration like 2h, or RFC3339 timestamp — sees the whole boot
            window, not just the last N lines); --follow streams until
            ctrl-c (clean exit).
  restart  restart the api service (init-aware: rc-service on Alpine, systemctl
           on systemd hosts).
  shell    interactive sh inside the running api container; ` + "`exec -- <cmd>`" + `
           runs a one-shot command instead.

ROLLING (the default) — zero-downtime shadow double re-point:
  the new version boots as <container>-shadow from the SHA tag (never
  :latest) on the kamal network with a loopback-only probe port; the health
  gate passes BEFORE any traffic can reach it; kamal-proxy re-points to the
  shadow (first switch); the old container is init-STOPPED (respawn
  discipline — never podman stop); :latest is retagged; the service starts;
  the new main is health-gated; kamal-proxy re-points back (second switch);
  the shadow is removed LAST. On every failure path the safer state
  survives: a bad shadow never took traffic (removed, main untouched);
  a switch already made leaves the shadow SERVING — exit LOUD and finish
  with "kampodra deploy converge".

  sqlite two-writer overlap: during the switch BOTH containers run against
  the same sqlite database. The overlap is deliberately bounded — the
  shadow health gate happens before the switch (main still the only
  writer of record), the drain budget (--drain-timeout, default 10s)
  lets in-flight requests finish, and the init stop ends the old writer;
  worst case ≈ health+drain+stop budgets, with sqlite's file locking
  (busy timeout) bridging the gap.

Examples:
  kampodra deploy --host root@203.0.113.10
  kampodra deploy --host root@203.0.113.10 --rolling --env-file ./ops/env.production
  kampodra deploy --host root@203.0.113.10 --rollback            # stamp-resolved
  kampodra deploy --host root@203.0.113.10 --rollback ccc3333    # explicit
  kampodra deploy --host root@203.0.113.10 --sha ccc3333 --skip-smoke
  kampodra deploy --host root@203.0.113.10 --disk-threshold 85
  kampodra deploy list --host root@203.0.113.10
  kampodra deploy prune --host root@203.0.113.10 --dry-run
  kampodra deploy logs --host root@203.0.113.10 --lines 200 --follow
  kampodra deploy restart --host root@203.0.113.10
  kampodra deploy shell --host root@203.0.113.10 exec -- df -h /var/lib/containers
`

func newDeployCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy [--host root@<ip>] [--profile <name>] [--sha <sha7>] [--rollback [<sha7>]] [--rolling] [--dockerfile <path>] [--env-file <path>] [--ssh-key <path>] [--skip-smoke] [--disk-threshold <pct>]",
		Short: "stream deploy (podman save | ssh podman load) with sha-verified health gate; --rollback [sha] = instant image-tag rollback; --rolling = zero-downtime shadow switch",
		Args:  cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			return runDeployRoot(d, c, args)
		},
	}
	cmd.PersistentFlags().String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODRA_HOST and any profile")
	cmd.PersistentFlags().String("profile", "", "per-instance profile (~/.kampodra/config.json) — beats KAMPODRA_PROFILE / defaultProfile")
	cmd.PersistentFlags().String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY; empty = agent / ssh config")
	cmd.Flags().String("dockerfile", "", "Containerfile/Dockerfile to build (pipeline; context = the repo root) — default: the project config's dockerfile (kampodra.json/env/profile, else Dockerfile)")
	cmd.Flags().String("sha", "", "stream an existing local build of this sha (required, never inferred)")
	cmd.Flags().String("rollback", "", "instant image-tag rollback: explicit sha, else the VM's deployed-sha stamp, else die (never HEAD)")
	cmd.Flags().Lookup("rollback").NoOptDefVal = "-" // bare --rollback = stamp-resolved
	cmd.Flags().Bool("rolling", true, "zero-downtime: shadow container double re-point — the DEFAULT since the 2026-10-10 promotion (OCI live-fire drill + two clean prod rolling deploys)")
	cmd.Flags().Bool("in-place", false, "restart-in-place deploy (the pre-promotion default) — beats --rolling")
	cmd.Flags().String("group", "", "deploy to EVERY profile in this group sequentially, fail-fast (mutually exclusive with --host/--profile/--sha/--rollback)")
	cmd.Flags().Int("drain-timeout", deployRollingDrainTimeout, "seconds to wait for the proxy switch to confirm before continuing (rolling mode only)")
	cmd.Flags().String("disk-threshold", "", "fail closed when VM disk usage >= pct (pipeline)")
	cmd.Flags().String("env-file", "", "push this env file (0600 + atomic mv, API_GIT_SHA stamped) before restart (pipeline)")
	cmd.Flags().Bool("skip-smoke", false, "skip the public smoke (pipeline)")
	cmd.Flags().String("binary", "", "fast-push a declared artifact (kampodra.json \"binary\") instead of the image: local build → atomic remote swap → restart → the same gates (kind static skips the restart — the app serves the mount per-request, gated on /build-id.txt). Bare --binary picks the lone block; code-only — dependency changes redeploy the image")
	cmd.Flags().Lookup("binary").NoOptDefVal = "-" // bare --binary = the lone block
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), deployHelp)
	})

	resolveDeploy := func(c *cobra.Command, profileOverride string) (Target, error) {
		host, _ := c.Flags().GetString("host")
		key, _ := c.Flags().GetString("ssh-key")
		profile := profileOverride
		if profile == "" {
			profile, _ = c.Flags().GetString("profile")
		}
		cfg, err := state.LoadConfig(d.Home)
		if err != nil {
			return Target{}, err
		}
		mf, err := d.manifestFor()
		if err != nil {
			return Target{}, err
		}
		return ResolveTarget(cfg, host, key, profile, mf, d.Env)
	}

	// --- subcommands (one builder each; they share the resolver closure) ----
	cmd.AddCommand(
		newDeployConvergeSub(d, resolveDeploy),
		newDeployListSub(d, resolveDeploy),
		newDeployPruneSub(d, resolveDeploy),
		newDeployLogsSub(d, resolveDeploy),
		newDeployRestartSub(d, resolveDeploy),
		newDeployShellSub(d, resolveDeploy),
	)
	return cmd
}
