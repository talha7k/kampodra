// The init-unit and watcher-script renderers: OpenRC units (Alpine) and
// their systemd siblings (Ubuntu), plus the anchor watcher script and the
// sshd hardening drop-in — same semantics per guest OS, separate renderers.

package vmbootstrap

import (
	"fmt"
)

// RenderInitDAPI ports the api OpenRC unit: supervise-daemon
// around a FOREGROUND `podman run` (Alpine has no systemd; respawns are
// offline-safe against the local :latest).
func RenderInitDAPI(n Naming) string {
	return fmt.Sprintf(`#!/sbin/openrc-run
# Managed by kampodra vm-prepare — do not hand-edit.
#
# THE app container: one image = web SPA + api, same-origin. Alpine has no
# systemd (no quadlets, no podman restart policies) — supervise-daemon wraps
# the FOREGROUND `+"`podman run`"+`: if the container (or the podman run itself)
# dies, the WHOLE run restarts against the local :latest (no pull inside the
# run — restarts are offline-safe).
#
# Env: %[2]s (0600 root, written by kampodra env push per deploy) is
# the single SECRETS source (podman --env-file reads KEY=VALUE lines directly).
# The generic CLEAR vars (NODE_ENV, PORT — the profile's envClearKeys) are
# owned HERE via -e — one source of truth per key; the app's own contract
# vars ride the env file / its schema, never this unit.
# %[3]s is the ONLY host data path (app data) — without the bind
# mount, data would be container-ephemeral and lost on every restart.

	name="%[4]s"
description="%[4]s container (%[5]s on %[6]s)"

supervisor=supervise-daemon
command="/usr/bin/podman"
# ONE command_args line: supervise-daemon reads it as a plain shell
# variable — LAST assignment wins, so multi-line accumulation silently
# drops everything before the final overwrite (2026-10-10 live fire: the
# container lost run/--rm/-p/-v and podman died on the first -e).
command_args="%[10]s"

# Respawn forever: a crash loop self-heals at the next deploy's restart;
# the 10s delay bounds log noise.
respawn_delay=10
respawn_max=0

# podman run's OWN stderr (missing image, name conflicts, netavark failures);
# container stdout/stderr go to `+"`podman logs %[4]s`"+`.
supervise_daemon_args="--stderr %[9]s"

depend() {
	need net
	after kampodra-datamount
}

start_pre() {
	# SIGKILLed runs leave the container name holding the port (--rm only
	# cleans CLEAN exits) — sweep any leftover before the supervised run.
	podman container exists %[4]s 2>/dev/null && podman rm -f %[4]s >/dev/null 2>&1
	return 0
}

stop_post() {
	# supervise-daemon killed the podman client; make sure the container goes
	# down too (TERM -> graceful shutdown, then a tolerant sweep).
	podman stop --time 10 %[4]s >/dev/null 2>&1 || true
	podman rm -f --time 0 %[4]s >/dev/null 2>&1 || true
}
`, n.Registry, n.EnvFile, n.DataDir, n.Container, n.ImageRef, n.Port, n.Network, ClearEnvArgs(n.Port), APIStderrLogPath, n.runArgs())
}

// runArgs composes the api unit's ENTIRE podman run argument list as ONE
// string (single command_args line — see the template note). Flag order
// matters: --entrypoint is a run FLAG (pre-image); a node override is a
// CMD replacement (post-image). Static mounts/envs ride with the
// pre-image flags — a post-image positional would become the CMD.
func (n Naming) runArgs() string {
	args := fmt.Sprintf("run --rm --name %s --network %s -p %s:%s -v %s:%s",
		n.Container, n.Network, n.Port, n.Port, n.DataDir, n.DataDir)
	if n.BinaryMount != "" {
		args += " " + n.BinaryMount
	}
	if n.BinaryEntrypointFlag != "" {
		args += " " + n.BinaryEntrypointFlag
	}
	args += " " + ClearEnvArgs(n.Port)
	for _, m := range n.StaticMounts {
		args += " " + m
	}
	for _, e := range n.StaticEnvs {
		args += " " + e
	}
	for _, e := range n.UnitEnvs {
		args += " " + e
	}
	args += " --env-file " + n.EnvFile + " " + n.ImageRef
	if n.BinaryCmdOverride != "" {
		args += " " + n.BinaryCmdOverride
	}
	return args
}

