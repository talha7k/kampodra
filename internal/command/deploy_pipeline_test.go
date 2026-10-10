package command_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/command"
)

// The deploy build/stream pipeline tests: exec-shim/fixture pattern like
// the lifecycle tests, extended with local `git` + `podman` shims (the
// pipeline runs git and podman on the deploy machine and ssh on the VM —
// all three record into ONE invocations file, so ORDER pins span the whole
// pipeline). No test touches a real VM, podman machine, or network.

const (
	pipelineVer      = "abc1234"
	pipelineSubject  = "feat: deploy pipeline"
	pipelineImageTag = deployImageRepo + ":" + pipelineVer
	pipelineProxyCmd = "podman exec kamal-proxy kamal-proxy deploy " + deployContainer
)

// The remote env/stamp/health paths render from the project defaults —
// never hardcoded here (project.go is the ONE source of the naming).
var (
	pipelineEnvFile    = project.LoadDefault().EnvFile
	pipelineStamp      = project.LoadDefault().DeployedShaFile
	pipelineHealthPath = project.LoadDefault().HealthPath
)

// setupDeployPipeline builds the full shim harness: PATH-shimmed ssh, git,
// and podman; fixtures for every fixture-fed read; control files for
// per-test failure injection (`failon` = substring → that step exits 1).
func setupDeployPipeline(t *testing.T) (deps command.Deps, stubDir string, stdout, stderr *bytes.Buffer, reporoot string) {
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
	reporoot = filepath.Join(stubDir, "repo")
	if err := os.MkdirAll(reporoot, 0o700); err != nil {
		t.Fatal(err)
	}
	inv := filepath.Join(stubDir, "invocations")

	fixture := func(name, content string) string {
		p := filepath.Join(stubDir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	fixture("images", "abc1234|2026-10-08 12:00:00 +0000 UTC|936 MB\nbbb2222|2026-10-07 09:00:00 +0000 UTC|936 MB\nccc3333|2026-10-06 08:00:00 +0000 UTC|936 MB\nddd4444|2026-10-05 07:00:00 +0000 UTC|936 MB\nlatest|2026-10-08 12:00:00 +0000 UTC|936 MB")
	fixture("ps", deployImageRepo+":ccc3333\nlocalhost/kamal-proxy:latest")
	fixture("psnames", "kamal-proxy\n")
	fixture("psanames", "kamal-proxy\n")
	fixture("df", statusDF)
	fixture("logs", "10:00:00 starting api\n10:00:01 listening on :8080\n")
	fixture("svcs", deployContainer+": running\n")
	fixture("health", `{"ok":true,"git":"`+pipelineVer+`"}`)
	fixture("served-sha", pipelineVer)
	fixture("proxy-ls", deployContainer+"-shadow:8080")
	fixture("stamp", "ccc3333\n")
	fixture("shadowimg", deployImageRepo+":"+pipelineVer)
	fixture("gitsha", pipelineVer+"\n")
	fixture("gitstatus", "")
	fixture("gitsubject", pipelineSubject+"\n")
	fixture("vmimageexists", "0")
	fixture("localimageexists", "1")
	fixture("failon", "")
	stampFixture := fixture("reporoot", reporoot)

	// The smoke handler and the VM-side health fetch both read the
	// served-sha fixture — one knob flips every health outcome per test:
	// rewrite the `health` + `served-sha` fixtures via writeFixture.

	// The smoke handler reads the served-sha fixture per request, so tests
	// flip the public edge's answer by rewriting one file.
	smokeMux := http.NewServeMux()
	readStub := func(name string) string {
		data, err := os.ReadFile(filepath.Join(stubDir, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	smokeMux.HandleFunc(pipelineHealthPath, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"git":"%s"}`, readStub("served-sha"))
	})
	// The plain /up liveness probe is a separate route — unless the health
	// path IS /up, in which case the health payload doubles as liveness.
	if pipelineHealthPath != "/up" {
		smokeMux.HandleFunc("/up", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	}
	smokeMux.HandleFunc("/build-id.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s\n", readStub("served-sha"))
	})

	sshShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + inv + "'\n" +
		"cmd=\"${*: -1}\"\n" +
		"failon=\"$(cat '" + filepath.Join(stubDir, "failon") + "' 2>/dev/null)\"\n" +
		"if [ -n \"$failon\" ] && [[ \"$cmd\" == *\"$failon\"* ]]; then exit 1; fi\n" +
		"case \"$cmd\" in\n" +
		"  *\"cat > \"*) cat > '" + filepath.Join(stubDir, "stdin-capture") + "'; exit 0 ;;\n" +
		"  *\"podman images --format\"*) cat '" + filepath.Join(stubDir, "images") + "'; exit 0 ;;\n" +
		"  *\"podman ps -a --format '{{.Names}}'\"*) cat '" + filepath.Join(stubDir, "psanames") + "'; exit 0 ;;\n" +
		"  *\"podman ps --format '{{.Names}}'\"*) cat '" + filepath.Join(stubDir, "psnames") + "'; exit 0 ;;\n" +
		"  *\"podman ps --format '{{.Image}}'\"*) cat '" + filepath.Join(stubDir, "ps") + "'; exit 0 ;;\n" +
		"  *\"podman image exists\"*) exit \"$(cat '" + filepath.Join(stubDir, "vmimageexists") + "')\" ;;\n" +
		"  *\"podman image prune\"*) exit 0 ;;\n" +
		"  *\"podman rmi\"*) exit 0 ;;\n" +
		"  *\"podman rm -f\"*) exit 0 ;;\n" +
		"  *\"podman run -d\"*) exit 0 ;;\n" +
		"  *\"podman tag\"*) exit 0 ;;\n" +
		"  *\"podman load\"*) exit 0 ;;\n" +
		"  *\"podman inspect\"*) cat '" + filepath.Join(stubDir, "shadowimg") + "'; exit 0 ;;\n" +
		"  *\"podman logs -f\"*) printf 'stream tick\\n'; exit 0 ;;\n" +
		"  *\"podman logs\"*) cat '" + filepath.Join(stubDir, "logs") + "'; exit 0 ;;\n" +
		"  *\"df -P /var/lib/containers\"*) cat '" + filepath.Join(stubDir, "df") + "'; exit 0 ;;\n" +
		"  *deployed-sha*) cat '" + filepath.Join(stubDir, "stamp") + "' 2>/dev/null; exit $? ;;\n" +
		"  *\"command -v rc-service\"*) printf 'openrc\\nhttpc=wget\\n'; exit 0 ;;\n" +
		"  *\"rc-service " + deployContainer + "\"*) exit 0 ;;\n" +
		"  *\"kamal-proxy ls\"*) cat '" + filepath.Join(stubDir, "proxy-ls") + "'; exit 0 ;;\n" +
		"  *\"kamal-proxy deploy\"*) exit 0 ;;\n" +
		"  *http://127.0.0.1:*) cat '" + filepath.Join(stubDir, "health") + "'; exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"

	gitShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"git $*\" >> '" + inv + "'\n" +
		"if [ -f '" + filepath.Join(stubDir, "norepo") + "' ]; then exit 1; fi\n" +
		"case \"$1\" in\n" +
		"  rev-parse) case \"$2\" in\n" +
		"    --show-toplevel) cat '" + stampFixture + "' ;;\n" +
		"    --short) cat '" + filepath.Join(stubDir, "gitsha") + "' ;;\n" +
		"  esac ;;\n" +
		"  status) cat '" + filepath.Join(stubDir, "gitstatus") + "' ;;\n" +
		"  log) cat '" + filepath.Join(stubDir, "gitsubject") + "' ;;\n" +
		"esac\n"

	podmanShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"podman $*\" >> '" + inv + "'\n" +
		"failon=\"$(cat '" + filepath.Join(stubDir, "failon") + "' 2>/dev/null)\"\n" +
		"if [ -n \"$failon\" ] && [[ \"podman $*\" == *\"$failon\"* ]]; then exit 1; fi\n" +
		"case \"$1\" in\n" +
		"  info) exit 0 ;;\n" +
		"  build) exit 0 ;;\n" +
		"  save) printf 'fake-image-layer-bytes\\n' ;;\n" +
		"  image) if [ \"$2\" = exists ]; then exit \"$(cat '" + filepath.Join(stubDir, "localimageexists") + "')\"; fi ;;\n" +
		"esac\n"

	for name, content := range map[string]string{"ssh": sshShim, "git": gitShim, "podman": podmanShim} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_PROXY_HOST",
		"KAMPODRA_CONTAINER", "KAMPODRA_IMAGE_PREFIX", "KAMPODRA_HEALTH_PATH", "KAMPODRA_PORT",
		"KAMPODRA_NETWORK", "KAMPODRA_SHADOW_PROBE_PORT", "KAMPODRA_DEPLOYED_SHA_FILE", "KAMPODRA_ENV_FILE"} {
		t.Setenv(k, "")
	}

	fastBudgets(t)

	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	return command.Deps{
		Home:   home,
		Env:    systemLookup,
		Stdout: stdout,
		Stderr: stderr,
		Stdin:  strings.NewReader(""),
		Prober: &probe.Prober{HTTPClient: &http.Client{Transport: routeTransport{smokeMux}}},
	}, stubDir, stdout, stderr, reporoot
}

// setupPipeline is the 4-value form of the harness (reporoot dropped) for
// tests that don't pin the build-context path.
func setupPipeline(t *testing.T) (deps command.Deps, stubDir string, stdout, stderr *bytes.Buffer) {
	t.Helper()
	deps, stubDir, stdout, stderr, _ = setupDeployPipeline(t)
	return deps, stubDir, stdout, stderr
}

// fastBudgets shrinks the health-gate and drain waits to test scale (the
// exported budget vars exist ONLY for this).
func fastBudgets(t *testing.T) {
	t.Helper()
	oldAttempts, oldSleep, oldPoll := command.DeployGateAttempts, command.DeployGateSleep, command.DeployDrainPollInterval
	command.DeployGateAttempts = 2
	command.DeployGateSleep = 0
	command.DeployDrainPollInterval = time.Millisecond
	t.Cleanup(func() {
		command.DeployGateAttempts, command.DeployGateSleep, command.DeployDrainPollInterval = oldAttempts, oldSleep, oldPoll
	})
}

// routeTransport serves every request from an in-memory handler — the
// public smoke never leaves the process.
type routeTransport struct{ handler http.Handler }

func (t routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func writeFixture(t *testing.T, stubDir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stubDir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// indexOf returns the byte offset of substr in s (-1 when absent) — the
// primitive behind every ORDER pin.
func indexOf(s, substr string) int { return strings.Index(s, substr) }

func mustContain(t *testing.T, where, what string) {
	t.Helper()
	if !strings.Contains(where, what) {
		t.Errorf("missing %q in:\n%s", what, where)
	}
}

func TestDeployPipelineHappyPathSequencePinned(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)

	// The full argv pins — build identity, sha tag, stream, retag, restart,
	// health, proxy re-point.
	mustContain(t, inv, "git rev-parse --show-toplevel")
	mustContain(t, inv, "git status --porcelain")
	mustContain(t, inv, "podman info")
	mustContain(t, inv, "podman build --platform linux/arm64 -f Dockerfile --build-arg GIT_SHA="+pipelineVer+" -t "+pipelineImageTag+" "+reporoot)
	mustContain(t, inv, "podman save --format docker-archive "+pipelineImageTag)
	mustContain(t, inv, "podman load")
	mustContain(t, inv, "podman tag "+pipelineImageTag+" "+deployImageRepo+":latest")
	mustContain(t, inv, "rc-service "+deployContainer+" restart")
	mustContain(t, inv, "http://127.0.0.1:8080"+pipelineHealthPath)
	mustContain(t, inv, pipelineProxyCmd+" --host app.example.com --target "+deployContainer+":8080 --tls --health-check-path "+pipelineHealthPath)

	// ORDER pins: gate before build; both stream legs after the build (save
	// and load are CONCURRENT — a pipe — so their log lines race and may
	// legitimately land in either order); retag after the stream; restart
	// after retag; health after restart; proxy after health; cleanup rmi
	// after the proxy switch; stamp after cleanup.
	order := [][]string{
		{"git status --porcelain", "podman build"},
		{"podman build", "podman save --format docker-archive"},
		{"podman build", "podman load"},
		{"podman load", "podman tag " + pipelineImageTag},
		{"podman tag " + pipelineImageTag, "rc-service " + deployContainer + " restart"},
		{"rc-service " + deployContainer + " restart", "http://127.0.0.1:8080"},
		{"http://127.0.0.1:8080", "kamal-proxy deploy"},
		{"kamal-proxy deploy", "podman image prune"},
		{"podman image prune", "echo " + pipelineVer + " > " + pipelineStamp},
	}
	for _, pair := range order {
		if indexOf(inv, pair[0]) < 0 || indexOf(inv, pair[0]) > indexOf(inv, pair[1]) {
			t.Errorf("pipeline order violated: %q must precede %q\ninvocations:\n%s", pair[0], pair[1], inv)
		}
	}

	// Cleanup keep-set: PruneSelect(keep 3) removes the 4th-newest sha tag.
	mustContain(t, inv, "podman rmi "+deployImageRepo+":ddd4444")
	if strings.Contains(inv, "podman rmi "+deployImageRepo+":abc1234") {
		t.Error("cleanup touched the just-deployed tag")
	}

	// Ledger: success line appended, footer counts it, subject = git log -1.
	lines := ledgerLines(readLedgerFile(t, deps.Home))
	last := lines[len(lines)-1]
	for _, want := range []string{`"sha":"` + pipelineVer + `"`, `"result":"success"`, `"subject":"` + pipelineSubject + `"`} {
		if !strings.Contains(last, want) {
			t.Errorf("ledger line %s missing %s", last, want)
		}
	}
	mustContain(t, stdout.String(), fmt.Sprintf("Total deployments: 4 · current tag: %s", pipelineVer))
	mustContain(t, stdout.String(), "done (version: "+pipelineVer+"). instant rollback: kampodra deploy --rollback")
}

func TestDeployPipelineVersionModeVMHasTagSkipsStream(t *testing.T) {
	deps, stubDir, stdout, _, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "gitstatus", " M dirty.txt\n") // dirty tree MUST NOT block version mode
	writeFixture(t, stubDir, "gitsha", "def9999\n")
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"def9999"}`)
	writeFixture(t, stubDir, "served-sha", "def9999")
	if code := runDeploy(t, deps, "--host", statusHost, "--sha", "def9999"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderrString(t, deps))
	}
	inv := invocations(t, stubDir)
	if strings.Contains(inv, "podman build") {
		t.Errorf("version mode must never rebuild:\n%s", inv)
	}
	if strings.Contains(inv, "podman save --format docker-archive") {
		t.Errorf("version mode with the tag already on the VM must skip streaming:\n%s", inv)
	}
	mustContain(t, stdout.String(), "already on the VM — skipping build/stream")
	if strings.Contains(inv, "git status --porcelain") {
		t.Error("version mode must skip the clean-tree gate (the build already happened when that sha was HEAD)")
	}
	mustContain(t, stdout.String(), "Total deployments: 4 · current tag: def9999")
}

func TestDeployPipelineVersionModeStreamsWhenVMLacksTag(t *testing.T) {
	deps, stubDir, _, _, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "gitstatus", " M dirty.txt\n")
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"def9999"}`)
	writeFixture(t, stubDir, "served-sha", "def9999")
	writeFixture(t, stubDir, "vmimageexists", "1") // VM does NOT have it
	if code := runDeploy(t, deps, "--host", statusHost, "--sha", "def9999"); code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderrString(t, deps))
	}
	inv := invocations(t, stubDir)
	if !strings.Contains(inv, "podman save --format docker-archive "+deployImageRepo+":def9999") {
		t.Errorf("version mode must stream the existing local build when the VM lacks it:\n%s", inv)
	}
}

func TestDeployPipelineDirtyTreeDiesBeforeBuild(t *testing.T) {
	deps, stubDir, _, stderr, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "gitstatus", " M app.go\n")
	if code := runDeploy(t, deps, "--host", statusHost); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if inv := invocations(t, stubDir); strings.Contains(inv, "podman build") {
		t.Errorf("dirty tree must die BEFORE the build:\n%s", inv)
	}
	mustContain(t, stderr.String(), "dirty tree")
	mustContain(t, stderr.String(), "COMMITTED")
}

func TestDeployPipelineNonRepoWithoutVersionDies(t *testing.T) {
	deps, stubDir, _, stderr, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "norepo", "1")
	if code := runDeploy(t, deps, "--host", statusHost); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	mustContain(t, stderr.String(), "not a git repository")
	mustContain(t, stderr.String(), "--sha")
}

func TestDeployPipelineDiskThresholdGateFailsClosed(t *testing.T) {
	deps, stubDir, _, stderr, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "df", strings.Replace(statusDF, "62%", "95%", 1))
	if code := runDeploy(t, deps, "--host", statusHost, "--disk-threshold", "90"); code != 1 {
		t.Fatalf("exit = %d, want 1 (fail closed BEFORE the build)", code)
	}
	if inv := invocations(t, stubDir); strings.Contains(inv, "podman build") {
		t.Errorf("disk gate must fire before the build:\n%s", inv)
	}
	mustContain(t, stderr.String(), "VM disk at 95%")
	mustContain(t, stderr.String(), "--disk-threshold 90")
	mustContain(t, stderr.String(), "prune --dry-run")
}

func TestDeployPipelineDiskThresholdValidation(t *testing.T) {
	deps, _, _, stderr, _ := setupDeployPipeline(t)
	if code := runDeploy(t, deps, "--host", statusHost, "--disk-threshold", "101"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	mustContain(t, stderr.String(), "--disk-threshold must be a percentage 0-100")
}

func TestDeployPipelineEnvPushSequence(t *testing.T) {
	deps, stubDir, stdout, _, _ := setupDeployPipeline(t)
	envPath := filepath.Join(stubDir, "app.env")
	if err := os.WriteFile(envPath, []byte("FIELD_ONE=alpha\nSECRET_TOKEN=tokensecretvalue1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDeploy(t, deps, "--host", statusHost, "--env-file", envPath); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	inv := invocations(t, stubDir)
	// Reuse of the env family: fingerprint summary, NEVER values.
	out := stdout.String()
	mustContain(t, out, "FIELD_ONE")
	mustContain(t, out, "SECRET_TOKEN")
	if strings.Contains(out, "tokensecretvalue1234") || strings.Contains(out, "alpha=") {
		t.Errorf("env push printed a VALUE — fingerprints only:\n%s", out)
	}
	// The pushed payload carries the deploy's sha stamp.
	stdin, err := os.ReadFile(filepath.Join(stubDir, "stdin-capture"))
	if err != nil {
		t.Fatalf("env upload stdin was never captured: %v", err)
	}
	if !strings.Contains(string(stdin), "FIELD_ONE=alpha") || !strings.Contains(string(stdin), "API_GIT_SHA="+pipelineVer) {
		t.Errorf("uploaded env = %q, want the file contents + API_GIT_SHA=%s", string(stdin), pipelineVer)
	}
	if !strings.Contains(inv, "umask 077; cat > "+pipelineEnvFile+".tmp.") {
		t.Errorf("env push must upload through the 0600-from-creation temp:\n%s", inv)
	}
	if !strings.Contains(inv, "chmod 600 "+pipelineEnvFile+".tmp.") || !strings.Contains(inv, "mv -f "+pipelineEnvFile+".tmp.") {
		t.Errorf("env push must atomically install (chmod 600 + mv -f):\n%s", inv)
	}
}

func TestDeployPipelineBuildFailureRecordsFailedLedger(t *testing.T) {
	deps, stubDir, stdout, _, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "failon", "podman build")
	if code := runDeploy(t, deps, "--host", statusHost); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	ledger := readLedgerFile(t, deps.Home)
	lines := ledgerLines(ledger)
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"result":"failed"`) || !strings.Contains(last, `"sha":"`+pipelineVer+`"`) {
		t.Errorf("failed deploy never reached the ledger: %s", last)
	}
	mustContain(t, stdout.String(), fmt.Sprintf("Total deployments: 4 · FAILED — attempted tag: %s (not activated)", pipelineVer))
}

