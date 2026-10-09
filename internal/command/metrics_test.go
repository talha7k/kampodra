package command_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// metrics.sh port: ONE-shot VM snapshot over SSH (load, memory, disk
// breakdown, containers, top procs) — the rendering contract lives in the
// osfacts adapter tests; here we pin the command shape: resolution ladder,
// --disk-threshold gating, --watch/--count loop.

func metricsFixture(diskPct int) string {
	tmpl := `%%KAMPODRA:LOAD%%
0.52 0.58 0.59 2/320 12345
%%KAMPODRA:CPU%%
4
%%KAMPODRA:MEM%%
MemTotal: 3990000 kB
MemAvailable: 2100000 kB
%%KAMPODRA:UPTIME%%
123456.78 98765.43
%%KAMPODRA:DISK%%
/dev/vda1 8378360 7700000 678360 %DISKPCT%% /
%%KAMPODRA:DU%%
1234 /var/lib/containers
567 /data
%%KAMPODRA:PODMAN%%
kampodine-api|1.5%|492.1MB / 1GB
%%KAMPODRA:TOP%%
root     12345  0.0 12.3 123456 789012 ? Sl 10:00 0:10 podman run --name api
%%KAMPODRA:END%%
`
	return strings.ReplaceAll(tmpl, "%DISKPCT%", fmt.Sprint(diskPct))
}

func setupMetrics(t *testing.T, fixture string) (command.Deps, *bytes.Buffer, *bytes.Buffer, func() int) {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	inv := filepath.Join(stub, "invocations")
	fixturePath := filepath.Join(stub, "snapshot")
	os.WriteFile(fixturePath, []byte(fixture), 0o600)
	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"cat '" + fixturePath + "'\n"
	os.WriteFile(filepath.Join(stub, "ssh"), []byte(shim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE"} {
		t.Setenv(k, "")
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	deps := command.Deps{
		Home:   home,
		Env:    systemLookup,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
	}
	count := func() int {
		data, _ := os.ReadFile(inv)
		n := 0
		for _, l := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(l) != "" {
				n++
			}
		}
		return n
	}
	return deps, stdout, stderr, count
}

func runMetrics(t *testing.T, deps command.Deps, args ...string) int {
	t.Helper()
	return command.Execute("test", deps, append([]string{"metrics"}, args...))
}

func TestMetricsOneShotRendersSnapshot(t *testing.T) {
	deps, stdout, stderr, count := setupMetrics(t, metricsFixture(72))
	if code := runMetrics(t, deps, "--host", "root@203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"[metrics] == metrics (root@203.0.113.9) ==",
		"uptime    : 1d 10h 17m",
		"load      : 0.52 0.58 0.59 (4 CPUs)",
		"memory    : used 1846 MiB · avail 2050 MiB · total 3896 MiB",
		"disk /    : 72% used",
		"kampodine-api",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q:\n%s", want, out)
		}
	}
	if count() != 1 {
		t.Errorf("one-shot must run exactly one snapshot, got %d", count())
	}
}

func TestMetricsWarnDiskBelowThresholdPasses(t *testing.T) {
	deps, _, stderr, _ := setupMetrics(t, metricsFixture(72))
	if code := runMetrics(t, deps, "--host", "root@h", "--disk-threshold", "90"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
}

func TestMetricsWarnDiskAtThresholdFails(t *testing.T) {
	deps, stdout, _, _ := setupMetrics(t, metricsFixture(90))
	if code := runMetrics(t, deps, "--host", "root@h", "--disk-threshold", "90"); code != 1 {
		t.Fatalf("exit = %d, want 1 (>= --disk-threshold)", code)
	}
	if !strings.Contains(stdout.String(), "free space first") || !strings.Contains(stdout.String(), "90%") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestMetricsAlwaysWarnsAbove90(t *testing.T) {
	deps, stdout, _, _ := setupMetrics(t, metricsFixture(92))
	if code := runMetrics(t, deps, "--host", "root@h", "--disk-threshold", "95"); code != 0 {
		t.Fatalf("exit = %d (92 < 95)", code)
	}
	if !strings.Contains(stdout.String(), "old sha-tagged deploy images pile up") {
		t.Errorf("the always-on >90 warning is missing:\n%s", stdout.String())
	}
}

func TestMetricsWarnDiskValidation(t *testing.T) {
	for _, bad := range []string{"0", "101", "abc", ""} {
		deps, _, stderr, _ := setupMetrics(t, metricsFixture(72))
		if code := runMetrics(t, deps, "--host", "root@h", "--disk-threshold", bad); code != 1 {
			t.Errorf("--disk-threshold %q: exit = %d, want 1", bad, code)
		}
		if !strings.Contains(stderr.String(), "--disk-threshold must be a percentage 1-100") {
			t.Errorf("--disk-threshold %q: stderr = %q", bad, stderr.String())
		}
	}
}

func TestMetricsRequiresTarget(t *testing.T) {
	deps, _, stderr, _ := setupMetrics(t, metricsFixture(72))
	if code := runMetrics(t, deps); code != 1 || !strings.Contains(stderr.String(), "target required") {
		t.Errorf("exit=%d stderr=%q", code, stderr.String())
	}
}

func TestMetricsCountBoundsIterations(t *testing.T) {
	deps, _, stderr, count := setupMetrics(t, metricsFixture(72))
	if code := runMetrics(t, deps, "--host", "root@h", "--watch", "1", "--count", "2"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if got := count(); got != 2 {
		t.Errorf("snapshots = %d, want 2 (--count bound)", got)
	}
}

func TestMetricsWatchAndCountValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--host", "root@h", "--watch", "0"},
		{"--host", "root@h", "--watch", "-3"},
		{"--host", "root@h", "--count", "0"},
	} {
		deps, _, stderr, _ := setupMetrics(t, metricsFixture(72))
		if code := runMetrics(t, deps, args...); code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
		if !strings.Contains(stderr.String(), "positive") {
			t.Errorf("%v: stderr = %q", args, stderr.String())
		}
	}
}
