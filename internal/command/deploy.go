package command

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/osfacts"
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/runtime"
	"github.com/talha7k/kampodra/internal/adapter/state"
	"github.com/talha7k/kampodra/internal/adapter/transport"
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
  kampodra deploy logs   [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--lines N] [--follow]
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
  logs     tail the running api container's logs (--lines N); --follow streams
           until ctrl-c (clean exit).
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
	cmd.Flags().Int("drain-timeout", deployRollingDrainTimeout, "seconds to wait for the proxy switch to confirm before continuing (rolling mode only)")
	cmd.Flags().String("disk-threshold", "", "fail closed when VM disk usage >= pct (pipeline)")
	cmd.Flags().String("env-file", "", "push this env file (0600 + atomic mv, API_GIT_SHA stamped) before restart (pipeline)")
	cmd.Flags().Bool("skip-smoke", false, "skip the public smoke (pipeline)")
	cmd.Flags().String("binary", "", "fast-push a declared artifact (kampodra.json \"binary\") instead of the image: local build → atomic remote swap → restart → the same gates. Bare --binary picks the lone block; code-only — dependency changes redeploy the image")
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

// newDeployConvergeSub is deploy's `converge` subcommand: finish an
// interrupted --rolling deploy (retag → restart → gate → re-point → rm
// shadow).
func newDeployConvergeSub(d Deps, resolve func(*cobra.Command, string) (Target, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "converge [<sha7>]",
		Short: "finish an interrupted --rolling deploy: retag → restart → health gate → re-point to main → remove the shadow",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			target, err := resolve(c, "")
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			sha := ""
			if len(args) == 1 {
				sha = args[0]
				if !shaFragmentRe.MatchString(sha) {
					return fmt.Errorf("converge <sha> must be a git sha fragment (got: %s)", sha)
				}
			}
			drain, _ := c.Flags().GetInt("drain-timeout")
			return runDeployConverge(d, c.Context(), target, sha, drain)
		},
	}
	cmd.Flags().Int("drain-timeout", deployRollingDrainTimeout, "drain budget carried from the interrupted deploy (s)")
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), deployHelp)
	})
	return cmd
}

// newDeployListSub is deploy's `list` subcommand: deployment history,
// one section per profile (--all-profiles / --group fan-out).
func newDeployListSub(d Deps, resolve func(*cobra.Command, string) (Target, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "deployment history: VM sha-tagged images (running one marked) merged with the local ledger + total deployments count",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := state.LoadConfig(d.Home)
			if err != nil {
				return err
			}
			allProfiles, _ := c.Flags().GetBool("all-profiles")
			flagGroup, _ := c.Flags().GetString("group")
			names, err := fanOutProfileNames(cfg, allProfiles, flagGroup, state.ConfigPath(d.Home))
			if err != nil {
				return err
			}
			for _, name := range names {
				target, err := resolve(c, name)
				if err != nil {
					return err
				}
				if err := requireHost(target); err != nil {
					return err
				}
				if name != "" {
					fmt.Fprintf(d.Stdout, "== profile: %s ==\n", name)
				}
				if err := runDeployList(d, c.Context(), target); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().Bool("all-profiles", false, "render one history section per configured profile")
	cmd.Flags().String("group", "", "render history for every profile in this group (exclusive with --all-profiles/--host/--profile)")
	return cmd
}

// newDeployPruneSub is deploy's `prune` subcommand: reclaim VM disk by
// removing old sha-tagged images (keeps running + ts-rollback + newest N).
func newDeployPruneSub(d Deps, resolve func(*cobra.Command, string) (Target, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "reclaim VM disk: remove old sha-tagged images (keeps running + ts-rollback + newest N); --dry-run prints exact commands",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolve(c, "")
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			keep, _ := c.Flags().GetString("keep")
			if !positiveIntRe.MatchString(keep) {
				return fmt.Errorf("--keep must be a non-negative integer (got: %s)", keep)
			}
			// Validated non-negative by the regex above — Atoi cannot fail
			// here; the error return exists so the check is explicit.
			keepN, err := strconv.Atoi(keep)
			if err != nil {
				return fmt.Errorf("--keep must be a non-negative integer (got: %s)", keep)
			}
			dryRun, _ := c.Flags().GetBool("dry-run")
			return runDeployPrune(d, c.Context(), target, keepN, dryRun)
		},
	}
	cmd.Flags().String("keep", fmt.Sprintf("%d", deployKeepN), "newest N sha-tagged images to always keep")
	cmd.Flags().Bool("dry-run", false, "print the exact podman rmi commands and remove nothing")
	return cmd
}

// newDeployLogsSub is deploy's `logs` subcommand: tail the running api
// container's logs; --follow streams until ctrl-c.
func newDeployLogsSub(d Deps, resolve func(*cobra.Command, string) (Target, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "tail the running api container's logs (--lines N); --follow streams until ctrl-c",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolve(c, "")
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			lines, _ := c.Flags().GetString("lines")
			if !nonZeroIntRe.MatchString(lines) {
				return fmt.Errorf("--lines must be a positive integer (got: %s)", lines)
			}
			follow, _ := c.Flags().GetBool("follow")
			return runDeployLogs(d, c, target, lines, follow)
		},
	}
	cmd.Flags().String("lines", deployLogLines, "number of lines to tail")
	cmd.Flags().Bool("follow", false, "stream the logs until ctrl-c (clean exit)")
	return cmd
}

