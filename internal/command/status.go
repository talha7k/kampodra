package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/osfacts"
	"github.com/talha7k/kampodra/internal/adapter/runtime"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// diskPath is the container engine's storage path (infra constant, not
// project config).
const diskPath = "/var/lib/containers"

// pruneKeepN is status's estimate window (the shell calls prune_select
// with 2 — the same default as deploy prune).
const pruneKeepN = 2

// statusHelp is the shell usage() heredoc, extended with the
// kampodra-native --all-profiles and kampodra naming.
const statusHelp = `Usage:
  kampodra status [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--verbose] [--all-profiles] [--group <g>]

Examples:
  kampodra status                          # deployments (ledger) + live health
  kampodra status --host root@203.0.113.10 # + VM disk usage, image/prune estimate, service states
  kampodra status --profile prod           # resolve host/key from a config profile
  kampodra status --all-profiles           # one full section per configured profile
  kampodra status --group web              # one section per profile in the group
  kampodra status --host root@203.0.113.10 --verbose  # + the full metrics snapshot
                                            #   (load, memory, disk breakdown, containers, top procs)

Env: KAMPODRA_PROXY_HOST, KAMPODRA_HOST, KAMPODRA_SSH_KEY, KAMPODRA_PROFILE,
KAMPODRA_SERVICES, KAMPODRA_IMAGE_PREFIX, KAMPODRA_HEALTH_PATH.
`

func newStatusCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--verbose] [--all-profiles]",
		Short: "live health + deployment count + VM disk/image/service state + metrics (--verbose)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runStatus(d, c)
		},
	}
	cmd.Flags().String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODRA_HOST and any profile")
	cmd.Flags().String("profile", "", "per-instance profile (~/.kampodra/config.json) — beats KAMPODRA_PROFILE / defaultProfile")
	cmd.Flags().String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY; empty = agent / ssh config")
	cmd.Flags().Bool("verbose", false, "add the full metrics snapshot (needs a target)")
	cmd.Flags().Bool("all-profiles", false, "render one full status section per configured profile")
	cmd.Flags().String("group", "", "render status for every profile in this group (exclusive with --all-profiles/--host/--profile)")
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), statusHelp)
	})
	return cmd
}

func runStatus(d Deps, cmd *cobra.Command) error {
	flagHost, _ := cmd.Flags().GetString("host")
	flagKey, _ := cmd.Flags().GetString("ssh-key")
	flagProfile, _ := cmd.Flags().GetString("profile")
	verbose, _ := cmd.Flags().GetBool("verbose")
	allProfiles, _ := cmd.Flags().GetBool("all-profiles")
	flagGroup, _ := cmd.Flags().GetString("group")

	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	names, err := fanOutProfileNames(cfg, allProfiles, flagGroup, state.ConfigPath(d.Home))
	if err != nil {
		return err
	}
	mf, err := d.manifestFor()
	if err != nil {
		return err
	}
	for _, name := range names {
		profileParam := name
		if name == "" {
			profileParam = flagProfile
		}
		target, err := ResolveTarget(cfg, flagHost, flagKey, profileParam, mf, d.Env)
		if err != nil {
			return err
		}
		if name != "" {
			fmt.Fprintf(d.Stdout, "== profile: %s ==\n", name)
		}
		if err := statusBody(d, cmd.Context(), target, verbose); err != nil {
			return err
		}
	}
	return nil
}

