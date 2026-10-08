package command

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/osfacts"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// metricsHelp is the shell metrics.sh usage heredoc, kampodra-fied.
const metricsHelp = `Usage:
  kampodra metrics [--profile <name>] [--host <user@ip>] [--ssh-key <path>]
                   [--warn-disk <pct>] [--watch <sec>] [--count <n>]

One SSH round-trip per snapshot; busybox-safe remote commands; rendering is
client-side. NO continuous monitoring — one-shot CLI view; --watch N
re-snapshots every N seconds (Ctrl-C ends), --count M bounds the
iterations. --warn-disk <pct> (default 90) exits 1 when root disk usage is
at or above the threshold — the SAME verdict logic as deploy's fail-closed
gate. Host resolution: --host | --profile <name> | KAMPODRA_PROFILE |
config defaultProfile | KAMPODRA_HOST.

Examples:
  kampodra metrics                              # profile/host from config or env
  kampodra metrics --host root@203.0.113.10     # explicit target
  kampodra metrics --profile prod               # per-instance profile
  kampodra metrics --warn-disk 85               # exit 1 at >= 85% (default 90)
  kampodra metrics --watch 10 --count 6         # snapshot every 10s, 6 times
  kampodra status --verbose                     # the same snapshot inside status
`

func newMetricsCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metrics [--host root@<ip>] [--profile <name>] [--warn-disk <pct>] [--watch <sec>] [--count <n>]",
		Short: "one-shot VM snapshot over SSH: load, memory, disk breakdown, containers, top procs",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runMetrics(d, c)
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), metricsHelp)
	})
	cmd.Flags().String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODRA_HOST and any profile")
	cmd.Flags().String("profile", "", "per-instance profile (~/.kampodra/config.json) — beats KAMPODRA_PROFILE / defaultProfile")
	cmd.Flags().String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY; empty = agent / ssh config")
	cmd.Flags().String("warn-disk", "90", "exit 1 when root disk usage is >= this percentage (1-100)")
	cmd.Flags().String("watch", "", "re-snapshot every N seconds (positive integer; ctrl-c ends)")
	cmd.Flags().String("count", "", "bound the number of snapshots (positive integer)")
	return cmd
}

func runMetrics(d Deps, c *cobra.Command) error {
	warnDisk, _ := c.Flags().GetString("warn-disk")
	watch, _ := c.Flags().GetString("watch")
	count, _ := c.Flags().GetString("count")
	host, _ := c.Flags().GetString("host")
	key, _ := c.Flags().GetString("ssh-key")
	profile, _ := c.Flags().GetString("profile")

	if n, err := strconv.Atoi(warnDisk); err != nil || n < 1 || n > 100 {
		return fmt.Errorf("--warn-disk must be a percentage 1-100 (got: %s)", warnDisk)
	}
	watchSec := 0
	if watch != "" {
		n, err := strconv.Atoi(watch)
		if err != nil || n < 1 {
			return fmt.Errorf("--watch must be a positive-integer number of seconds (got: %s)", watch)
		}
		watchSec = n
	}
	countN := 0
	if count != "" {
		n, err := strconv.Atoi(count)
		if err != nil || n < 1 {
			return fmt.Errorf("--count must be a positive integer (got: %s)", count)
		}
		countN = n
	}

	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	mf, mfErr := d.manifestFor()
	if mfErr != nil {
		return mfErr
	}
	target, err := ResolveTarget(cfg, host, key, profile, mf, d.Env)
	if err != nil {
		return err
	}
	if err := requireHost(target); err != nil {
		return fmt.Errorf("target required: --host root@<ip>, --profile <name>, KAMPODRA_PROFILE, config defaultProfile, or KAMPODRA_HOST=root@<ip>")
	}

	ctx := c.Context()
	for n := 1; ; n++ {
		raw, err := d.Runner.Run(ctx, target.HostSpec, osfacts.MetricsRemoteCmd)
		if err != nil {
			return fmt.Errorf("cannot reach VM %s (ssh failed) — nothing to report", target.HostSpec.Host)
		}
		fmt.Fprintf(d.Stdout, "[metrics] == metrics (%s) ==\n", target.HostSpec.Host)
		snap := osfacts.ParseMetrics(raw)
		fmt.Fprintln(d.Stdout, osfacts.RenderMetricsSnapshot(snap))
		verdict := osfacts.VerdictUnknown
		var pct int
		if p, ok := osfacts.RootDiskPct(snap); ok {
			pct = p
			verdict = osfacts.DiskVerdict(strconv.Itoa(p), warnDisk)
		}
		switch verdict {
		case osfacts.VerdictFail:
			fmt.Fprintf(d.Stdout, "WARNING: root disk at %d%% (>= --warn-disk %s%%) — free space first: kampodra deploy prune --dry-run\n", pct, warnDisk)
			return &exitError{code: 1}
		case osfacts.VerdictWarn:
			fmt.Fprintf(d.Stdout, "WARNING: root disk above 90%% (%d%%) — old sha-tagged deploy images pile up; reclaim: kampodra deploy prune --dry-run\n", pct)
		}
		if countN > 0 && n >= countN {
			return nil
		}
		if watchSec == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(watchSec) * time.Second):
		}
	}
}