// newDeployRestartSub is deploy's `restart` subcommand: init-aware api
// service restart (rc-service on Alpine, systemctl on systemd hosts).
func newDeployRestartSub(d Deps, resolve func(*cobra.Command, string) (Target, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "restart the api service (init-aware: rc-service on Alpine, systemctl on systemd hosts)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolve(c, "")
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			return runDeployRestart(d, c.Context(), target)
		},
	}
}

// newDeployShellSub is deploy's `shell` subcommand: interactive sh in the
// api container (exec -- <cmd> for one-shot).
func newDeployShellSub(d Deps, resolve func(*cobra.Command, string) (Target, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "shell [exec -- <cmd>...]",
		Short: "interactive sh in the api container (exec -- <cmd> for one-shot)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			target, err := resolve(c, "")
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			return runDeployShell(d, c, target, args)
		},
	}
}

// runDeployRoot dispatches the pipeline surface: --rollback / --sha /
// plain (optionally --rolling), with the shell's argument grammar (a
// positional in rollback mode is the sha; anything else is unknown).
// resolveRollingMode returns the effective deploy mode: rolling is the
// DEFAULT since the 2026-10-10 promotion (OCI live-fire drill + two clean
// prod rolling deploys); --in-place is the escape hatch (the pre-promotion
// default) and beats --rolling.
func resolveRollingMode(c *cobra.Command) bool {
	rolling, _ := c.Flags().GetBool("rolling")
	inPlace, _ := c.Flags().GetBool("in-place")
	return rolling && !inPlace
}

// validateDeployArgs enforces deploy's flag contract: mode exclusivity,
// percentage/fragment shapes, and the positional-sha rules (only the
// rollback mode takes one; --rollback=<sha> + positional = given twice).
// Returns the positional rollback sha ("" = none).
func validateDeployArgs(c *cobra.Command, args []string) (string, error) {
	shaArg, _ := c.Flags().GetString("sha")
	rollbackSet := c.Flags().Changed("rollback")
	rolling := resolveRollingMode(c)
	drain, _ := c.Flags().GetInt("drain-timeout")
	diskThreshold, _ := c.Flags().GetString("disk-threshold")

	// The --binary selector's positional form (`--binary api-go` — the
	// same grammar as --rollback's NoOptDefVal).
	args, err := applyBinaryPositional(c, args)
	if err != nil {
		return "", err
	}

	if c.Flags().Changed("sha") && rollbackSet {
		return "", fmt.Errorf("--rollback and --sha are exclusive")
	}
	// Only an EXPLICIT --rolling conflicts with --rollback: a bare
	// --rollback under the rolling default is the everyday instant
	// rollback and stays valid (rollback never boots a shadow — it is
	// inherently in-place; assembleDeployRun forces it).
	if c.Flags().Changed("rolling") && rolling && rollbackSet {
		return "", fmt.Errorf("--rolling and --rollback are exclusive (rollback is instant — the old image is already on the VM)")
	}
	if c.Flags().Changed("drain-timeout") && !rolling {
		return "", fmt.Errorf("--drain-timeout requires the rolling mode (the default) — not available with --in-place")
	}
	if drain < 0 {
		return "", fmt.Errorf("--drain-timeout must be a non-negative number of seconds (got: %d)", drain)
	}
	if diskThreshold != "" {
		if n, err := strconv.Atoi(diskThreshold); err != nil || n < 0 || n > 100 {
			return "", fmt.Errorf("--disk-threshold must be a percentage 0-100 (got: %s)", diskThreshold)
		}
	}
	if c.Flags().Changed("sha") && !shaFragmentRe.MatchString(shaArg) {
		return "", fmt.Errorf("--sha must be a git sha fragment (got: %s)", shaArg)
	}
	rollbackSha, err := positionalRollbackSha(c, args)
	if err != nil {
		return "", err
	}
	if err := validateBinaryExclusives(c, rolling, rollbackSet, rollbackSha); err != nil {
		return "", err
	}
	return rollbackSha, nil
}

