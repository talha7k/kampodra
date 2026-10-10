// bluegreen init + provision: reserved-IP creation and idempotence,
// color validation, template requirements, unknown --os failing closed,
// native-image and golden-qcow2 inject routes (incl. ubuntu), and
// image-OCID precedence.

package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
