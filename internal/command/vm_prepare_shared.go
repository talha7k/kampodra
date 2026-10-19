// The guest-OS-agnostic provisioning steps: sshd hardening, the managed
// state files, the podman network, and the kamal-proxy image — the SAME
// paths and flow on both guests (only the unit syntax differs, and that
// lives in the per-OS sequences).

package command

import (
	"fmt"

	"github.com/talha7k/kampodra/internal/adapter/vmbootstrap"
)

// sshdHardening — the SAME paths/flow on both guests: ensure the Include
// line, land the drop-in, restart, then gate. Only the restart command
// differs (OpenRC's sshd unit vs systemd's ssh unit).
func (p *vmProvisioner) sshdHardening(restart string) error {
	p.say("sshd hardening: ensure drop-in + Include + restart…")
	if err := p.runRoot(`grep -q "^Include /etc/ssh/sshd_config.d/*.conf" /etc/ssh/sshd_config || sed -i "1i Include /etc/ssh/sshd_config.d/*.conf" /etc/ssh/sshd_config`); err != nil {
		return p.die("could not ensure the sshd_config Include line", err)
	}
	if err := p.runRoot("mkdir -p /etc/ssh/sshd_config.d && chmod 700 /etc/ssh/sshd_config.d"); err != nil {
		return p.die("could not create /etc/ssh/sshd_config.d", err)
	}
	if err := p.upload(vmbootstrap.SSHDHardeningPath, vmbootstrap.RenderSSHDHardening()); err != nil {
		return err
	}
	// VALIDATE BEFORE RESTART: a broken sshd config + a restart = locked
	// out of the VM (live-smoke incident: stray invalid drop-ins made the
	// restart fail with sshd down). On a failed sshd -t the drop-in is
	// removed and ssh restarted — the host heals itself, the run dies
	// naming the config error.
	if err := p.runRoot("sshd -t"); err != nil {
		p.runRoot("rm -f " + vmbootstrap.SSHDHardeningPath + " && systemctl restart ssh 2>/dev/null || service sshd restart 2>/dev/null || true")
		return p.die("sshd config invalid after hardening — drop-in removed, ssh restored, fix the render and re-run", err)
	}
	if err := p.run(restart); err != nil {
		return p.die("sshd restart failed after hardening ensure", err)
	}
	p.say("gate: sshd hardening effective")
	if err := p.run(vmbootstrap.GateSSHDHardened); err != nil { // sshd -T is a PRIVILEGED read (host keys) — it must elevate
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
	if err := p.runRoot("mkdir -p " + n.EnvDir + " && chmod 700 " + n.EnvDir); err != nil {
		return p.die("state dir create failed", err)
	}
	// The env file itself must EXIST: the app unit's --env-file parse
	// fails closed on a missing file, and a fresh estate deploys without
	// an env push (live-smoke finding — the shadow died on
	// "env: no such file or directory"). Touch it 0600; env push/env set
	// overwrite content later.
	p.say("ensuring the env file exists (empty, 0600 — fresh estates deploy without an env push)…")
	if err := p.runSh(vmbootstrap.TouchEnvFileSnippet(n.EnvFile)); err != nil {
		return p.die("env file ensure failed", err)
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
	// The deploy --binary artifact mount: same crash-loop hazard as the
	// dataDir (podman run statfs on a missing path) plus, for the node
	// kind, the deps symlink — the mount carries SOURCE only, deps come
	// from the image (node_modules -> the image's own install). Written
	// here so a binary push never depends on provisioning order.
	if n.BinaryDir != "" {
		p.say("ensuring the binary artifact dir %s (the app unit bind-mounts it)…", n.BinaryDir)
		if err := p.runRoot("mkdir -p " + n.BinaryDir + " && chmod 755 " + n.BinaryDir); err != nil {
			return p.die("binary artifact dir create failed", err)
		}
		if n.BinaryDepsPath != "" {
			p.say("linking %s/node_modules -> %s (in-image deps; the mount never carries them)…", n.BinaryDir, n.BinaryDepsPath)
			if err := p.run("ln -sfn " + n.BinaryDepsPath + " " + n.BinaryDir + "/node_modules"); err != nil {
				return p.die("node_modules symlink failed", err)
			}
		}
		// The static kind's release layout: pushes land as
		// releases/<sha>/ and the atomic flip moves the `current`
		// symlink — all INSIDE the mounted inode (a directory rename at
		// the mount root is invisible to the container).
		if n.StaticReleases {
			if err := p.run("mkdir -p " + n.BinaryDir + "/releases"); err != nil {
				return p.die("static releases dir create failed", err)
			}
		}
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
	if err := p.runRoot(fmt.Sprintf("podman network exists %s 2>/dev/null || podman network create %s", n.Network, n.Network)); err != nil {
		return p.die(fmt.Sprintf("podman network create %s failed", n.Network), err)
	}
	return nil
}

// pullProxyImage — the shared kamal-proxy pre-pull (the unit/service start
// stays offline-fast).
func (p *vmProvisioner) pullProxyImage() error {
	pullProxy := fmt.Sprintf("podman image exists %s || podman pull %s", vmbootstrap.ProxyImageRef, vmbootstrap.ProxyImageRef)
	if err := p.runRoot(pullProxy); err != nil { // reads the 0600 registries.conf — needs the elevation
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
