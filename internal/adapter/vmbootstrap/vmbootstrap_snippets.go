// The POSIX-safe remote snippets and the managed config renderers: the
// container registry conf, sysctl conf, the env-clearing podman args, and
// the busybox-safe shell snippets piped over `sh -s` (both guest OSes).

package vmbootstrap

import (
	"fmt"
)

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

// ClearEnvArgs is the CLEAR env every app container gets ON TOP of the env
// file (envClearKeys: NODE_ENV, PORT — the env file deliberately omits
// them). ONE source for BOTH the init units and the rolling shadow run:
// a shadow without these probes the app's compiled default port, not the
// published one (2026-10-09 live fire).
func ClearEnvArgs(port string) string {
	return "-e NODE_ENV=production -e PORT=" + port
}

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

// ApkMariaClientSnippet installs the MariaDB client stack (dump + query)
// on Alpine — idempotent, no server.
func ApkMariaClientSnippet() string {
	return `apk add --no-progress mariadb-client`
}

// AptMariaClientSnippet is the Ubuntu sibling: non-interactive, no server.
func AptMariaClientSnippet() string {
	return `export DEBIAN_FRONTEND=noninteractive
apt-get install -y mariadb-client`
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
