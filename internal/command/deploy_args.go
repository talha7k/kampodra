// deploy's flag/argument grammar and target assembly: mode exclusivity,
// the positional-sha rules, the --binary positional form, the rollback
// resolution ladder, and the profile fan-out names shared with status.

package command

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

var (
	positiveIntRe = regexp.MustCompile(`^[0-9]+$`)
	nonZeroIntRe  = regexp.MustCompile(`^[1-9][0-9]*$`)
)

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
	// same grammar as --rollback's NoOptDefVal). With --rollback also
	// bare, a sha-looking positional stays the rollback target; anything
	// else names the binary block (a block named like a sha fragment
	// uses the explicit --binary=<name> form).
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
// remaining args. A sha-looking positional alongside bare --rollback
// keeps priority as the rollback target.
func applyBinaryPositional(c *cobra.Command, args []string) ([]string, error) {
	name, _ := c.Flags().GetString("binary")
	if !c.Flags().Changed("binary") || name != "-" || len(args) == 0 {
		return args, nil
	}
	if c.Flags().Changed("rollback") && shaFragmentRe.MatchString(args[0]) {
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
