package command

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
	"github.com/talha7k/kampodra/internal/adapter/transport"
	"github.com/talha7k/kampodra/internal/adapter/vmbootstrap"
)

// vmPrepareHelp — the Alpine host bootstrap (vm-prepare.sh port). The VM
// goes to production from its first boot: no blue->green flip, no cert
// carry; the first TLS certificate issues during the first deploy.
const vmPrepareHelp = `Usage:
  kampodra vm-prepare --host root@<new-ip> [--pull-images] [--ssh-key <path>] [--profile <name>] [--ansible <playbook>]

First-run bootstrap of a bare Alpine VM (idempotent — safe to re-run):
  1. sanity gates (UEFI boot, no systemd anywhere, OpenRC tooling)
  2. sshd hardening ensured (drop-in + Include + restart, then gated)
  3. apk community repo + the podman stack (netavark backend)
  4. the project state dir + /etc/containers/registries.conf (insecure registry)
  5. sysctl net.ipv4.ip_unprivileged_port_start=80 (persisted + live)
  6. OpenRC services: the api container + kamal-proxy (supervise-daemon),
     rc-update'd into the default runlevel — that IS boot survival
  7. kampodra-anchor: the blue/green flip's guest half (inert without the conf)
  8. kamal-proxy image pulled + service UP (first deploy issues fresh ACME TLS)

The api container is NOT started on a fresh VM: neither its image nor the
env file exists yet — the FIRST DEPLOY provides both.

--pull-images pre-pulls the app image directly from the ImagePrefix registry
(kampodra itself streams images on deploy, so this only matters when a
registry actually serves that prefix — kampodine's Mac-local tunnel is gone).
--ansible <playbook> runs ansible-playbook against the host AFTER bootstrap
(inventory derived from --host, --private-key from the resolved ssh key).

Host/key resolution: --host | --profile <name> | KAMPODRA_PROFILE |
config defaultProfile; --ssh-key | profile sshKey | KAMPODRA_SSH_KEY.
`

// waitPolicy is the retry shape of the two readiness loops (shrunk by the
// internal test; never by production code paths).
type waitPolicy struct {
	attempts int
	interval time.Duration
}

var (
	vmPrepareSSHWait   = waitPolicy{attempts: 60, interval: 5 * time.Second}
	vmPrepareProxyWait = waitPolicy{attempts: 20, interval: 3 * time.Second}
)

func newVMPrepareCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm-prepare --host root@<new-ip> [--pull-images] [--ansible <playbook>]",
		Short: "first-run bootstrap of a bare Alpine host: gates, sshd hardening, podman stack, OpenRC services, kamal-proxy edge, anchor watcher",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runVMPrepare(d, c)
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), vmPrepareHelp)
	})
	cmd.Flags().String("host", "", "target VM (user@ip or ssh-config alias)")
	cmd.Flags().String("profile", "", "per-instance profile (~/.kampodra/config.json)")
	cmd.Flags().String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY")
	cmd.Flags().Bool("pull-images", false, "pre-pull the app image from the ImagePrefix registry after bootstrap")
	cmd.Flags().String("ansible", "", "run this ansible-playbook file against the host after bootstrap")
	return cmd
}

// namingFromProject derives the bootstrap naming from the resolved project
// config (the adapter never imports the project adapter sideways).
func namingFromProject(pj project.Config) vmbootstrap.Naming {
	envDir := pj.EnvFilePath
	if i := strings.LastIndex(pj.EnvFilePath, "/"); i > 0 {
		envDir = pj.EnvFilePath[:i]
	}
	return vmbootstrap.Naming{
		Container:       pj.Container,
		EnvFilePath:     pj.EnvFilePath,
		EnvDir:          envDir,
		AnchorConf:      envDir + "/anchor.conf",
		DataDir:         pj.DataDir,
		ImagePrefix:     pj.ImagePrefix,
		Registry:        imageRegistryHost(pj.ImagePrefix),
		ImageRef:        pj.ImagePrefix + ":latest",
		Port:            pj.Port,
		Network:         pj.Network,
		DeployedShaFile: pj.DeployedShaFile,
	}
}

