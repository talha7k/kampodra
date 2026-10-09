package command_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/command"
)

type bgHarness struct {
	deps   command.Deps
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	ociLog string
	sshLog string
}

func (h *bgHarness) run(t *testing.T, args ...string) int {
	t.Helper()
	return command.Execute("test", h.deps, append([]string{"bluegreen"}, args...))
}

func (h *bgHarness) ociCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.ociLog)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func (h *bgHarness) sshCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.sshLog)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// bgRoundTripper answers flip HTTPS verifies: 200 + the served body.
type bgRoundTripper struct {
	body string
}

func (b bgRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(b.body)),
		Header:     make(http.Header),
	}, nil
}

func greenRow() string {
	return `{"data":[{"id":"ocid1.instance.green1","availability-domain":"AD-1","time-created":"2026-10-01T00:00:00Z"}]}`
}

func blueRow() string {
	return `{"data":[{"id":"ocid1.instance.blue1","availability-domain":"AD-1","time-created":"2026-09-01T00:00:00Z"}]}`
}

func setupBG(t *testing.T, ociFixtures map[string]string, sshEnv map[string]string) *bgHarness {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	ociLog := filepath.Join(stub, "oci-calls.log")
	sshLog := filepath.Join(stub, "ssh-calls.log")

	def := func(key, val string) {
		if _, ok := ociFixtures[key]; !ok {
			ociFixtures[key] = val
		}
	}
	def("compartments", `{"data":[{"id":"ocid1.compartment.test","name":"test-compartment"}]}`)
	def("reserved", `ocid1.publicip.r1 203.0.113.50 -`)
	def("instances-blue", `{"data":[]}`)
	def("instances-green", `{"data":[]}`)
	if _, ok := ociFixtures["instances-green-later"]; !ok {
		ociFixtures["instances-green-later"] = ociFixtures["instances-green"]
	}
	def("instance", `{"data":{"lifecycle-state":"RUNNING","image-id":"ocid1.image.platform","availability-domain":"AD-1"}}`)
	def("vnic", `{"data":[{"vnic-id":"ocid1.vnic.v1","subnet-id":"ocid1.subnet.s1"}]}`)
	def("vnicGet", `{"data":{"public-ip":"198.51.100.7"}}`)
	def("privateIPs", `{"data":[{"id":"ocid1.privateip.primary","is-primary":true},{"id":"ocid1.privateip.anchor","is-primary":false}]}`)
	def("privateGet", `{"data":{"ip-address":"10.0.0.7"}}`)
	def("subnet", `{"data":{"cidr-block":"10.0.0.0/24"}}`)
	def("images", `{"data":[]}`)
	def("imageGet", `{"data":{"launch-options":{"firmware":"UEFI_64"}}}`)

	files := map[string]string{}
	for k, v := range ociFixtures {
		p := filepath.Join(stub, k+".json")
		os.WriteFile(p, []byte(v), 0o600)
		files[k] = p
	}

	ociShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + ociLog + "'\n" +
		"case \"$1 $2\" in\n" +
		"  \"iam compartment\") cat '" + files["compartments"] + "'; exit 0 ;;\n" +
		"  \"network public-ip\") if [[ \"$3\" == list ]]; then cat '" + files["reserved"] + "'; elif [[ \"$3\" == create ]]; then printf '{\"data\":{\"ip-address\":\"203.0.113.50\"}}'; fi; exit 0 ;;\n" +
		"  \"compute instance\") if [[ \"$3\" == list ]]; then " +
		"if [[ \"$*\" == *\"app-blue\"* ]]; then cat '" + files["instances-blue"] + "'; " +
		"elif grep -q \"app-green\" '" + ociLog + "' 2>/dev/null && [ \"$(grep -c \"app-green\" '" + ociLog + "')\" -gt 1 ]; then cat '" + files["instances-green-later"] + "'; " +
		"else cat '" + files["instances-green"] + "'; fi; exit 0; " +
		"elif [[ \"$3\" == launch ]]; then printf 'ocid1.instance.new-1'; else cat '" + files["instance"] + "'; fi; exit 0 ;;\n" +
		"  \"compute vnic-attachment\") cat '" + files["vnic"] + "'; exit 0 ;;\n" +
		"  \"compute image\") if [[ \"$3\" == list ]]; then cat '" + files["images"] + "'; elif [[ \"$3\" == import ]]; then printf '{\"data\":{\"id\":\"ocid1.image.imp\"}}'; else cat '" + files["imageGet"] + "'; fi; exit 0 ;;\n" +
		"  \"network vnic\") cat '" + files["vnicGet"] + "'; exit 0 ;;\n" +
		"  \"network private-ip\") if [[ \"$3\" == list ]]; then cat '" + files["privateIPs"] + "'; elif [[ \"$3\" == create ]]; then printf '{\"data\":{\"id\":\"ocid1.privateip.new\"}}'; else cat '" + files["privateGet"] + "'; fi; exit 0 ;;\n" +
		"  \"network subnet\") cat '" + files["subnet"] + "'; exit 0 ;;\n" +
		"  \"os ns\") printf '{\"data\":\"test-ns\"}'; exit 0 ;;\n" +
		"  \"os object\") exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "oci"), []byte(ociShim), 0o755)

	envVal := func(k, def string) string {
		if v, ok := sshEnv[k]; ok {
			return v
		}
		return def
	}
	sshShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + sshLog + "'\n" +
		"remote=\"${*: -1}\"\n" +
		"case \"$remote\" in\n" +
		"  *\"cat /etc/alpine-release\"*) printf '" + envVal("ALPINE", "3.22.6") + "'; exit 0 ;;\n" +
		"  *\"cat /etc/os-release\"*) printf 'NAME=\"Ubuntu\"\\nID=ubuntu\\nVERSION_ID=\"24.04\"\\n'; exit 0 ;;\n" +
		"  *\"cat /etc/kampodra/deployed-sha\"*) printf '" + envVal("SHA", "abc1234") + "'; exit 0 ;;\n" +
		"  *\"grep -h\"*) printf '" + envVal("CONFADDR", "") + "'; exit 0 ;;\n" +
		"  *\"rc-service app status\"*) exit " + envVal("HEALTHY", "0") + " ;;\n" +
		"  *wget*) exit " + envVal("HEALTHY", "0") + " ;;\n" +
		"  *\"ANCHOR_CONF_WRITTEN\"*|*\"anchor.conf\"*printf*) printf 'ANCHOR_CONF_WRITTEN'; exit 0 ;;\n" +
		"  *\"ip -4 addr show | grep\"*) exit " + envVal("ADDR_READY", "0") + " ;;\n" +
		"  *\"kamal-proxy deploy\"*) exit " + envVal("ACME", "0") + " ;;\n" +
		"  *\"lsblk\"*) printf 'sda'; exit 0 ;;\n" +
		"  *\"findmnt\"*) printf '/dev/sda1'; exit 0 ;;\n" +
		"  *\"gunzip\"*|*\"reboot\"*|*\"ip addr del\"*|*\"rm -f\"*) exit 0 ;;\n" +
		"  *\"true\"*) exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "ssh"), []byte(sshShim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	t.Setenv("OCI_COMPARTMENT", "test-compartment")
	for _, k := range []string{"KAMPODRA_PROFILE", "OCI_PROFILE"} {
		t.Setenv(k, "")
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &bgHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
			Prober: &probe.Prober{HTTPClient: &http.Client{Transport: bgRoundTripper{body: `{"git":"abc1234"}`}}},
		},
		stdout: stdout, stderr: stderr, ociLog: ociLog, sshLog: sshLog,
	}
}

