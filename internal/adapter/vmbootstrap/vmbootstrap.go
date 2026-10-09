// Package vmbootstrap renders the guest host bootstrap payloads
// (vm-prepare.sh port): the managed config files, the init units (OpenRC on
// Alpine, systemd on Ubuntu), the blue/green anchor watcher, and the
// POSIX-safe remote snippets + gates.
//
// Each supported guest OS is a SIBLING implementation of first-boot
// provisioning behind the same Naming surface — Alpine (OpenRC + apk) and
// Ubuntu 24.04 (systemd + apt) today. The OS contract is explicit and
// fail-fast: the command layer reads the guest's /etc/os-release and
// dispatches to one sibling BEFORE anything mutates. A third guest OS
// arrives the same way — its own sibling renderers/snippets/gates — never
// as branches inside a sibling. The adapter owns host-OS specifics ONLY —
// all deployed-project naming arrives through Naming (built by the command
// layer from ProjectConfig; adapters never import siblings). kampodra
// branding replaces the shell's tool-owned names (anchor unit, log paths,
// drop-in file names); the app container naming rides Naming exactly as the
// profile config says.
//
// Content parity with the retired shell heredocs is the contract: only the
// parameterized names differ. Sibling renderers mirror their Alpine
// counterpart's semantics (the api unit runs the SAME podman run; the
// anchor watcher tees to the SAME log file the gates grep) — they never
// fork the Alpine renderers' content.
package vmbootstrap

import (
	"fmt"
	"strings"
)

// Naming is the naming surface vm-prepare renders against (one struct —
// the command layer fills it from project.Config + derived paths).
type Naming struct {
	Container       string // the api container / service name
	EnvFile         string // remote env file (0600)
	EnvDir          string // dir(EnvFile) — the VM's kampodra state dir
	AnchorConf      string // the blue/green anchor conf (in EnvDir)
	DataDir         string // tenant data dir
	ImagePrefix     string // VM-local image repository path
	Registry        string // the ImagePrefix host:port (registries.conf insecure entry)
	ImageRef        string // ImagePrefix + ":latest"
	Port            string // the container's published port
	Network         string // the podman network shared with kamal-proxy
	DeployedShaFile string // the VM's deployed-sha stamp
}

// RegistriesConfPath / SysctlConfPath / SSHDHardeningPath / AnchorScriptPath
// — the managed files' remote destinations (infra paths, kampodra-branded).
const (
	RegistriesConfPath  = "/etc/containers/registries.conf"
	SysctlConfPath      = "/etc/sysctl.d/60-kampodra.conf"
	SSHDHardeningPath   = "/etc/ssh/sshd_config.d/99-kampodra-hardening.conf"
	AnchorScriptPath    = "/usr/local/sbin/kampodra-anchor.sh"
	ProxyContainer      = "kamal-proxy"
	ProxyInitDPath      = "/etc/init.d/kamal-proxy"
	ProxyImageRef       = "docker.io/basecamp/kamal-proxy:latest"
	ProxyConfigVolume   = "kamal-proxy-config"
	APIStderrLogPath    = "/var/log/kampodra-service.log"
	ProxyStderrLogPath  = "/var/log/kamal-proxy-service.log"
	AnchorLogPath       = "/var/log/kampodra-anchor.log"
	KamalProxyLogMarker = "kamal-proxy"
)

// The Ubuntu sibling of the init.d destinations: systemd unit files under
// /etc/systemd/system. The api unit's name IS the container name (same rule
// as its init.d sibling), so its path/name derive from Naming.
const (
	ProxyUnitPath  = "/etc/systemd/system/kamal-proxy.service"
	AnchorUnitPath = "/etc/systemd/system/kampodra-anchor.service"
	ProxyUnitName  = ProxyContainer + ".service"
	AnchorUnitName = "kampodra-anchor.service"
)

