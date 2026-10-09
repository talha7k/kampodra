package transport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveHostLadder(t *testing.T) {
	tests := []struct {
		name      string
		flagHost  string
		env       map[string]string
		want      string
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "explicit flag beats every env",
			flagHost: "root@203.0.113.9",
			env:      map[string]string{"KAMPODRA_HOST": "root@from-env", "ESPELLAR_HOST": "root@legacy"},
			want:     "root@203.0.113.9",
		},
		{
			name:     "KAMPODRA_HOST when no flag",
			flagHost: "",
			env:      map[string]string{"KAMPODRA_HOST": "root@from-env"},
			want:     "root@from-env",
		},
		{
			name:      "no flag no env fails closed",
			wantErr:   true,
			errSubstr: "no target host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				v, ok := tt.env[key]
				return v, ok
			}
			got, err := ResolveHost(tt.flagHost, lookup)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveHost() = %q, want error", got)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("ResolveHost() error = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveHost() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveKeyLadder(t *testing.T) {
	tests := []struct {
		name    string
		flagKey string
		envKey  string
		want    string
	}{
		{name: "flag first", flagKey: "/tmp/id_flag", envKey: "/tmp/id_env", want: "/tmp/id_flag"},
		{name: "env second", flagKey: "", envKey: "/tmp/id_env", want: "/tmp/id_env"},
		{name: "empty falls through to agent/ssh-config", flagKey: "", envKey: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) (string, bool) {
				if key == "KAMPODRA_SSH_KEY" && tt.envKey != "" {
					return tt.envKey, true
				}
				return "", false
			}
			if got := ResolveKey(tt.flagKey, lookup); got != tt.want {
				t.Errorf("ResolveKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSSHArgs(t *testing.T) {
	tests := []struct {
		name string
		host HostSpec
		cmd  string
		want []string
	}{
		{
			name: "matches the shell vm() helper exactly",
			host: HostSpec{Host: "root@203.0.113.9"},
			cmd:  "df -P /var/lib/containers",
			want: []string{"-o", "ConnectTimeout=10", "-o", "BatchMode=yes", "root@203.0.113.9", "df -P /var/lib/containers"},
		},
		{
			name: "identity file inserted before the host",
			host: HostSpec{Host: "root@203.0.113.9", SSHKey: "/tmp/id_ed25519"},
			cmd:  "podman ps",
			want: []string{"-o", "ConnectTimeout=10", "-o", "BatchMode=yes", "-i", "/tmp/id_ed25519", "root@203.0.113.9", "podman ps"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SSHArgs(tt.host, tt.cmd)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("SSHArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The PATH-shim pattern from the shell repo's status-ops.test.ts, ported:
// a fake `ssh` records its invocation and prints a fixture, so no test ever
// touches a real network.
func TestSSHRunnerExecutesShimmedSSH(t *testing.T) {
	stubDir := t.TempDir()
	invocations := filepath.Join(stubDir, "invocations")
	fixture := filepath.Join(stubDir, "fixture")
	os.WriteFile(fixture, []byte("hello from the shim"), 0o644)
	shim := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + quote(invocations) + "\n" +
		"cat " + quote(fixture) + "\n"
	os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755)

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")

	r := &SSHRunner{}
	out, err := r.Run(t.Context(), HostSpec{Host: "root@203.0.113.9", SSHKey: "/tmp/id_ed25519"}, "podman ps --format '{{.Image}}'")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := strings.TrimSpace(out); got != "hello from the shim" {
		t.Errorf("Run() stdout = %q", got)
	}
	recorded, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatalf("shim was not invoked: %v", err)
	}
	want := "-o ConnectTimeout=10 -o BatchMode=yes -i /tmp/id_ed25519 root@203.0.113.9 podman ps --format '{{.Image}}'"
	if got := strings.TrimSpace(string(recorded)); got != want {
		t.Errorf("shim recorded args %q, want %q", got, want)
	}
}

func TestSSHRunnerFailsClosedWithoutSSHBinary(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	r := &SSHRunner{}
	if _, err := r.Run(t.Context(), HostSpec{Host: "root@nowhere"}, "true"); err == nil {
		t.Fatal("Run() with no ssh binary on PATH must fail closed")
	}
}

// A remote failure must not discard the remote's captured stdout — the
// restore-verify report IS the diagnosis (2026-10-09 live fire: exit 1 with
// the FAIL lines printed to stdout and swallowed by the error path).
func TestSSHRunnerReturnsStdoutAlongsideRemoteFailure(t *testing.T) {
	stubDir := t.TempDir()
	shim := "#!/bin/sh\n" +
		"echo '[FAIL] pih_linkage: artifact scan failed'\n" +
		"echo 'RESULT: NOT SAFE' \n" +
		"echo 'some stderr detail' >&2\n" +
		"exit 1\n"
	os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755)
	t.Setenv("PATH", stubDir+":/usr/bin:/bin")

	r := &SSHRunner{}
	out, err := r.Run(t.Context(), HostSpec{Host: "root@203.0.113.9"}, "restore-verify -db /tmp/x.db")
	if err == nil {
		t.Fatal("Run() must surface the remote failure")
	}
	if !strings.Contains(out, "[FAIL] pih_linkage") || !strings.Contains(out, "RESULT: NOT SAFE") {
		t.Errorf("Run() discarded the remote report on failure: out=%q err=%v", out, err)
	}
	if !strings.Contains(err.Error(), "some stderr detail") {
		t.Errorf("Run() error must still carry stderr: %v", err)
	}
}

func quote(s string) string { return "'" + s + "'" }