// statusBody renders one target's full status view (deployments header,
// live edge, VM state).
func statusBody(d Deps, ctx context.Context, target Target, verbose bool) error {
	out := d.Stdout
	ledgerPath := state.LedgerPath(d.Home)

	// --- deployment count (the owner's "total deployments", always visible)
	fmt.Fprintf(out, "== deployments (ledger: %s) ==\n", ledgerPath)
	if target.HostSpec.Host != "" {
		fmt.Fprintf(out, "Total deployments: %d · deployments to %s\n",
			state.LedgerCount(ledgerPath, target.HostSpec.Host), target.HostSpec.Host)
	} else {
		fmt.Fprintf(out, "Total deployments: %d · across all hosts — pass --host root@<ip> for VM state\n",
			state.LedgerCount(ledgerPath, ""))
		if verbose {
			fmt.Fprintln(out, "metrics snapshot: needs a target — add --host root@<ip> (or --profile <name>)")
		}
	}
	fmt.Fprintln(out)

	// --- live (through the proxy)
	fmt.Fprintf(out, "== live (through the proxy: https://%s) ==\n", target.ProxyHost)
	if body, ok := d.Prober.LiveStatus(ctx, target.ProxyHost, target.Project.HealthPath); ok {
		fmt.Fprintf(out, "HEALTH OK: %s\n", body)
		buildID := "unreachable"
		if id, ok := d.Prober.BuildID(ctx, target.ProxyHost); ok {
			buildID = id
		}
		fmt.Fprintf(out, "build-id  : %s\n", buildID)
	} else {
		fmt.Fprintln(out, "UNREACHABLE or unhealthy — check the VM (ssh) and kamal-proxy")
	}

	if target.HostSpec.Host != "" {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "== VM (%s) ==\n", target.HostSpec.Host)
		// The shell's vm() helper as one closure: every VM read composes a
		// remote command and tolerates failure (`|| true` degradation).
		run := func(remote string) string {
			o, _ := d.Runner.Run(ctx, target.HostSpec, remote)
			return o
		}
		runErr := func(ctx context.Context, remote string) (string, error) {
			return d.Runner.Run(ctx, target.HostSpec, remote)
		}

		// disk
		pct, ok := osfacts.DiskUsedPct(run(fmt.Sprintf("df -P %s 2>/dev/null", diskPath)))
		switch osfacts.DiskVerdict(pctString(pct, ok), "") {
		case osfacts.VerdictOK, osfacts.VerdictWarn:
			fmt.Fprintf(out, "disk    : %d%% used on %s\n", pct, diskPath)
			if pct > 90 {
				fmt.Fprintln(out, "WARNING : VM disk above 90% — old sha-tagged deploy images pile up (~1GB each); reclaim: kampodra deploy prune --dry-run")
			}
		default:
			fmt.Fprintln(out, "disk    : unknown (df unreadable)")
		}

		// images + prune estimate
		images := runtime.ShaTagged(runtime.ParseImages(
			run(fmt.Sprintf("podman images --format '{{.Tag}}|{{.CreatedAt}}|{{.Size}}' %s 2>/dev/null", target.Project.ImagePrefix))))
		removals, pruneErr := runtime.PruneSelect(images, run("podman ps --format '{{.Image}}' 2>/dev/null"), pruneKeepN)
		reclaimTags := "(none)"
		sizes := make([]string, 0, len(removals))
		if pruneErr == nil && len(removals) > 0 {
			tags := make([]string, 0, len(removals))
			for _, r := range removals {
				tags = append(tags, r.Tag)
				sizes = append(sizes, r.Size)
			}
			reclaimTags = strings.Join(tags, ",")
		}
		fmt.Fprintf(out, "images  : %d sha-tagged deploy image(s); prune would remove %d %s (%s): kampodra deploy prune --dry-run\n",
			len(images), len(removals), reclaimTags, runtime.SumSizesHuman(sizes))

		// services (init-aware: openrc keeps the historical shape)
		fmt.Fprintln(out, "services:")
		initSys, _, _ := initadapter.Detect(ctx, runErr, target.ProfileInit)
		if states, err := initadapter.ServiceStates(ctx, runErr, initSys, target.Project.Services); err != nil {
			fmt.Fprintln(out, "(service roll call failed)")
		} else {
			for _, s := range states {
				state := "not-running"
				if s.Running {
					state = "running"
				}
				fmt.Fprintf(out, "%s: %s\n", s.Name, state)
			}
		}

		// verbose metrics snapshot
		if verbose {
			fmt.Fprintln(out)
			fmt.Fprintln(out, "== metrics snapshot ==")
			if raw, err := d.Runner.Run(ctx, target.HostSpec, osfacts.MetricsRemoteCmd); err != nil {
				fmt.Fprintln(out, "(metrics snapshot failed — VM unreachable?)")
			} else {
				snap := osfacts.ParseMetrics(raw)
				fmt.Fprintln(out, osfacts.RenderMetricsSnapshot(snap))
				if rootPct, ok := osfacts.RootDiskPct(snap); ok && rootPct > 90 {
					fmt.Fprintln(out, "WARNING : root disk above 90% — reclaim: kampodra deploy prune --dry-run")
				}
			}
		}
	}

	return nil
}

func pctString(pct int, ok bool) string {
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d", pct)
}
