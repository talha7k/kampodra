package transport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vm-prepare bootstraps FIRST-CONTACT hosts: its ssh leg carries
// StrictHostKeyChecking=accept-new (the shell's SSH_ARGS in vm-prepare.sh).
func TestSSHArgsAcceptNewHostKey(t *testing.T) {
	got := SSHArgs(HostSpec{Host: "root@203.0.113.9", AcceptNewHostKey: true}, "true")
	want := "-o ConnectTimeout=10 -o BatchMode=yes -o StrictHostKeyChecking=accept-new root@203.0.113.9 true"
	if strings.Join(got, " ") != want {
		t.Errorf("SSHArgs() = %v, want %q", got, want)
	}
	// The flag never leaks into ordinary runs.
	plain := SSHArgs(HostSpec{Host: "root@203.0.113.9"}, "true")
	for _, a := range plain {
		if a == "StrictHostKeyChecking=accept-new" {
			t.Errorf("accept-new leaked into the default args: %v", plain)
		}
	}
}

func TestSSHRunnerAcceptNewHostKeyReachesShim(t *testing.T) {
	stubDir := t.TempDir()
	invocations := filepath.Join(stubDir, "invocations")
	shim := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + quote(invocations) + "\n"
	os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755)
	t.Setenv("PATH", stubDir+":/usr/bin:/bin")

	r := &SSHRunner{}
	_, err := r.Run(t.Context(), HostSpec{Host: "root@203.0.113.9", AcceptNewHostKey: true}, "true")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	recorded, _ := os.ReadFile(invocations)
	if !strings.Contains(string(recorded), "StrictHostKeyChecking=accept-new") {
		t.Errorf("shim recorded %q", recorded)
	}
}