// validateBinaryExclusives pins the fast path's flag grammar: it is its
// own artifact+switch strategy — it always builds (no existing image to
// stream) and restarts in place (no shadow).
func validateBinaryExclusives(c *cobra.Command, rolling, rollbackSet bool, rollbackSha string) error {
	if !c.Flags().Changed("binary") {
		return nil
	}
	if c.Flags().Changed("sha") {
		return fmt.Errorf("--binary and --sha are exclusive — the fast path always builds (no image to stream)")
	}
	if c.Flags().Changed("rolling") && rolling {
		return fmt.Errorf("--binary and --rolling are exclusive — the fast path restarts in place (image deploys use the rolling shadow)")
	}
	if rollbackSet && rollbackSha != "" {
		return fmt.Errorf("--binary --rollback takes no sha — it restores the recorded previous artifact (image rollback: drop --binary)")
	}
	return nil
}

// applyBinaryPositional absorbs the `--binary <name>` positional form
// (NoOptDefVal "-" makes bare --binary flag-like) and returns the
// remaining args. Rollback's positional (the sha) keeps priority.
func applyBinaryPositional(c *cobra.Command, args []string) ([]string, error) {
	name, _ := c.Flags().GetString("binary")
	if !c.Flags().Changed("binary") || name != "-" || len(args) == 0 || c.Flags().Changed("rollback") {
		return args, nil
	}
	if err := c.Flags().Set("binary", args[0]); err != nil {
		return nil, err
	}
	return args[1:], nil
}

// positionalRollbackSha applies the positional grammar: only the rollback
// mode takes a positional (the sha); --rollback=<sha> + positional = given
// twice; whatever survives must be a sha fragment.
func positionalRollbackSha(c *cobra.Command, args []string) (string, error) {
	rollbackRaw, _ := c.Flags().GetString("rollback")
	rollbackSet := c.Flags().Changed("rollback")
	rollbackSha := ""
	if rollbackSet && rollbackRaw != "-" {
		rollbackSha = rollbackRaw
	}
	if len(args) > 0 {
		if !rollbackSet {
			return "", fmt.Errorf("unknown argument: %s (--help)", args[0])
		}
		if rollbackSha != "" {
			return "", fmt.Errorf("rollback sha given twice (--rollback=%s and %s)", rollbackSha, args[0])
		}
		rollbackSha = args[0]
	}
	if rollbackSet && rollbackSha != "" && !shaFragmentRe.MatchString(rollbackSha) {
		return "", fmt.Errorf("--rollback must be a git sha fragment (or bare for stamp-resolved rollback; got: %s)", rollbackSha)
	}
	return rollbackSha, nil
}