// APIUnitPath derives the api container's systemd unit destination from
// Naming (the Ubuntu sibling of the command layer's "/etc/init.d/"+name).
func APIUnitPath(n Naming) string { return "/etc/systemd/system/" + n.Container + ".service" }

// APIUnitName derives the api container's systemd unit name from Naming.
func APIUnitName(n Naming) string { return n.Container + ".service" }

// OSReleaseRead is the remote probe whose output the command layer parses
// with GuestOSID: the guest OS is auto-detected (no flag), and dispatch to
// a provisioner happens BEFORE anything mutates.
const OSReleaseRead = `cat /etc/os-release`

// GuestOSID parses the ID= value out of an os-release body (os-release(5):
// KEY=value lines, values may be double-quoted). Returns "" when absent —
// callers fail closed on it. ID_LIKE is deliberately NOT consulted: the
// provisioners implement the golden images, not their families.
func GuestOSID(osRelease string) string {
	for _, line := range strings.Split(osRelease, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ID="); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// NamingFromValues derives Naming from the project config fields (lives in
// the command layer's import reach, not here — see vm_prepare.go).

// RenderRegistriesConf ports the registries.conf heredoc: the VM-local
// registry is plain HTTP over the deploy ssh tunnel by design — insecure.
func RenderRegistriesConf(n Naming) string {
	return fmt.Sprintf(`# Managed by kampodra vm-prepare — do not hand-edit.
# %[1]s is the deploy-machine-local registry reached over the ssh reverse
# tunnel (plain HTTP by design — hence insecure); docker.io serves public
# base/proxy images (kamal-proxy).
unqualified-search-registries = ["docker.io"]

[[registry]]
location = "%[1]s"
insecure = true
`, n.Registry)
}

// RenderSysctlConf ports the sysctl drop-in: unprivileged/container paths
// bind port 80 (kamal-proxy publishes 80+443).
func RenderSysctlConf() string {
	return `# Managed by kampodra vm-prepare — do not hand-edit.
# OpenRC contract: let unprivileged/container paths bind port 80
# (kamal-proxy publishes 80+443; belt-and-braces for a future rootless move).
net.ipv4.ip_unprivileged_port_start=80
`
}

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
command_args="run --rm --name %[4]s --network %[7]s -p %[6]s:%[6]s"
command_args="$command_args -v %[3]s:%[3]s"
command_args="$command_args -e NODE_ENV=production -e PORT=%[6]s"
command_args="$command_args --env-file %[2]s %[5]s"

# Respawn forever: a crash loop self-heals at the next deploy's restart;
# the 10s delay bounds log noise.
respawn_delay=10
respawn_max=0

# podman run's OWN stderr (missing image, name conflicts, netavark failures);
# container stdout/stderr go to `+"`podman logs %[4]s`"+`.
supervise_daemon_args="--stderr %[8]s"

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
`, n.Registry, n.EnvFile, n.DataDir, n.Container, n.ImageRef, n.Port, n.Network, APIStderrLogPath)
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
ExecStart=/usr/bin/podman run --rm --name %[3]s --network %[6]s -p %[5]s:%[5]s -v %[2]s:%[2]s -e NODE_ENV=production -e PORT=%[5]s --env-file %[1]s %[4]s

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
`, n.EnvFile, n.DataDir, n.Container, n.ImageRef, n.Port, n.Network)
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

// CommunityRepoSnippet ensures the Alpine community repo (podman lives
// there), deriving the mirror from the enabled main repo, then apk update.
func CommunityRepoSnippet() string {
	return `grep -Eq '^[^#].*/community' /etc/apk/repositories || {
  M=$(grep -E '^[^#].*/main$' /etc/apk/repositories | sed -n '1s#/main$##p')
  [ -n "$M" ] || M="https://dl-cdn.alpinelinux.org/alpine/v3.22"
  echo "$M/community" >> /etc/apk/repositories
}
apk update`
}

// PodmanStackSnippet installs the full podman stack (idempotent).
func PodmanStackSnippet() string {
	return `apk add --no-progress podman podman-docker crun catatonit netavark aardvark-dns fuse-overlayfs iptables`
}

// PodstackGateSnippet verifies the stack: podman present, aardvark-dns in
// place, netavark as the network backend.
func PodstackGateSnippet() string {
	return `command -v podman >/dev/null
test -x /usr/libexec/podman/aardvark-dns
[ "$(podman info --format '{{.Host.NetworkBackend}}' 2>/dev/null)" = netavark ]`
}

// SysctlApplySnippet persists the sysctl service into the boot runlevel and
// applies the unprivileged-port setting live.
func SysctlApplySnippet() string {
	return `rc-update show boot | grep -Eq '^[[:space:]]*sysctl[[:space:]]*\|' || rc-update add sysctl boot
sysctl -w net.ipv4.ip_unprivileged_port_start=80 > /dev/null`
}

// APIConvergeSnippet starts the api service ONLY when a previous deploy
// left both image + env (a fresh VM defers to the first deploy).
func APIConvergeSnippet(n Naming) string {
	return fmt.Sprintf(`if [ -f %[1]s ] && podman image exists %[2]s; then
  rc-service %[3]s start
  echo "%[3]s started (image + env present)"
else
  echo "%[3]s deferred: no image and/or %[1]s yet (normal on a fresh VM — first deploy handles it)"
fi`, n.EnvFile, n.ImageRef, n.Container)
}

// UnitDriftSnippet warns (never auto-restarts) when a rewrite changed a
// unit of a RUNNING service — re-runs after a deploy must not bounce prod.
func UnitDriftSnippet() string {
	return `for SVC in kampodra-anchor; do
  if rc-service "$SVC" status > /dev/null 2>&1; then
    echo "RUNNING: $SVC (unit file was overwritten — rc-service $SVC restart to apply, on your call)"
  fi
done`
}

// --- Ubuntu sibling snippets (apt; piped over `sh -s`) ----------------------

// AptPodmanSnippet is the Ubuntu sibling of CommunityRepoSnippet +
// PodmanStackSnippet in one fail-closed block: ensure the universe
// component (podman lives there), refresh the apt index, install the
// podman stack with the netavark backend. add-apt-repository only runs
// when universe is missing from every apt source (idempotent no-op on the
// golden image); DEBIAN_FRONTEND keeps the install non-interactive.
func AptPodmanSnippet() string {
	return `grep -qsE 'universe' /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources 2>/dev/null || add-apt-repository -y universe
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y podman netavark aardvark-dns crun`
}

// SysctlApplyUbuntuSnippet applies the sysctl drop-in live (systemd re-reads
// /etc/sysctl.d on every boot via systemd-sysctl; --system re-applies now).
func SysctlApplyUbuntuSnippet() string {
	return `sysctl --system`
}

// APIConvergeUbuntuSnippet is the Ubuntu sibling of APIConvergeSnippet: it
// starts the api unit ONLY when a previous deploy left both image + env (a
// fresh VM defers to the first deploy — the unit is enabled but NOT
// --now'd for exactly this reason).
func APIConvergeUbuntuSnippet(n Naming) string {
	return fmt.Sprintf(`if [ -f %[1]s ] && podman image exists %[2]s; then
  systemctl start %[3]s
  echo "%[3]s started (image + env present)"
else
  echo "%[3]s deferred: no image and/or %[1]s yet (normal on a fresh VM — first deploy handles it)"
fi`, n.EnvFile, n.ImageRef, APIUnitName(n))
}

// UnitDriftUbuntuSnippet is the Ubuntu sibling of UnitDriftSnippet: warns
// (never auto-restarts) when a rewrite changed the unit of a RUNNING
// service — re-runs after a deploy must not bounce prod.
func UnitDriftUbuntuSnippet() string {
	return `for UNIT in ` + AnchorUnitName + `; do
  if systemctl is-active --quiet "$UNIT"; then
    echo "RUNNING: $UNIT (unit file was overwritten — systemctl restart $UNIT to apply, on your call)"
  fi
done`
}

// --- gates (fail-closed) -----------------------------------------------------

const (
	// GateUEFI — the golden image is UEFI-only (both guest OSes).
	GateUEFI = `test -d /sys/firmware/efi`
	// NOTE: the per-OS gate lives in the command layer's dispatch — it
	// reads the guest's /etc/os-release (OSReleaseRead + GuestOSID) and
	// routes to one provisioner BEFORE anything mutates. The gates below
	// are the per-sibling init gates; each provisioner runs its own.
	// GateNoSystemd — start-fresh has NO systemd anywhere.
	GateNoSystemd = `! readlink /proc/1/exe 2>/dev/null | grep -q systemd`
	// GateSystemdPID1 — the Ubuntu golden image's init IS systemd (the
	// inverse of GateNoSystemd). Runs before anything mutates: a foreign
	// guest must fail here, never halfway through apt/systemctl steps.
	GateSystemdPID1 = `readlink /proc/1/exe 2>/dev/null | grep -q systemd`
	// GateAptOperational — apt must be operational (mirror reachable, index
	// refreshable) before the bootstrap mutates anything.
	GateAptOperational = `apt-get update`
	// GateOpenRCTooling — the init toolchain must be operational.
	GateOpenRCTooling = `command -v openrc >/dev/null && command -v rc-service >/dev/null && command -v rc-update >/dev/null && command -v supervise-daemon >/dev/null && rc-status --servicelist >/dev/null`
	// GateSSHDHardened — the hardening ensure must be effective.
	GateSSHDHardened = `sshd -T 2>/dev/null | grep -qi "^passwordauthentication no"`
	// GatePodmanVersion — netavark needs podman >= 4.9.
	GatePodmanVersion = `V=$(podman --version | grep -oE "[0-9]+\.[0-9]+\.[0-9]+"); echo "$V" | awk -F. '{exit !($1>4 || ($1==4 && $2>=9))}'`
	// GateNetavark — the network backend must be netavark.
	GateNetavark = `podman info --format "{{.Host.NetworkBackend}}" | grep -qx netavark`
	// GateSysctl — the live sysctl value must be 80.
	GateSysctl = `sysctl -n net.ipv4.ip_unprivileged_port_start 2>/dev/null | grep -qx 80`
	// GateAnchorIdleLog — the watcher logged its idle transition (loop live).
	GateAnchorIdleLog = `grep -q "no anchor conf" /var/log/kampodra-anchor.log`
)

// RegistriesGate verifies the rendered registries.conf landed intact.
func RegistriesGate(n Naming) string {
	return fmt.Sprintf(`grep -q "%[1]s" /etc/containers/registries.conf && grep -q "insecure = true" /etc/containers/registries.conf && grep -q "docker.io" /etc/containers/registries.conf`, n.Registry)
}

// RunlevelGate verifies one service is enabled in the default runlevel.
func RunlevelGate(service string) string {
	return fmt.Sprintf(`rc-update show default | grep -qE "^[[:space:]]*%[1]s[[:space:]]*\|"`, service)
}

// SystemdEnabledGate is the Ubuntu sibling of RunlevelGate: the unit is
// enabled (boot survival).
func SystemdEnabledGate(unit string) string {
	return fmt.Sprintf(`systemctl is-enabled --quiet %[1]s`, unit)
}

// SystemdActiveGate is the Ubuntu sibling of `rc-service X status`: the
// unit is up RIGHT NOW.
func SystemdActiveGate(unit string) string {
	return fmt.Sprintf(`systemctl is-active --quiet %[1]s`, unit)
}

// SuggestedProxyHost renders the sslip.io suggestion for the summary: an IP
// with dashes resolves automatically.
func SuggestedProxyHost(ip string) string {
	return strings.ReplaceAll(ip, ".", "-") + ".sslip.io"
}
