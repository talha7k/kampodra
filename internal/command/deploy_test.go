package command_test

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/command"
)

// Ports of the shell repo's deploy-lifecycle contract
// (scripts/deploy-lifecycle.sh + scripts/common.sh): ledger JSONL merge,
// VM sha-tag rows, keep-set prune, init-aware restart, exec/interactive
// shell — fixture-tested through the PATH ssh shim, never a real VM.

const (
	deployContainer = "kampodine-api"
	deployImageRepo = "127.0.0.1:5000/kampodine-api"
)

const deployLedger = `{"ts":"2026-10-08T10:05:00Z","host":"root@203.0.113.9","sha":"aaa1111000000000000000000000000000000000","tag":"aaa1111","result":"success","duration_ms":182000,"subject":"feat: ledger merge"}
{"ts":"2026-10-08T11:00:00Z","host":"root@203.0.113.9","sha":"ccc3333000000000000000000000000000000000","tag":"ccc3333","result":"rollback","duration_ms":42000,"subject":"fix: tls edge"}
{"ts":"2026-10-04T09:00:00Z","host":"root@203.0.113.9","sha":"eee5555000000000000000000000000000000000","tag":"eee5555","result":"rollback","duration_ms":42000,"subject":"vanished from VM"}
{"ts":"2026-10-06T08:00:00Z","host":"root@10.0.0.1","sha":"beef888000000000000000000000000000000000","tag":"beef888","result":"success","duration_ms":120000,"subject":"chore: other host"}
`

func setupDeploy(t *testing.T) (deps command.Deps, stubDir string, stdout, stderr *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".kampodra")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deployments.jsonl"), []byte(deployLedger), 0o600); err != nil {
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
	fLogs := fixture("logs", "10:00:00 starting api\n10:00:01 listening on :8080\n")
	fSvcs := fixture("svcs", "kampodine-api: running\n")

	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"cmd=\"${*: -1}\"\n" +
		"case \"$cmd\" in\n" +
		"  *\"podman images --format\"*) cat '" + fImages + "'; exit 0 ;;\n" +
		"  *\"podman ps --format\"*) cat '" + fPS + "'; exit 0 ;;\n" +
		"  *\"podman images >/dev/null\"*) exit 0 ;;\n" +
		"  *\"podman rmi\"*) exit 0 ;;\n" +
		"  *\"podman logs -f\"*) printf 'stream tick\\n'; exit 0 ;;\n" +
		"  *\"podman logs\"*) cat '" + fLogs + "'; exit 0 ;;\n" +
		"  *\"df -P /var/lib/containers\"*) cat '" + fDF + "'; exit 0 ;;\n" +
		"  *\"command -v rc-service\"*) printf 'openrc\\nhttpc=wget\\n'; exit 0 ;;\n" +
		"  *\"rc-service " + deployContainer + " restart\"*) exit 0 ;;\n" +
		"  *\"rc-service " + deployContainer + " status\"*) cat '" + fSvcs + "'; exit 0 ;;\n" +
		"  *\"for s in\"*) cat '" + fSvcs + "'; exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_PROXY_HOST"} {
		t.Setenv(k, "")
	}

	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	return command.Deps{
		Home:   home,
		Env:    systemLookup,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
		Prober: &probe.Prober{HTTPClient: &http.Client{Transport: deadTransport{}}},
	}, stubDir, stdout, stderr
}

func runDeploy(t *testing.T, deps command.Deps, args ...string) int {
	t.Helper()
	return command.Execute("test", deps, append([]string{"deploy"}, args...))
}

func invocations(t *testing.T, stubDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stubDir, "invocations"))
	if err != nil {
		t.Fatalf("shim was never invoked: %v", err)
	}
	return string(data)
}