// assembleDeployRun resolves the deploy target and pipeline options:
// config + manifest + host resolution, the --dockerfile ladder, and the
// sidecar set off the repo manifest.
func assembleDeployRun(d Deps, c *cobra.Command) (Target, deployOpts, error) {
	host, _ := c.Flags().GetString("host")
	key, _ := c.Flags().GetString("ssh-key")
	profile, _ := c.Flags().GetString("profile")
	rolling := resolveRollingMode(c)
	// Rollback never boots a shadow: it is instant and inherently
	// in-place (the old image is already on the VM).
	if c.Flags().Changed("rollback") {
		rolling = false
	}
	drain, _ := c.Flags().GetInt("drain-timeout")
	dockerfile, _ := c.Flags().GetString("dockerfile")
	envFile, _ := c.Flags().GetString("env-file")
	diskThreshold, _ := c.Flags().GetString("disk-threshold")
	skipSmoke, _ := c.Flags().GetBool("skip-smoke")
	binaryName, _ := c.Flags().GetString("binary")

	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return Target{}, deployOpts{}, err
	}
	mf, err := d.manifestFor()
	if err != nil {
		return Target{}, deployOpts{}, err
	}
	target, err := ResolveTarget(cfg, host, key, profile, mf, d.Env)
	if err != nil {
		return Target{}, deployOpts{}, err
	}
	if err := requireHost(target); err != nil {
		return Target{}, deployOpts{}, err
	}
	// The --dockerfile ladder: explicit flag > project config (whose value
	// already merged env > profile block > kampodra.json > "Dockerfile").
	dockerfile = firstNonEmpty(dockerfile, target.Project.Dockerfile)

	// Sidecars (kampodra.json images.sidecars): a repo property read
	// straight off the manifest — no ladder (per-host profiles and env
	// have nothing to say about which secondary images a REPO ships).
	sidecars := []project.ManifestSidecar{}
	if mf != nil && mf.Fields.Images != nil {
		sidecars = mf.Fields.Images.Sidecars
	}
	// The fast path resolves its artifact selector BEFORE the pipeline
	// runs: a wrong --binary name dies here, not at the remote swap.
	if c.Flags().Changed("binary") {
		if _, err := ChooseBinaryBlock(target.Binary, binaryName); err != nil {
			return Target{}, deployOpts{}, err
		}
	} else {
		binaryName = ""
	}
	return target, deployOpts{
		dockerfile:    dockerfile,
		envFile:       envFile,
		diskThreshold: diskThreshold,
		skipSmoke:     skipSmoke,
		rolling:       rolling,
		drainTimeout:  drain,
		sidecars:      sidecars,
		binary:        binaryName,
	}, nil
}

func runDeployRoot(d Deps, c *cobra.Command, args []string) error {
	rollbackSha, err := validateDeployArgs(c, args)
	if err != nil {
		return err
	}
	rollbackSet := c.Flags().Changed("rollback")
	shaArg, _ := c.Flags().GetString("sha")
	target, opts, err := assembleDeployRun(d, c)
	if err != nil {
		return err
	}
	ctx := c.Context()

	if rollbackSet {
		if opts.binary != "" {
			// Binary rollback's identity is the recorded marker (the
			// pipeline resolves it) — the image stamp ladder does not
			// apply.
			return runDeployPipeline(d, ctx, target, "", "rollback", opts)
		}
		// THE resolution ladder: explicit arg > the VM's deployed-sha stamp
		// file > die. NEVER git HEAD (the shell's silent-HEAD fallback
		// redeployed the very build being rolled back from).
		sha, _, err := resolveRollbackSha(d, ctx, target, rollbackSha)
		if err != nil {
			return err
		}
		return runDeployPipeline(d, ctx, target, sha, "rollback", opts)
	}

	ver := shaArg
	mode := "deploy"
	if c.Flags().Changed("sha") {
		// version mode: stream an EXISTING build — no rebuild, no clean-tree
		// gate (the build already happened when that sha was HEAD).
		mode = "sha"
	} else {
		// deploy mode: build HEAD — the sha comes from git rev-parse (the
		// clean-tree gate guarantees the tree shipped what HEAD names).
		out, err := localOutput(ctx, "", "git", "rev-parse", "--short", "HEAD")
		if err != nil {
			return fmt.Errorf("cannot resolve HEAD (not a git repository?) — deploy stamps the git sha; from a non-repo directory use --sha <sha7> to stream an existing build")
		}
		ver = strings.TrimSpace(out)
	}
	return runDeployPipeline(d, ctx, target, ver, mode, opts)
}

