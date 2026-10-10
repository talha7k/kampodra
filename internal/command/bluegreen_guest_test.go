// The resolved-target-key contract: every guest leg (health probe, anchor
// write/poll/cleanup, sha read, ACME) rides the RESOLVED target ssh key —
// the 2026-10-09 live-fire catch where a bare HostSpec dropped --ssh-key.

package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
