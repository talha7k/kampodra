package command_test

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/command"
)

// Port of the shell repo's fixture pattern (test/status-ops.test.ts): a
// PATH-shimmed ssh records invocations and serves fixtures, and the live
// edge probe is injected at a dead port — no test ever touches a real
// network or a real VM.

const statusHost = "root@203.0.113.9" // TEST-NET-3, never routed

const (
	statusImages = "aaa1111|2026-10-08 10:00:00 +0000 UTC|936 MB\nbbb2222|2026-10-07 09:00:00 +0000 UTC|936 MB\nccc3333|2026-10-06 08:00:00 +0000 UTC|936 MB\nddd4444|2026-10-05 07:00:00 +0000 UTC|936 MB\nlatest|2026-10-08 10:00:00 +0000 UTC|936 MB"
	statusPS     = "127.0.0.1:5000/kampodine-api:ccc3333\nlocalhost/kamal-proxy:latest"
	statusDF     = "Filesystem     1K-blocks      Used Available Use% Mounted on\n/dev/vda3       82078644  50778644  2973092  62% /"
	statusDFRoot = "Filesystem     1K-blocks      Used Available Use% Mounted on\n/dev/vda1        82078644  12345678  69732966  15% /"
	statusSvcs   = "kampodine-api: running\nkamal-proxy: running\nwalshipper: not-running"
	statusDU     = "4096\t/var/lib/containers\n512\t/data"
)

var statusMetrics = strings.Join([]string{
	"%%KAMPODINE:LOAD%%", "0.52 0.58 0.59 2/123 4567",
	"%%KAMPODINE:CPU%%", "4",
	"%%KAMPODINE:MEM%%", "MemTotal: 1573248 kB\nMemAvailable: 1048576 kB",
	"%%KAMPODINE:UPTIME%%", "1046160.5 2.36",
	"%%KAMPODINE:DISK%%", statusDFRoot,
	"%%KAMPODINE:DU%%", statusDU,
	"%%KAMPODINE:PODMAN%%", "kampodine-api|2.10%|512MiB / 2GiB",
	"%%KAMPODINE:TOP%%", "root 123 0.5 1.2 123456 65432 ? Sl 10:00 0:01 podman serve",
	"%%KAMPODINE:END%%",
}, "\n")

const statusLedger = `{"ts":"2026-10-08T10:05:00Z","host":"root@203.0.113.9","sha":"aaa1111000000000000000000000000000000000","tag":"aaa1111","result":"success","duration_ms":182000,"subject":"feat: ledger merge"}
{"ts":"2026-10-08T11:00:00Z","host":"root@203.0.113.9","sha":"ccc3333000000000000000000000000000000000","tag":"ccc3333","result":"rollback","duration_ms":42000,"subject":"fix: tls edge"}
{"ts":"2026-10-06T08:00:00Z","host":"root@10.0.0.1","sha":"beef888000000000000000000000000000000000","tag":"beef888","result":"success","duration_ms":120000,"subject":"chore: other host"}
`

func systemLookup(key string) (string, bool) { return os.LookupEnv(key) }

// setupStatus builds HOME (ledger), a PATH-shimmed ssh serving fixtures,
// and a prober pointed at a dead port (deterministically unreachable).
func setupStatus(t *testing.T) (deps command.Deps, stubDir string, stdout *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".kampodine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deployments.jsonl"), []byte(statusLedger), 0o600); err != nil {
		t.Fatal(err)
	}

	stubDir = t.TempDir()
	inv := filepath.Join(stubDir, "invocations")
	fixture := func(name, content string) string {
		p := filepath.Join(stubDir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fImages := fixture("images", statusImages)
	fPS := fixture("ps", statusPS)
	fDF := fixture("df", statusDF)
	fSVC := fixture("svcs", statusSvcs)
	fMetrics := fixture("metrics", statusMetrics)

	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"cmd=\"${*: -1}\"\n" +
		"case \"$cmd\" in\n" +
		"  *\"podman images --format\"*) cat '" + fImages + "'; exit 0 ;;\n" +
		"  *\"podman ps --format\"*) cat '" + fPS + "'; exit 0 ;;\n" +
		"  *\"df -P /var/lib/containers\"*) cat '" + fDF + "'; exit 0 ;;\n" +
		"  *\"for s in\"*) cat '" + fSVC + "'; exit 0 ;;\n" +
		"  *\"%%KAMPODINE:LOAD%%\"*) cat '" + fMetrics + "'; exit 0 ;;\n" +
		"  *\"command -v rc-service\"*) printf 'openrc\\nhttpc=wget\\n'; exit 0 ;;\n" +
		"  *) cat >/dev/null 2>&1; exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODINE_HOST", "KAMPODINE_SSH_KEY", "KAMPODINE_PROFILE", "APP_HOST_HEADER"} {
		t.Setenv(k, "")
	}

	out := &bytes.Buffer{}
	return command.Deps{
		Home:   home,
		Env:    systemLookup,
		Stdout: out,
		Stderr: &bytes.Buffer{},
		// Prober at a dead port: deterministic UNREACHABLE, never the real net.
		Prober: &probe.Prober{HTTPClient: &http.Client{Transport: deadTransport{}}},
	}, stubDir, out
}

type deadTransport struct{}