func TestBluegreenStatusRendersPair(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 ocid1.privateip.anchor",
		"instances-green": greenRow(),
	}, map[string]string{"HEALTHY": "0"})
	if code := h.run(t, "status"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"== app blue/green pair (compartment test-compartment) ==",
		"reserved IP : 203.0.113.50 (ocid1.publicip.r1)",
		"assigned to : ocid1.privateip.anchor",
		"app-blue : not provisioned",
		"app=HEALTHY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestBluegreenStatusNoReservedIP(t *testing.T) {
	h := setupBG(t, map[string]string{"reserved": ""}, nil)
	if code := h.run(t, "status"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "reserved IP : NONE") || !strings.Contains(out, "not provisioned") {
		t.Errorf("status missing NONE/not-provisioned:\n%s", out)
	}
}

func TestBluegreenInitCreatesWhenAbsent(t *testing.T) {
	h := setupBG(t, map[string]string{"reserved": ""}, nil)
	if code := h.run(t, "init"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "public-ip create") || !strings.Contains(joined, "kampodra-active") {
		t.Errorf("init must create the dormant reserved IP:\n%s", joined)
	}
	if !strings.Contains(h.stdout.String(), "DORMANT reserved IP: 203.0.113.50") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestBluegreenInitIdempotent(t *testing.T) {
	h := setupBG(t, map[string]string{"reserved": "ocid1.publicip.r1 203.0.113.50 -"}, nil)
	if code := h.run(t, "init"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "public-ip create") {
			t.Errorf("init must not recreate: %s", call)
		}
	}
	if !strings.Contains(h.stdout.String(), "already exists") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestBluegreenProvisionRejectsBadColorAndExisting(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	if code := h.run(t, "provision", "purple"); code == 0 {
		t.Error("bad color must fail")
	}
	h = setupBG(t, map[string]string{"instances-green": greenRow()}, nil)
	if code := h.run(t, "provision", "green"); code == 0 {
		t.Error("provisioning an already-RUNNING color must fail")
	}
	if !strings.Contains(h.stderr.String(), "already RUNNING") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestBluegreenProvisionNeedsTemplate(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	if code := h.run(t, "provision", "green"); code == 0 {
		t.Error("provision without the other color running must fail")
	}
	if !strings.Contains(h.stderr.String(), "need its AD/subnet as the pair template") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

// --os must fail closed BEFORE any network work on provision too.
func TestBluegreenProvisionRejectsUnknownOS(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	if code := h.run(t, "provision", "green", "--os", "debian"); code == 0 {
		t.Error("unknown --os must fail")
	}
	errText := h.stderr.String()
	if !strings.Contains(errText, "debian") ||
		!strings.Contains(errText, "alpine") || !strings.Contains(errText, "ubuntu") {
		t.Errorf("unknown --os must fail naming itself and the supported set: %q", errText)
	}
	if calls := h.ociCalls(t); len(calls) != 0 {
		t.Errorf("unknown --os must fail before any oci call, got:\n%s", strings.Join(calls, "\n"))
	}
}

func TestBluegreenProvisionNativeRoute(t *testing.T) {
	h := setupBG(t, map[string]string{
		"instances-blue": blueRow(),
		"images":         `{"data":[{"id":"ocid1.image.golden","display-name":"app-alpine-3.22","time-created":"2026-09-01T00:00:00Z"}]}`,
	}, nil)
	if code := h.run(t, "provision", "green"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "instance launch") || !strings.Contains(joined, "VM.Standard.A1.Flex") ||
		!strings.Contains(joined, "--display-name app-green") || !strings.Contains(joined, "ocid1.image.golden") {
		t.Errorf("native launch vector wrong:\n%s", joined)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "LAUNCHED app-green: ocid1.instance.new-1") ||
		!strings.Contains(out, "kampodra bluegreen flip --to green") {
		t.Errorf("stdout = %q", out)
	}
}

// --os ubuntu generalizes the golden-image lookup: the newest
// <container>-ubuntu-24.04* image is the native-route pick (a newer
// alpine-prefixed image must NOT win).
func TestBluegreenProvisionNativeRouteUbuntu(t *testing.T) {
	h := setupBG(t, map[string]string{
		"instances-blue": blueRow(),
		"images": `{"data":[
			{"id":"ocid1.image.alpine-newer","display-name":"app-alpine-3.23","time-created":"2026-10-01T00:00:00Z"},
			{"id":"ocid1.image.ubuntu-golden","display-name":"app-ubuntu-24.04-20260901","time-created":"2026-09-01T00:00:00Z"}]}`,
	}, nil)
	if code := h.run(t, "provision", "green", "--os", "ubuntu"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "ocid1.image.ubuntu-golden") {
		t.Errorf("--os ubuntu must launch the ubuntu-prefixed image:\n%s", joined)
	}
	if strings.Contains(joined, "ocid1.image.alpine-newer") {
		t.Errorf("--os ubuntu must not launch the alpine-prefixed image:\n%s", joined)
	}
}

// Explicit --image-id still wins over the per-OS prefix lookup (no image
// list call happens at all).
func TestBluegreenProvisionImageIDWinsOSPrefix(t *testing.T) {
	h := setupBG(t, map[string]string{
		"instances-blue": blueRow(),
		"images":         `{"data":[{"id":"ocid1.image.ubuntu-golden","display-name":"app-ubuntu-24.04-20260901","time-created":"2026-09-01T00:00:00Z"}]}`,
	}, nil)
	if code := h.run(t, "provision", "green", "--os", "ubuntu", "--image-id", "ocid1.image.explicit"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "ocid1.image.explicit") {
		t.Errorf("explicit --image-id must win:\n%s", joined)
	}
	if strings.Contains(joined, "image list") {
		t.Errorf("--image-id must skip the prefix lookup entirely:\n%s", joined)
	}
}

func TestBluegreenProvisionInjectRoute(t *testing.T) {
	h := setupBG(t, map[string]string{
		"instances-blue":        blueRow(),
		"instances-green-later": `{"data":[{"id":"ocid1.instance.new-1","availability-domain":"AD-1","time-created":"2026-10-09T00:00:00Z"}]}`,
		"images":                `{"data":[{"id":"ocid1.image.bios","display-name":"app-alpine-3.21","time-created":"2026-08-01T00:00:00Z"}]}`,
		"imageGet":              `{"data":{"launch-options":{"firmware":"BIOS"}}}`,
	}, nil)
	qcow2 := filepath.Join(t.TempDir(), "app-alpine-3.22.6-aarch64.qcow2")
	os.WriteFile(qcow2, []byte("fake-qcow2-bytes"), 0o600)
	opsKey := filepath.Join(t.TempDir(), "ops.pub")
	os.WriteFile(opsKey, []byte("ssh-ed25519 FAKE"), 0o600)
	// Fake qemu-img: convert -O raw SRC DST -> plain copy.
	qemuShim := "#!/bin/bash\ncp \"$4\" \"$5\"\n"
	shimPath := filepath.Join(t.TempDir(), "qemu-img")
	os.WriteFile(shimPath, []byte(qemuShim), 0o755)
	t.Setenv("PATH", filepath.Dir(shimPath)+":"+os.Getenv("PATH"))

	if code := h.run(t, "provision", "green", "--qcow2", qcow2, "--platform-key", opsKey); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "INJECTED app-green: Alpine 3.22.6 boots") {
		t.Errorf("stdout = %q", out)
	}
	sshJoined := strings.Join(h.sshCalls(t), "\n")
	if !strings.Contains(sshJoined, "gunzip") || !strings.Contains(sshJoined, "reboot") {
		t.Errorf("inject stream/reboot missing:\n%s", sshJoined)
	}
}

// Inject route with --os ubuntu: the boot verify runs the OS-specific
// remote command (cat /etc/os-release) — never the alpine one.
func TestBluegreenProvisionInjectRouteUbuntu(t *testing.T) {
	h := setupBG(t, map[string]string{
		"instances-blue":        blueRow(),
		"instances-green-later": `{"data":[{"id":"ocid1.instance.new-1","availability-domain":"AD-1","time-created":"2026-10-09T00:00:00Z"}]}`,
		"images":                `{"data":[{"id":"ocid1.image.bios","display-name":"app-alpine-3.21","time-created":"2026-08-01T00:00:00Z"}]}`,
		"imageGet":              `{"data":{"launch-options":{"firmware":"BIOS"}}}`,
	}, nil)
	qcow2 := filepath.Join(t.TempDir(), "app-ubuntu-24.04-aarch64.qcow2")
	os.WriteFile(qcow2, []byte("fake-qcow2-bytes"), 0o600)
	opsKey := filepath.Join(t.TempDir(), "ops.pub")
	os.WriteFile(opsKey, []byte("ssh-ed25519 FAKE"), 0o600)
	// Fake qemu-img: convert -O raw SRC DST -> plain copy.
	qemuShim := "#!/bin/bash\ncp \"$4\" \"$5\"\n"
	shimPath := filepath.Join(t.TempDir(), "qemu-img")
	os.WriteFile(shimPath, []byte(qemuShim), 0o755)
	t.Setenv("PATH", filepath.Dir(shimPath)+":"+os.Getenv("PATH"))

	if code := h.run(t, "provision", "green", "--os", "ubuntu", "--qcow2", qcow2, "--platform-key", opsKey); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "INJECTED app-green:") {
		t.Errorf("stdout = %q", out)
	}
	sshJoined := strings.Join(h.sshCalls(t), "\n")
	if !strings.Contains(sshJoined, "cat /etc/os-release") {
		t.Errorf("--os ubuntu verify must read /etc/os-release:\n%s", sshJoined)
	}
	if strings.Contains(sshJoined, "alpine-release") {
		t.Errorf("--os ubuntu verify must not use the alpine check:\n%s", sshJoined)
	}
}

func TestBluegreenFlipRefusesUnhealthy(t *testing.T) {
	h := setupBG(t, map[string]string{"instances-green": greenRow()}, map[string]string{"HEALTHY": "1"})
	if code := h.run(t, "flip", "--to", "green"); code == 0 {
		t.Error("unhealthy target must refuse without --force")
	}
	if !strings.Contains(h.stderr.String(), "UNHEALTHY") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "public-ip update") {
			t.Errorf("refused flip must not touch the reserved IP: %s", call)
		}
	}
}

