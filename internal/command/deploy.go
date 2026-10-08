package command

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/osfacts"
	"github.com/talha7k/kampodra/internal/adapter/runtime"
	"github.com/talha7k/kampodra/internal/adapter/state"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// Ports of scripts/deploy.sh (surface) and scripts/deploy-lifecycle.sh +
// scripts/common.sh (behavior): the deployment lifecycle over ssh. The
// build/stream pipeline itself is NOT ported yet — the surface is complete,
// the pipeline invocation degrades to an honest NOT_YET_PORTED message
// (fail closed: nothing was deployed).
const (
	deployContainer = "kampodine-api" // the VM-side service/container name
	deployDiskPath  = "/var/lib/containers"
	deployKeepN     = 2 // prune's --keep default
	deployLogLines  = "100"
)

const deployHelp = `Usage:
  kampodine deploy [--host root@<ip>] [--profile <name>] [--version <sha7>] [--rollback [<sha7>]]
                   [--dockerfile <path>] [--ssh-key <path>] [--skip-smoke] [--refresh-config]
                   [--require-disk <pct>]
  kampodra deploy list   [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--all-profiles]
  kampodra deploy prune  [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--keep N] [--dry-run]
  kampodra deploy logs   [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--lines N] [--follow]
  kampodra deploy restart [--host root@<ip>] [--profile <name>] [--ssh-key <path>]
  kampodra deploy shell  [--host root@<ip>] [--profile <name>] [exec -- <cmd>...]

Lifecycle subcommands are --host-aware with the SAME resolution as deploy:
--host | --profile <name> | KAMPODINE_PROFILE | config defaultProfile |
KAMPODINE_HOST; --ssh-key | profile sshKey | KAMPODINE_SSH_KEY | ssh-agent /
~/.ssh/config.

  list     deployment history: the VM's sha-tagged images (running one marked)
           merged with the local ledger (~/.kampodine/deployments.jsonl) with a
           "Total deployments" footer. VM unreachable degrades to a
           ledger-only view. --all-profiles renders one section per profile.
  prune    reclaim VM disk: removes OLD sha-tagged deploy images. ALWAYS kept:
           the currently-running image, ts-rollback, and the newest --keep N
           (default 2). --dry-run prints the exact podman rmi commands.
  logs     tail the running api container's logs (--lines N); --follow streams
           until ctrl-c (clean exit).
  restart  restart the api service (init-aware: rc-service on Alpine, systemctl
           on systemd hosts).
  shell    interactive sh inside the running api container; ` + "`exec -- <cmd>`" + `
           runs a one-shot command instead.

Examples:
  kampodra deploy list --host root@203.0.113.10
  kampodra deploy prune --host root@203.0.113.10 --dry-run
  kampodra deploy logs --host root@203.0.113.10 --lines 200 --follow
  kampodra deploy restart --host root@203.0.113.10
  kampodra deploy shell --host root@203.0.113.10 exec -- df -h /var/lib/containers
`

// pipelineFlags mark deploy modes whose implementation is the build/stream
// pipeline (not ported yet) — naming them keeps the honest failure.
var pipelineFlags = []string{"dockerfile", "refresh-config", "require-disk", "skip-smoke", "version", "rollback"}

func newDeployCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy [--host root@<ip>] [--profile <name>] [--version <sha7>] [--rollback [<sha7>]] [--dockerfile <path>] [--ssh-key <path>] [--skip-smoke] [--refresh-config] [--require-disk <pct>]",
		Short: "stream deploy (podman save | ssh podman load) with sha-verified health gate; --rollback [sha] = instant image-tag rollback",
		Args:  cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			for _, f := range pipelineFlags {
				if c.Flags().Changed(f) {
					return fmt.Errorf("the deploy build/stream pipeline (--%s) is NOT_YET_PORTED in kampodra — the lifecycle subcommands are live: kampodra deploy list | logs | prune | restart | shell", f)
				}
			}
			return fmt.Errorf("the deploy build/stream pipeline is NOT_YET_PORTED in kampodra — the lifecycle subcommands are live: kampodra deploy list | logs | prune | restart | shell")
		},
	}
	cmd.Flags().String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODINE_HOST and any profile")
	cmd.Flags().String("profile", "", "per-instance profile (~/.kampodine/config.json) — beats KAMPODINE_PROFILE / defaultProfile")
	cmd.Flags().String("ssh-key", "", "identity file — beats KAMPODINE_SSH_KEY; empty = agent / ssh config")
	cmd.Flags().String("dockerfile", "", "Dockerfile to build (pipeline)")
	cmd.Flags().String("version", "", "deploy a specific version (git sha fragment) (pipeline)")
	cmd.Flags().String("rollback", "", "instant image-tag rollback to the previous (or given) sha (pipeline)")
	cmd.Flags().String("require-disk", "", "fail closed when VM disk usage >= pct (pipeline)")
	cmd.Flags().Bool("skip-smoke", false, "skip the public smoke (pipeline)")
	cmd.Flags().Bool("refresh-config", false, "refresh the config service BEFORE restart (pipeline)")
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), deployHelp)
	})

	deployTargetFlags := func(c *cobra.Command) {
		c.Flags().String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODINE_HOST and any profile")
		c.Flags().String("profile", "", "per-instance profile (~/.kampodine/config.json) — beats KAMPODINE_PROFILE / defaultProfile")
		c.Flags().String("ssh-key", "", "identity file — beats KAMPODINE_SSH_KEY; empty = agent / ssh config")
	}
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
		return ResolveTarget(cfg, host, key, profile, d.Env)
	}

	// --- list ---------------------------------------------------------------
	subList := &cobra.Command{
		Use:   "list",
		Short: "deployment history: VM sha-tagged images (running one marked) merged with the local ledger + total deployments count",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := state.LoadConfig(d.Home)
			if err != nil {
				return err
			}
			allProfiles, _ := c.Flags().GetBool("all-profiles")
			names, err := fanOutProfileNames(cfg, allProfiles, state.ConfigPath(d.Home))
			if err != nil {
				return err
			}
			for _, name := range names {
				target, err := resolveDeploy(c, name)
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
	deployTargetFlags(subList)
	subList.Flags().Bool("all-profiles", false, "render one history section per configured profile")

	// --- prune ---------------------------------------------------------------
	subPrune := &cobra.Command{
		Use:   "prune",
		Short: "reclaim VM disk: remove old sha-tagged images (keeps running + ts-rollback + newest N); --dry-run prints exact commands",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolveDeploy(c, "")
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
			keepN := 0
			fmt.Sscanf(keep, "%d", &keepN)
			dryRun, _ := c.Flags().GetBool("dry-run")
			return runDeployPrune(d, c.Context(), target, keepN, dryRun)
		},
	}
	deployTargetFlags(subPrune)
	subPrune.Flags().String("keep", fmt.Sprintf("%d", deployKeepN), "newest N sha-tagged images to always keep")
	subPrune.Flags().Bool("dry-run", false, "print the exact podman rmi commands and remove nothing")

	// --- logs ---------------------------------------------------------------
	subLogs := &cobra.Command{
		Use:   "logs",
		Short: "tail the running api container's logs (--lines N); --follow streams until ctrl-c",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolveDeploy(c, "")
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
	deployTargetFlags(subLogs)
	subLogs.Flags().String("lines", deployLogLines, "number of lines to tail")
	subLogs.Flags().Bool("follow", false, "stream the logs until ctrl-c (clean exit)")

	// --- restart ---------------------------------------------------------------
	subRestart := &cobra.Command{
		Use:   "restart",
		Short: "restart the api service (init-aware: rc-service on Alpine, systemctl on systemd hosts)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolveDeploy(c, "")
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			return runDeployRestart(d, c.Context(), target)
		},
	}
	deployTargetFlags(subRestart)

	// --- shell ---------------------------------------------------------------
	subShell := &cobra.Command{
		Use:   "shell [exec -- <cmd>...]",
		Short: "interactive sh in the api container (exec -- <cmd> for one-shot)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			target, err := resolveDeploy(c, "")
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			return runDeployShell(d, c, target, args)
		},
	}
	deployTargetFlags(subShell)

	cmd.AddCommand(subList, subPrune, subLogs, subRestart, subShell)
	return cmd
}

