package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// vm-prepare: the host bootstrap (vm-prepare.sh port), OS-dispatching since
// the Ubuntu 24.04 sibling landed. The ssh shim fakes a fresh golden image:
// every probe exits 0 by default, uploads are captured per destination
// basename, and a `failon` file makes matching commands fail (gate tests).
// The guest OS is faked too: any `…/etc/os-release` read echoes the
// `os-release` fixture file when present, the ALPINE golden image's body
// otherwise.

type prepareHarness struct {
	deps    command.Deps
	stub    string
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	inv     string
	uploads string
	ansible string
}

func setupPrepare(t *testing.T) *prepareHarness {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	inv := filepath.Join(stub, "invocations")
	uploads := filepath.Join(stub, "uploads")
	os.MkdirAll(uploads, 0o700)
	ansibleLog := filepath.Join(stub, "ansible.log")
	ansibleInvLog := filepath.Join(stub, "ansible-inventory-content.log")

	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"cmd=\"${*: -1}\"\n" +
		"failon=\"$(cat '" + filepath.Join(stub, "failon") + "' 2>/dev/null)\"\n" +
		"if [ -n \"$failon\" ] && [[ \"$cmd\" == *\"$failon\"* ]]; then exit 1; fi\n" +
		"tries=\"$(cat '" + filepath.Join(stub, "unreachable") + "' 2>/dev/null || echo 0)\"\n" +
		"if [ \"$cmd\" = \"true\" ] && [ \"$tries\" -gt 0 ] 2>/dev/null; then echo $((tries-1)) > '" + filepath.Join(stub, "unreachable") + "'; exit 1; fi\n" +
		"case \"$cmd\" in\n" +
		"  \"umask 077; cat >\"*)\n" +
		"    dest=\"${cmd##*cat > }\"\n" +
		"    base=\"${dest##*/}\"\n" +
		"    cat > \"" + uploads + "/upload-$base\"\n" +
		"    exit 0 ;;\n" +
		"  *\"/etc/os-release\"*)\n" +
		"    # guest-OS fixture: the test's os-release file when present, the\n" +
		"    # Alpine golden image's body otherwise\n" +
		"    cat '" + filepath.Join(stub, "os-release") + "' 2>/dev/null || printf 'NAME=\"Alpine Linux\"\\nID=alpine\\n'\n" +
		"    exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	os.WriteFile(filepath.Join(stub, "ssh"), []byte(shim), 0o755)

	// ansible-playbook shim: records the invocation args + snapshots any
	// .ini inventory file's CONTENT (kampodra removes the temp file after
	// the run — the snapshot is the only way to assert what ansible saw)
	ansibleShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + ansibleLog + "'\n" +
		"for a in \"$@\"; do case \"$a\" in *.ini) [ -f \"$a\" ] && cat \"$a\" >> '" + ansibleInvLog + "' ;; esac; done\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "ansible-playbook"), []byte(ansibleShim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE"} {
		t.Setenv(k, "")
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &prepareHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
		},
		stub:    stub,
		stdout:  stdout,
		stderr:  stderr,
		inv:     inv,
		uploads: uploads,
		ansible: ansibleInvLog,
	}
}

func (h *prepareHarness) calls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.inv)
	if err != nil {
		return ""
	}
	return string(data)
}