func TestBluegreenFlipBadColorUsage(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	if code := h.run(t, "flip", "--to", "purple"); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

func TestBluegreenFlipHappyPath(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 -",
		"instances-green": greenRow(),
	}, map[string]string{"HEALTHY": "0", "ADDR_READY": "0", "ACME": "0", "SHA": "abc1234"})
	if code := h.run(t, "flip", "--to", "green"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{"flip[1/4]", "flip[2/4]", "flip[3/4]", "flip[4/4]", "FLIPPED:"} {
		if !strings.Contains(out, want) {
			t.Errorf("flip sequence missing %q:\n%s", want, out)
		}
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "public-ip update") || !strings.Contains(joined, "--wait-for-state ASSIGNED") {
		t.Errorf("assign must wait ASSIGNED:\n%s", joined)
	}
	sshJoined := strings.Join(h.sshCalls(t), "\n")
	if !strings.Contains(sshJoined, "anchor.conf") || !strings.Contains(sshJoined, "kamal-proxy deploy app --host=") {
		t.Errorf("ACME-first sequence missing guest steps:\n%s", sshJoined)
	}
}

func TestBluegreenFlipACMEFailureAutoRollsBack(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 -",
		"instances-green": greenRow(),
	}, map[string]string{"HEALTHY": "0", "ADDR_READY": "0", "ACME": "1", "SHA": "abc1234"})
	if code := h.run(t, "flip", "--to", "green"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "public-ip update") {
		t.Errorf("auto-rollback must reassign or unassign:\n%s", joined)
	}
	if !strings.Contains(h.stderr.String(), "auto-rollback") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestBluegreenRollbackDormant(t *testing.T) {
	h := setupBG(t, map[string]string{"reserved": "ocid1.publicip.r1 203.0.113.50 -"}, nil)
	if code := h.run(t, "rollback"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.stdout.String(), "already UNASSIGNED (dormant)") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "public-ip update") {
			t.Errorf("dormant rollback must not call update: %s", call)
		}
	}
}