// RenderInitDProxy ports the kamal-proxy OpenRC unit: the TLS edge with a
// persistent cert/config volume; the first deploy execs into it to issue
// the fresh ACME certificate.
func RenderInitDProxy(n Naming) string {
	return fmt.Sprintf(`#!/sbin/openrc-run
# Managed by kampodra vm-prepare — do not hand-edit.
#
# TLS edge (kamal-proxy, Let's Encrypt HTTP-01 on :80). Publishes 80+443; the
# LE certs + host->target registrations persist in the %[2]s named
# volume — the fresh ACME cert issued on the first deploy survives container
# recreation AND reboots. Start-fresh: nothing is carried from any retired
# VM. Target registration happens at deploy time (kampodra deploy: podman
# exec %[1]s kamal-proxy deploy <service> --host=<proxyHost> --target=<svc>:%[3]s --tls).

name="%[1]s"
description="%[1]s edge container (80/443, persistent cert/config volume)"

supervisor=supervise-daemon
command="/usr/bin/podman"
command_args="run --rm --name %[1]s --network %[4]s -p 80:80 -p 443:443 --cap-add NET_BIND_SERVICE"
command_args="$command_args -v %[2]s:/home/kamal-proxy/.config %[5]s"

respawn_delay=10
respawn_max=0
supervise_daemon_args="--stderr %[6]s"

depend() {
	need net
}

start_pre() {
	podman container exists %[1]s 2>/dev/null && podman rm -f %[1]s >/dev/null 2>&1
	return 0
}

stop_post() {
	podman stop --time 10 %[1]s >/dev/null 2>&1 || true
	podman rm -f --time 0 %[1]s >/dev/null 2>&1 || true
}
`, ProxyContainer, ProxyConfigVolume, n.Port, n.Network, ProxyImageRef, ProxyStderrLogPath)
}

// RenderAnchorScript ports the blue/green reserved-ip flip's GUEST half: a
// busybox watcher (no container) that polls the anchor conf and `ip addr
// add`s the anchor address at flip time. ADD-ONLY by design; inert without
// the conf.
func RenderAnchorScript(n Naming) string {
	return fmt.Sprintf(`#!/bin/sh
# kampodra-anchor.sh — guest half of the blue-green reserved-ip flip.
#
# OCI assigns the reserved PUBLIC ip to a SECONDARY private ip ("the anchor")
# on the instance VNIC, but a fresh image has NO oracle-cloud-agent: nothing
# configures that private ip inside the guest, so packets to the reserved ip
# die until the address exists on the interface. `+"`kampodra bluegreen flip`"+` writes
# %[1]s over ssh at flip time; this watcher polls it and
# runs `+"`ip addr add`"+` within one interval. The unit is enabled+started on every
# VM by default and is INERT without the conf — a VM that never flips never
# touches its addresses.
#
# ADD-ONLY BY DESIGN: this script NEVER removes or flushes addresses (rollback
# cleanup is the flip tool's explicit ssh job). Only state TRANSITIONS are
# logged (bounded log noise), to stdout — supervise-daemon tees it to %[2]s.

CONF="%[1]s"
INTERVAL="${KAMPODRA_ANCHOR_INTERVAL:-5}"
STATE="boot" # last logged transition (boot | idle | added | error)

log() { printf '%%s kampodra-anchor: %%s\n' "$(date '+%%Y-%%m-%%dT%%H:%%M:%%S%%z')" "$*"; }

# supervise-daemon SIGTERMs on stop — die cleanly with the current tick.
trap 'exit 0' TERM INT

default_iface() {
  ip -4 route show default 2>/dev/null | awk '{print $5; exit}'
}

# Dotted-quad/prefix sanity gate before anything touches `+"`ip addr add`"+` — the
# conf is operator-tooling written, but sourcing it must never turn garbage
# into an `+"`ip`"+` invocation.
addr_wellformed() {
  case "$1" in
    [0-9]*.[0-9]*.[0-9]*.[0-9]*/*) return 0 ;;
    *) return 1 ;;
  esac
}

while :; do
  ANCHOR_ADDR=""
  ANCHOR_IFACE=""
  [ -f "$CONF" ] && . "$CONF"
  if [ -n "${ANCHOR_ADDR:-}" ] && addr_wellformed "$ANCHOR_ADDR"; then
    iface="${ANCHOR_IFACE:-$(default_iface)}"
    if [ -n "$iface" ] && ip -4 addr show dev "$iface" 2>/dev/null | grep -qF -- "$ANCHOR_ADDR"; then
      [ "$STATE" = added ] || { log "anchor $ANCHOR_ADDR present on $iface"; STATE=added; }
    elif [ -n "$iface" ] && ip addr add "$ANCHOR_ADDR" dev "$iface" 2>/dev/null; then
      log "anchor ADDED $ANCHOR_ADDR dev $iface"
      STATE=added
    else
      [ "$STATE" = error ] || { log "anchor add FAILED for $ANCHOR_ADDR (iface ${iface:-none})"; STATE=error; }
    fi
  else
    [ "$STATE" = idle ] || { log "no anchor conf (${CONF}) — idling"; STATE=idle; }
  fi
  sleep "$INTERVAL"
done
`, n.AnchorConf, AnchorLogPath)
}

