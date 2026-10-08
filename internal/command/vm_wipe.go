package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	initadapter "github.com/talha7k/kampodra/internal/adapter/init"
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// vmWipeHelp — vm-wipe is kampodra-NATIVE (beyond the frozen shell spec;
// the parity guard review-flags it). It exists so a full fresh-VM rebuild
// can run entirely through kampodra: wipe first, vm-prepare after.
const vmWipeHelp = `Usage:
  kampodra vm-wipe --yes [--keep-data] [--force] [--host root@<ip>] [--profile <name>] [--ssh-key <path>]

kampodra-native: tear the project stack DOWN on the target VM — stop +
disable every project service, remove the project containers, prune the
image store, and delete the env file / deployed-sha stamp / state dirs.
DESTRUCTIVE: --yes gates every mutation (scripts never prompt).
  --keep-data   preserve the tenant data dir (everything else goes)
  --force       override the services-mismatch refusal (wrong host?)

Examples:
  kampodra vm-wipe --profile prod --yes              # full teardown
  kampodra vm-wipe --profile prod --yes --keep-data  # keep tenant data
  kampodra vm-wipe --profile prod --yes --force      # unmatched host, mean it
`

// proxyContainer is the TLS edge container (infra constant — the same name
// deploy_pipeline re-points; not project naming).
const proxyContainer = "kamal-proxy"

func newVMWipeCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm-wipe --yes [--keep-data] [--force]",
		Short: "kampodra-native teardown: stop+disable services, remove project containers, prune images, delete env/stamps/state (DESTRUCTIVE, --yes gated)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runVMWipe(d, c)
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), vmWipeHelp)
	})
	cmd.Flags().String("host", "", "target VM (user@ip or ssh-config alias)")
	cmd.Flags().String("profile", "", "per-instance profile (~/.kampodra/config.json)")
	cmd.Flags().String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY")
	cmd.Flags().Bool("yes", false, "confirm the teardown (required; scripts never prompt)")
	cmd.Flags().Bool("keep-data", false, "preserve the tenant data dir")
	cmd.Flags().Bool("force", false, "override the services-mismatch refusal")
	return cmd
}

// projectContainerNames is the container set vm-wipe removes: the app
// container, its rolling-deploy shadow, and the TLS edge.
func projectContainerNames(pj projectConfigShape) []string {
	return []string{pj.Container, pj.Container + pj.ShadowSuffix, proxyContainer}
}

type projectConfigShape = project.Config

func runVMWipe(d Deps, c *cobra.Command) error {
	if !flagBool(c, "yes") {
		return fmt.Errorf("vm-wipe is DESTRUCTIVE (stops services, removes containers + images, deletes env/stamps/state) — pass --yes to confirm")
	}
	keepData := flagBool(c, "keep-data")
	force := flagBool(c, "force")

	host, _ := c.Flags().GetString("host")
	key, _ := c.Flags().GetString("ssh-key")
	profile, _ := c.Flags().GetString("profile")
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
		return err
	}

	ctx := c.Context()
	pj := target.Project
	run := func(remote string) (string, error) {
		return d.Runner.Run(ctx, target.HostSpec, remote)
	}
	runTolerant := func(remote string) {
		if _, err := run(remote); err != nil {
			fmt.Fprintf(d.Stdout, "[vm-wipe] (tolerated failure: %s — %v)\n", remote, err)
		}
	}

	// --- match check: never tear down a host that clearly isn't ours ------
	initSys, _, _ := initadapter.Detect(ctx, func(_ context.Context, remote string) (string, error) {
		return run(remote)
	}, target.ProfileInit)
	containerOut, _ := run("podman ps -a --format '{{.Names}}'")
	containers := splitLines(containerOut)
	projectSet := map[string]bool{}
	for _, name := range projectContainerNames(pj) {
		projectSet[name] = true
	}
	matchedContainer := false
	for _, name := range containers {
		if projectSet[name] {
			matchedContainer = true
		}
	}
	matchedService := false
	states, _ := initadapter.ServiceStates(ctx, func(_ context.Context, remote string) (string, error) {
		return run(remote)
	}, initSys, pj.Services)
	for _, s := range states {
		if s.Running {
			matchedService = true
		}
	}
	if !force && !matchedContainer && !matchedService {
		return fmt.Errorf("no kampodra services found on %s — refusing to wipe (wrong host, or wrong profile? use a matching --profile, or --force to mean it)", target.HostSpec.Host)
	}

	fmt.Fprintf(d.Stdout, "[vm-wipe] tearing down %s (profile: %s)\n", target.HostSpec.Host, target.ProfileName)
	var removed []string

	// --- 1. stop + disable every project service (init-aware) -------------
	for _, svc := range pj.Services {
		if stopCmd, err := initadapter.ActionCommand(initSys, svc, "stop"); err == nil {
			runTolerant(stopCmd)
			removed = append(removed, svc+" (service stopped)")
		}
		switch initSys {
		case initadapter.SystemOpenRC:
			runTolerant("rc-update del " + svc + " default")
		case initadapter.SystemSystemd:
			runTolerant("systemctl disable " + svc)
		}
		removed = append(removed, svc+" (service disabled)")
	}

	// --- 2. remove the project containers (present ones only) -------------
	for _, name := range projectContainerNames(pj) {
		if containsString(containers, name) {
			runTolerant("podman rm -f " + name)
			removed = append(removed, name+" (container)")
		}
	}

	// --- 3. prune the image store (everything unreferenced is dust now) ---
	runTolerant("podman image prune -a -f")
	removed = append(removed, "podman images (pruned)")

	// --- 4. remove env file, stamps, state dirs; data dir last ------------
	envDir := pathDir(pj.EnvFilePath)
	runTolerant("rm -f " + pj.EnvFilePath)
	runTolerant("rm -f " + pj.DeployedShaFile)
	runTolerant("rm -f " + envDir + "/anchor.conf")
	runTolerant("rm -rf " + envDir)
	removed = append(removed, pj.EnvFilePath+" (env file)", pj.DeployedShaFile+" (stamp)", envDir+" (state dir)")
	if keepData {
		fmt.Fprintf(d.Stdout, "[vm-wipe] kept: %s (tenant data — --keep-data)\n", pj.DataDir)
	} else {
		runTolerant("rm -rf " + pj.DataDir)
		removed = append(removed, pj.DataDir+" (tenant data dir)")
	}

	for _, r := range removed {
		fmt.Fprintf(d.Stdout, "[vm-wipe] removed: %s\n", r)
	}
	fmt.Fprintf(d.Stdout, "[vm-wipe] DONE — %s is clean; rebuild with: kampodra vm-prepare --profile %s\n",
		target.HostSpec.Host, target.ProfileName)
	return nil
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// pathDir is filepath.Dir without importing path/filepath into the command
// layer's hot path — remote paths are always slash-separated.
func pathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}
