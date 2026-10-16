// bluegreen deploy — the standby-update composite: standby-not-running
// refusal, the happy binary fast-push flow (push → health → flip argv
// sequence, ordered across the WHOLE composite), and the safety property
// (pipeline failure = NO flip argv). The harness mirrors setupBG
// (PATH-shimmed oci, fixture-fed instance rows) extended with the deploy
// pipeline's local git/go shims and the VM-side pipeline legs — every shim
// logs into ONE invocations file, so order pins span build → push → gate →
// flip exactly like the deploy pipeline pins, plus the flip's oci legs.

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

type bgDeployHarness struct {
	deps    command.Deps
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	stubDir string
}

func (h *bgDeployHarness) run(t *testing.T, args ...string) int {
	t.Helper()
	return command.Execute("test", h.deps, append([]string{"bluegreen"}, args...))
}

func (h *bgDeployHarness) invocations(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.stubDir, "invocations"))
	if err != nil {
		return ""
	}
	return string(data)
}

// setupBGDeploy is setupBG extended to the composite's needs: the same
// oci shim + instance fixtures (combined invocations log), an ssh shim
// answering BOTH the bluegreen guest legs and the deploy pipeline's VM
// legs, and local git/go shims for the pipeline's deploy-machine gates.
// deps.Dir points at a reporoot so the repo manifest (kampodra.json
// "binary" blocks) resolves per-test.
func setupBGDeploy(t *testing.T) (h *bgDeployHarness, reporoot string) {
	t.Helper()
	home := t.TempDir()
	stubDir := t.TempDir()
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
	// Instance fixtures (setupBG's shape): green RUNNING, blue absent.
	// One file per answer — the composite's repeat reads get the same row.
	green := fixture("instances-green", greenRow())
	fixture("instances-blue", `{"data":[]}`)
	comp := fixture("compartments", `{"data":[{"id":"ocid1.compartment.test","name":"test-compartment"}]}`)
	reserved := fixture("reserved", "ocid1.publicip.r1 203.0.113.50 -")
	instance := fixture("instance", `{"data":{"lifecycle-state":"RUNNING","image-id":"ocid1.image.platform","availability-domain":"AD-1"}}`)
	vnic := fixture("vnic", `{"data":[{"vnic-id":"ocid1.vnic.v1","subnet-id":"ocid1.subnet.s1"}]}`)
	vnicGet := fixture("vnicGet", `{"data":{"public-ip":"198.51.100.7"}}`)
	privateIPs := fixture("privateIPs", `{"data":[{"id":"ocid1.privateip.primary","is-primary":true},{"id":"ocid1.privateip.anchor","is-primary":false}]}`)
	privateGet := fixture("privateGet", `{"data":{"ip-address":"10.0.0.7"}}`)
	subnet := fixture("subnet", `{"data":{"cidr-block":"10.0.0.0/24"}}`)
	// Pipeline fixtures: the VM-side health fetch + the flip's served-sha
	// verify all ride the same one-knob body (serves the pushed sha).
	health := fixture("health", `{"ok":true,"git":"`+pipelineVer+`"}`)
	fixture("gitsha", pipelineVer+"\n")
	fixture("gitstatus", "")
	fixture("gitsubject", pipelineSubject+"\n")
	gitTop := fixture("gittop", reporoot)
	fixture("stamp", pipelineVer+"\n")

	ociShim := "#!/bin/bash\n" +
		"printf 'oci %s\\n' \"$*\" >> '" + inv + "'\n" +
		"case \"$1 $2\" in\n" +
		"  \"iam compartment\") cat '" + comp + "'; exit 0 ;;\n" +
		"  \"network public-ip\") if [[ \"$3\" == list ]]; then cat '" + reserved + "'; fi; exit 0 ;;\n" +
		"  \"compute instance\") if [[ \"$3\" == list ]]; then " +
		"if [[ \"$*\" == *\"app-blue\"* ]]; then cat '" + fixture("instances-blue-x", `{"data":[]}`) + "'; " +
		"else cat '" + green + "'; fi; exit 0; " +
		"else cat '" + instance + "'; fi; exit 0 ;;\n" +
		"  \"compute vnic-attachment\") cat '" + vnic + "'; exit 0 ;;\n" +
		"  \"network vnic\") cat '" + vnicGet + "'; exit 0 ;;\n" +
		"  \"network private-ip\") if [[ \"$3\" == list ]]; then cat '" + privateIPs + "'; else cat '" + privateGet + "'; fi; exit 0 ;;\n" +
		"  \"network subnet\") cat '" + subnet + "'; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "oci"), []byte(ociShim), 0o755); err != nil {
		t.Fatal(err)
	}

	// The merged ssh shim. Case order matters: the upload/hash legs come
	// first; the VM-side health fetch (http://127.0.0.1:) answers the
	// health fixture AND doubles as the instanceHealthy probe answer; the
	// bluegreen guest legs (anchor.conf, addr pickup, ACME) keep the
	// setupBG contract; everything else exits 0 (restart, swap, stamp…).
	sshShim := "#!/bin/bash\n" +
		"printf 'ssh %s\\n' \"$*\" >> '" + inv + "'\n" +
		"remote=\"${*: -1}\"\n" +
		"case \"$remote\" in\n" +
		"  *\"cat > \"*) cat > '" + filepath.Join(stubDir, "stdin-capture") + "'; exit 0 ;;\n" +
		"  *sha256sum*) { sha256sum '" + filepath.Join(stubDir, "stdin-capture") + "' 2>/dev/null || shasum -a 256 '" + filepath.Join(stubDir, "stdin-capture") + "'; } | cut -d' ' -f1; exit 0 ;;\n" +
		"  *\"http://127.0.0.1:\"*) cat '" + health + "'; exit 0 ;;\n" +
		"  *\"rc-service app status\"*) exit 0 ;;\n" +
		"  *deployed-sha*) printf '" + pipelineVer + "'; exit 0 ;;\n" +
		"  *\"command -v rc-service\"*) printf 'openrc\\nhttpc=wget\\n'; exit 0 ;;\n" +
		"  *\"cat /etc/alpine-release\"*) printf '3.22.6'; exit 0 ;;\n" +
		"  *\"grep -h\"*) printf ''; exit 0 ;;\n" +
		"  *\"ANCHOR_CONF_WRITTEN\"*|*\"anchor.conf\"*printf*) printf 'ANCHOR_CONF_WRITTEN'; exit 0 ;;\n" +
		"  *\"ip -4 addr show | grep\"*) exit 0 ;;\n" +
		"  *\"kamal-proxy deploy\"*) exit 0 ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ssh"), []byte(sshShim), 0o755); err != nil {
		t.Fatal(err)
	}

	// The deploy-machine legs (deploy pipeline preflight + go fast-push).
	gitShim := "#!/bin/bash\n" +
		"printf 'git %s\\n' \"$*\" >> '" + inv + "'\n" +
		"case \"$1\" in\n" +
		"  rev-parse) case \"$2\" in\n" +
		"    --show-toplevel) cat '" + gitTop + "' ;;\n" +
		"    --short) cat '" + fixture("gitsha-file", pipelineVer+"\n") + "' ;;\n" +
		"  esac ;;\n" +
		"  status) cat '" + fixture("gitstatus-file", "") + "' ;;\n" +
		"  log) cat '" + fixture("gitsubject-file", pipelineSubject+"\n") + "' ;;\n" +
		"esac\n"
	goShim := "#!/bin/bash\n" +
		"printf 'go %s\\n' \"$*\" >> '" + inv + "'\n" +
		"env | grep -E '^(CGO_ENABLED|GOOS|GOARCH)=' | sort | sed 's/^/goenv /' >> '" + inv + "'\n" +
		"out=\"\"; prev=\"\"\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"-o\" ]; then out=\"$a\"; fi\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		"if [ -n \"$out\" ]; then printf 'fake-go-binary\\n' > \"$out\"; fi\n"
	for name, content := range map[string]string{"git": gitShim, "go": goShim} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	t.Setenv("OCI_COMPARTMENT", "test-compartment")
	for _, k := range []string{"KAMPODRA_PROFILE", "OCI_PROFILE", "KAMPODRA_HOST", "KAMPODRA_SSH_KEY"} {
		t.Setenv(k, "")
	}
	fastBudgets(t)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	h = &bgDeployHarness{
		deps: command.Deps{
			Home:   home,
			Dir:    reporoot,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
			Prober: &probe.Prober{HTTPClient: &http.Client{Transport: bgRoundTripper{body: `{"git":"` + pipelineVer + `"`}}},
		},
		stdout: stdout, stderr: stderr, stubDir: stubDir,
	}
	return h, reporoot
}

