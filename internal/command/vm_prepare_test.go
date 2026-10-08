package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// vm-prepare: the Alpine host bootstrap (vm-prepare.sh port). The ssh shim
// fakes a fresh golden image: every probe exits 0 by default, uploads are
// captured per destination basename, and a `failon` file makes matching
// commands fail (gate tests).

type prepareHarness struct {
	deps    command.Deps
	stub    string
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	inv     string
	uploads string
}

func setupPrepare(t *testing.T) *prepareHarness {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	inv := filepath.Join(stub, "invocations")
	uploads := filepath.Join(stub, "uploads")
	os.MkdirAll(uploads, 0o700)
	ansibleLog := filepath.Join(stub, "ansible.log")

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
		"  *) exit 0 ;;\n" +
		"esac\n"
	os.WriteFile(filepath.Join(stub, "ssh"), []byte(shim), 0o755)

	// ansible-playbook shim: records the inventory line + playbook args
	ansibleShim := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> '" + ansibleLog + "'\nexit 0\n"
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
		"mkdir -p /etc/kampodine && chmod 700 /etc/kampodine",
		"chmod 755 /etc/init.d/kampodine-api /etc/init.d/kamal-proxy",
		"mkdir -p /usr/local/sbin",
		"sh -n /usr/local/sbin/kampodra-anchor.sh",
		"sh -n /etc/init.d/kampodra-anchor",
		"rc-update add kampodra-anchor default",
		"rc-service kampodra-anchor start",
		"rc-update add kampodine-api default",
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
		"! test -e /etc/kampodine/anchor.conf",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("vm-prepare sequence missing %q", want)
		}
	}

	// uploads landed (basenames of the managed files)
	for _, name := range []string{
		"registries.conf", "60-kampodra.conf", "kampodine-api", "kamal-proxy",
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

func TestVMPreparePullImagesFlag(t *testing.T) {
	h := setupPrepare(t)
	if code := h.run(t, "--host", "root@203.0.113.9", "--pull-images"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.calls(t), "podman pull --tls-verify=false 127.0.0.1:5000/kampodine-api:latest") {
		t.Errorf("--pull-images pull command missing")
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
	if !strings.Contains(logged, "ansible_host=203.0.113.9") || !strings.Contains(logged, "ansible_user=deploy") {
		t.Errorf("inventory line wrong: %s", logged)
	}
	if !strings.Contains(logged, playbook) || !strings.Contains(logged, "--private-key /keys/id") {
		t.Errorf("playbook/key args wrong: %s", logged)
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