func TestBluegreenRollbackHolderCleanup(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 ocid1.privateip.anchor",
		"instances-green": greenRow(),
	}, map[string]string{"CONFADDR": "10.0.0.7/24"})
	if code := h.run(t, "rollback"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, "current holder: app-green") || !strings.Contains(out, "ROLLED BACK") {
		t.Errorf("stdout = %q", out)
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "--wait-for-state AVAILABLE") {
		t.Errorf("unassign must wait AVAILABLE:\n%s", joined)
	}
}

func TestBluegreenRequiresCompartment(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	t.Setenv("OCI_COMPARTMENT", "")
	if code := h.run(t, "status"); code != 1 || !strings.Contains(h.stderr.String(), "OCI_COMPARTMENT") {
		t.Errorf("missing compartment must fail naming the fix: exit=%d stderr=%q", code, h.stderr.String())
	}
}

func TestBluegreenSubcommandHelpHermetic(t *testing.T) {
	h := setupBG(t, map[string]string{}, nil)
	t.Setenv("OCI_COMPARTMENT", "")
	if code := h.run(t, "flip", "--help"); code != 0 {
		t.Errorf("help must work without config: exit=%d", code)
	}
}

// TestBluegreenGuestLegsCarryResolvedKey: every bluegreen ssh leg at the
// guest (health probe, anchor write/poll/cleanup, sha read, ACME) rides
// the RESOLVED target ssh key — a bare HostSpec only worked when the
// agent happened to hold the ops key (the 2026-10-09 live fire: the
// status probe reported the healthy green UNHEALTHY because the probe
// spec dropped --ssh-key).
func TestBluegreenGuestLegsCarryResolvedKey(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 ocid1.privateip.anchor",
		"instances-green": greenRow(),
	}, map[string]string{"HEALTHY": "0"})
	// greenRow() gives the pair instance a public ip — the status probe
	// sshes to it; the key must be on that invocation.
	if code := h.run(t, "status", "--ssh-key", "/tmp/livefire-ops-key"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.sshCalls(t)
	if len(calls) == 0 {
		t.Fatal("no ssh calls recorded")
	}
	for i, call := range calls {
		if !strings.Contains(call, "-i /tmp/livefire-ops-key") {
			t.Errorf("ssh call %d missing the resolved identity:\n%s", i, call)
		}
	}
}

