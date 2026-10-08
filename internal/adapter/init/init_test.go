package init

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingRun is a fake RunFunc: it records the remote command and serves
// a fixture — no ssh, no PATH shims needed at this layer (the ssh seam
// itself is exercised in the transport and command-level tests).
type recordingRun struct {
	commands []string
	output   string
	err      error
}

func (r *recordingRun) run(_ context.Context, remoteCmd string) (string, error) {
	r.commands = append(r.commands, remoteCmd)
	return r.output, r.err
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name     string
		probeOut string
		probeErr error
		wantInit System
		wantOK   bool
	}{
		{name: "alpine host", probeOut: "openrc\nhttpc=wget\n", wantInit: SystemOpenRC, wantOK: true},
		{name: "systemd host", probeOut: "systemd\nhttpc=curl\n", wantInit: SystemSystemd, wantOK: true},
		{name: "unknown init", probeOut: "none\nhttpc=none\n", wantOK: false},
		{name: "probe failure degrades", probeErr: errors.New("ssh down"), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &recordingRun{output: tt.probeOut, err: tt.probeErr}
			got, ok, err := Detect(context.Background(), run.run, "")
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("Detect() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.wantInit {
				t.Errorf("Detect() = %q, want %q", got, tt.wantInit)
			}
			if tt.probeErr == nil && len(run.commands) != 1 || tt.probeErr != nil && len(run.commands) != 1 {
				t.Errorf("Detect() must probe exactly once, commands = %v", run.commands)
			}
		})
	}
}

func TestDetectUsesTheShellProbeCommand(t *testing.T) {
	run := &recordingRun{output: "openrc\nhttpc=wget\n"}
	Detect(context.Background(), run.run, "")
	if len(run.commands) != 1 || !strings.Contains(run.commands[0], "command -v rc-service") ||
		!strings.Contains(run.commands[0], "command -v systemctl") {
		t.Errorf("probe command lost the detection ladder: %v", run.commands)
	}
}

func TestDetectPrefersProfileCache(t *testing.T) {
	run := &recordingRun{output: "openrc\n"}
	got, ok, err := Detect(context.Background(), run.run, "systemd")
	if err != nil || !ok || got != SystemSystemd {
		t.Fatalf("Detect(cached=systemd) = %q ok=%v err=%v", got, ok, err)
	}
	if len(run.commands) != 0 {
		t.Error("cached verdict must short-circuit the probe entirely (one round-trip ever)")
	}
}

func TestServiceStatesRemoteCommand(t *testing.T) {
	rollCall := []string{"kampodine-api", "kamal-proxy", "walshipper"} // the project default roll call
	run := &recordingRun{output: "kampodine-api: running\nkamal-proxy: not-running\nwalshipper: not-running\n"}
	states, err := ServiceStates(context.Background(), run.run, SystemOpenRC, rollCall)
	if err != nil {
		t.Fatalf("ServiceStates() error = %v", err)
	}
	want := []ServiceState{
		{Name: "kampodine-api", Running: true},
		{Name: "kamal-proxy", Running: false},
		{Name: "walshipper", Running: false},
	}
	if states[0] != want[0] || states[1] != want[1] || states[2] != want[2] {
		t.Errorf("ServiceStates() = %+v, want %+v", states, want)
	}
	if len(run.commands) != 1 {
		t.Fatalf("expected one roll call, got %v", run.commands)
	}
	// The historical openrc shape is a pinned contract (byte-identical
	// "for s in … rc-service …" — the shell comment promises it).
	wantCmd := `for s in kampodine-api kamal-proxy walshipper; do if rc-service "$s" status >/dev/null 2>&1; then echo "$s: running"; else echo "$s: not-running"; fi; done`
	if run.commands[0] != wantCmd {
		t.Errorf("openrc roll call drifted:\n got %s\nwant %s", run.commands[0], wantCmd)
	}
}

func TestServiceStatesSystemdProbe(t *testing.T) {
	rollCall := []string{"kampodine-api", "kamal-proxy", "walshipper"}
	run := &recordingRun{output: "kampodine-api: running\nkamal-proxy: not-running\nwalshipper: not-running\n"}
	states, err := ServiceStates(context.Background(), run.run, SystemSystemd, rollCall)
	if err != nil {
		t.Fatalf("ServiceStates() error = %v", err)
	}
	if len(run.commands) != 1 || !strings.Contains(run.commands[0], `systemctl status "$s"`) {
		t.Errorf("systemd hosts must probe with systemctl (svc_action arg order): %v", run.commands)
	}
	if states[1].Name != "kamal-proxy" || states[1].Running {
		t.Errorf("states[1] = %+v, want kamal-proxy not-running", states[1])
	}
}

func TestServiceStatesFailsClosedOnSSHFailure(t *testing.T) {
	rollCall := []string{"kampodine-api", "kamal-proxy", "walshipper"}
	run := &recordingRun{err: errors.New("ssh down")}
	if _, err := ServiceStates(context.Background(), run.run, SystemOpenRC, rollCall); err == nil {
		t.Fatal("ServiceStates() must fail closed when the VM is unreachable (caller prints '(service roll call failed)')")
	}
}

func TestServiceStatesUnknownInitFailsClosed(t *testing.T) {
	rollCall := []string{"kampodine-api", "kamal-proxy", "walshipper"}
	run := &recordingRun{}
	if _, err := ServiceStates(context.Background(), run.run, System(""), rollCall); err == nil {
		t.Fatal("no detected init must fail (svc_action parity) — callers degrade")
	}
	if len(run.commands) != 0 {
		t.Error("no ssh round-trip may happen without a detected init")
	}
}

func TestStatusProbeCommand(t *testing.T) {
	if got, err := StatusProbeCommand(SystemOpenRC); err != nil || got != "rc-service" {
		t.Errorf("StatusProbeCommand(openrc) = %q, %v", got, err)
	}
	if got, err := StatusProbeCommand(SystemSystemd); err != nil || got != "systemctl" {
		t.Errorf("StatusProbeCommand(systemd) = %q, %v", got, err)
	}
	if _, err := StatusProbeCommand(System("")); err == nil {
		t.Error("StatusProbeCommand(unknown) must fail (svc_action parity)")
	}
}

func TestActionCommand(t *testing.T) {
	tests := []struct {
		name    string
		sys     System
		service string
		action  string
		want    string
		wantErr bool
	}{
		{name: "openrc keeps the historical byte shape", sys: SystemOpenRC, service: "kampodine-api", action: "restart", want: "rc-service kampodine-api restart"},
		{name: "systemd uses the verb-first order", sys: SystemSystemd, service: "kampodine-api", action: "restart", want: "systemctl restart kampodine-api"},
		{name: "openrc status probe", sys: SystemOpenRC, service: "kamal-proxy", action: "status", want: "rc-service kamal-proxy status"},
		{name: "no init detected fails (restart is fatal)", sys: System(""), service: "x", action: "restart", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ActionCommand(tt.sys, tt.service, tt.action)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ActionCommand() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ActionCommand() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("ActionCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}
