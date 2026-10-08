package command

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// The readiness loops must GIVE UP (fail closed) after the configured
// attempts — with a shrunken policy so no test ever waits real seconds.

type failingRunner struct{ calls int }

func (f *failingRunner) Run(context.Context, transport.HostSpec, string) (string, error) {
	f.calls++
	return "", errors.New("unreachable")
}
func (f *failingRunner) RunWithStdin(context.Context, transport.HostSpec, string, io.Reader) (string, error) {
	return "", errors.New("unreachable")
}
func (f *failingRunner) Stream(context.Context, []string) (int, error) {
	return 1, errors.New("unreachable")
}

func TestWaitForSSHGivesUpAfterAttempts(t *testing.T) {
	old := vmPrepareSSHWait
	vmPrepareSSHWait = waitPolicy{attempts: 3, interval: time.Millisecond}
	defer func() { vmPrepareSSHWait = old }()

	runner := &failingRunner{}
	err := waitForSSH(context.Background(), runner, transport.HostSpec{Host: "root@h"}, vmPrepareSSHWait)
	if err == nil || !strings.Contains(err.Error(), "no ssh after") {
		t.Fatalf("waitForSSH err = %v, want the no-ssh-after failure", err)
	}
	if runner.calls != 3 {
		t.Errorf("attempts = %d, want 3", runner.calls)
	}
}

func TestWaitUntilStopsOnSuccess(t *testing.T) {
	old := vmPrepareProxyWait
	vmPrepareProxyWait = waitPolicy{attempts: 5, interval: time.Millisecond}
	defer func() { vmPrepareProxyWait = old }()

	runner := &flakyRunner{failFirst: 2}
	if err := waitUntil(context.Background(), runner, transport.HostSpec{Host: "root@h"},
		vmPrepareProxyWait, "probe", "never"); err != nil {
		t.Fatalf("waitUntil err = %v", err)
	}
	if runner.calls != 3 {
		t.Errorf("calls = %d, want 3 (two failures then success)", runner.calls)
	}
}

type flakyRunner struct {
	failFirst int
	calls     int
}

func (f *flakyRunner) Run(context.Context, transport.HostSpec, string) (string, error) {
	f.calls++
	if f.calls <= f.failFirst {
		return "", errors.New("not yet")
	}
	return "ok", nil
}
func (f *flakyRunner) RunWithStdin(context.Context, transport.HostSpec, string, io.Reader) (string, error) {
	return "", nil
}
func (f *flakyRunner) Stream(context.Context, []string) (int, error) { return 0, nil }