func (h *prepareHarness) callList(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(h.calls(t), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func (h *prepareHarness) run(t *testing.T, args ...string) int {
	t.Helper()
	return command.Execute("test", h.deps, append([]string{"vm-prepare"}, args...))
}

func TestVMPrepareHappyPathSequence(t *testing.T) {
	h := setupPrepare(t)
	if code := h.run(t, "--host", "root@203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.callList(t)
	joined := h.calls(t)

	// Gates run BEFORE any mutation (apk).
	idx := func(substr string) int {
		for i, c := range calls {
			if strings.Contains(c, substr) {
				return i
			}
		}
		return -1
	}
	if idx("test -d /sys/firmware/efi") > idx("apk add") && idx("apk add") >= 0 {
		t.Error("UEFI gate must precede the apk install")
	}
	if idx("ID=alpine") > idx("test -d /sys/firmware/efi") && idx("test -d /sys/firmware/efi") >= 0 {
		t.Error("Alpine OS gate must precede every other gate (foreign guests fail first, never mid-bootstrap)")
	}
	if idx("rc-service sshd restart") > idx("apk add") && idx("apk add") >= 0 {
		t.Error("sshd hardening must precede the apk install")
	}

	for _, want := range []string{
		// gates
		"test -d /sys/firmware/efi",
		"! readlink /proc/1/exe 2>/dev/null | grep -q systemd",
		"command -v openrc",
		// sshd hardening ensure + restart + gate
		"mkdir -p /etc/ssh/sshd_config.d",
		"rc-service sshd restart",
		"sshd -T",
		// community repo + podman stack (piped as sh -s with the snippet on stdin)
		"grep -Eq \"^[^#].*/community\" /etc/apk/repositories",
		"rc-update show boot | grep -q cgroups",
		// state dir + managed files
		"mkdir -p /etc/kampodra && chmod 700 /etc/kampodra",
		"chmod 755 /etc/init.d/app /etc/init.d/kamal-proxy",
		"mkdir -p /usr/local/sbin",
		"sh -n /usr/local/sbin/kampodra-anchor.sh",
		"sh -n /etc/init.d/kampodra-anchor",
		"rc-update add kampodra-anchor default",
		"rc-service kampodra-anchor start",
		"rc-update add app default",
		"rc-update add kamal-proxy default",
		// network + proxy
		"podman network exists kamal 2>/dev/null || podman network create kamal",
		"podman image exists docker.io/basecamp/kamal-proxy:latest || podman pull docker.io/basecamp/kamal-proxy:latest",
		"rc-service kamal-proxy start",
		"podman ps --format \"{{.Names}}\" | grep -qx kamal-proxy",
		// sysctl live apply rides INSIDE a `sh -s` snippet (its content is
		// pinned by the vmbootstrap adapter tests, not by the argv log)
		"sh -s",
		// post gates
		"podman info --format \"{{.Host.NetworkBackend}}\" | grep -qx netavark",
		"sysctl -n net.ipv4.ip_unprivileged_port_start 2>/dev/null | grep -qx 80",
		"! test -e /etc/kampodra/anchor.conf",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("vm-prepare sequence missing %q", want)
		}
	}

	// uploads landed (basenames of the managed files)
	for _, name := range []string{
		"registries.conf", "60-kampodra.conf", "app", "kamal-proxy",
		"kampodra-anchor.sh", "kampodra-anchor", "99-kampodra-hardening.conf",
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

func TestVMPrepareGateFailureFailsClosed(t *testing.T) {
	h := setupPrepare(t)
	os.WriteFile(filepath.Join(h.stub, "failon"), []byte("/sys/firmware/efi"), 0o600)
	if code := h.run(t, "--host", "root@203.0.113.9"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "UEFI") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	for _, c := range h.callList(t) {
		if strings.Contains(c, "apk add") || strings.Contains(c, "rc-update add") {
			t.Errorf("gate failure must not mutate the host: %s", c)
		}
	}
}

func TestVMPrepareNoUploadsBeforeSshdHardening(t *testing.T) {
	h := setupPrepare(t)
	os.WriteFile(filepath.Join(h.stub, "failon"), []byte("rc-service sshd restart"), 0o600)
	if code := h.run(t, "--host", "root@203.0.113.9"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, c := range h.callList(t) {
		if strings.Contains(c, "cat >") && strings.Contains(c, "init.d") {
			t.Errorf("init.d upload before sshd hardening completed: %s", c)
		}
	}
}

func TestVMPrepareAnsiblePlaybook(t *testing.T) {
	h := setupPrepare(t)
	playbook := filepath.Join(t.TempDir(), "playbook.yml")
	os.WriteFile(playbook, []byte("---\n- hosts: all\n"), 0o600)

	if code := h.run(t, "--host", "deploy@203.0.113.9", "--ssh-key", "/keys/id", "--ansible", playbook); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	logData, err := os.ReadFile(filepath.Join(h.stub, "ansible.log"))
	if err != nil {
		t.Fatalf("ansible-playbook never ran: %v", err)
	}
	logged := string(logData)
	// Exact inventory shape (2026-10-09 live fire): ansible-core 2.21 parses
	// the -i STRING as a bare host list — k=v vars inline made it ssh to a
	// hostname "ansible_user=root". kampodra must write a temp INI inventory
	// file (alias + k=v under an [alias] group); the shim snapshots its
	// content at invocation time (the temp file is removed after the run).
	if !strings.Contains(logged, " --private-key /keys/id") || !strings.Contains(logged, "playbook.yml") {
		t.Errorf("playbook/key args wrong: %s", logged)
	}
	invData, err := os.ReadFile(h.ansible)
	if err != nil {
		t.Fatalf("no .ini inventory seen by ansible: %v (args: %s)", err, logged)
	}
	want := "[kampodra-vm]\nkampodra-vm ansible_host=203.0.113.9 ansible_user=deploy\n"
	if string(invData) != want {
		t.Errorf("inventory content = %q, want %q", invData, want)
	}
}

// The 2026-10-09 live-fire shape: a RELATIVE --ansible path. runAnsiblePlaybook
// chdirs to the playbook's directory before exec — the relative arg must be
// absolutized FIRST or ansible re-resolves it from the new cwd and dies with
// "the playbook … could not be found".
func TestVMPrepareAnsibleRelativePlaybookResolvesBeforeChdir(t *testing.T) {
	h := setupPrepare(t)
	tmp := t.TempDir()
	playbook := "playbook.yml"
	os.WriteFile(filepath.Join(tmp, playbook), []byte("---\n- hosts: all\n"), 0o600)
	t.Chdir(tmp)

	if code := h.run(t, "--host", "deploy@203.0.113.9", "--ssh-key", "/keys/id", "--ansible", playbook); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	logData, err := os.ReadFile(filepath.Join(h.stub, "ansible.log"))
	if err != nil {
		t.Fatalf("ansible-playbook never ran: %v", err)
	}
	if logged := string(logData); !strings.Contains(logged, filepath.Join(tmp, playbook)) {
		t.Errorf("relative playbook was not absolutized before the chdir: %s", logged)
	}
}

func TestVMPrepareAnsibleMissingPlaybookFails(t *testing.T) {
	h := setupPrepare(t)
	if code := h.run(t, "--host", "root@203.0.113.9", "--ansible", "/nonexistent/playbook.yml"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "playbook") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestVMPrepareRequiresHost(t *testing.T) {
	h := setupPrepare(t)
	if code := h.run(t); code != 1 || !strings.Contains(h.stderr.String(), "--host") {
		t.Errorf("exit=%d stderr=%q", code, h.stderr.String())
	}
}

// The dispatch gate: a guest that is neither alpine nor ubuntu fails closed
// BEFORE any mutation, with the detected ID in the message. (Was
// TestVMPrepareAlpineGateFailsClosed — the hard Alpine gate moved into the
// OS dispatch; the Alpine gates live on inside the Alpine provisioner.)
func TestVMPrepareUnknownOSFailsClosed(t *testing.T) {
	h := setupPrepare(t)
	os.WriteFile(filepath.Join(h.stub, "os-release"), []byte("PRETTY_NAME=\"Debian GNU/Linux 12\"\nID=debian\n"), 0o600)
	if code := h.run(t, "--host", "root@203.0.113.9"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "unsupported guest OS") || !strings.Contains(h.stderr.String(), "debian") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	for _, c := range h.callList(t) {
		if strings.Contains(c, "apk add") || strings.Contains(c, "rc-update add") ||
			strings.Contains(c, "apt-get install") || strings.Contains(c, "systemctl") {
			t.Errorf("gate failure must not mutate the host: %s", c)
		}
	}
}

// ubuntuOSRelease is the stub body of the Ubuntu 24.04 golden image's
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
