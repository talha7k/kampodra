package command

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// configHelp is the shell config.sh usage heredoc, kampodra-fied.
const configHelp = `Usage:
  kampodra config init --name <name> --host <user@ip> --ssh-key <path> [--proxy-host <host>] [--group <g>] [--set-default] [--force]
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
survives — config init never manages it).

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
	// Shared surface, declared once (the frozen spec parses flags flat per
	// script; the parity snapshot takes the union either way).
	pf := cmd.PersistentFlags()
	pf.String("name", "", "profile name to create ([A-Za-z0-9][A-Za-z0-9_-]*, max 64)")
	pf.String("host", "", "target VM (user@ip or ssh-config alias) for init")
	pf.String("ssh-key", "", "identity file PATH for init (tilde expanded; secrets never live in the config)")
	pf.String("proxy-host", "", "public TLS edge hostname for init")
	pf.String("group", "", "cosmetic group (list --group filter)")
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
				return runConfigSetDefault(d, c, args)
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
	)
	return cmd
}

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

func runConfigInit(d Deps, c *cobra.Command) error {
	name := flagString(c, "name")
	host := flagString(c, "host")
	key := flagString(c, "ssh-key")
	proxy := flagString(c, "proxy-host")
	group := flagString(c, "group")
	setDefault := flagBool(c, "set-default")
	force := flagBool(c, "force")

	if name == "" {
		return fmt.Errorf("--name is required (--help)")
	}
	if !state.ValidProfileName(name) {
		return fmt.Errorf("invalid --name '%s' — use [A-Za-z0-9][A-Za-z0-9_-]* (max 64)", name)
	}
	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	if _, exists := cfg.Profiles[name]; exists && !force {
		return fmt.Errorf("profile '%s' already exists — pass --force to overwrite (re-init also refreshes fields; the cached init field is kept unless you change host/ssh-key)", name)
	}
	if err := state.RejectConfigValue(host, "--host"); err != nil {
		return err
	}
	if err := state.RejectConfigValue(key, "--ssh-key"); err != nil {
		return err
	}
	if proxy != "" {
		if err := state.RejectConfigValue(proxy, "--proxy-host"); err != nil {
			return err
		}
	}
	if group != "" {
		if err := state.RejectConfigValue(group, "--group"); err != nil {
			return err
		}
	}
	keyAbs := state.ExpandTilde(key, d.Home)
	if _, err := os.Stat(keyAbs); err != nil {
		fmt.Fprintf(d.Stdout, "[config] note: ssh key file does not exist (yet): %s — stored as a path, deploy will fail until it does\n", keyAbs)
	}

	// Upsert + default rules: explicit --set-default wins; a config without
	// any default adopts this profile. The tooling-owned fields survive:
	// the cached init verdict stays while host/ssh-key are unchanged (it
	// names the host it was detected on), and the project block always
	// survives (config init never manages it).
	prev := cfg.Profiles[name]
	initCached := prev.Init
	if prev.Host != host || prev.SSHKey != keyAbs {
		initCached = ""
	}
	cfg.Profiles[name] = state.Profile{
		Host:      host,
		SSHKey:    keyAbs,
		ProxyHost: proxy,
		Group:     group,
		Init:      initCached,
		Project:   prev.Project,
	}
	if setDefault || cfg.DefaultProfile == "" {
		cfg.DefaultProfile = name
	}
	if err := state.SaveConfig(d.Home, cfg); err != nil {
		return err
	}
	path := state.ConfigPath(d.Home)
	if cfg.DefaultProfile == name {
		fmt.Fprintf(d.Stdout, "[config] saved profile '%s' -> %s (0600) — default\n", name, path)
	} else {
		fmt.Fprintf(d.Stdout, "[config] saved profile '%s' -> %s (0600)\n", name, path)
		fmt.Fprintf(d.Stdout, "[config] make default later: kampodra config set-default %s\n", name)
	}
	return nil
}

func runConfigList(d Deps, c *cobra.Command) error {
	group := flagString(c, "group")
	cfg, err := loadConfigForRead(d)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Fprintf(d.Stdout, "%-12s %-10s %-8s %-26s %-30s %-26s\n", "NAME", "GROUP", "INIT", "HOST", "SSH-KEY (path)", "PROXY-HOST")
	rendered := 0
	for _, n := range names {
		p := cfg.Profiles[n]
		if group != "" && p.Group != group {
			continue
		}
		rendered++
		marker := ""
		if n == cfg.DefaultProfile {
			marker = "  * default"
		}
		fmt.Fprintf(d.Stdout, "%-12s %-10s %-8s %-26s %-30s %-26s%s\n",
			n, dashEmpty(p.Group), dashEmpty(p.Init), dashEmpty(p.Host), dashEmpty(p.SSHKey), dashEmpty(p.ProxyHost), marker)
	}
	if rendered == 0 {
		if group != "" {
			fmt.Fprintf(d.Stdout, "[config] (no profiles in group '%s')\n", group)
		} else {
			fmt.Fprintln(d.Stdout, "[config] (no profiles)")
		}
	}
	return nil
}

func dashEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func runConfigShow(d Deps, c *cobra.Command) error {
	cfg, err := loadConfigForRead(d)
	if err != nil {
		return err
	}
	name := flagString(c, "profile")
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name == "" {
		return fmt.Errorf("no default profile — pass --profile <name> (known: %s)", strings.Join(sortedProfileNames(cfg), " "))
	}
	p, err := cfg.Profile(name)
	if err != nil {
		return fmt.Errorf("unknown profile '%s' — known: %s", name, strings.Join(sortedProfileNames(cfg), " "))
	}
	label := name
	if name == cfg.DefaultProfile {
		label = name + " (default)"
	}
	fmt.Fprintf(d.Stdout, "[config] profile: %s\n", label)
	fmt.Fprintf(d.Stdout, "host      : %s\n", p.Host)
	fmt.Fprintf(d.Stdout, "sshKey    : %s   # path only — secrets never live in the config\n", p.SSHKey)
	fmt.Fprintf(d.Stdout, "proxyHost : %s\n", p.ProxyHost)
	fmt.Fprintf(d.Stdout, "group     : %s\n", p.Group)
	if p.Init != "" {
		fmt.Fprintf(d.Stdout, "init      : %s   # cached by host auto-detection\n", p.Init)
	}
	return nil
}

func sortedProfileNames(cfg *state.Config) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func runConfigSetDefault(d Deps, c *cobra.Command, args []string) error {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	if target == "" {
		return fmt.Errorf("usage: kampodra config set-default <name>")
	}
	cfg, err := loadConfigForRead(d)
	if err != nil {
		return err
	}
	if _, err := cfg.Profile(target); err != nil {
		return fmt.Errorf("unknown profile '%s' — known: %s", target, strings.Join(sortedProfileNames(cfg), " "))
	}
	cfg.DefaultProfile = target
	if err := state.SaveConfig(d.Home, cfg); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[config] default profile: %s\n", target)
	return nil
}

func runConfigRemove(d Deps, c *cobra.Command, args []string) error {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	if target == "" {
		return fmt.Errorf("usage: kampodra config remove <name> [--force]")
	}
	force := flagBool(c, "force")
	path := state.ConfigPath(d.Home)
	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	if len(cfg.Profiles) == 0 {
		return fmt.Errorf("no kampodra config at %s — nothing to remove", path)
	}
	if _, err := cfg.Profile(target); err != nil {
		return fmt.Errorf("unknown profile '%s' — known: %s", target, strings.Join(sortedProfileNames(cfg), " "))
	}
	if len(cfg.Profiles) == 1 && !force {
		return fmt.Errorf("refusing to remove the last profile ('%s') — pass --force if you really mean it", target)
	}
	delete(cfg.Profiles, target)
	if cfg.DefaultProfile == target {
		names := sortedProfileNames(cfg)
		cfg.DefaultProfile = ""
		if len(names) > 0 {
			cfg.DefaultProfile = names[0]
		}
	}
	if err := state.SaveConfig(d.Home, cfg); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[config] removed profile '%s' (%d remaining)\n", target, len(cfg.Profiles))
	if cfg.DefaultProfile != "" && cfg.DefaultProfile != target {
		fmt.Fprintf(d.Stdout, "[config] default profile is now: %s\n", cfg.DefaultProfile)
	}
	return nil
}

// provenanceTag renders one field's winning layer for config print: the
// source plus, where meaningful, exactly where it came from.
func provenanceTag(tr project.FieldTrace) string {
	switch tr.Source {
	case project.SourceEnv:
		return string(tr.Source) + " (" + tr.Origin + ")"
	case project.SourceRepoFile:
		return string(tr.Source) + " (" + tr.Origin + ")"
	case project.SourceProfile:
		return string(project.SourceProfile)
	case project.SourceFlag:
		return string(project.SourceFlag)
	default:
		return string(project.SourceDefault)
	}
}

// runConfigPrint renders the fully resolved effective project config with
// per-field provenance — the debugging tool for the resolution ladder.
func runConfigPrint(d Deps, c *cobra.Command) error {
	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	profileFlag, _ := c.Flags().GetString("profile")
	name, err := state.SelectProfileName(cfg, profileFlag, d.Env)
	if err != nil {
		return err
	}
	var profile state.Profile
	if name != "" {
		if profile, err = cfg.Profile(name); err != nil {
			return err
		}
	}

	// Fail closed on a malformed manifest BEFORE rendering anything.
	mf, err := d.manifestFor()
	if err != nil {
		return err
	}

	pc, traces := project.ResolveTraced(nil, d.Env, profile.Project, mf)

	label := "<none>"
	if name != "" {
		label = name
		if name == cfg.DefaultProfile {
			label = name + " (default)"
		}
	}
	if mf != nil {
		fmt.Fprintf(d.Stdout, "[config] effective project config (profile: %s, manifest: %s)\n", label, mf.Path)
	} else {
		fmt.Fprintf(d.Stdout, "[config] effective project config (profile: %s, no kampodra.json in this tree)\n", label)
	}
	for _, v := range project.FieldViews(pc, traces) {
		fmt.Fprintf(d.Stdout, "%-16s %-30s (%s)\n", v.Key, v.Value, provenanceTag(v.Trace))
	}
	return nil
}