// RenderInitDAnchor ports the anchor OpenRC unit: supervise-daemon runs the
// watcher; started+enabled on EVERY VM, inert without the conf.
func RenderInitDAnchor(n Naming) string {
	return fmt.Sprintf(`#!/sbin/openrc-run
# Managed by kampodra vm-prepare — do not hand-edit.
#
# Guest half of the blue-green reserved-ip flip: supervise-daemon runs the
# kampodra-anchor.sh watcher, which polls %[1]s (written by
# `+"`kampodra bluegreen flip`"+` over ssh at flip time) and `+"`ip addr add`"+`s the
# anchor address when it appears. Started+enabled on EVERY vm by default and
# INERT without the conf: a VM that never flips never touches its addresses.
# Transitions land in %[2]s.

name="kampodra-anchor"
description="Blue-green reserved-ip anchor address watcher (guest half of the flip)"

supervisor=supervise-daemon
command="%[3]s"

# Respawn forever: the watcher itself never exits (trap TERM/INT -> exit 0 on
# stop), so a respawn means the script died abnormally — retry gently.
respawn_delay=5
respawn_max=0

supervise_daemon_args="--stdout %[2]s --stderr %[2]s"

depend() {
	need net
}
`, n.AnchorConf, AnchorLogPath, AnchorScriptPath)
}

// RenderSSHDHardening ports the sshd drop-in (keys-only management + the
// remote-forward allowance the registry tunnel needs).
func RenderSSHDHardening() string {
	return `# Managed by kampodra vm-prepare — do not hand-edit.
# Keys-only management: this VM's ssh surface is the ONLY management path
# (cloud security lists keep 22 closed to the world; access via temporary
# scoped rule or bastion).
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
PermitEmptyPasswords no
MaxAuthTries 3
X11Forwarding no
GatewayPorts no
ClientAliveInterval 30
ClientAliveCountMax 6

# deploy streams ride a REMOTE forward (ssh -R) — Alpine stock sshd_config
# ships AllowTcpForwarding no; remote-only keeps -L clients off.
AllowTcpForwarding yes
`
}

// --- Ubuntu 24.04 sibling renderers (systemd units; same Naming) -----------
//
// Siblings of the OpenRC renderers above, never forks: identical podman run
// semantics, identical volumes/env/ports, identical log destinations the
// gates grep — only the init system's wrapper differs.

