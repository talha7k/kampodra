// config subcommand bodies: init | list | show | set-default | remove |
// clone | print. Wiring lives in config.go; this file holds the RunE
// implementations and their local formatters.

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
	// Fail closed BEFORE any mutation: moving an existing default to a
	// different profile via --set-default is a confirmation-worthy move
	// (the 2026-10-16 incident: an automation's --set-default silently
	// re-pointed every no-flag command at a test instance).
	if setDefault && !force && cfg.DefaultProfile != "" && cfg.DefaultProfile != name {
		return fmt.Errorf("refusing to move the default from %s to %s — pass --force to confirm", cfg.DefaultProfile, name)
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
		Cloud:     prev.Cloud,
	}
	oldDefault := cfg.DefaultProfile
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
	// The first-profile auto-adopt and a confirmed --set-default move are
	// LOUD: the banner rides after the saved line (the incident's quiet
	// "(0600) — default" footnote is exactly what failed).
	if cfg.DefaultProfile == name && oldDefault != name {
		sayDefaultBanner(d, name, host)
	}
	return nil
}

// sayDefaultBanner announces a default-profile change in no uncertain
// terms: every command without --profile now targets <name> (<host>).
func sayDefaultBanner(d Deps, name, host string) {
	fmt.Fprintf(d.Stdout, "[config] DEFAULT PROFILE IS NOW %s (%s) — every command without --profile targets it (override: --profile or KAMPODRA_PROFILE)\n", name, host)
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
	// The SAME profile ladder as print/deploy: explicit --profile >
	// KAMPODRA_PROFILE env > config defaultProfile.
	name, err := state.SelectProfileName(cfg, flagString(c, "profile"), d.Env)
	if err != nil {
		return err
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
	if len(p.Cloud) > 0 {
		fmt.Fprintf(d.Stdout, "cloud     : configured (provider-CLI auth overrides)\n")
	}
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

func runConfigSetDefault(d Deps, args []string) error {
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
	oldDefault := cfg.DefaultProfile
	cfg.DefaultProfile = target
	if err := state.SaveConfig(d.Home, cfg); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[config] default profile: %s\n", target)
	if target != oldDefault {
		sayDefaultBanner(d, target, cfg.Profiles[target].Host)
	}
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
		sayDefaultBanner(d, cfg.DefaultProfile, cfg.Profiles[cfg.DefaultProfile].Host)
	}
	return nil
}

// runConfigClone copies an existing profile to a new name with explicit
// values — the convenience of inheritance with none of its opacity: the
// new profile OWNS its fields from here on (no live link to src). --host
// is required (a clone targets a DIFFERENT instance); --ssh-key,
// --proxy-host, and --group override, everything else (including the
// project block) copies. The cached init verdict never copies — it names
// the host it was detected on.
func runConfigClone(d Deps, c *cobra.Command, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: kampodra config clone <src> <new> --host root@<ip> (--help)")
	}
	src, name := args[0], args[1]
	host := flagString(c, "host")
	key := flagString(c, "ssh-key")
	proxy := flagString(c, "proxy-host")
	group := flagString(c, "group")
	setDefault := flagBool(c, "set-default")
	force := flagBool(c, "force")

	if host == "" {
		return fmt.Errorf("--host is required — a clone targets a DIFFERENT instance (to re-point an existing name: config init <name> --force)")
	}
	if !state.ValidProfileName(name) {
		return fmt.Errorf("invalid profile name '%s' — use [A-Za-z0-9][A-Za-z0-9_-]* (max 64)", name)
	}
	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	prevSrc, ok := cfg.Profiles[src]
	if !ok {
		return fmt.Errorf("unknown profile '%s' — known: %s", src, strings.Join(sortedProfileNames(cfg), " "))
	}
	if _, exists := cfg.Profiles[name]; exists && !force {
		return fmt.Errorf("profile '%s' already exists — pass --force to overwrite", name)
	}
	for flag, val := range map[string]string{"--host": host, "--ssh-key": key, "--proxy-host": proxy, "--group": group} {
		if val == "" {
			continue // not provided — the src value (if any) was already validated at its own init/clone time
		}
		if err := state.RejectConfigValue(val, flag); err != nil {
			return err
		}
	}
	keyAbs := prevSrc.SSHKey
	if key != "" {
		keyAbs = state.ExpandTilde(key, d.Home)
		if _, err := os.Stat(keyAbs); err != nil {
			fmt.Fprintf(d.Stdout, "[config] note: ssh key file does not exist (yet): %s — stored as a path, deploy will fail until it does\n", keyAbs)
		}
	}
	if proxy == "" {
		proxy = prevSrc.ProxyHost
	}
	if group == "" {
		group = prevSrc.Group
	}
	cfg.Profiles[name] = state.Profile{
		Host:      host,
		SSHKey:    keyAbs,
		ProxyHost: proxy,
		Group:     group,
		Project:   prevSrc.Project,
		Cloud:     prevSrc.Cloud,
	}
	if setDefault || cfg.DefaultProfile == "" {
		cfg.DefaultProfile = name
	}
	if err := state.SaveConfig(d.Home, cfg); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[config] cloned '%s' -> '%s' (host %s) -> %s (0600)\n", src, name, host, state.ConfigPath(d.Home))
	if cfg.DefaultProfile == name {
		fmt.Fprintf(d.Stdout, "[config] '%s' is now the default\n", name)
	} else {
		fmt.Fprintf(d.Stdout, "[config] make default later: kampodra config set-default %s\n", name)
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
