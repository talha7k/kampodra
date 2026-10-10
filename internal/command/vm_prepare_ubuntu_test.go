// The Ubuntu 24.04 provisioner legs: the same fail-closed contract as
// Alpine rendered for systemd + apt — happy-path sequence, the apt and
// systemd gates, and the dataDir ensure.

package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// /etc/os-release (the fixture the ssh shim echoes for the dispatch).
const ubuntuOSRelease = "PRETTY_NAME=\"Ubuntu 24.04 LTS\"\nNAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\nHOME_URL=\"https://www.ubuntu.com/\"\n"

// callIdx returns the first invocation-log entry containing substr (-1 if
// absent) — the fixture's gate-order assertion primitive.
func callIdx(calls []string, substr string) int {
	for i, c := range calls {
		if strings.Contains(c, substr) {
			return i
		}
	}
	return -1
}

// assertUbuntuGateOrder pins the dispatch shape: OS detect first after the
// connectivity probe, the common UEFI gate before every mutation, then
// pid1 -> apt -> hardening, and the apt gate before the install snippet.
func assertUbuntuGateOrder(t *testing.T, calls []string) {
	t.Helper()
	if len(calls) < 2 || !strings.Contains(calls[1], "/etc/os-release") {
		t.Errorf("guest OS detect must be the first post-connectivity call, got: %v", calls)
	}
	for _, mut := range []string{"apt-get update", "systemctl restart ssh", "systemctl daemon-reload", "systemctl enable"} {
		if callIdx(calls, mut) >= 0 && callIdx(calls, "test -d /sys/firmware/efi") > callIdx(calls, mut) {
			t.Errorf("UEFI gate must precede %q", mut)
		}
	}
	if callIdx(calls, "readlink /proc/1/exe 2>/dev/null | grep -q systemd") > callIdx(calls, "apt-get update") && callIdx(calls, "apt-get update") >= 0 {
		t.Error("systemd-pid1 gate must precede the apt operational gate")
	}
	if callIdx(calls, "apt-get update") > callIdx(calls, "systemctl restart ssh") && callIdx(calls, "systemctl restart ssh") >= 0 {
		t.Error("the apt operational gate must precede the sshd hardening restart")
	}
	// apt before podman install: the gate precedes the install snippet (the
	// first `sh -s` on the ubuntu path).
	if callIdx(calls, "apt-get update") > callIdx(calls, "sh -s") && callIdx(calls, "sh -s") >= 0 {
		t.Error("apt operational gate must precede the podman install snippet")
	}
}

// assertUbuntuUploadsBeforeEnable pins the unit lifecycle: every unit file
// lands BEFORE daemon-reload/enable touches it.
func assertUbuntuUploadsBeforeEnable(t *testing.T, calls []string) {
	t.Helper()
	reloadIdx, enableIdx := callIdx(calls, "systemctl daemon-reload"), callIdx(calls, "systemctl enable")
	for _, unit := range []string{
		"/etc/systemd/system/app.service",
		"/etc/systemd/system/kamal-proxy.service",
		"/etc/systemd/system/kampodra-anchor.service",
	} {
		up := callIdx(calls, "cat > "+unit)
		if up < 0 {
			t.Fatalf("unit upload missing: %s", unit)
		}
		if up > reloadIdx || up > enableIdx {
			t.Errorf("%s must be uploaded before daemon-reload/enable", unit)
		}
	}
}