func TestDeployPipelineUnhealthyGateDiesWithRollbackHint(t *testing.T) {
	deps, stubDir, _, stderr, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "health", `{"ok":true,"git":"0000000"}`)
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	errStr := stderr.String()
	mustContain(t, errStr, "never became healthy")
	mustContain(t, errStr, "--rollback")
	// The hint resolves the stamp best-effort: the fixture serves ccc3333.
	mustContain(t, errStr, "--rollback ccc3333")
	// Diagnostics before dying: container logs were pulled.
	mustContain(t, invocations(t, stubDir), "podman logs --tail 30 "+deployContainer)
}

func TestDeployPipelineSmokeMismatchDies(t *testing.T) {
	deps, stubDir, _, stderr, _ := setupDeployPipeline(t)
	writeFixture(t, stubDir, "served-sha", "bbb2222") // edge still serving the OLD build
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	mustContain(t, stderr.String(), "served git sha mismatch")
}

func TestDeployPipelineSkipSmoke(t *testing.T) {
	deps, _, stdout, _, _ := setupDeployPipeline(t)
	// Dead prober: with --skip-smoke the deploy must still succeed.
	deps.Prober = &probe.Prober{HTTPClient: &http.Client{Transport: deadTransport{}}}
	if code := runDeploy(t, deps, "--host", statusHost, "--skip-smoke", "--in-place"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	mustContain(t, stdout.String(), "--skip-smoke")
}

func TestDeployPipelineFlagValidation(t *testing.T) {
	t.Run("--rollback and --sha are exclusive", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--rollback", "--sha", "abc1234"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "exclusive")
	})
	t.Run("--sha requires a sha fragment", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--sha", "release-42"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "git sha fragment")
	})
	t.Run("--rolling and --rollback are exclusive", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--rolling", "--rollback"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "exclusive")
	})
	t.Run("--drain-timeout requires rolling (dies with --in-place)", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--drain-timeout", "5", "--in-place"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "--drain-timeout requires the rolling mode")
	})
	t.Run("unknown positional argument dies", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "wat"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		mustContain(t, stderr.String(), "unknown argument")
	})
	t.Run("retired --refresh-config is gone", func(t *testing.T) {
		deps, _, _, stderr, _ := setupDeployPipeline(t)
		if code := runDeploy(t, deps, "--host", statusHost, "--refresh-config"); code != 1 {
			t.Fatalf("exit = %d, want 1 (unknown flag — no legacy aliases)", code)
		}
		mustContain(t, stderr.String(), "unknown flag")
	})
}

