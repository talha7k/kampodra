package transport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHInteractiveArgs(t *testing.T) {
	// deploy shell interactive parity: the shell runs
	//   ssh "${SSH_ARGS[@]}" -t "$KAMPODRA_HOST" "podman exec -it $CONTAINER sh"
	tests := []struct {
		name string
		host HostSpec
		cmd  string
		want []string
	}{
		{
			name: "forced tty before the host, composed remote after",
			host: HostSpec{Host: "root@203.0.113.9"},
			cmd:  "podman exec -it kampodine-api sh",
			want: []string{"-o", "ConnectTimeout=10", "-o", "BatchMode=yes", "-t", "root@203.0.113.9", "podman exec -it kampodine-api sh"},
		},
		{
			name: "identity file still leads",
			host: HostSpec{Host: "root@x", SSHKey: "/tmp/id"},
			cmd:  "podman exec -it c sh",
			want: []string{"-o", "ConnectTimeout=10", "-o", "BatchMode=yes", "-i", "/tmp/id", "-t", "root@x", "podman exec -it c sh"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SSHInteractiveArgs(tt.host, tt.cmd)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("SSHInteractiveArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSSHLoginArgs(t *testing.T) {
	// kampodra ssh (no command): interactive login shell through the
	// profile's host/key — no remote command vector at all.
	got := SSHLoginArgs(HostSpec{Host: "root@203.0.113.9", SSHKey: "/tmp/id"})
	want := []string{"-o", "ConnectTimeout=10", "-o", "BatchMode=yes", "-i", "/tmp/id", "-t", "root@203.0.113.9"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("SSHLoginArgs() = %v, want %v", got, want)
	}
}

func TestSSHPassthroughArgs(t *testing.T) {
	// kampodra ssh <cmd...>: the argv passes through verbatim — ssh composes
	// it remotely, kampodra never joins it into one string.
	got := SSHPassthroughArgs(HostSpec{Host: "root@x"}, []string{"df", "-h", "/var/lib/containers"})
	want := []string{"-o", "ConnectTimeout=10", "-o", "BatchMode=yes", "root@x", "df", "-h", "/var/lib/containers"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("SSHPassthroughArgs() = %v, want %v", got, want)
	}
}

// Stream + RunWithStdin ride the same PATH-shim pattern as Run.
func shimSSH(t *testing.T, body string) string {
	t.Helper()
	stubDir := t.TempDir()
	shim := "#!/bin/sh\n" + body
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	return stubDir
}

func TestStreamPropagatesExitCode(t *testing.T) {
	invocations := filepath.Join(t.TempDir(), "invocations")
	shimSSH(t, "printf '%s\\n' \"$*\" >> "+quote(invocations)+"\nexit 7\n")
	r := &SSHRunner{}
	code, err := r.Stream(t.Context(), SSHInteractiveArgs(HostSpec{Host: "root@x"}, "podman exec -it c sh"))
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if code != 7 {
		t.Fatalf("Stream() code = %d, want 7 (ssh's exit code propagates)", code)
	}
	recorded, _ := os.ReadFile(invocations)
	if got, want := strings.TrimSpace(string(recorded)), "-o ConnectTimeout=10 -o BatchMode=yes -t root@x podman exec -it c sh"; got != want {
		t.Errorf("shim recorded %q, want %q", got, want)
	}
}

func TestStreamLocalFailureIsAnError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	r := &SSHRunner{}
	if _, err := r.Stream(t.Context(), SSHLoginArgs(HostSpec{Host: "root@x"})); err == nil {
		t.Fatal("Stream() with no ssh binary must fail (local failure, not an exit code)")
	}
}

func TestRunWithStdinPipesStdinAndCaptures(t *testing.T) {
	stubDir := t.TempDir()
	uploaded := filepath.Join(stubDir, "uploaded")
	invocations := filepath.Join(stubDir, "invocations")
	shimSSH(t, "printf '%s\\n' \"$*\" >> "+quote(invocations)+"\ncat > "+quote(uploaded)+"\nprintf 'installed\\n'\n")
	r := &SSHRunner{}
	out, err := r.RunWithStdin(t.Context(), HostSpec{Host: "root@x", SSHKey: "/tmp/id"}, "umask 077; cat > /etc/kampodine/env.tmp.42", strings.NewReader("A=1\nSECRET=hush\n"))
	if err != nil {
		t.Fatalf("RunWithStdin() error = %v", err)
	}
	if got := strings.TrimSpace(out); got != "installed" {
		t.Errorf("RunWithStdin() stdout = %q", got)
	}
	data, err := os.ReadFile(uploaded)
	if err != nil || string(data) != "A=1\nSECRET=hush\n" {
		t.Errorf("shim received stdin %q (err=%v), want the verbatim upload stream", string(data), err)
	}
	recorded, _ := os.ReadFile(invocations)
	if got, want := strings.TrimSpace(string(recorded)), "-o ConnectTimeout=10 -o BatchMode=yes -i /tmp/id root@x umask 077; cat > /etc/kampodine/env.tmp.42"; got != want {
		t.Errorf("shim recorded %q, want %q", got, want)
	}
}

func TestRunWithStdinFailsWithRemoteWords(t *testing.T) {
	shimSSH(t, "printf 'disk full\\n' >&2\nexit 3\n")
	r := &SSHRunner{}
	_, err := r.RunWithStdin(t.Context(), HostSpec{Host: "root@x"}, "cat > /tmp/x", strings.NewReader("A=1\n"))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("RunWithStdin() error = %v, want the remote's words attached", err)
	}
}
