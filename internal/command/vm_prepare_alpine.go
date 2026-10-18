// provisionAlpine — the Alpine sequence, unchanged by the dispatch: the
// fail-closed gates through the post gates, the summary, the ansible tail.

package command

import (
	"fmt"

	"github.com/talha7k/kampodra/internal/adapter/vmbootstrap"
)

// provisionAlpine — the Alpine sequence, unchanged by the dispatch: the
// fail-closed gates through the post gates, the summary, the ansible tail.
func (p *vmProvisioner) provisionAlpine(ansiblePath string) error {
	err := runProvisionSequence(
		p.detectSudo,
		p.alpineInitGates,
		func() error { return p.sshdHardening("rc-service sshd restart") },
		p.alpinePodmanStack,
		p.alpineDBClientIfDeclared,
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

// alpineDBClientIfDeclared installs ONLY the client tooling the unit env's
// declared engine needs (mariadb-client / curl) — vm-prepare installs no
// database tooling the estate didn't ask for.
func (p *vmProvisioner) alpineDBClientIfDeclared() error {
	var snippet string
	switch p.target.UnitEnvs["KAMPODRA_DB_ENGINE"] {
	case "mariadb":
		snippet = vmbootstrap.ApkMariaClientSnippet()
	case "libsql":
		snippet = vmbootstrap.ApkCurlSnippet()
	default:
		return nil
	}
	p.say("installing the db client stack (unit env declares the engine)…")
	if err := p.runSh(snippet); err != nil {
		return p.die("db client install failed (apk output above)", err)
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
	if err := p.runRoot(`rc-update show boot | grep -q cgroups || rc-update add cgroups boot`); err != nil {
		return p.die("rc-update cgroups failed", err)
	}
	if err := p.runRoot(`rc-service cgroups status >/dev/null 2>&1 || rc-service cgroups start`); err != nil {
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
	if err := p.guardUnitEnvDrop(apiInitD, vmbootstrap.RenderInitDAPI(n)); err != nil {
		return err
	}
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
	if err := p.runRoot(`rc-update show default | grep -qE "^[[:space:]]*kampodra-anchor[[:space:]]*\|" || rc-update add kampodra-anchor default`); err != nil {
		return p.die("rc-update add kampodra-anchor default failed", err)
	}
	if err := p.run("rc-service kampodra-anchor start"); err != nil {
		return p.die("rc-service kampodra-anchor start failed", err)
	}

	for _, svc := range []string{n.Container, vmbootstrap.ProxyContainer} {
		if err := p.runRoot(fmt.Sprintf(`rc-update show default | grep -qE "^[[:space:]]*%[1]s[[:space:]]*\|" || rc-update add %[1]s default`, svc)); err != nil {
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