func (deadTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("deterministically unreachable (test)")
}

func runStatus(t *testing.T, deps command.Deps, args ...string) int {
	t.Helper()
	return command.Execute("test", deps, append([]string{"status"}, args...))
}

func TestStatusHostless(t *testing.T) {
	deps, _, stdout := setupStatus(t)
	if code := runStatus(t, deps); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "== deployments (ledger: ") {
		t.Errorf("missing deployments header:\n%s", out)
	}
	if !strings.Contains(out, "Total deployments: 3 · across all hosts — pass --host root@<ip> for VM state") {
		t.Errorf("missing hostless total line:\n%s", out)
	}
	if strings.Contains(out, "== VM (") {
		t.Errorf("hostless run must not print a VM section:\n%s", out)
	}
}

func TestStatusWithHost(t *testing.T) {
	deps, _, stdout := setupStatus(t)
	if code := runStatus(t, deps, "--host", statusHost); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	want := []string{
		"== deployments (ledger: ",
		"Total deployments: 2 · deployments to " + statusHost,
		"== live (through the proxy: https://app.example.com)",
		"UNREACHABLE or unhealthy — check the VM (ssh) and kamal-proxy",
		"== VM (" + statusHost + ")",
		"disk    : 62% used on /var/lib/containers",
		"images  : 4 sha-tagged deploy image(s); prune would remove 1 ddd4444 (~936 MB): kampodine deploy prune --dry-run",
		"services:",
		"kampodine-api: running",
		"kamal-proxy: running",
		"walshipper: not-running",
		"== blue/green pair ==",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q:\n%s", w, out)
		}
	}
}

func TestStatusVerboseMetrics(t *testing.T) {
	deps, _, stdout := setupStatus(t)
	if code := runStatus(t, deps, "--host", statusHost, "--verbose"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	want := []string{
		"== metrics snapshot ==",
		"uptime    : 12d 2h 36m",
		"load      : 0.52 0.58 0.59 (4 CPUs)",
		"memory    : used 512 MiB · avail 1024 MiB · total 1536 MiB",
		"disk /    : 15% used",
		"  images  : 4096 MB (/var/lib/containers)",
		"  data    : 512 MB (/data)",
		"containers:",
		"kampodine-api          2.10%    512MiB / 2GiB",
		"top rss   :",
		"65432 KB  root",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("verbose output missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "WARNING : root disk above 90%") {
		t.Errorf("15%% root disk must not warn:\n%s", out)
	}
}

func TestStatusVerboseWithoutHostHints(t *testing.T) {
	deps, _, stdout := setupStatus(t)
	if code := runStatus(t, deps, "--verbose"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "metrics snapshot: needs a target — add --host root@<ip> (or --profile <name>)") {
		t.Errorf("missing verbose hint:\n%s", stdout.String())
	}
}

func TestStatusDiskWarningAbove90(t *testing.T) {
	deps, stubDir, stdout := setupStatus(t)
	if err := os.WriteFile(filepath.Join(stubDir, "df"), []byte(
		"Filesystem     1K-blocks      Used Available Use% Mounted on\n/dev/vda3       82078644  78783432   1173092  96% /"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := runStatus(t, deps, "--host", statusHost); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, w := range []string{
		"disk    : 96% used on /var/lib/containers",
		"WARNING : VM disk above 90% — old sha-tagged deploy images pile up (~1GB each); reclaim: kampodine deploy prune --dry-run",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
}

func TestStatusSSHFailureDegrades(t *testing.T) {
	deps, stubDir, stdout := setupStatus(t)
	// Remove the ssh shim entirely: every VM read must degrade, not crash.
	if err := os.Remove(filepath.Join(stubDir, "ssh")); err != nil {
		t.Fatal(err)
	}
	if code := runStatus(t, deps, "--host", statusHost); code != 0 {
		t.Fatalf("exit = %d (VM reads degrade; ledger + live sections still print)", code)
	}
	out := stdout.String()
	for _, w := range []string{
		"Total deployments: 2 · deployments to " + statusHost,
		"disk    : unknown (df unreadable)",
		"images  : 0 sha-tagged deploy image(s); prune would remove 0 (none) (~0 MB): kampodine deploy prune --dry-run",
		"(service roll call failed)",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
}

func TestStatusUnknownFlagFails(t *testing.T) {
	deps, _, stderr := setupStatus(t)
	code := runStatus(t, deps, "--bogus")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (die parity)", code)
	}
	depsStderr := deps.Stderr.(*bytes.Buffer)
	if !strings.Contains(depsStderr.String(), "bogus") {
		t.Errorf("stderr must name the offender: %q", stderr.String())
	}
}

func TestStatusHelpMatchesShellUsage(t *testing.T) {
	deps, _, stdout := setupStatus(t)
	if code := runStatus(t, deps, "--help"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	want := []string{
		"Usage:",
		"kampodine status [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--verbose]",
		"Examples:",
		"kampodine status                          # deployments (ledger) + live health + blue/green pair",
		"kampodine status --host root@203.0.113.10 # + VM disk usage, image/prune estimate, service states",
		"kampodine status --profile prod           # resolve host/key from a config profile",
		"Env: APP_HOST_HEADER (default app.example.com), KAMPODINE_HOST,",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("--help missing %q:\n%s", w, out)
		}
	}
}
