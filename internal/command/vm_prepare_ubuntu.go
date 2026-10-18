package command

import (
	"fmt"
	"strings"

	"github.com/talha7k/kampodra/internal/adapter/vmbootstrap"
)

// provisionUbuntu — the Ubuntu 24.04 sibling of provisionAlpine: the same
// contract (fail-closed gates before ANY mutation, the same sshd hardening
// paths, the same managed files, the same container semantics, the same
// readiness gates) rendered for systemd + apt instead of OpenRC + apk.
func (p *vmProvisioner) provisionUbuntu(ansiblePath string) error {
	err := runProvisionSequence(
		p.detectSudo,
		p.ubuntuInitGates,
		func() error { return p.sshdHardening("systemctl restart ssh") },
		p.ubuntuPodmanStack,
		p.ubuntuDBClientIfDeclared,
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

// ubuntuDBClientIfDeclared is the apt sibling of the Alpine dispatcher.
func (p *vmProvisioner) ubuntuDBClientIfDeclared() error {
	var snippet string
	switch p.target.UnitEnvs["KAMPODRA_DB_ENGINE"] {
	case "mariadb":
		snippet = vmbootstrap.AptMariaClientSnippet()
	case "libsql":
		snippet = vmbootstrap.AptCurlSnippet()
	default:
		return nil
	}
	p.say("installing the db client stack (unit env declares the engine)…")
	if err := p.runSh(snippet); err != nil {
		return p.die("db client install failed (apt output above)", err)
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
	if err := p.guardUnitEnvDrop(apiUnit, vmbootstrap.RenderAPIUnit(n)); err != nil {
		return err
	}
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
