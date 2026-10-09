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

// vmPrepareHelp — the host bootstrap (vm-prepare.sh port), OS-dispatching:
// the guest's /etc/os-release picks the provisioner. The VM goes to
// production from its first boot: no blue->green flip, no cert carry; the
// first TLS certificate issues during the first deploy.
const vmPrepareHelp = `Usage:
  kampodra vm-prepare --host root@<new-ip> [--ssh-key <path>] [--profile <name>] [--ansible <playbook>]

First-run bootstrap of a bare VM — Alpine or Ubuntu 24.04, auto-detected
from the guest's /etc/os-release (no flag; idempotent — safe to re-run):

  common:      connectivity, guest OS detect, UEFI gate (both golden
               images are UEFI-only)

  alpine:      1. sanity gates (no systemd anywhere, OpenRC tooling)
               2. sshd hardening ensured (drop-in + Include + restart, then gated)
               3. apk community repo + the podman stack (netavark backend)
               4. the project state dir + /etc/containers/registries.conf (insecure registry)
               5. sysctl net.ipv4.ip_unprivileged_port_start=80 (persisted + live)
               6. OpenRC services: the api container + kamal-proxy (supervise-daemon),
                  rc-update'd into the default runlevel — that IS boot survival
               7. kampodra-anchor: the blue/green flip's guest half (inert without the conf)
               8. kamal-proxy image pulled + service UP (first deploy issues fresh ACME TLS)

  ubuntu 24.04: 1. sanity gates (systemd IS pid1, apt operational)
                2. the same sshd hardening (systemctl restart ssh, then gated)
                3. universe + the podman stack via apt (netavark backend)
                4. the same state dir + managed files (sysctl --system)
                5. systemd units: api + kamal-proxy + kampodra-anchor —
                   daemon-reload, then enable (the api defers its start to
                   the first deploy) and enable --now (edge + anchor)
                6. is-enabled / is-active readiness gates

The api container is NOT started on a fresh VM: neither its image nor the
env file exists yet — the FIRST DEPLOY provides both (both OSes).

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

// vmProvisioner carries the shared bootstrap machinery (the ssh closures +
// the resolved target/naming) into one provisioner per guest OS. Dispatch
// happens ONCE, after connectivity, before any mutation: the provisioners
// speak through the same helpers and differ only in their sequences
// (siblings behind one Naming surface — never branches inside a sequence).
type vmProvisioner struct {
	ctx    context.Context
	d      Deps
	target Target
	n      vmbootstrap.Naming
}

func (p *vmProvisioner) say(format string, args ...any) {
	fmt.Fprintf(p.d.Stdout, "[vm-prepare] "+format+"\n", args...)
}

func (p *vmProvisioner) run(remote string) error {
	_, err := p.d.Runner.Run(p.ctx, p.target.HostSpec, remote)
	return err
}

func (p *vmProvisioner) runSh(snippet string) error {
	_, err := p.d.Runner.RunWithStdin(p.ctx, p.target.HostSpec, "sh -s", strings.NewReader(snippet))
	return err
}

func (p *vmProvisioner) upload(path, content string) error {
	if _, err := p.d.Runner.RunWithStdin(p.ctx, p.target.HostSpec, "umask 077; cat > "+path, strings.NewReader(content)); err != nil {
		return fmt.Errorf("upload %s: %w", path, err)
	}
	return nil
}

func (p *vmProvisioner) die(context string, err error) error {
	return fmt.Errorf("%s: %w", context, err)
}

func newVMPrepareCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm-prepare --host root@<new-ip> [--ansible <playbook>]",
		Short: "first-run bootstrap of a bare host (Alpine or Ubuntu 24.04, auto-detected): gates, sshd hardening, podman stack, init services, kamal-proxy edge, anchor watcher",
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
	cmd.Flags().String("ansible", "", "run this ansible-playbook file against the host after bootstrap")
	return cmd
}

// namingFromProject derives the bootstrap naming from the resolved project
// config (the adapter never imports the project adapter sideways).
func namingFromProject(pj project.Config) vmbootstrap.Naming {
	envDir := pj.EnvFile
	if i := strings.LastIndex(pj.EnvFile, "/"); i > 0 {
		envDir = pj.EnvFile[:i]
	}
	return vmbootstrap.Naming{
		Container:       pj.Container,
		EnvFile:         pj.EnvFile,
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
	ansiblePath, _ := c.Flags().GetString("ansible")

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
		return fmt.Errorf("--host root@<ip> required (--profile <name> or KAMPODRA_HOST also work; --help for usage)")
	}
	// First contact: the bootstrap leg accepts unknown host keys.
	target.HostSpec.AcceptNewHostKey = true

	ctx := c.Context()
	p := &vmProvisioner{ctx: ctx, d: d, target: target, n: namingFromProject(target.Project)}

	// --- 0. connectivity ------------------------------------------------------
	p.say("waiting for ssh on %s…", target.HostSpec.Host)
	if err := waitForSSH(ctx, d.Runner, target.HostSpec, vmPrepareSSHWait); err != nil {
		return err
	}

	// --- 1. guest OS detect — the dispatch happens ONCE, here, BEFORE any -----
	// mutation (os-release(5) read remotely; the ID= value picks the sibling).
	p.say("detecting the guest OS (/etc/os-release)…")
	osRelease, err := d.Runner.Run(ctx, target.HostSpec, vmbootstrap.OSReleaseRead)
	if err != nil {
		return p.die("could not read /etc/os-release (not a kampodra golden image?)", err)
	}
	guestOS := vmbootstrap.GuestOSID(osRelease)

	// --- 2. common pre gate: UEFI (both golden images are UEFI-only) ----------
	p.say("gate: UEFI boot")
	if err := p.run(vmbootstrap.GateUEFI); err != nil {
		return p.die("not booted via UEFI (golden image is UEFI-only — wrong machine/image?)", err)
	}

	// --- 3. dispatch — one provisioner per guest OS -----------------------------
	switch guestOS {
	case "alpine":
		p.say("guest OS: alpine → the Alpine provisioner (OpenRC)")
		return p.provisionAlpine(ansiblePath)
	case "ubuntu":
		p.say("guest OS: ubuntu → the Ubuntu 24.04 provisioner (systemd)")
		return p.provisionUbuntu(ansiblePath)
	default:
		return fmt.Errorf("unsupported guest OS %q — vm-prepare provisions Alpine or Ubuntu 24.04 only (a third guest OS arrives as its own provisioner, not as branches here)", guestOS)
	}
}

// --- shared provisioner stages (identical between the OS siblings) ---------
//
// The siblings differ ONLY in init system + package manager; these stages
// are the OS-agnostic skeleton both sequences call in the same order.

// sshdHardening — the SAME paths/flow on both guests: ensure the Include
// line, land the drop-in, restart, then gate. Only the restart command
// differs (OpenRC's sshd unit vs systemd's ssh unit).
func (p *vmProvisioner) sshdHardening(restart string) error {
	p.say("sshd hardening: ensure drop-in + Include + restart…")
	if err := p.run(`grep -q "^Include /etc/ssh/sshd_config.d/*.conf" /etc/ssh/sshd_config || sed -i "1i Include /etc/ssh/sshd_config.d/*.conf" /etc/ssh/sshd_config`); err != nil {
		return p.die("could not ensure the sshd_config Include line", err)
	}
	if err := p.run("mkdir -p /etc/ssh/sshd_config.d && chmod 700 /etc/ssh/sshd_config.d"); err != nil {
		return p.die("could not create /etc/ssh/sshd_config.d", err)
	}
	if err := p.upload(vmbootstrap.SSHDHardeningPath, vmbootstrap.RenderSSHDHardening()); err != nil {
		return err
	}
	if err := p.run(restart); err != nil {
		return p.die("sshd restart failed after hardening ensure", err)
	}
	p.say("gate: sshd hardening effective")
	if err := p.run(vmbootstrap.GateSSHDHardened); err != nil {
		return p.die("sshd still allows passwords (hardening ensure failed?)", err)
	}
	return nil
}

// managedState — the state dir + managed config files (registries.conf,
// the sysctl drop-in); only the sysctl APPLY differs per OS (OpenRC's
// sysctl service vs `sysctl --system`).
func (p *vmProvisioner) managedState(sysctlSnippet string) error {
	n := p.n
	p.say("creating %s (the env file lands here via kampodra env push, 0600 root)…", n.EnvDir)
	if err := p.run("mkdir -p " + n.EnvDir + " && chmod 700 " + n.EnvDir); err != nil {
		return p.die("state dir create failed", err)
	}
	// The app unit bind-mounts the dataDir — ensure it exists. A
	// volume-less host's datamount tolerates absence and only mkdirs when
	// it mounts a volume; without this the first deploy's podman run
	// crash-loops on `statfs <dataDir>: no such file or directory`
	// (2026-10-09 live fire, disposable VM). On a mounted volume this is a
	// no-op (the mountpoint exists).
	p.say("ensuring the data dir %s (the app unit bind-mounts it)…", n.DataDir)
	if err := p.run("mkdir -p " + n.DataDir); err != nil {
		return p.die("data dir create failed", err)
	}
	p.say("writing /etc/containers/registries.conf (insecure %s; search docker.io)…", n.Registry)
	if err := p.run("mkdir -p /etc/containers"); err != nil {
		return p.die("mkdir /etc/containers failed", err)
	}
	if err := p.upload(vmbootstrap.RegistriesConfPath, vmbootstrap.RenderRegistriesConf(n)); err != nil {
		return err
	}
	p.say("writing %s + applying live…", vmbootstrap.SysctlConfPath)
	if err := p.upload(vmbootstrap.SysctlConfPath, vmbootstrap.RenderSysctlConf()); err != nil {
		return err
	}
	if err := p.runSh(sysctlSnippet); err != nil {
		return p.die("sysctl ensure/apply failed", err)
	}
	return nil
}

// ensurePodmanNetwork — the shared app<->proxy network ensure (the proxy
// re-point resolves the api by network DNS).
func (p *vmProvisioner) ensurePodmanNetwork() error {
	n := p.n
	p.say("ensuring the '%s' podman network (the proxy re-point resolves %s:%s by network DNS)…", n.Network, n.Container, n.Port)
	if err := p.run(fmt.Sprintf("podman network exists %s 2>/dev/null || podman network create %s", n.Network, n.Network)); err != nil {
		return p.die(fmt.Sprintf("podman network create %s failed", n.Network), err)
	}
	return nil
}

// pullProxyImage — the shared kamal-proxy pre-pull (the unit/service start
// stays offline-fast).
func (p *vmProvisioner) pullProxyImage() error {
	pullProxy := fmt.Sprintf("podman image exists %s || podman pull %s", vmbootstrap.ProxyImageRef, vmbootstrap.ProxyImageRef)
	if err := p.run(pullProxy); err != nil {
		return p.die("kamal-proxy image pull failed (docker.io reachable from the VM?)", err)
	}
	return nil
}

// awaitProxy — the shared kamal-proxy readiness loop (statusHint names the
// guest's init-system status command for the failure message).
func (p *vmProvisioner) awaitProxy(statusHint string) error {
	proxyPS := fmt.Sprintf(`podman ps --format "{{.Names}}" | grep -qx %s`, vmbootstrap.ProxyContainer)
	return waitUntil(p.ctx, p.d.Runner, p.target.HostSpec, vmPrepareProxyWait, proxyPS,
		"kamal-proxy container never came up (podman logs kamal-proxy; "+statusHint+")")
}

// runProvisionSequence — both siblings are the same fail-closed pipeline:
// run each stage in order, stop at the first failure. The stages carry the
// OS specifics; the pipeline shape is shared.
func runProvisionSequence(steps ...func() error) error {
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// provisionAlpine — the Alpine sequence, unchanged by the dispatch: the
// fail-closed gates through the post gates, the summary, the ansible tail.
func (p *vmProvisioner) provisionAlpine(ansiblePath string) error {
	err := runProvisionSequence(
		p.alpineInitGates,
		func() error { return p.sshdHardening("rc-service sshd restart") },
		p.alpinePodmanStack,
		func() error { return p.managedState(vmbootstrap.SysctlApplySnippet()) },
		p.alpineOpenRCUnits,
		p.alpineProxyUp,
		p.alpineConvergeAPI,
		p.alpinePostGates,
	)
	if err != nil {
		return err
	}
	return p.finish("(OpenRC, no systemd anywhere)", ansiblePath)
}

// alpineInitGates — the Alpine pre gates (fail BEFORE mutating anything).
func (p *vmProvisioner) alpineInitGates() error {
	p.say("gate: OpenRC init + tooling")
	if err := p.run(vmbootstrap.GateNoSystemd); err != nil {
		return p.die("systemd is PID1 — this is not the Alpine golden image (start-fresh has NO systemd anywhere)", err)
	}
	if err := p.run(vmbootstrap.GateOpenRCTooling); err != nil {
		return p.die("OpenRC tooling missing or not operational (openrc/rc-service/rc-update/supervise-daemon, rc-status)", err)
	}
	return nil
}

// alpinePodmanStack — apk community repo + the podman stack + cgroups.
func (p *vmProvisioner) alpinePodmanStack() error {
	p.say("ensuring the community repo (podman stack lives there)…")
	if err := p.runSh(vmbootstrap.CommunityRepoSnippet()); err != nil {
		return p.die("community repo enable / apk update failed", err)
	}
	if err := p.run(`grep -Eq "^[^#].*/community" /etc/apk/repositories`); err != nil {
		return p.die("community repo could not be enabled (/etc/apk/repositories)", err)
	}
	p.say("installing the podman stack (idempotent)…")
	if err := p.runSh(vmbootstrap.PodmanStackSnippet()); err != nil {
		return p.die("podman stack install failed (apk output above)", err)
	}
	if err := p.runSh(vmbootstrap.PodstackGateSnippet()); err != nil {
		return p.die("podman stack install failed", err)
	}
	if err := p.run(`rc-update show boot | grep -q cgroups || rc-update add cgroups boot`); err != nil {
		return p.die("rc-update cgroups failed", err)
	}
	if err := p.run(`rc-service cgroups status >/dev/null 2>&1 || rc-service cgroups start`); err != nil {
		return p.die("cgroups start failed", err)
	}
	return nil
}

// alpineOpenRCUnits — the network, the supervise-daemon wrappers, and the
// anchor watcher, rc-update'd into the default runlevel (boot survival).
func (p *vmProvisioner) alpineOpenRCUnits() error {
	n := p.n
	if err := p.ensurePodmanNetwork(); err != nil {
		return err
	}
	p.say("installing OpenRC services %s + %s (supervise-daemon around podman run)…", n.Container, vmbootstrap.ProxyContainer)
	apiInitD := "/etc/init.d/" + n.Container
	if err := p.upload(apiInitD, vmbootstrap.RenderInitDAPI(n)); err != nil {
		return err
	}
	if err := p.upload(vmbootstrap.ProxyInitDPath, vmbootstrap.RenderInitDProxy(n)); err != nil {
		return err
	}
	if err := p.run("chmod 755 " + apiInitD + " " + vmbootstrap.ProxyInitDPath); err != nil {
		return p.die("chmod init.d units failed", err)
	}

	// --- the anchor watcher (blue/green flip guest half) -----------------------
	p.say("installing the kampodra-anchor watcher service (blue-green flip guest half)…")
	if err := p.run("mkdir -p /usr/local/sbin"); err != nil {
		return p.die("mkdir /usr/local/sbin failed", err)
	}
	if err := p.upload(vmbootstrap.AnchorScriptPath, vmbootstrap.RenderAnchorScript(n)); err != nil {
		return err
	}
	anchorInitD := "/etc/init.d/kampodra-anchor"
	if err := p.upload(anchorInitD, vmbootstrap.RenderInitDAnchor(n)); err != nil {
		return err
	}
	if err := p.run("chmod 755 " + vmbootstrap.AnchorScriptPath + " " + anchorInitD); err != nil {
		return p.die("chmod kampodra-anchor files failed", err)
	}
	if err := p.run("sh -n " + vmbootstrap.AnchorScriptPath); err != nil {
		return p.die(vmbootstrap.AnchorScriptPath+" does not parse (busybox sh)", err)
	}
	if err := p.run("sh -n " + anchorInitD); err != nil {
		return p.die(anchorInitD+" does not parse", err)
	}
	if err := p.run(`rc-update show default | grep -qE "^[[:space:]]*kampodra-anchor[[:space:]]*\|" || rc-update add kampodra-anchor default`); err != nil {
		return p.die("rc-update add kampodra-anchor default failed", err)
	}
	if err := p.run("rc-service kampodra-anchor start"); err != nil {
		return p.die("rc-service kampodra-anchor start failed", err)
	}

	for _, svc := range []string{n.Container, vmbootstrap.ProxyContainer} {
		if err := p.run(fmt.Sprintf(`rc-update show default | grep -qE "^[[:space:]]*%[1]s[[:space:]]*\|" || rc-update add %[1]s default`, svc)); err != nil {
			return p.die(fmt.Sprintf("rc-update add %s default failed", svc), err)
		}
	}

	// Warn (never auto-restart) if a rewrite changed a unit of a RUNNING
	// service — re-runs after a deploy must not bounce production.
	_ = p.runSh(vmbootstrap.UnitDriftSnippet())
	return nil
}

// alpineProxyUp — pull the edge image, start the service, await the container.
func (p *vmProvisioner) alpineProxyUp() error {
	p.say("pulling kamal-proxy image + starting the service…")
	if err := p.pullProxyImage(); err != nil {
		return err
	}
	if err := p.run("rc-service " + vmbootstrap.ProxyContainer + " start"); err != nil {
		return p.die("rc-service kamal-proxy start failed", err)
	}
	return p.awaitProxy("rc-service kamal-proxy status")
}

// alpineConvergeAPI — start the api ONLY if a previous deploy left image + env.
func (p *vmProvisioner) alpineConvergeAPI() error {
	n := p.n
	p.say("%s start check (needs image + %s — first deploy provides both)…", n.Container, n.EnvFile)
	if err := p.runSh(vmbootstrap.APIConvergeSnippet(n)); err != nil {
		return p.die(fmt.Sprintf("%s start failed (image + env present — investigate: podman logs %s)", n.Container, n.Container), err)
	}
	return nil
}

// alpinePostGates — readiness, fail-closed.
func (p *vmProvisioner) alpinePostGates() error {
	n := p.n
	p.say("gate: podman >= 4.9 + netavark backend")
	if err := p.run(vmbootstrap.GatePodmanVersion); err != nil {
		return p.die("podman too old (need >= 4.9 for netavark)", err)
	}
	if err := p.run(vmbootstrap.GateNetavark); err != nil {
		return p.die("network backend is not netavark", err)
	}
	p.say("gate: sysctl effective")
	if err := p.run(vmbootstrap.GateSysctl); err != nil {
		return p.die("net.ipv4.ip_unprivileged_port_start is not 80", err)
	}
	p.say("gate: registries.conf effective")
	if err := p.run(vmbootstrap.RegistriesGate(n)); err != nil {
		return p.die("registries.conf incomplete ("+n.Registry+" insecure + docker.io search)", err)
	}
	p.say("gate: services enabled in the default runlevel")
	for _, svc := range []string{n.Container, vmbootstrap.ProxyContainer} {
		if err := p.run(vmbootstrap.RunlevelGate(svc)); err != nil {
			return p.die(svc+" not in the default runlevel", err)
		}
	}
	if err := p.run("rc-service " + vmbootstrap.ProxyContainer + " status >/dev/null"); err != nil {
		return p.die("kamal-proxy service not started", err)
	}
	p.say("gate: kampodra-anchor watcher running + INERT (no anchor conf on a fresh VM)")
	if err := p.run(vmbootstrap.RunlevelGate("kampodra-anchor")); err != nil {
		return p.die("kampodra-anchor not in the default runlevel", err)
	}
	if err := p.run("rc-service kampodra-anchor status >/dev/null"); err != nil {
		return p.die("kampodra-anchor service not started", err)
	}
	if err := p.run("! test -e " + n.AnchorConf); err != nil {
		return p.die(n.AnchorConf+" already exists on a fresh VM (wrong machine?)", err)
	}
	if err := p.run(vmbootstrap.GateAnchorIdleLog); err != nil {
		return p.die("kampodra-anchor is started but never logged its idle transition (watcher loop not running?)", err)
	}
	return nil
}

// provisionUbuntu — the Ubuntu 24.04 sibling of provisionAlpine: the same
// contract (fail-closed gates before ANY mutation, the same sshd hardening
// paths, the same managed files, the same container semantics, the same
// readiness gates) rendered for systemd + apt instead of OpenRC + apk.
func (p *vmProvisioner) provisionUbuntu(ansiblePath string) error {
	err := runProvisionSequence(
		p.ubuntuInitGates,
		func() error { return p.sshdHardening("systemctl restart ssh") },
		p.ubuntuPodmanStack,
		func() error { return p.managedState(vmbootstrap.SysctlApplyUbuntuSnippet()) },
		p.ubuntuSystemdUnits,
		p.ubuntuUnitsLive,
		p.ubuntuConvergeAPI,
		p.ubuntuPostGates,
	)
	if err != nil {
		return err
	}
	return p.finish("(systemd, Ubuntu 24.04)", ansiblePath)
}

// ubuntuInitGates — the Ubuntu pre gates (fail BEFORE mutating anything):
// systemd must BE pid1 (the inverse of Alpine's no-systemd gate) and apt
// must be operational.
func (p *vmProvisioner) ubuntuInitGates() error {
	p.say("gate: systemd is PID1")
	if err := p.run(vmbootstrap.GateSystemdPID1); err != nil {
		return p.die("systemd is NOT PID1 — this is not the Ubuntu golden image (start-fresh boots systemd)", err)
	}
	p.say("gate: apt operational")
	if err := p.run(vmbootstrap.GateAptOperational); err != nil {
		return p.die("apt is not operational (apt-get update failed — is the mirror reachable?)", err)
	}
	return nil
}

// ubuntuPodmanStack — universe + the podman stack via apt, then the
// podman/netavark verify (the apt sibling of PodstackGateSnippet's role).
func (p *vmProvisioner) ubuntuPodmanStack() error {
	p.say("ensuring universe + installing the podman stack (idempotent)…")
	if err := p.runSh(vmbootstrap.AptPodmanSnippet()); err != nil {
		return p.die("podman stack install failed (apt output above)", err)
	}
	p.say("gate: podman >= 4.9 + netavark backend")
	if err := p.run(vmbootstrap.GatePodmanVersion); err != nil {
		return p.die("podman too old (need >= 4.9 for netavark — Ubuntu 24.04 ships 4.9.x)", err)
	}
	if err := p.run(vmbootstrap.GateNetavark); err != nil {
		return p.die("network backend is not netavark", err)
	}
	return nil
}

// ubuntuSystemdUnits — the network, the systemd units (api + kamal-proxy +
// the anchor watcher), syntax-checked but not yet live.
func (p *vmProvisioner) ubuntuSystemdUnits() error {
	n := p.n
	if err := p.ensurePodmanNetwork(); err != nil {
		return err
	}
	p.say("installing systemd units %s + %s (podman run under Restart=always)…", n.Container, vmbootstrap.ProxyContainer)
	apiUnit := vmbootstrap.APIUnitPath(n)
	if err := p.upload(apiUnit, vmbootstrap.RenderAPIUnit(n)); err != nil {
		return err
	}
	if err := p.upload(vmbootstrap.ProxyUnitPath, vmbootstrap.RenderProxyUnit(n)); err != nil {
		return err
	}
	p.say("installing the kampodra-anchor watcher unit (blue-green flip guest half)…")
	if err := p.run("mkdir -p /usr/local/sbin"); err != nil {
		return p.die("mkdir /usr/local/sbin failed", err)
	}
	if err := p.upload(vmbootstrap.AnchorScriptPath, vmbootstrap.RenderAnchorScript(n)); err != nil {
		return err
	}
	if err := p.upload(vmbootstrap.AnchorUnitPath, vmbootstrap.RenderAnchorUnit(n)); err != nil {
		return err
	}
	if err := p.run("chmod 755 " + vmbootstrap.AnchorScriptPath); err != nil {
		return p.die("chmod kampodra-anchor script failed", err)
	}
	if err := p.run("chmod 644 " + apiUnit + " " + vmbootstrap.ProxyUnitPath + " " + vmbootstrap.AnchorUnitPath); err != nil {
		return p.die("chmod systemd units failed", err)
	}
	if err := p.run("sh -n " + vmbootstrap.AnchorScriptPath); err != nil {
		return p.die(vmbootstrap.AnchorScriptPath+" does not parse (sh)", err)
	}

	// Warn (never auto-restart) if a rewrite changed a unit of a RUNNING
	// service — re-runs after a deploy must not bounce production.
	_ = p.runSh(vmbootstrap.UnitDriftUbuntuSnippet())
	return nil
}

// ubuntuUnitsLive — daemon-reload after the unit uploads, enable the api
// (its start defers to the first deploy), pull the edge image, then
// enable --now the edge and the watcher.
func (p *vmProvisioner) ubuntuUnitsLive() error {
	n := p.n
	p.say("reloading systemd + enabling units (the api defers its start to the first deploy)…")
	if err := p.run("systemctl daemon-reload"); err != nil {
		return p.die("systemctl daemon-reload failed", err)
	}
	if err := p.run("systemctl enable " + vmbootstrap.APIUnitName(n)); err != nil {
		return p.die(fmt.Sprintf("systemctl enable %s failed", vmbootstrap.APIUnitName(n)), err)
	}
	p.say("pulling kamal-proxy image + starting the edge and the watcher…")
	if err := p.pullProxyImage(); err != nil {
		return err
	}
	if err := p.run("systemctl enable --now " + vmbootstrap.ProxyUnitName + " " + vmbootstrap.AnchorUnitName); err != nil {
		return p.die("systemctl enable --now kamal-proxy/kampodra-anchor failed", err)
	}
	return p.awaitProxy("systemctl status kamal-proxy")
}

// ubuntuConvergeAPI — start the api unit ONLY if a previous deploy left
// image + env.
func (p *vmProvisioner) ubuntuConvergeAPI() error {
	n := p.n
	p.say("%s start check (needs image + %s — first deploy provides both)…", n.Container, n.EnvFile)
	if err := p.runSh(vmbootstrap.APIConvergeUbuntuSnippet(n)); err != nil {
		return p.die(fmt.Sprintf("%s start failed (image + env present — investigate: podman logs %s)", n.Container, n.Container), err)
	}
	return nil
}

// ubuntuPostGates — readiness, fail-closed (systemd siblings of the Alpine
// post gates; the podman/sysctl/registries/anchor gates are OS-agnostic).
func (p *vmProvisioner) ubuntuPostGates() error {
	n := p.n
	p.say("gate: podman >= 4.9 + netavark backend")
	if err := p.run(vmbootstrap.GatePodmanVersion); err != nil {
		return p.die("podman too old (need >= 4.9 for netavark)", err)
	}
	if err := p.run(vmbootstrap.GateNetavark); err != nil {
		return p.die("network backend is not netavark", err)
	}
	p.say("gate: sysctl effective")
	if err := p.run(vmbootstrap.GateSysctl); err != nil {
		return p.die("net.ipv4.ip_unprivileged_port_start is not 80", err)
	}
	p.say("gate: registries.conf effective")
	if err := p.run(vmbootstrap.RegistriesGate(n)); err != nil {
		return p.die("registries.conf incomplete ("+n.Registry+" insecure + docker.io search)", err)
	}
	p.say("gate: units enabled (boot survival)")
	for _, unit := range []string{vmbootstrap.APIUnitName(n), vmbootstrap.ProxyUnitName, vmbootstrap.AnchorUnitName} {
		if err := p.run(vmbootstrap.SystemdEnabledGate(unit)); err != nil {
			return p.die(unit+" not enabled", err)
		}
	}
	p.say("gate: edge + watcher active")
	for _, unit := range []string{vmbootstrap.ProxyUnitName, vmbootstrap.AnchorUnitName} {
		if err := p.run(vmbootstrap.SystemdActiveGate(unit)); err != nil {
			return p.die(unit+" not active", err)
		}
	}
	p.say("gate: kampodra-anchor watcher running + INERT (no anchor conf on a fresh VM)")
	if err := p.run("! test -e " + n.AnchorConf); err != nil {
		return p.die(n.AnchorConf+" already exists on a fresh VM (wrong machine?)", err)
	}
	if err := p.run(vmbootstrap.GateAnchorIdleLog); err != nil {
		return p.die("kampodra-anchor is started but never logged its idle transition (watcher loop not running?)", err)
	}
	return nil
}

// finish — the shared summary tail + the optional ansible converge
// (initNote names the guest's init contract for the operator).
func (p *vmProvisioner) finish(initNote, ansiblePath string) error {
	ip := p.target.HostSpec.Host
	if i := strings.LastIndex(p.target.HostSpec.Host, "@"); i >= 0 {
		ip = p.target.HostSpec.Host[i+1:]
	}
	p.say("PREPARED: %s passes all gates %s.", p.target.HostSpec.Host, initNote)
	p.say("next — first deploy (fresh ACME TLS issued during it):")
	p.say("  PROXY_HOST=%s kampodra deploy --host %s", vmbootstrap.SuggestedProxyHost(ip), p.target.HostSpec.Host)
	p.say("  (DNS must point at %s first — <ip-dashes>.sslip.io resolves automatically)", ip)

	if ansiblePath != "" {
		return runAnsiblePlaybook(p.d, p.ctx, p.target, ansiblePath)
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
// bootstrapped host (inline inventory derived from --host + --private-key
// from the resolved ssh key). Output streams; failure is fatal (explicit
// flag = intent). ctx is the command's context: ctrl-C kills ansible
// (it used to run on a background context and outlive the interrupt).
func runAnsiblePlaybook(d Deps, ctx context.Context, target Target, playbookPath string) error {
	// Absolutize BEFORE the chdir: the exec below runs ansible from the
	// playbook's directory, so a relative arg would re-resolve against it
	// and die with "the playbook … could not be found" (2026-10-09 live fire).
	absPath, err := filepath.Abs(playbookPath)
	if err != nil {
		return fmt.Errorf("ansible playbook path: %w", err)
	}
	if _, err := os.Stat(absPath); err != nil {
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
	// The -i STRING form is a bare host list — ansible-core 2.21 parses
	// k=v pairs there as separate HOSTNAMES ("hostname contains invalid
	// characters"; ssh to "ansible_user=root", 2026-10-09 live fire).
	// Inline host vars are only reliably parsed in an inventory FILE:
	// write a temp INI file (alias + k=v under an [alias] group).
	invFile, err := os.CreateTemp("", "kampodra-inventory-*.ini")
	if err != nil {
		return fmt.Errorf("ansible inventory temp file: %w", err)
	}
	invPath := invFile.Name()
	defer os.Remove(invPath)
	if _, err := fmt.Fprintf(invFile, "[kampodra-vm]\nkampodra-vm ansible_host=%s ansible_user=%s\n", host, user); err != nil {
		invFile.Close()
		return fmt.Errorf("ansible inventory write: %w", err)
	}
	if err := invFile.Close(); err != nil {
		return fmt.Errorf("ansible inventory close: %w", err)
	}
	args := []string{"-i", invPath, absPath}
	if target.HostSpec.SSHKey != "" {
		args = append(args, "--private-key", target.HostSpec.SSHKey)
	}
	fmt.Fprintf(d.Stdout, "[vm-prepare] ansible-playbook (bootstrap converge)…\n")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = filepath.Dir(absPath)
	cmd.Stdout = d.Stdout
	cmd.Stderr = d.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ansible playbook failed: %w", err)
	}
	fmt.Fprintf(d.Stdout, "[vm-prepare] ansible playbook done\n")
	return nil
}