func TestDeployListMergesVMTagsWithLedger(t *testing.T) {
	deps, _, stdout, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "list"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	want := []string{
		"== deployment history for " + statusHost + " (ledger: ",
		"TAG       CREATED           STATE     RESULT    SUBJECT",
		// VM rows, newest first; the running one marked; ledger enrichment.
		"aaa1111   2026-10-08 10:00  on VM     success   feat: ledger merge",
		"bbb2222   2026-10-07 09:00  on VM     -         -",
		"ccc3333   2026-10-06 08:00  RUNNING   rollback  fix: tls edge",
		"ddd4444   2026-10-05 07:00  on VM     -         -",
		// Ledger-only row (pruned on the VM), and only for THIS host. The
		// full ISO ts overflows its %-17s column — exactly one separator
		// space after it, like the shell printf.
		"eee5555   2026-10-04T09:00:00Z not on VM rollback  vanished from VM",
		// Total = rendered rows (VM truth merged with ledger), host-scoped.
		"Total deployments: 5 · on " + statusHost,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("deploy list output missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "beef888") {
		t.Errorf("deploy list leaked another host's ledger row:\n%s", out)
	}
}

func TestDeployListVMUnreachableDegradesToLedgerOnly(t *testing.T) {
	deps, stubDir, stdout, _ := setupDeploy(t)
	// Fail-fast ssh (exit 255 = ssh's own connection failure code): every VM
	// read degrades instantly, no test time spent on the unroutable host.
	shim := "#!/bin/sh\nexit 255\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runDeploy(t, deps, "--host", statusHost, "list"); code != 0 {
		t.Fatalf("exit = %d (VM unreachable degrades, never crashes)", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"WARNING: cannot reach VM " + statusHost + " — showing ledger-only history (no VM truth)",
		"not on VM",
		"Total deployments: 3 · on " + statusHost,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ledger-only view missing %q:\n%s", want, out)
		}
	}
}

