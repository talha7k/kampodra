package command

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/state"
)

// configHelp is the shell config.sh usage heredoc, kampodra-fied.
const configHelp = `Usage:
  kampodra config init --name <name> --host <user@ip> --ssh-key <path> [--proxy-host <host>] [--proxy-hosts <h1,h2,...>] [--group <g>] [--set-default] [--force]
  kampodra config list [--group <g>]
  kampodra config show [--profile <name>]
  kampodra config print [--profile <name>]
  kampodra config set-default <name>
  kampodra config remove <name> [--force]

Per-instance profiles (~/.kampodra/config.json, dir 0700 / file 0600).
FLAT — no inheritance; ` + "`group`" + ` is a cosmetic list filter. sshKey is stored
as a PATH only; secrets never live in the config. Resolution everywhere:
explicit flag > KAMPODRA_PROFILE env > defaultProfile. The first profile
auto-becomes the default; removing the LAST profile requires --force.
Re-init (--force) refreshes the flat fields and keeps the tooling-owned
cached init verdict unless host/ssh-key changed (the project block always
survives — config init never manages it). --proxy-hosts is kept too when
the flag is omitted: the list names every host the proxy serves, and a
casual re-init must never silently drop them.

Project shape resolution (the FULL ladder, every command):

  flags > KAMPODRA_* env > profile "project" block > kampodra.json > defaults

kampodra.json is the repo-level project manifest (the vercel.json pattern):
discovered upward from the working directory like package.json, nearest
file wins. It is COMMITTED PER-PROJECT and carries project naming only —
NEVER hosts, ssh keys, or any secret material (those stay in ~/.kampodra
profiles, 0600). A malformed manifest FAILS CLOSED.
` + "`" + `config print` + "`" + ` renders the fully resolved effective config with
per-field provenance — the debugging tool for the ladder.

Examples:
  kampodra config init --name prod --host root@203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --proxy-host app.example.com --set-default
  kampodra config init --name prod --host root@203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --proxy-host app.example.com --proxy-hosts app.example.com,www.app.example.com,api.app.example.com
  kampodra config init --name staging --host root@203.0.113.8 --ssh-key ~/.ssh/id_ed25519 --group preview
  kampodra config list                 # all profiles (default marked *)
  kampodra config list --group main    # cosmetic group filter
  kampodra config show --profile prod
  kampodra config print                # effective project config + per-field provenance
  kampodra config set-default staging
  kampodra config remove staging
  kampodra deploy --profile prod       # profiles feed every subcommand
`

func newConfigCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "per-instance profiles (~/.kampodra/config.json, 0700/0600): init | list | show | set-default | remove",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), configHelp)
			return nil
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), configHelp)
	})
	// Shared surface, declared once: persistent flags are inherited by
	// every config subcommand.
	pf := cmd.PersistentFlags()
	pf.String("name", "", "profile name to create ([A-Za-z0-9][A-Za-z0-9_-]*, max 64)")
	pf.String("host", "", "target VM (user@ip or ssh-config alias) for init")
	pf.String("ssh-key", "", "identity file PATH for init (tilde expanded; secrets never live in the config)")
	pf.String("proxy-host", "", "public TLS edge hostname for init")
	pf.String("proxy-hosts", "", "comma-separated list of every host the kamal-proxy edge serves (omitted = keep the profile's existing list)")
	pf.String("group", "", "group label — filters config list / status / deploy list --group")
	pf.String("profile", "", "profile to show")
	pf.Bool("set-default", false, "make this profile the default at init")
	pf.Bool("force", false, "overwrite an existing profile (init) / remove the last profile (remove)")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "init",
			Short: "create or --force-refresh a profile; the first one auto-becomes the default",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runConfigInit(d, c)
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "all profiles (default marked *); --group filters",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runConfigList(d, c)
			},
		},
		&cobra.Command{
			Use:   "show",
			Short: "one profile's fields (--profile <name>, else the default)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runConfigShow(d, c)
			},
		},
		&cobra.Command{
			Use:   "print",
			Short: "the fully resolved effective project config with per-field provenance (flag/env/profile/repo-file/default)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runConfigPrint(d, c)
			},
		},
		&cobra.Command{
			Use:   "set-default <name>",
			Short: "point defaultProfile at an existing profile",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				return runConfigSetDefault(d, args)
			},
		},
		&cobra.Command{
			Use:   "remove <name>",
			Short: "delete a profile (the LAST one needs --force); the default reassigns sorted-first",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				return runConfigRemove(d, c, args)
			},
		},
		&cobra.Command{
			Use:   "clone <src> <new>",
			Short: "copy a profile to a new name; --host REQUIRED (a clone targets a different instance) — --ssh-key/--proxy-host/--group override, the rest copies",
			Args:  cobra.ExactArgs(2),
			RunE: func(c *cobra.Command, args []string) error {
				return runConfigClone(d, c, args)
			},
		},
	)
	return cmd
}

// Shared flag/config readers used by the config subcommands below and by
// sibling commands in this package.

func flagString(c *cobra.Command, name string) string {
	v, _ := c.Flags().GetString(name)
	return v
}

func flagBool(c *cobra.Command, name string) bool {
	v, _ := c.Flags().GetBool(name)
	return v
}

func loadConfigForRead(d Deps) (*state.Config, error) {
	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return nil, err
	}
	if len(cfg.Profiles) == 0 {
		return nil, fmt.Errorf("no kampodra config yet — create one: kampodra config init --name <name> --host root@<ip> (--help)")
	}
	return cfg, nil
}