// imageRegistryHost strips the path off a repo reference (the ImagePrefix's
// host:port — e.g. "127.0.0.1:5000/repo-name" -> "127.0.0.1:5000").
func imageRegistryHost(imagePrefix string) string {
	if i := strings.Index(imagePrefix, "/"); i > 0 {
		return imagePrefix[:i]
	}
	return imagePrefix
}

func runVMPrepare(d Deps, c *cobra.Command) error {
	host, _ := c.Flags().GetString("host")
	key, _ := c.Flags().GetString("ssh-key")
	profile, _ := c.Flags().GetString("profile")
	doPull, _ := c.Flags().GetBool("pull-images")
	ansiblePath, _ := c.Flags().GetString("ansible")

	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	target, err := ResolveTarget(cfg, host, key, profile, d.Env)
	if err != nil {
		return err
	}
	if err := requireHost(target); err != nil {
		return fmt.Errorf("--host root@<ip> required (--profile <name> or KAMPODRA_HOST also work; --help for usage)")
	}
	// First contact: the bootstrap leg accepts unknown host keys.
	target.HostSpec.AcceptNewHostKey = true

	ctx := c.Context()
	n := namingFromProject(target.Project)
	say := func(format string, args ...any) {
		fmt.Fprintf(d.Stdout, "[vm-prepare] "+format+"\n", args...)
	}
	run := func(remote string) error {
		_, err := d.Runner.Run(ctx, target.HostSpec, remote)
		return err
	}
	runSh := func(snippet string) error {
		_, err := d.Runner.RunWithStdin(ctx, target.HostSpec, "sh -s", strings.NewReader(snippet))
		return err
	}
	upload := func(path, content string, mode string) error {
		if _, err := d.Runner.RunWithStdin(ctx, target.HostSpec, "umask 077; cat > "+path, strings.NewReader(content)); err != nil {
			return fmt.Errorf("upload %s: %w", path, err)
		}
		if mode != "" {
			return run("chmod " + mode + " " + path)
		}
		return nil
	}
	die := func(context string, err error) error {
		return fmt.Errorf("%s: %w", context, err)
	}

	// --- 0. connectivity ------------------------------------------------------
	say("waiting for ssh on %s…", target.HostSpec.Host)
	if err := waitForSSH(ctx, d.Runner, target.HostSpec, vmPrepareSSHWait); err != nil {
		return err
	}

	// --- 1. pre gates (fail BEFORE mutating anything) -------------------------
	say("gate: UEFI boot")
	if err := run(vmbootstrap.GateUEFI); err != nil {
		return die("not booted via UEFI (golden image is UEFI-only — wrong machine/image?)", err)
	}
	say("gate: OpenRC init + tooling")
	if err := run(vmbootstrap.GateNoSystemd); err != nil {
		return die("systemd is PID1 — this is not the Alpine golden image (start-fresh has NO systemd anywhere)", err)
	}
	if err := run(vmbootstrap.GateOpenRCTooling); err != nil {
		return die("OpenRC tooling missing or not operational (openrc/rc-service/rc-update/supervise-daemon, rc-status)", err)
	}

	say("sshd hardening: ensure drop-in + Include + restart…")
	if err := run(`grep -q "^Include /etc/ssh/sshd_config.d/*.conf" /etc/ssh/sshd_config || sed -i "1i Include /etc/ssh/sshd_config.d/*.conf" /etc/ssh/sshd_config`); err != nil {
		return die("could not ensure the sshd_config Include line", err)
	}
	if err := run("mkdir -p /etc/ssh/sshd_config.d && chmod 700 /etc/ssh/sshd_config.d"); err != nil {
		return die("could not create /etc/ssh/sshd_config.d", err)
	}
	if err := upload(vmbootstrap.SSHDHardeningPath, vmbootstrap.RenderSSHDHardening(), ""); err != nil {
		return err
	}
	if err := run("rc-service sshd restart"); err != nil {
		return die("sshd restart failed after hardening ensure", err)
	}
	say("gate: sshd hardening effective")
	if err := run(vmbootstrap.GateSSHDHardened); err != nil {
		return die("sshd still allows passwords (hardening ensure failed?)", err)
	}

	// --- 2. apk repositories + podman stack -----------------------------------
	say("ensuring the community repo (podman stack lives there)…")
	if err := runSh(vmbootstrap.CommunityRepoSnippet()); err != nil {
		return die("community repo enable / apk update failed", err)
	}
	if err := run(`grep -Eq "^[^#].*/community" /etc/apk/repositories`); err != nil {
		return die("community repo could not be enabled (/etc/apk/repositories)", err)
	}
	say("installing the podman stack (idempotent)…")
	if err := runSh(vmbootstrap.PodmanStackSnippet()); err != nil {
		return die("podman stack install failed (apk output above)", err)
	}
	if err := runSh(vmbootstrap.PodstackGateSnippet()); err != nil {
		return die("podman stack install failed", err)
	}
	if err := run(`rc-update show boot | grep -q cgroups || rc-update add cgroups boot`); err != nil {
		return die("rc-update cgroups failed", err)
	}
	if err := run(`rc-service cgroups status >/dev/null 2>&1 || rc-service cgroups start`); err != nil {
		return die("cgroups start failed", err)
	}

	// --- 3. state dir + managed config files -----------------------------------
	say("creating %s (the env file lands here via kampodra env push, 0600 root)…", n.EnvDir)
	if err := run("mkdir -p " + n.EnvDir + " && chmod 700 " + n.EnvDir); err != nil {
		return die("state dir create failed", err)
	}
	say("writing /etc/containers/registries.conf (insecure %s; search docker.io)…", n.Registry)
	if err := run("mkdir -p /etc/containers"); err != nil {
		return die("mkdir /etc/containers failed", err)
	}
	if err := upload(vmbootstrap.RegistriesConfPath, vmbootstrap.RenderRegistriesConf(n), ""); err != nil {
		return err
	}
	say("writing %s + applying live…", vmbootstrap.SysctlConfPath)
	if err := upload(vmbootstrap.SysctlConfPath, vmbootstrap.RenderSysctlConf(), ""); err != nil {
		return err
	}
	if err := runSh(vmbootstrap.SysctlApplySnippet()); err != nil {
		return die("sysctl ensure/apply failed", err)
	}

	// --- 4. OpenRC services -----------------------------------------------------
	say("ensuring the '%s' podman network (the proxy re-point resolves %s:%s by network DNS)…", n.Network, n.Container, n.Port)
	if err := run(fmt.Sprintf("podman network exists %s 2>/dev/null || podman network create %s", n.Network, n.Network)); err != nil {
		return die(fmt.Sprintf("podman network create %s failed", n.Network), err)
	}
	say("installing OpenRC services %s + %s (supervise-daemon around podman run)…", n.Container, vmbootstrap.ProxyContainer)
	apiInitD := "/etc/init.d/" + n.Container
	if err := upload(apiInitD, vmbootstrap.RenderInitDAPI(n), ""); err != nil {
		return err
	}
	if err := upload(vmbootstrap.ProxyInitDPath, vmbootstrap.RenderInitDProxy(n), ""); err != nil {
		return err
	}
	if err := run("chmod 755 " + apiInitD + " " + vmbootstrap.ProxyInitDPath); err != nil {
		return die("chmod init.d units failed", err)
	}

	// --- 4b. the anchor watcher (blue/green flip guest half) --------------------
	say("installing the kampodra-anchor watcher service (blue-green flip guest half)…")
	if err := run("mkdir -p /usr/local/sbin"); err != nil {
		return die("mkdir /usr/local/sbin failed", err)
	}
	if err := upload(vmbootstrap.AnchorScriptPath, vmbootstrap.RenderAnchorScript(n), ""); err != nil {
		return err
	}
	anchorInitD := "/etc/init.d/kampodra-anchor"
	if err := upload(anchorInitD, vmbootstrap.RenderInitDAnchor(n), ""); err != nil {
		return err
	}
	if err := run("chmod 755 " + vmbootstrap.AnchorScriptPath + " " + anchorInitD); err != nil {
		return die("chmod kampodra-anchor files failed", err)
	}
	if err := run("sh -n " + vmbootstrap.AnchorScriptPath); err != nil {
		return die(vmbootstrap.AnchorScriptPath+" does not parse (busybox sh)", err)
	}
	if err := run("sh -n " + anchorInitD); err != nil {
		return die(anchorInitD+" does not parse", err)
	}
	if err := run(`rc-update show default | grep -qE "^[[:space:]]*kampodra-anchor[[:space:]]*\|" || rc-update add kampodra-anchor default`); err != nil {
		return die("rc-update add kampodra-anchor default failed", err)
	}
	if err := run("rc-service kampodra-anchor start"); err != nil {
		return die("rc-service kampodra-anchor start failed", err)
	}

	for _, svc := range []string{n.Container, vmbootstrap.ProxyContainer} {
		if err := run(fmt.Sprintf(`rc-update show default | grep -qE "^[[:space:]]*%[1]s[[:space:]]*\|" || rc-update add %[1]s default`, svc)); err != nil {
			return die(fmt.Sprintf("rc-update add %s default failed", svc), err)
		}
	}

	// Warn (never auto-restart) if a rewrite changed a unit of a RUNNING
	// service — re-runs after a deploy must not bounce production.
	_ = runSh(vmbootstrap.UnitDriftSnippet())

	// --- 5. kamal-proxy up ------------------------------------------------------
	say("pulling kamal-proxy image + starting the service…")
	pullProxy := fmt.Sprintf("podman image exists %s || podman pull %s", vmbootstrap.ProxyImageRef, vmbootstrap.ProxyImageRef)
	if err := run(pullProxy); err != nil {
		return die("kamal-proxy image pull failed (docker.io reachable from the VM?)", err)
	}
	if err := run("rc-service " + vmbootstrap.ProxyContainer + " start"); err != nil {
		return die("rc-service kamal-proxy start failed", err)
	}
	proxyPS := fmt.Sprintf(`podman ps --format "{{.Names}}" | grep -qx %s`, vmbootstrap.ProxyContainer)
	if err := waitUntil(ctx, d.Runner, target.HostSpec, vmPrepareProxyWait, proxyPS,
		"kamal-proxy container never came up (podman logs kamal-proxy; rc-service kamal-proxy status)"); err != nil {
		return err
	}

	// --- 6. optional: pre-pull the app image ------------------------------------
	if doPull {
		say("pre-pulling the app image (%s) — needs a registry actually serving that prefix…", n.ImageRef)
		if err := run(fmt.Sprintf("podman pull --tls-verify=false %s", n.ImageRef)); err != nil {
			return die("app image pull failed", err)
		}
	} else {
		say("skipping app-image pre-pull (pass --pull-images, or let the first deploy pull)")
	}

	// --- 7. converge the api ONLY if a previous deploy left image + env ---------
	say("%s start check (needs image + %s — first deploy provides both)…", n.Container, n.EnvFilePath)
	if err := runSh(vmbootstrap.APIConvergeSnippet(n)); err != nil {
		return die(fmt.Sprintf("%s start failed (image + env present — investigate: podman logs %s)", n.Container, n.Container), err)
	}

	// --- 8. post gates (readiness, fail-closed) ---------------------------------
	say("gate: podman >= 4.9 + netavark backend")
	if err := run(vmbootstrap.GatePodmanVersion); err != nil {
		return die("podman too old (need >= 4.9 for netavark)", err)
	}
	if err := run(vmbootstrap.GateNetavark); err != nil {
		return die("network backend is not netavark", err)
	}
	say("gate: sysctl effective")
	if err := run(vmbootstrap.GateSysctl); err != nil {
		return die("net.ipv4.ip_unprivileged_port_start is not 80", err)
	}
	say("gate: registries.conf effective")
	if err := run(vmbootstrap.RegistriesGate(n)); err != nil {
		return die("registries.conf incomplete ("+n.Registry+" insecure + docker.io search)", err)
	}
	say("gate: services enabled in the default runlevel")
	for _, svc := range []string{n.Container, vmbootstrap.ProxyContainer} {
		if err := run(vmbootstrap.RunlevelGate(svc)); err != nil {
			return die(svc+" not in the default runlevel", err)
		}
	}
	if err := run("rc-service " + vmbootstrap.ProxyContainer + " status >/dev/null"); err != nil {
		return die("kamal-proxy service not started", err)
	}
	say("gate: kampodra-anchor watcher running + INERT (no anchor conf on a fresh VM)")
	if err := run(vmbootstrap.RunlevelGate("kampodra-anchor")); err != nil {
		return die("kampodra-anchor not in the default runlevel", err)
	}
	if err := run("rc-service kampodra-anchor status >/dev/null"); err != nil {
		return die("kampodra-anchor service not started", err)
	}
	if err := run("! test -e " + n.AnchorConf); err != nil {
		return die(n.AnchorConf+" already exists on a fresh VM (wrong machine?)", err)
	}
	if err := run(vmbootstrap.GateAnchorIdleLog); err != nil {
		return die("kampodra-anchor is started but never logged its idle transition (watcher loop not running?)", err)
	}

	// --- summary ------------------------------------------------------------------
	ip := target.HostSpec.Host
	if i := strings.LastIndex(target.HostSpec.Host, "@"); i >= 0 {
		ip = target.HostSpec.Host[i+1:]
	}
	say("PREPARED: %s passes all gates (OpenRC, no systemd anywhere).", target.HostSpec.Host)
	say("next — first deploy (fresh ACME TLS issued during it):")
	say("  PROXY_HOST=%s kampodra deploy --host %s", vmbootstrap.SuggestedProxyHost(ip), target.HostSpec.Host)
	say("  (DNS must point at %s first — <ip-dashes>.sslip.io resolves automatically)", ip)

	// --- 9. optional ansible converge ---------------------------------------------
	if ansiblePath != "" {
		return runAnsiblePlaybook(d, target, ansiblePath)
	}
	return nil
}