func TestVMPrepareUbuntuHappyPathSequence(t *testing.T) {
	h := setupPrepare(t)
	os.WriteFile(filepath.Join(h.stub, "os-release"), []byte(ubuntuOSRelease), 0o600)
	if code := h.run(t, "--host", "root@203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.callList(t)
	joined := h.calls(t)

	assertUbuntuGateOrder(t, calls)

	for _, want := range []string{
		// gates
		"test -d /sys/firmware/efi",
		"readlink /proc/1/exe 2>/dev/null | grep -q systemd",
		"apt-get update",
		// sshd hardening ensure + restart + gate (systemd's unit is `ssh`)
		"mkdir -p /etc/ssh/sshd_config.d",
		"systemctl restart ssh",
		"sshd -T",
		// state dir + managed files (the sysctl apply rides INSIDE a
		// `sh -s` snippet — its content is pinned by the vmbootstrap
		// adapter tests, not by the argv log)
		"mkdir -p /etc/kampodra && chmod 700 /etc/kampodra",
		"mkdir -p /etc/containers",
		// network + units
		"podman network exists kamal 2>/dev/null || podman network create kamal",
		"mkdir -p /usr/local/sbin",
		"chmod 755 /usr/local/sbin/kampodra-anchor.sh",
		"sh -n /usr/local/sbin/kampodra-anchor.sh",
		"systemctl daemon-reload",
		"systemctl enable app.service",
		"systemctl enable --now kamal-proxy.service kampodra-anchor.service",
		"podman image exists docker.io/basecamp/kamal-proxy:latest || podman pull docker.io/basecamp/kamal-proxy:latest",
		"podman ps --format \"{{.Names}}\" | grep -qx kamal-proxy",
		// post gates
		"systemctl is-enabled --quiet app.service",
		"systemctl is-enabled --quiet kamal-proxy.service",
		"systemctl is-enabled --quiet kampodra-anchor.service",
		"systemctl is-active --quiet kamal-proxy.service",
		"systemctl is-active --quiet kampodra-anchor.service",
		"podman info --format \"{{.Host.NetworkBackend}}\" | grep -qx netavark",
		"sysctl -n net.ipv4.ip_unprivileged_port_start 2>/dev/null | grep -qx 80",
		"! test -e /etc/kampodra/anchor.conf",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("vm-prepare (ubuntu) sequence missing %q", want)
		}
	}

	assertUbuntuUploadsBeforeEnable(t, calls)

	// NO apk/openrc commands EVER on the ubuntu path
	for _, banned := range []string{"apk ", "rc-update", "rc-service", "supervise-daemon", "openrc"} {
		if strings.Contains(joined, banned) {
			t.Errorf("ubuntu path must never issue %q (got %q in the log)", banned, banned)
		}
	}

	// uploads landed (basenames of the managed files)
	for _, name := range []string{
		"registries.conf", "60-kampodra.conf", "app.service", "kamal-proxy.service",
		"kampodra-anchor.service", "kampodra-anchor.sh", "99-kampodra-hardening.conf",
	} {
		if _, err := os.Stat(filepath.Join(h.uploads, "upload-"+name)); err != nil {
			t.Errorf("upload missing: %s (%v)", name, err)
		}
	}

	// summary suggests the sslip.io proxy host
	if !strings.Contains(h.stdout.String(), "203-0-113-9.sslip.io") {
		t.Errorf("summary missing the suggested proxy host:\n%s", h.stdout.String())
	}
}

// The ubuntu fail-closed gates: apt NOT operational → die before any
// systemctl/upload mutation.
func TestVMPrepareUbuntuAptGateFailsClosed(t *testing.T) {
	h := setupPrepare(t)
	os.WriteFile(filepath.Join(h.stub, "os-release"), []byte(ubuntuOSRelease), 0o600)
	os.WriteFile(filepath.Join(h.stub, "failon"), []byte("apt-get update"), 0o600)
	if code := h.run(t, "--host", "root@203.0.113.9"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "apt is not operational") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	for _, c := range h.callList(t) {
		if strings.Contains(c, "systemctl") || strings.Contains(c, "cat >") {
			t.Errorf("gate failure must not mutate the host: %s", c)
		}
	}
}

// A non-systemd box served up behind an ubuntu ID (wrong machine/image):
// the pid1 gate fails closed with the golden-image message, before any
// mutation.
func TestVMPrepareUbuntuSystemdGateFailsClosed(t *testing.T) {
	h := setupPrepare(t)
	os.WriteFile(filepath.Join(h.stub, "os-release"), []byte(ubuntuOSRelease), 0o600)
	os.WriteFile(filepath.Join(h.stub, "failon"), []byte("readlink /proc/1/exe"), 0o600)
	if code := h.run(t, "--host", "root@203.0.113.9"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "not the Ubuntu golden image") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	for _, c := range h.callList(t) {
		if strings.Contains(c, "apt-get install") || strings.Contains(c, "systemctl") || strings.Contains(c, "cat >") {
			t.Errorf("gate failure must not mutate the host: %s", c)
		}
	}
}

// TestVMPrepareEnsuresDataDir: the app unit bind-mounts the project
// dataDir — vm-prepare must ENSURE it exists. A volume-less host's
// datamount tolerates absence and only mkdirs WHEN MOUNTING a volume, so
// the first deploy's podman run crash-loops on `statfs /data/tenants: no
// such file or directory` (2026-10-09 live fire, disposable VM).
func TestVMPrepareEnsuresDataDir(t *testing.T) {
	h := setupPrepare(t)
	if code := h.run(t, "--host", "root@203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.calls(t), "mkdir -p /data") {
		t.Errorf("vm-prepare must ensure the dataDir exists:\n%s", h.calls(t))
	}
}
