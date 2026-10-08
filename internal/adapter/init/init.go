// Package init is the service-supervision adapter: one-shot init detection
// (OpenRC vs systemd vs neither — groundwork for non-Alpine targets) and the
// service roll call, ported from the committed kampodine scripts/common.sh
// (init_detect, svc_action) and scripts/status.sh.
package init

import (
	"context"
	"fmt"
	"strings"
)

// System is a detected service supervisor.
type System string

const (
	SystemOpenRC  System = "openrc"
	SystemSystemd System = "systemd"
)

// RunFunc is the remote-execution seam (the shell's `<run-fn>` pattern:
// init_detect receives the script's vm() wrapper). The command layer closes
// it over the transport runner + resolved host; this adapter never imports
// the transport adapter (layering).
type RunFunc func(ctx context.Context, remoteCmd string) (string, error)

// Services is the roll-call list (the shell's status.sh constant).
var Services = []string{"kampodine-api", "kamal-proxy", "walshipper"}

// DetectCommand is the one-shot probe: init system + remote http client in
// a single ssh round-trip (the shell's init_detect probe, byte-equal).
const DetectCommand = `if command -v rc-service >/dev/null 2>&1; then echo openrc; elif command -v systemctl >/dev/null 2>&1; then echo systemd; else echo none; fi; if command -v wget >/dev/null 2>&1; then echo httpc=wget; elif command -v curl >/dev/null 2>&1; then echo httpc=curl; else echo httpc=none; fi`

// Detect probes the host once for its init system. cachedInit is the
// profile's cached verdict (the shell caches into the profile's "init"
// field) — a non-empty cache short-circuits the probe entirely (one
// round-trip ever, per host).
func Detect(ctx context.Context, run RunFunc, cachedInit string) (System, bool, error) {
	if cachedInit != "" {
		switch System(cachedInit) {
		case SystemOpenRC, SystemSystemd:
			return System(cachedInit), true, nil
		}
	}
	out, err := run(ctx, DetectCommand)
	if err != nil {
		return "", false, nil // detection is best-effort; callers degrade
	}
	for _, line := range strings.Split(out, "\n") {
		switch System(strings.TrimSpace(line)) {
		case SystemOpenRC, SystemSystemd:
			return System(strings.TrimSpace(line)), true, nil
		}
	}
	return "", false, nil
}

// StatusProbeCommand ports svc_action's probe verb: the command used inside
// the roll call to test one service.
func StatusProbeCommand(sys System) (string, error) {
	switch sys {
	case SystemOpenRC:
		return "rc-service", nil
	case SystemSystemd:
		return "systemctl", nil
	default:
		return "", fmt.Errorf("no init system detected — cannot probe services")
	}
}

// ActionCommand ports svc_action: the init-aware service command. OpenRC
// output is byte-identical to the historical strings (`rc-service <svc>
// <action>`); systemd is verb-first (`systemctl <action> <svc>`). Fails
// when no init was detected — callers decide whether that is fatal
// (restart: yes, diagnostics: no).
func ActionCommand(sys System, service, action string) (string, error) {
	switch sys {
	case SystemOpenRC:
		return fmt.Sprintf("rc-service %s %s", service, action), nil
	case SystemSystemd:
		return fmt.Sprintf("systemctl %s %s", action, service), nil
	default:
		return "", fmt.Errorf("neither rc-service nor systemctl found — cannot %s %s (host init auto-detection detected neither; groundwork supports openrc + systemd)", action, service)
	}
}

// ServiceState is one roll-call row.
type ServiceState struct {
	Name    string
	Running bool
}

// ServiceStates runs the roll call remotely and parses the states. The
// remote command keeps the historical openrc shape byte-identical and swaps
// the probe verb on systemd hosts — WITH svc_action's argument order:
// `rc-service <svc> status` vs `systemctl status <svc>`.
func ServiceStates(ctx context.Context, run RunFunc, sys System, services []string) ([]ServiceState, error) {
	probe, err := StatusProbeCommand(sys)
	if err != nil {
		return nil, err
	}
	probeExpr := fmt.Sprintf(`%s "$s" status`, probe) // openrc shape
	if sys == SystemSystemd {
		probeExpr = fmt.Sprintf(`%s status "$s"`, probe)
	}
	list := strings.Join(services, " ")
	remote := fmt.Sprintf(`for s in %s; do if %s >/dev/null 2>&1; then echo "$s: running"; else echo "$s: not-running"; fi; done`, list, probeExpr)
	out, err := run(ctx, remote)
	if err != nil {
		return nil, err
	}
	var states []ServiceState
	running := map[string]bool{}
	order := append([]string(nil), services...)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		name, found := strings.CutSuffix(line, ": running")
		if found {
			running[name] = true
		}
	}
	for _, name := range order {
		states = append(states, ServiceState{Name: name, Running: running[name]})
	}
	return states, nil
}