// TestBluegreenDeployRefusesStandbyNotRunning: a standby that is not
// RUNNING (not provisioned, or stopped) refuses BEFORE any deploy or flip
// machinery — no build, no push, no anchor, no reserved-ip call.
func TestBluegreenDeployRefusesStandbyNotRunning(t *testing.T) {
	h, _ := setupBGDeploy(t)
	// Override the green fixture AFTER setup: empty row = not provisioned.
	writeFixture(t, h.stubDir, "instances-green", `{"data":[]}`)
	writeFixture(t, h.stubDir, "instances-green-later", `{"data":[]}`)
	// The shim reads the per-name fixture files — repoint them too.
	writeFixture(t, h.stubDir, "instances-green-later-file", `{"data":[]}`)
	if code := h.run(t, "deploy", "--to", "green"); code == 0 {
		t.Fatalf("standby not RUNNING must refuse, stdout: %s", h.stdout.String())
	}
	out := h.stdout.String() + h.stderr.String()
	if !strings.Contains(out, "app-green is not RUNNING") {
		t.Errorf("refusal must name the color with the ops' vocabulary:\n%s", out)
	}
	inv := h.invocations(t)
	for _, forbidden := range []string{"go build", "cat > /data/app/", "anchor.conf", "oci network public-ip"} {
		if strings.Contains(inv, forbidden) {
			t.Errorf("refused deploy must not run %q:\n%s", forbidden, inv)
		}
	}
}