// TestBluegreenProvisionInjectPlatformLegUsesResolvedKey: the inject
// route's PLATFORM ssh legs (first-boot probe, dd stream, reboot, verify)
// must ride the RESOLVED private identity (--ssh-key), never the
// --platform-key PUBKEY file. The pubkey file is only the
// --ssh-authorized-keys-file payload; `ssh -i ops.pub` limped through on
// agent fallback in past drills and fails outright on an empty agent
// (2026-10-09 live fire).
func TestBluegreenProvisionInjectPlatformLegUsesResolvedKey(t *testing.T) {
	h := setupBG(t, map[string]string{
		"instances-blue":        blueRow(),
		"instances-green-later": `{"data":[{"id":"ocid1.instance.new-1","availability-domain":"AD-1","time-created":"2026-10-09T00:00:00Z"}]}`,
		"images":                `{"data":[{"id":"ocid1.image.bios","display-name":"app-alpine-3.21","time-created":"2026-08-01T00:00:00Z"}]}`,
		"imageGet":              `{"data":{"launch-options":{"firmware":"BIOS"}}}`,
	}, nil)
	qcow2 := filepath.Join(t.TempDir(), "app-alpine-3.22.6-aarch64.qcow2")
	os.WriteFile(qcow2, []byte("fake-qcow2-bytes"), 0o600)
	opsKey := filepath.Join(t.TempDir(), "ops.pub")
	os.WriteFile(opsKey, []byte("ssh-ed25519 FAKE"), 0o600)
	qemuShim := "#!/bin/bash\ncp \"$4\" \"$5\"\n"
	shimPath := filepath.Join(t.TempDir(), "qemu-img")
	os.WriteFile(shimPath, []byte(qemuShim), 0o755)
	t.Setenv("PATH", filepath.Dir(shimPath)+":"+os.Getenv("PATH"))

	if code := h.run(t, "provision", "green", "--qcow2", qcow2, "--platform-key", opsKey,
		"--ssh-key", "/tmp/livefire-ops-key"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	// The launch still authorizes the pubkey file (cloud-init contract).
	ociJoined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(ociJoin(ociJoined), opsKey) {
		t.Errorf("launch must pass --ssh-authorized-keys-file %s:\n%s", opsKey, ociJoined)
	}
	calls := h.sshCalls(t)
	if len(calls) == 0 {
		t.Fatal("no ssh calls recorded")
	}
	for i, call := range calls {
		if strings.Contains(call, " -i "+opsKey+" ") || strings.HasSuffix(call, " -i "+opsKey) {
			t.Errorf("ssh call %d must NOT use the pubkey file as identity:\n%s", i, call)
		}
		if !strings.Contains(call, "-i /tmp/livefire-ops-key") {
			t.Errorf("ssh call %d missing the resolved private identity:\n%s", i, call)
		}
	}
}

// ociJoin is a tiny helper so the authorized-keys assertion reads as one
// string scan.
func ociJoin(s string) string { return s }