// resolveRollbackSha is the rollback ladder: explicit argument first, then
// the VM's deployed-sha stamp file, then die naming both routes — the
// error never suggests git HEAD.
func resolveRollbackSha(d Deps, ctx context.Context, target Target, explicit string) (string, string, error) {
	if explicit != "" {
		return explicit, "explicit", nil
	}
	stampFile := target.Project.DeployedShaFile
	out, err := d.Runner.Run(ctx, target.HostSpec, fmt.Sprintf("cat %s 2>/dev/null", stampFile))
	sha := strings.TrimSpace(out)
	if err == nil && shaFragmentRe.MatchString(sha) {
		fmt.Fprintf(d.Stdout, "[deploy] rollback target: %s (from the VM's deployed-sha stamp)\n", sha)
		return sha, "stamp", nil
	}
	return "", "", fmt.Errorf("no rollback target: pass the sha (kampodra deploy --rollback <sha7>) or make sure %s exists on the VM (every successful deploy writes it; see: kampodra deploy list) — a bare --rollback never falls back to git HEAD", stampFile)
}

// fanOutProfileNames: single-shot resolution yields [""] (the normal
// flag/env/default ladder); --all-profiles yields every configured profile
// name sorted (one rendered section each).
// fanOutProfileNames resolves the profile set for the fan-out commands
// (status, deploy list): default = the single resolved profile ([]string{""}),
// --all-profiles = every configured profile, --group <g> = every profile in
// that group. --group and --all-profiles are exclusive; an empty group is
// an error naming the known groups.
func fanOutProfileNames(cfg *state.Config, allProfiles bool, group string, configPath string) ([]string, error) {
	if allProfiles && group != "" {
		return nil, fmt.Errorf("--group and --all-profiles are exclusive")
	}
	if group != "" {
		if len(cfg.Profiles) == 0 {
			return nil, fmt.Errorf("--group %s: no profiles configured in %s", group, configPath)
		}
		names := make([]string, 0, len(cfg.Profiles))
		groups := map[string]bool{}
		for name, p := range cfg.Profiles {
			if p.Group != "" {
				groups[p.Group] = true
			}
			if p.Group == group {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			known := make([]string, 0, len(groups))
			for g := range groups {
				known = append(known, g)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("--group %s: no profiles in that group (known groups: %s)", group, strings.Join(known, ", "))
		}
		sort.Strings(names)
		return names, nil
	}
	if !allProfiles {
		return []string{""}, nil
	}
	if len(cfg.Profiles) == 0 {
		return nil, fmt.Errorf("--all-profiles: no profiles configured in %s", configPath)
	}
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// runDeployList ports the `deploy list` case: header, VM truth (sha-tagged
// images + running) merged with the local ledger, Total deployments footer.
func runDeployList(d Deps, ctx context.Context, target Target) error {
	host := target.HostSpec.Host
	ledgerPath := state.LedgerPath(d.Home)
	fmt.Fprintf(d.Stdout, "== deployment history for %s (ledger: %s) ==\n", host, ledgerPath)

	imagesRaw, imagesErr := d.Runner.Run(ctx, target.HostSpec,
		fmt.Sprintf("podman images --format '{{.Tag}}|{{.CreatedAt}}|{{.Size}}' %s 2>/dev/null", target.Project.ImagePrefix))
	runningRaw, runningErr := d.Runner.Run(ctx, target.HostSpec, "podman ps --format '{{.Image}}' 2>/dev/null")
	if imagesErr != nil && runningErr != nil {
		// Reachable-but-empty is NOT unreachable: probe once more, and only
		// a dead VM degrades the view loudly.
		if _, probeErr := d.Runner.Run(ctx, target.HostSpec, "podman images >/dev/null 2>&1"); probeErr != nil {
			fmt.Fprintf(d.Stdout, "WARNING: cannot reach VM %s — showing ledger-only history (no VM truth)\n", host)
		}
	}

	ledger := state.LedgerEntries(ledgerPath, host)
	rows := buildDeployListRows(runtime.ParseImages(imagesRaw), runningRaw, ledger)
	fmt.Fprint(d.Stdout, headerDeployList)
	if len(rows) == 0 {
		fmt.Fprintln(d.Stdout, "(no deployments recorded yet — the ledger fills as you deploy)")
	} else {
		fmt.Fprint(d.Stdout, renderDeployListRows(rows))
	}
	fmt.Fprintf(d.Stdout, "Total deployments: %d · on %s\n", len(rows), host)
	return nil
}

const headerDeployList = "TAG       CREATED           STATE     RESULT    SUBJECT\n"

// runDeployPrune ports the `deploy prune` case: plan from the shared
// keep-set math, --dry-run prints and stops, apply removes one by one and
// re-reads the disk guard.
func runDeployPrune(d Deps, ctx context.Context, target Target, keepN int, dryRun bool) error {
	imagesRaw, err := d.Runner.Run(ctx, target.HostSpec,
		fmt.Sprintf("podman images --format '{{.Tag}}|{{.CreatedAt}}|{{.Size}}' %s 2>/dev/null", target.Project.ImagePrefix))
	if err != nil {
		return fmt.Errorf("cannot list images on %s (ssh failed — refusing to prune blind)", target.HostSpec.Host)
	}
	runningRaw, _ := d.Runner.Run(ctx, target.HostSpec, "podman ps --format '{{.Image}}' 2>/dev/null")
	removals, err := runtime.PruneSelect(runtime.ShaTagged(runtime.ParseImages(imagesRaw)), runningRaw, keepN)
	if err != nil {
		return fmt.Errorf("prune selection failed: %w", err)
	}
	if len(removals) == 0 {
		fmt.Fprintf(d.Stdout, "nothing to prune (keep set: running + ts-rollback + newest %d)\n", keepN)
		return nil
	}
	sizes := make([]string, 0, len(removals))
	for _, r := range removals {
		sizes = append(sizes, r.Size)
	}
	fmt.Fprintf(d.Stdout, "plan: %d image(s), %s reclaimed (keep: running + ts-rollback + newest %d)\n",
		len(removals), runtime.SumSizesHuman(sizes), keepN)
	for _, r := range removals {
		fmt.Fprintf(d.Stdout, "  podman rmi %s:%s   # %s\n", target.Project.ImagePrefix, r.Tag, r.Size)
	}
	if dryRun {
		fmt.Fprintln(d.Stdout, "dry-run: nothing removed — re-run without --dry-run to apply")
		return nil
	}
	for _, r := range removals {
		fmt.Fprintf(d.Stdout, "removing %s:%s\n", target.Project.ImagePrefix, r.Tag)
		if _, err := d.Runner.Run(ctx, target.HostSpec, fmt.Sprintf("podman rmi %s:%s", target.Project.ImagePrefix, r.Tag)); err != nil {
			return fmt.Errorf("podman rmi %s:%s failed (in use? remove manually after checking podman ps)", target.Project.ImagePrefix, r.Tag)
		}
	}
	if dfOut, err := d.Runner.Run(ctx, target.HostSpec, fmt.Sprintf("df -P %s 2>/dev/null", deployDiskPath)); err == nil {
		if pct, ok := osfacts.DiskUsedPct(dfOut); ok {
			fmt.Fprintf(d.Stdout, "VM disk after prune: %d%% used on %s\n", pct, deployDiskPath)
			if pct > 90 {
				fmt.Fprintf(d.Stdout, "WARNING: disk still above 90%% — investigate before the next deploy\n")
			}
		}
	}
	return nil
}

// runDeployLogs ports the `deploy logs` case; --follow streams with a
// clean-exit ctrl-c contract (signal-cancelled context, exit 0).
func runDeployLogs(d Deps, cmd *cobra.Command, target Target, lines string, follow bool) error {
	host := target.HostSpec.Host
	verb, tail := "", fmt.Sprintf("--tail %s %s", lines, target.Project.Container)
	if follow {
		verb = "-f "
		fmt.Fprintf(d.Stdout, "[deploy logs] streaming %s on %s (last %s lines) — ctrl-c to stop\n", target.Project.Container, host, lines)
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		code, err := d.Runner.Stream(ctx, transport.SSHArgs(target.HostSpec, fmt.Sprintf("podman logs %s%s", verb, tail)))
		if err != nil {
			return err
		}
		if code != 0 {
			return &exitError{code: code}
		}
		return nil
	}
	fmt.Fprintf(d.Stdout, "[deploy logs] tailing %s on %s (last %s lines)\n", target.Project.Container, host, lines)
	out, err := d.Runner.Run(cmd.Context(), target.HostSpec, fmt.Sprintf("podman logs %s%s", verb, tail))
	if err != nil {
		return err
	}
	fmt.Fprint(d.Stdout, out)
	return nil
}

// runDeployRestart ports the `deploy restart` case: init detection (cached
// in the profile), the svc_action restart, then a best-effort status check.
func runDeployRestart(d Deps, ctx context.Context, target Target) error {
	runErr := func(ctx context.Context, remote string) (string, error) {
		return d.Runner.Run(ctx, target.HostSpec, remote)
	}
	initSys, _, _ := initadapter.Detect(ctx, runErr, target.ProfileInit)
	restartCmd, err := initadapter.ActionCommand(initSys, target.Project.Container, "restart")
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "%s (deploy's restart path: supervise-daemon on Alpine re-runs podman run on :latest)\n", restartCmd)
	if _, err := d.Runner.Run(ctx, target.HostSpec, restartCmd); err != nil {
		return fmt.Errorf("%s failed: %w", restartCmd, err)
	}
	statusCmd, err := initadapter.ActionCommand(initSys, target.Project.Container, "status")
	if err == nil {
		fmt.Fprintln(d.Stdout, "service status:")
		if out, err := d.Runner.Run(ctx, target.HostSpec, statusCmd); err != nil {
			fmt.Fprintln(d.Stdout, "status command failed — check: kampodra deploy logs")
		} else {
			fmt.Fprint(d.Stdout, out)
		}
	}
	fmt.Fprintln(d.Stdout, "done — verify: kampodra deploy logs --lines 50")
	return nil
}

// runDeployShell ports the `deploy shell` case: interactive (forced tty)
// or `exec -- <cmd>` one-shot with POSIX sh-quoting (shell parity). pflag
// consumes the `--` terminator, so the command vector is located via
// ArgsLenAtDash — the same contract the shell parser implements by hand.
func runDeployShell(d Deps, c *cobra.Command, target Target, args []string) error {
	if len(args) == 0 {
		fmt.Fprintf(d.Stdout, "[deploy shell] interactive sh in %s on %s (exit to leave)\n", target.Project.Container, target.HostSpec.Host)
		fmt.Fprintln(d.Stdout, "[deploy shell] one-shot instead: kampodra deploy shell exec -- <cmd>")
		code, err := d.Runner.Stream(c.Context(), transport.SSHInteractiveArgs(target.HostSpec, fmt.Sprintf("podman exec -it %s sh", target.Project.Container)))
		if err != nil {
			return err
		}
		if code != 0 {
			return &exitError{code: code}
		}
		return nil
	}
	dash := c.ArgsLenAtDash()
	if args[0] != "exec" || dash != 1 {
		if args[0] != "exec" {
			return fmt.Errorf("unknown shell argument: %s (--help; one-shot is: kampodra deploy shell exec -- <cmd>)", args[0])
		}
		return fmt.Errorf("shell exec requires -- before the command: kampodra deploy shell exec -- <cmd>")
	}
	argv := args[dash:]
	if len(argv) == 0 {
		return fmt.Errorf("shell exec -- requires a command")
	}
	code, err := d.Runner.Stream(c.Context(), transport.SSHArgs(target.HostSpec,
		fmt.Sprintf("podman exec %s %s", target.Project.Container, shQuoteArgs(argv))))
	if err != nil {
		return err
	}
	if code != 0 {
		return &exitError{code: code}
	}
	return nil
}

// shQuoteArgs ports sh_quote_args: POSIX-single-quote each argument for
// safe composition into one remote sh command (busybox-safe).
func shQuoteArgs(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}

var (
	positiveIntRe = regexp.MustCompile(`^[0-9]+$`)
	nonZeroIntRe  = regexp.MustCompile(`^[1-9][0-9]*$`)
)