// fanOutProfileNames: single-shot resolution yields [""] (the normal
// flag/env/default ladder); --all-profiles yields every configured profile
// name sorted (one rendered section each).
func fanOutProfileNames(cfg *state.Config, allProfiles bool, configPath string) ([]string, error) {
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
		fmt.Sprintf("podman images --format '{{.Tag}}|{{.CreatedAt}}|{{.Size}}' %s 2>/dev/null", runtime.DeployImageRepo))
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
		fmt.Sprintf("podman images --format '{{.Tag}}|{{.CreatedAt}}|{{.Size}}' %s 2>/dev/null", runtime.DeployImageRepo))
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
		fmt.Fprintf(d.Stdout, "  podman rmi %s:%s   # %s\n", runtime.DeployImageRepo, r.Tag, r.Size)
	}
	if dryRun {
		fmt.Fprintln(d.Stdout, "dry-run: nothing removed — re-run without --dry-run to apply")
		return nil
	}
	for _, r := range removals {
		fmt.Fprintf(d.Stdout, "removing %s:%s\n", runtime.DeployImageRepo, r.Tag)
		if _, err := d.Runner.Run(ctx, target.HostSpec, fmt.Sprintf("podman rmi %s:%s", runtime.DeployImageRepo, r.Tag)); err != nil {
			return fmt.Errorf("podman rmi %s:%s failed (in use? remove manually after checking podman ps)", runtime.DeployImageRepo, r.Tag)
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
	verb, tail := "", fmt.Sprintf("--tail %s %s", lines, deployContainer)
	if follow {
		verb = "-f "
		fmt.Fprintf(d.Stdout, "[deploy logs] streaming %s on %s (last %s lines) — ctrl-c to stop\n", deployContainer, host, lines)
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
	fmt.Fprintf(d.Stdout, "[deploy logs] tailing %s on %s (last %s lines)\n", deployContainer, host, lines)
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
	restartCmd, err := initadapter.ActionCommand(initSys, deployContainer, "restart")
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "%s (deploy's restart path: supervise-daemon on Alpine re-runs podman run on :latest)\n", restartCmd)
	if _, err := d.Runner.Run(ctx, target.HostSpec, restartCmd); err != nil {
		return fmt.Errorf("%s failed: %w", restartCmd, err)
	}
	statusCmd, err := initadapter.ActionCommand(initSys, deployContainer, "status")
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
		fmt.Fprintf(d.Stdout, "[deploy shell] interactive sh in %s on %s (exit to leave)\n", deployContainer, target.HostSpec.Host)
		fmt.Fprintln(d.Stdout, "[deploy shell] one-shot instead: kampodra deploy shell exec -- <cmd>")
		code, err := d.Runner.Stream(c.Context(), transport.SSHInteractiveArgs(target.HostSpec, fmt.Sprintf("podman exec -it %s sh", deployContainer)))
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
		return fmt.Errorf("shell exec requires -- before the command: kampodra deploy shell exec -- <cmd>...")
	}
	argv := args[dash:]
	if len(argv) == 0 {
		return fmt.Errorf("shell exec -- requires a command")
	}
	code, err := d.Runner.Stream(c.Context(), transport.SSHArgs(target.HostSpec,
		fmt.Sprintf("podman exec %s %s", deployContainer, shQuoteArgs(argv))))
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