// TestBluegreenDeployBadColorUsage: a non-color --to is a usage error
// (exit 2), mirroring bluegreen flip's contract.
func TestBluegreenDeployBadColorUsage(t *testing.T) {
	h, _ := setupBGDeploy(t)
	if code := h.run(t, "deploy", "--to", "purple"); code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
}

// TestBluegreenDeployBinaryHappySequencePinned: the composite's whole
// story in one ordered log — the fast-push lands on the STANDBY's own ip
// (root@198.51.100.7), the served-sha health gate passes VM-side, and only
// then does the ACME-first flip touch the reserved IP.
func TestBluegreenDeployBinaryHappySequencePinned(t *testing.T) {
	h, reporoot := setupBGDeploy(t)
	writeBinaryManifest(t, reporoot, binaryGoManifest)
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	if code := h.run(t, "deploy", "--to", "green"); code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s\nstderr:\n%s", code, h.stdout.String(), h.stderr.String())
	}
	out := h.stdout.String()
	// The three phase banners, in order.
	p1 := indexOf(out, "bg-deploy[1/3]: building + pushing to app-green (198.51.100.7)…")
	p2 := indexOf(out, "bg-deploy[2/3]: health gate on the standby…")
	p3 := indexOf(out, "bg-deploy[3/3]: flip → green (health-gated)…")
	ordered := p1 >= 0 && p1 < p2 && p2 < p3
	if !ordered {
		t.Errorf("phase banners missing or out of order (1=%d 2=%d 3=%d):\n%s", p1, p2, p3, out)
	}
	if !strings.Contains(out, "FLIPPED:") {
		t.Errorf("composite must end flipped:\n%s", out)
	}
	// v1 boundary: rolling is NOT composed — no shadow machinery may run
	// on the standby (the flip is the cutover), and the pre-flip public
	// smoke is skipped (proxyHost is the OTHER color pre-flip; the flip's
	// own ACME + served-sha verify through the reserved ip is the
	// post-cutover smoke).
	inv := h.invocations(t)
	if strings.Contains(inv, "-shadow") {
		t.Errorf("v1 must not run the rolling shadow on the standby:\n%s", inv)
	}
	if strings.Contains(out, "smoke (public, through the proxy)") {
		t.Errorf("pre-flip public smoke probes the WRONG instance (the holder) — must be skipped:\n%s", out)
	}

	// The push legs ride the STANDBY's own ip (pins are the bare remote
	// commands — the ssh shim logs the full ssh argv).
	mustContain(t, inv, "umask 077; cat > /data/app/.esellar-api-go.new")
	mustContain(t, inv, "go build")
	mustContain(t, inv, "goenv GOOS=linux")
	mustContain(t, inv, "goenv GOARCH=arm64")
	// The flip legs: anchor conf on the standby, reserved-ip assign
	// waiting ASSIGNED, ACME through the reserved host.
	mustContain(t, inv, "anchor.conf")
	mustContain(t, inv, "--wait-for-state ASSIGNED")
	mustContain(t, inv, "kamal-proxy deploy app --host=")

	// THE order: build → push → swap → restart → health gate → stamp →
	// anchor → assign → ACME. Anything flipping before its health gate is
	// a safety regression.
	order := [][]string{
		{"go build", "cat > /data/app/.esellar-api-go.new"},
		{"cat > /data/app/.esellar-api-go.new", "mv /data/app/.esellar-api-go.new /data/app/esellar-api-go"},
		{"mv /data/app/.esellar-api-go.new /data/app/esellar-api-go", "rc-service app restart"},
		{"rc-service app restart", "http://127.0.0.1:8080/up"},
		{"http://127.0.0.1:8080/up", "echo " + pipelineVer + " > /etc/kampodra/deployed-sha"},
		{"echo " + pipelineVer + " > /etc/kampodra/deployed-sha", "anchor.conf"},
		{"anchor.conf", "--wait-for-state ASSIGNED"},
		{"--wait-for-state ASSIGNED", "kamal-proxy deploy app --host="},
	}
	for _, pair := range order {
		if indexOf(inv, pair[0]) < 0 || indexOf(inv, pair[0]) > indexOf(inv, pair[1]) {
			t.Errorf("composite order violated: %q must precede %q\ninvocations:\n%s", pair[0], pair[1], inv)
		}
	}
}