// waitForSSH polls the host until `true` succeeds (vm-prepare's readiness
// loop: 60 × 5s against a booting VM).
func waitForSSH(ctx context.Context, runner transport.Runner, spec transport.HostSpec, p waitPolicy) error {
	return waitUntil(ctx, runner, spec, p, "true", fmt.Sprintf("no ssh after %d attempts (firewall 22 rule for your IP? baked ops key in the agent?)", p.attempts))
}

func waitUntil(ctx context.Context, runner transport.Runner, spec transport.HostSpec, p waitPolicy, remote, failMsg string) error {
	for i := 0; i < p.attempts; i++ {
		if _, err := runner.Run(ctx, spec, remote); err == nil {
			return nil
		}
		if i < p.attempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.interval):
			}
		}
	}
	return fmt.Errorf("%s", failMsg)
}

// runAnsiblePlaybook runs the operator's playbook against the freshly
// bootstrapped host (the shell's --refresh-config shape: inline inventory
// + --private-key). Output streams; failure is fatal (explicit flag = intent).
func runAnsiblePlaybook(d Deps, target Target, playbookPath string) error {
	if _, err := os.Stat(playbookPath); err != nil {
		return fmt.Errorf("ansible playbook not found: %s", playbookPath)
	}
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		return fmt.Errorf("ansible-playbook not found on PATH — install ansible to use --ansible")
	}
	user, host := target.HostSpec.Host, target.HostSpec.Host
	if i := strings.LastIndex(target.HostSpec.Host, "@"); i >= 0 {
		user = target.HostSpec.Host[:i]
		host = target.HostSpec.Host[i+1:]
	}
	inventory := fmt.Sprintf("kampodra-vm ansible_host=%s,ansible_user=%s,", host, user)
	args := []string{"-i", inventory, playbookPath}
	if target.HostSpec.SSHKey != "" {
		args = append(args, "--private-key", target.HostSpec.SSHKey)
	}
	fmt.Fprintf(d.Stdout, "[vm-prepare] ansible-playbook (bootstrap converge)…\n")
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Dir = filepath.Dir(playbookPath)
	cmd.Stdout = d.Stdout
	cmd.Stderr = d.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ansible playbook failed: %w", err)
	}
	fmt.Fprintf(d.Stdout, "[vm-prepare] ansible playbook done\n")
	return nil
}
