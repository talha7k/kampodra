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
	BinaryMount     string // "" | "-v <dir>:<dir>" — the deploy --binary artifact bind mount
	// The artifact override, placed by kind: go/rust exec a DIFFERENT
	// binary → a --entrypoint run FLAG (pre-image); node replaces the
	// image CMD → a positional AFTER the image ref. Exactly one is set.
	BinaryEntrypointFlag string // "" | "--entrypoint <dir>/<entry>"
	BinaryCmdOverride    string // "" | "<exec> <dir>/<entry>"
	BinaryDir            string // the artifact's VM dir (mount/chown/symlink target; "" = undeclared)
	BinaryDepsPath       string // node: the in-image node_modules the mount symlinks at ("" = none)
	StaticReleases       bool   // a static block is declared: provision <dir>/releases (the release+symlink layout)
	// Static-kind blocks (SPA dist trees): every declared one renders its
	// mount + env passthrough — they carry no exec override (the image's
	// entrypoint serves the mounted dist per-request). Rendered in
	// ADDITION to the pinned block's mount/override.
	Provider     string // "" | "oci" (default) | "gcp" — selects the anchor watcher variant
	StaticMounts []string // each "-v <dir>:<dir>" (already-rendered mount args)
	StaticEnvs   []string // each "-e NAME=<dir>" (the app's read-through env)
	UnitEnvs     []string // each "-e K=V" (repo-owned via the manifest's unit.env)
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