// TestBluegreenDeployPipelineFailureNoFlip: the safety property — a deploy
// that fails its health gate leaves the pair EXACTLY as it was: the holder
// keeps serving, no anchor write, no reserved-ip call, no ACME.
func TestBluegreenDeployPipelineFailureNoFlip(t *testing.T) {
	h, reporoot := setupBGDeploy(t)
	writeBinaryManifest(t, reporoot, binaryGoManifest)
	writeTree(t, reporoot, "apps/api-go/main.go", "package main\n")
	// The standby's app serves an OLD sha — the pushed build never passes
	// the gate, the auto-restore re-verify also fails against the old
	// body, and the pipeline dies.
	writeFixture(t, h.stubDir, "health", `{"ok":true,"git":"old0001"}`)
	if code := h.run(t, "deploy", "--to", "green"); code == 0 {
		t.Fatal("failed pipeline must exit non-zero")
	}
	out := h.stdout.String()
	if !strings.Contains(out, "auto-restoring the previous artifact") {
		t.Errorf("the failed gate must auto-restore before dying:\n%s", out)
	}
	if strings.Contains(out, "bg-deploy[3/3]") || strings.Contains(out, "FLIPPED:") {
		t.Errorf("a failed pipeline must never reach the flip:\n%s", out)
	}
	inv := h.invocations(t)
	for _, forbidden := range []string{"anchor.conf", "--wait-for-state ASSIGNED", "kamal-proxy deploy app --host="} {
		if strings.Contains(inv, forbidden) {
			t.Errorf("failed pipeline must not run the flip leg %q:\n%s", forbidden, inv)
		}
	}
}
