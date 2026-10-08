package command

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/state"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

const sshHelp = `Usage:
  kampodra ssh [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [<cmd>...]

Host-level ssh passthrough through the profile's host/key (kampodra-native).
No command = interactive login shell; with a command the argv passes through
verbatim and ssh's exit code propagates.

Examples:
  kampodra ssh                                  # login shell on the default profile's host
  kampodra ssh --profile prod                   # login shell on that profile's host
  kampodra ssh --host root@203.0.113.10 df -h   # one-shot, argv verbatim
`

func newSSHCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh [<cmd>...]",
		Short: "host-level ssh passthrough through the profile's host/key (no command = interactive login)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			host, _ := c.Flags().GetString("host")
			key, _ := c.Flags().GetString("ssh-key")
			profile, _ := c.Flags().GetString("profile")
			cfg, err := state.LoadConfig(d.Home)
			if err != nil {
				return err
			}
			target, err := ResolveTarget(cfg, host, key, profile, d.Env)
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			sshArgs := transport.SSHPassthroughArgs(target.HostSpec, args)
			if len(args) == 0 {
				// Interactive login: forced tty, no remote command vector.
				sshArgs = transport.SSHLoginArgs(target.HostSpec)
			}
			code, err := d.Runner.Stream(c.Context(), sshArgs)
			if err != nil {
				return err
			}
			if code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	cmd.Flags().String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODINE_HOST and any profile")
	cmd.Flags().String("profile", "", "per-instance profile (~/.kampodine/config.json) — beats KAMPODINE_PROFILE / defaultProfile")
	cmd.Flags().String("ssh-key", "", "identity file — beats KAMPODINE_SSH_KEY; empty = agent / ssh config")
	// Passthrough discipline: everything after the first non-flag argument
	// belongs to the remote command — `-h`, `-p` and friends must never be
	// reinterpreted by cobra (ssh's own argv contract).
	cmd.Flags().SetInterspersed(false)
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), sshHelp)
	})
	return cmd
}