func TestDeployListWithoutTargetFails(t *testing.T) {
	deps, _, _, stderr := setupDeploy(t)
	if code := runDeploy(t, deps, "list"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "target required") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestDeployListAllProfilesRendersPerProfileSections(t *testing.T) {
	deps, _, stdout, _ := setupDeploy(t)
	cfgDir := filepath.Join(deps.Home, ".kampodra")
	cfg := `{"defaultProfile": "prod", "profiles": {"prod": {"host": "` + statusHost + `", "group": "edge"}, "staging": {"host": "root@198.51.100.7"}}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDeploy(t, deps, "list", "--all-profiles"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"== profile: prod ==", "== profile: staging ==",
		"== deployment history for " + statusHost,
		"== deployment history for root@198.51.100.7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--all-profiles missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "== profile: prod ==") > strings.Index(out, "== profile: staging ==") {
		t.Errorf("profiles must render in sorted order:\n%s", out)
	}
}

func TestDeployPruneDryRunPrintsPlanRemovesNothing(t *testing.T) {
	deps, stubDir, stdout, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "prune", "--dry-run"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"plan: 1 image(s), ~936 MB reclaimed (keep: running + ts-rollback + newest 2)",
		"  podman rmi " + deployImageRepo + ":ddd4444   # 936 MB",
		"dry-run: nothing removed — re-run without --dry-run to apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run plan missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(invocations(t, stubDir), "\n") {
		if strings.Contains(line, "podman rmi") {
			t.Errorf("dry-run must not remove anything, but the shim saw: %s", line)
		}
	}
}

func TestDeployPruneAppliesAndReportsDisk(t *testing.T) {
	deps, stubDir, stdout, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "prune"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	inv := invocations(t, stubDir)
	if !strings.Contains(inv, deployImageRepo+":ddd4444") {
		t.Errorf("prune never removed the selected image:\n%s", inv)
	}
	if strings.Contains(inv, deployImageRepo+":ccc3333") {
		t.Errorf("prune touched the RUNNING image:\n%s", inv)
	}
	if out := stdout.String(); !strings.Contains(out, "VM disk after prune: 62% used on /var/lib/containers") {
		t.Errorf("prune must report the post-prune disk:\n%s", out)
	}
}

func TestDeployPruneRefusesBlindWhenSSHFailed(t *testing.T) {
	deps, stubDir, _, stderr := setupDeploy(t)
	// Fail-fast ssh: the image listing never arrives.
	shim := "#!/bin/sh\nexit 255\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runDeploy(t, deps, "--host", statusHost, "prune"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "refusing to prune blind") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestDeployPruneValidatesKeep(t *testing.T) {
	deps, _, _, stderr := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "prune", "--keep", "abc"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--keep must be a non-negative integer") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestDeployPruneNothingToPrune(t *testing.T) {
	deps, _, stdout, _ := setupDeploy(t)
	// keep 9 spares all four sha images (running ccc3333 + newest 3): the
	// head -n N semantics that the slice-bound bug used to panic on.
	if code := runDeploy(t, deps, "--host", statusHost, "prune", "--keep", "9"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if out := stdout.String(); !strings.Contains(out, "nothing to prune (keep set: running + ts-rollback + newest 9)") {
		t.Errorf("nothing-to-prune message missing:\n%s", out)
	}
}

func TestDeployLogsTailsDefaultAndCustomLines(t *testing.T) {
	deps, stubDir, stdout, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "logs"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if inv := invocations(t, stubDir); !strings.Contains(inv, "podman logs --tail 100 "+deployContainer) {
		t.Errorf("logs default --lines 100 not sent:\n%s", inv)
	}
	if out := stdout.String(); !strings.Contains(out, "listening on :8080") {
		t.Errorf("logs output missing fixture content:\n%s", out)
	}

	deps2, stubDir2, _, _ := setupDeploy(t)
	if code := runDeploy(t, deps2, "--host", statusHost, "logs", "--lines", "50"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if inv := invocations(t, stubDir2); !strings.Contains(inv, "podman logs --tail 50 "+deployContainer) {
		t.Errorf("logs --lines 50 not sent:\n%s", inv)
	}
}

func TestDeployLogsFollowStreams(t *testing.T) {
	deps, stubDir, _, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "logs", "--lines", "20", "--follow"); code != 0 {
		t.Fatalf("exit = %d, want 0 (follow exits cleanly when the stream ends)", code)
	}
	if inv := invocations(t, stubDir); !strings.Contains(inv, "podman logs -f --tail 20 "+deployContainer) {
		t.Errorf("logs --follow must stream (podman logs -f):\n%s", inv)
	}
}

func TestDeployLogsValidatesLines(t *testing.T) {
	deps, _, _, stderr := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "logs", "--lines", "0"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "--lines must be a positive integer") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestDeployRestartOpenRC(t *testing.T) {
	deps, stubDir, stdout, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "restart"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	inv := invocations(t, stubDir)
	if !strings.Contains(inv, "rc-service "+deployContainer+" restart") {
		t.Errorf("restart never ran the openrc action:\n%s", inv)
	}
	if !strings.Contains(inv, "rc-service "+deployContainer+" status") {
		t.Errorf("restart never checked the service status:\n%s", inv)
	}
	out := stdout.String()
	for _, want := range []string{
		"rc-service " + deployContainer + " restart (deploy's restart path:",
		"service status:",
		"done — verify: kampodra deploy logs --lines 50",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("restart output missing %q:\n%s", want, out)
		}
	}
}

func TestDeployRestartWithoutInitDies(t *testing.T) {
	deps, stubDir, _, stderr := setupDeploy(t)
	// A host with neither supervisor: the probe shim answers nothing.
	shim := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runDeploy(t, deps, "--host", statusHost, "restart"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cannot restart") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestDeployShellInteractiveStreams(t *testing.T) {
	deps, stubDir, stdout, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "shell"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	inv := invocations(t, stubDir)
	if !strings.Contains(inv, "-t "+statusHost+" podman exec -it "+deployContainer+" sh") {
		t.Errorf("interactive shell must ssh with -t into the container:\n%s", inv)
	}
	if out := stdout.String(); !strings.Contains(out, "one-shot instead: kampodra deploy shell exec -- <cmd>") {
		t.Errorf("interactive banner missing the one-shot hint:\n%s", out)
	}
}

func TestDeployShellExecOneShot(t *testing.T) {
	deps, stubDir, _, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "shell", "exec", "--", "df", "-h", "/data"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	inv := invocations(t, stubDir)
	if !strings.Contains(inv, "podman exec "+deployContainer+" 'df' '-h' '/data'") {
		t.Errorf("one-shot exec must sh-quote the argv (shell parity):\n%s", inv)
	}
}

func TestDeployShellExecRequiresDashDash(t *testing.T) {
	deps, _, _, stderr := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "shell", "exec", "df"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "shell exec requires -- before the command") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestDeployShellExecRequiresACommand(t *testing.T) {
	deps, _, _, stderr := setupDeploy(t)
	if code := runDeploy(t, deps, "--host", statusHost, "shell", "exec", "--"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "shell exec -- requires a command") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestDeployHelpDocumentsLifecycle(t *testing.T) {
	deps, _, stdout, _ := setupDeploy(t)
	if code := runDeploy(t, deps, "--help"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"deploy list", "deploy prune", "deploy logs", "deploy restart", "deploy shell",
		"--keep N", "--dry-run", "--lines N", "--follow",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy --help missing %q:\n%s", want, out)
		}
	}
}
