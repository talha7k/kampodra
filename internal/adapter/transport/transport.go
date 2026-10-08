// Package transport is the ssh seam: kampodra shells out to the ssh BINARY
// (no SSH library) with exactly the shell version's invocation semantics —
// `-o ConnectTimeout=10 -o BatchMode=yes [-i <key>] <host> <remote-cmd>` —
// and its host/key resolution ladders:
//
//	host: --host flag > KAMPODRA_HOST (profile resolution happens above
//	      this seam; profiles FEED the flag/env values)
//	key:  --ssh-key flag > KAMPODRA_SSH_KEY > "" (agent and/or the
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
	"io"
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

// Runner executes composed remote commands over ssh (captured runs, stdin
// uploads, and stdio-passthrough streams — the three shapes the shell's
// vm()/pipe/-t invocations cover).
type Runner interface {
	Run(ctx context.Context, host HostSpec, remoteCmd string) (string, error)
	RunWithStdin(ctx context.Context, host HostSpec, remoteCmd string, stdin io.Reader) (string, error)
	Stream(ctx context.Context, sshArgs []string) (int, error)
}

// ResolveHost resolves the target host: explicit flag > KAMPODRA_HOST.
// Fails closed (a missing target is an error, never an implicit default).
func ResolveHost(flagHost string, lookup func(string) (string, bool)) (string, error) {
	if flagHost != "" {
		return flagHost, nil
	}
	if v, ok := lookup("KAMPODRA_HOST"); ok && v != "" {
		return v, nil
	}
	return "", errors.New("no target host: pass --host root@<ip> or set KAMPODRA_HOST")
}

// ResolveKey resolves the identity file: explicit flag > KAMPODRA_SSH_KEY >
// "" (agent / ssh config).
func ResolveKey(flagKey string, lookup func(string) (string, bool)) string {
	if flagKey != "" {
		return flagKey
	}
	if v, ok := lookup("KAMPODRA_SSH_KEY"); ok {
		return v
	}
	return ""
}

// SSHArgs builds the ssh argument vector — byte-compatible with the shell
// scripts' `vm()` helper: ConnectTimeout 10, BatchMode, optional -i, host,
// the composed remote command as ONE argument.
func SSHArgs(host HostSpec, remoteCmd string) []string {
	return sshBaseArgs(host, remoteCmd, nil)
}

// SSHInteractiveArgs composes a remote command with a forced tty — the
// deploy shell interactive shape (`ssh … -t host "podman exec -it … sh"`).
func SSHInteractiveArgs(host HostSpec, remoteCmd string) []string {
	return sshBaseArgs(host, remoteCmd, []string{"-t"})
}

// SSHLoginArgs opens an interactive login shell (kampodra ssh with no
// command): base options, forced tty, host, NO remote command.
func SSHLoginArgs(host HostSpec) []string {
	return sshBaseArgs(host, "", []string{"-t"})
}

// SSHPassthroughArgs hands a command argv to ssh verbatim (kampodra ssh
// <cmd>...): base options, host, then the argv — never joined client-side.
func SSHPassthroughArgs(host HostSpec, cmdAndArgs []string) []string {
	args := sshBaseArgs(host, "", nil)
	return append(args, cmdAndArgs...)
}

func sshBaseArgs(host HostSpec, remoteCmd string, extra []string) []string {
	args := []string{"-o", "ConnectTimeout=10", "-o", "BatchMode=yes"}
	if host.SSHKey != "" {
		args = append(args, "-i", host.SSHKey)
	}
	args = append(args, extra...)
	args = append(args, host.Host)
	if remoteCmd != "" {
		args = append(args, remoteCmd)
	}
	return args
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

// RunWithStdin executes remoteCmd with stdin wired to r (the env push
// upload stream: `umask 077; cat > <remote-tmp>` reads the local file
// verbatim over the ssh channel). Stdout is captured; stderr is attached to
// the error (fail closed, with the remote's words).
func (r *SSHRunner) RunWithStdin(ctx context.Context, host HostSpec, remoteCmd string, stdin io.Reader) (string, error) {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return "", fmt.Errorf("ssh binary not found on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, SSHArgs(host, remoteCmd)...)
	cmd.Env = r.env()
	cmd.Stdin = stdin
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

// Stream execs the ssh binary with stdio inherited — the interactive
// (deploy shell, ssh passthrough) and follow (deploy logs --follow) paths.
// sshArgs is the FULL argument vector after the binary; build it with
// SSHArgs / SSHInteractiveArgs / SSHLoginArgs / SSHPassthroughArgs. The
// returned code is ssh's exit code; error is reserved for LOCAL failures
// (binary missing). Remote output never passes through kampodra — the ssh
// process owns the terminal, ctrl-c semantics included.
func (r *SSHRunner) Stream(ctx context.Context, sshArgs []string) (int, error) {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return 1, fmt.Errorf("ssh binary not found on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, sshArgs...)
	cmd.Env = r.env()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		// The caller canceled (logs --follow ctrl-c): a clean stop, not a
		// failure — the streamed tail already reached the terminal.
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
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