// RenderAPIUnit is the Ubuntu sibling of RenderInitDAPI: a systemd unit
// around the SAME foreground `podman run` (same name/network/port publish,
// data bind, generic CLEAR vars, --env-file, image ref). Restart=always
// replaces supervise-daemon's respawn-forever; podman run's own stderr and
// the container output land in the journal (journalctl -u <unit>) — the
// systemd-native sibling of supervise-daemon's --stderr file.
func RenderAPIUnit(n Naming) string {
	return fmt.Sprintf(`# Managed by kampodra vm-prepare — do not hand-edit.
#
# THE app container: one image = web SPA + api, same-origin. systemd wraps
# the FOREGROUND `+"`podman run`"+`: if the container (or the podman run itself)
# dies, Restart=always reruns it against the local :latest (no pull inside the
# run — restarts are offline-safe).
#
# Env: %[1]s (0600 root, written by kampodra env push per deploy) is
# the single SECRETS source (podman --env-file reads KEY=VALUE lines directly).
# The generic CLEAR vars (NODE_ENV, PORT — the profile's envClearKeys) are
# owned HERE via -e — one source of truth per key; the app's own contract
# vars ride the env file / its schema, never this unit.
# %[2]s is the ONLY host data path (app data) — without the bind
# mount, data would be container-ephemeral and lost on every restart.

[Unit]
Description=%[3]s container (%[4]s on %[5]s)
Wants=network-online.target
After=network-online.target

[Service]
# SIGKILLed runs leave the container name holding the port (--rm only
# cleans CLEAN exits) — sweep any leftover before the supervised run
# (the "-" prefix tolerates "nothing to remove").
ExecStartPre=-/usr/bin/podman rm -f %[3]s
ExecStart=/usr/bin/podman %[9]s

# Respawn forever: a crash loop self-heals at the next deploy's restart; the
# 10s delay bounds log noise.
Restart=always
RestartSec=10

# systemd TERM -> graceful shutdown; then a tolerant sweep so the next start
# never finds a half-dead container name holding the port.
ExecStopPost=-/usr/bin/podman stop --time 10 %[3]s
ExecStopPost=-/usr/bin/podman rm -f --time 0 %[3]s

[Install]
WantedBy=multi-user.target
`, n.EnvFile, n.DataDir, n.Container, n.ImageRef, n.Port, n.Network, ClearEnvArgs(n.Port), APIStderrLogPath, n.runArgs())

}

// RenderProxyUnit is the Ubuntu sibling of RenderInitDProxy: the TLS edge
// with a persistent cert/config volume; the first deploy execs into it to
// issue the fresh ACME certificate.
func RenderProxyUnit(n Naming) string {
	return fmt.Sprintf(`# Managed by kampodra vm-prepare — do not hand-edit.
#
# TLS edge (kamal-proxy, Let's Encrypt HTTP-01 on :80). Publishes 80+443; the
# LE certs + host->target registrations persist in the %[1]s named
# volume — the fresh ACME cert issued on the first deploy survives container
# recreation AND reboots. Start-fresh: nothing is carried from any retired
# VM. Target registration happens at deploy time (kampodra deploy: podman
# exec %[2]s kamal-proxy deploy <service> --host=<proxyHost> --target=<svc>:%[3]s --tls).

[Unit]
Description=%[2]s edge container (80/443, persistent cert/config volume)
Wants=network-online.target
After=network-online.target

[Service]
ExecStartPre=-/usr/bin/podman rm -f %[2]s
ExecStart=/usr/bin/podman run --rm --name %[2]s --network %[4]s -p 80:80 -p 443:443 --cap-add NET_BIND_SERVICE -v %[1]s:/home/kamal-proxy/.config %[5]s

Restart=always
RestartSec=10

ExecStopPost=-/usr/bin/podman stop --time 10 %[2]s
ExecStopPost=-/usr/bin/podman rm -f --time 0 %[2]s

[Install]
WantedBy=multi-user.target
`, ProxyConfigVolume, ProxyContainer, n.Port, n.Network, ProxyImageRef)
}

// RenderAnchorUnit is the Ubuntu sibling of RenderInitDAnchor: systemd runs
// the kampodra-anchor.sh watcher; enabled+started on EVERY VM, inert without
// the conf. The transitions tee to the SAME log file as on Alpine — the
// idle-log gate greps that exact path on both guests.
func RenderAnchorUnit(n Naming) string {
	return fmt.Sprintf(`# Managed by kampodra vm-prepare — do not hand-edit.
#
# Guest half of the blue-green reserved-ip flip: systemd runs the
# kampodra-anchor.sh watcher, which polls %[1]s (written by
# `+"`kampodra bluegreen flip`"+` over ssh at flip time) and `+"`ip addr add`"+`s the
# anchor address when it appears. Enabled+started on EVERY vm by default and
# INERT without the conf: a VM that never flips never touches its addresses.
# Transitions land in %[2]s.

[Unit]
Description=Blue-green reserved-ip anchor address watcher (guest half of the flip)
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=%[3]s

# Respawn forever: the watcher itself never exits (trap TERM/INT -> exit 0 on
# stop), so a restart means the script died abnormally — retry gently.
Restart=always
RestartSec=5

# The SAME transitions log the Alpine watcher tees to (the idle-log gate
# greps it on both guests).
StandardOutput=append:%[2]s
StandardError=append:%[2]s

[Install]
WantedBy=multi-user.target
`, n.AnchorConf, AnchorLogPath, AnchorScriptPath)
}

// --- remote snippets (busybox-safe; piped over `sh -s`) ---------------------
