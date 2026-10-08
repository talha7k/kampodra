// Package transport is the ssh seam: kampodine shells out to the ssh BINARY
// (no SSH library) with exactly the shell version's invocation semantics —
// `-o ConnectTimeout=10 -o BatchMode=yes [-i <key>] <host> <remote-cmd>` —
// and its host/key resolution ladders:
//
//	host: --host flag > KAMPODINE_HOST (profile resolution happens above
//	      this seam; profiles FEED the flag/env values)
//	key:  --ssh-key flag > KAMPODINE_SSH_KEY > "" (agent and/or the
//	      operator's ~/.ssh/config Host block — the POSIX way)
//
// Never logs command output that could carry secret material; remote
// commands are composed client-side by design (the shell's documented
// pattern).
package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// HostSpec is one resolved ssh target.
type HostSpec struct {
	Host   string // user@ip or an ssh-config Host alias
	SSHKey string // identity file path; "" = agent / ssh config
}

// Runner executes composed remote commands over ssh.
type Runner interface {
	Run(ctx context.Context, host HostSpec, remoteCmd string) (string, error)
}

// ResolveHost resolves the target host: explicit flag > KAMPODINE_HOST.
// Fails closed (a missing target is an error, never an implicit default).
func ResolveHost(flagHost string, lookup func(string) (string, bool)) (string, error) {
	if flagHost != "" {
		return flagHost, nil
	}
	if v, ok := lookup("KAMPODINE_HOST"); ok && v != "" {
		return v, nil
	}
	return "", errors.New("no target host: pass --host root@<ip> or set KAMPODINE_HOST")
}

// ResolveKey resolves the identity file: explicit flag > KAMPODINE_SSH_KEY >
// "" (agent / ssh config).
func ResolveKey(flagKey string, lookup func(string) (string, bool)) string {
	if flagKey != "" {
		return flagKey
	}
	if v, ok := lookup("KAMPODINE_SSH_KEY"); ok {
		return v
	}
	return ""
}

// SSHArgs builds the ssh argument vector — byte-compatible with the shell
// scripts' `vm()` helper: ConnectTimeout 10, BatchMode, optional -i, host,
// the composed remote command as ONE argument.
func SSHArgs(host HostSpec, remoteCmd string) []string {
	args := []string{"-o", "ConnectTimeout=10", "-o", "BatchMode=yes"}
	if host.SSHKey != "" {
		args = append(args, "-i", host.SSHKey)
	}
	return append(args, host.Host, remoteCmd)
}

// SSHRunner is the default Runner: it execs the ssh binary found on PATH.
type SSHRunner struct {
	// GOOS lets tests exercise the darwin SSH_AUTH_SOCK repair; empty means
	// runtime.GOOS.
	GOOS string
}

// Run executes remoteCmd on host and returns stdout. Stderr is captured and
// included in the error (fail closed, with the remote's words attached).
func (r *SSHRunner) Run(ctx context.Context, host HostSpec, remoteCmd string) (string, error) {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return "", fmt.Errorf("ssh binary not found on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, SSHArgs(host, remoteCmd)...)
	cmd.Env = r.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("ssh %s: %w", host.Host, err)
		}
		return "", fmt.Errorf("ssh %s: %s: %w", host.Host, msg, err)
	}
	return stdout.String(), nil
}

// env repairs the macOS launchd quirk the shell scripts repair: when
// SSH_AUTH_SOCK is unset, ask launchd for the user's agent socket so
// BatchMode agent auth works from non-interactive contexts.
func (r *SSHRunner) env() []string {
	goos := r.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" || os.Getenv("SSH_AUTH_SOCK") != "" {
		return nil
	}
	out, err := exec.Command("launchctl", "getenv", "SSH_AUTH_SOCK").Output()
	if err != nil {
		return nil
	}
	sock := strings.TrimSpace(string(out))
	if sock == "" {
		return nil
	}
	return append(os.Environ(), "SSH_AUTH_SOCK="+sock)
}