func TestDeployHelpDocumentsPipelineRollingAndConverge(t *testing.T) {
	deps, _, stdout, _, _ := setupDeployPipeline(t)
	if code := runDeploy(t, deps, "--help"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"--rolling", "--drain-timeout", "--env-file", "converge",
		"deployed-sha stamp", "NEVER", "two-writer", "shadow",
		"podman save | ssh podman load", "--disk-threshold", "--skip-smoke",
		"keep-set image cleanup",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy --help missing %q", want)
		}
	}
	if strings.Contains(out, "--refresh-config") {
		t.Error("deploy --help still documents the retired --refresh-config no-op")
	}
	if strings.Contains(out, "newest 3") {
		t.Error("deploy --help hardcodes the pipeline keep count — reference the keep-set, not a number that contradicts prune's --keep")
	}
}

// --- small helpers -------------------------------------------------------

func readLedgerFile(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".kampodra", "deployments.jsonl"))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return string(data)
}

func ledgerLines(ledger string) []string {
	var out []string
	for _, line := range strings.Split(ledger, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func stderrString(t *testing.T, deps command.Deps) string {
	t.Helper()
	if bb, ok := deps.Stderr.(*bytes.Buffer); ok {
		return bb.String()
	}
	return ""
}

func TestDeployPipelineSidecarsStreamed(t *testing.T) {
	deps, stubDir, stdout, stderr, reporoot := setupDeployPipeline(t)
	manifest := `{"images":{"sidecars":[{"name":"backup","dockerfile":"deploy/backup.Dockerfile"}]}}`
	if err := os.WriteFile(filepath.Join(reporoot, "kampodra.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Dir = reporoot
	if code := runDeploy(t, deps, "--host", statusHost, "--in-place"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%sstderr:\n%s", code, stdout.String(), stderr.String())
	}
	inv := invocations(t, stubDir)
	sidecarTag := deployImageRepo + "-backup:latest"

	// build: same platform + identity stamp as the primary, own dockerfile,
	// :latest tag (sidecars are not sha-versioned — rollback never needs
	// an old sidecar). CONTEXT = the sidecar dockerfile's own directory —
	// the RUNBOOK contract (`podman build -f apps/api-go/Dockerfile.backup
	// apps/api-go`); the 2026-10-09 live fire caught repo-root context
	// failing on the first real sidecar Dockerfile (COPY go.mod go.sum).
	sidecarCtx := filepath.Dir(filepath.Join(reporoot, "deploy/backup.Dockerfile"))
	mustContain(t, inv, "podman build --platform linux/arm64 -f deploy/backup.Dockerfile --build-arg GIT_SHA="+pipelineVer+" -t "+sidecarTag+" "+sidecarCtx)
	// stream: the same save|load pipe as the primary…
	mustContain(t, inv, "podman save --format docker-archive "+sidecarTag)
	// …and a fail-closed existence check on the VM after the load.
	mustContain(t, inv, "podman image exists "+sidecarTag)

	// ORDER: the sidecar pass runs after the primary retag (the primary's
	// contract is settled before sidecars start) and before the restart.
	primary := indexOf(inv, "podman tag "+pipelineImageTag+" "+deployImageRepo+":latest")
	sidecar := indexOf(inv, "podman build --platform linux/arm64 -f deploy/backup.Dockerfile")
	restart := indexOf(inv, "rc-service "+deployContainer+" restart")
	if primary < 0 || sidecar < 0 || restart < 0 {
		t.Fatalf("sequence incomplete: primary=%d sidecar=%d restart=%d", primary, sidecar, restart)
	}
	if primary > sidecar {
		t.Error("sidecar build must follow the primary retag")
	}
	if sidecar > restart {
		t.Error("sidecar pass must precede the restart")
	}
}

func TestDeployPipelineRollbackNeverTouchesSidecars(t *testing.T) {
	deps, stubDir, _, _, reporoot := setupDeployPipeline(t)
	manifest := `{"images":{"sidecars":[{"name":"backup","dockerfile":"deploy/backup.Dockerfile"}]}}`
	if err := os.WriteFile(filepath.Join(reporoot, "kampodra.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.Dir = reporoot
	// The harness's rollback contract ends at the health gate (exit 1 —
	// served-sha != rolled-back sha), which is PAST the point where sidecar
	// invocations would have been recorded. Purity is the assertion, not
	// the exit code.
	runDeploy(t, deps, "--host", statusHost, "--rollback")
	inv := invocations(t, stubDir)
	if strings.Contains(inv, "-backup:latest") {
		t.Errorf("rollback must not build/stream/verify sidecars:\n%s", inv)
	}
}
