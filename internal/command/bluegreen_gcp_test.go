package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// The bluegreen gcp harness: setupBG's gcloud twin (mirroring
// setupDNSGCP). The gcloud CLI is PATH-shimmed with fixture files, an oci
// logging shim rides along to prove bluegreen never execs it on a gcp
// profile, and an always-healthy ssh shim serves the app probes.
// Fixtures mirror gcloud's native shapes: bare arrays, URL-valued
// zone/subnetwork fields (last segment = the bare name), users[] holder
// paths.

type bgGCPHarness struct {
	*bgHarness
	realOCILog string // the oci shim's log — must stay EMPTY on a gcp run
}

func (h *bgGCPHarness) ociExecs(t *testing.T) []string {
	t.Helper()
	return h.ociCallsFrom(t, h.realOCILog)
}

func (h *bgHarness) ociCallsFrom(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

const bgGCPZoneURL = "https://www.googleapis.com/compute/v1/projects/p1/zones/europe-west1-b"
const bgGCPHolderPath = "https://www.googleapis.com/compute/v1/projects/p1/zones/europe-west1-b/instances/app-blue"

func bgGCPRow(name, natIP, created string) string {
	return `{"name":"` + name + `","zone":"` + bgGCPZoneURL + `","creationTimestamp":"` + created +
		`","status":"RUNNING","networkInterfaces":[{"accessConfigs":[{"natIP":"` + natIP + `"}],"subnetwork":"https://www.googleapis.com/compute/v1/projects/p1/regions/europe-west1/subnetworks/kampodra"}]}`
}

func bgGCPGreenRow() string {
	return "[" + bgGCPRow("app-green", "203.0.113.9", "2026-10-01T00:00:00Z") + "]"
}

func bgGCPBlueRow() string {
	return "[" + bgGCPRow("app-blue", "203.0.113.8", "2026-09-01T00:00:00Z") + "]"
}

func bgGCPAddrList() string {
	return `[{"name":"kampodra-active","address":"203.0.113.50","status":"IN_USE","users":["` + bgGCPHolderPath + `"]}]`
}

// writeBGCPProfile writes the gcp profile WITHOUT a compartment — the
// real-world gcp shape (the project rides the cloud block / gcloud's
// active config; a compartment would mask the OCI-only scope gate).
func writeBGCPProfile(t *testing.T, h *bgGCPHarness) {
	t.Helper()
	writeDNSProfile(t, h.deps.Home, "gcp-prof",
		`{"host": "root@198.51.100.7", "cloud": {"provider": "gcp", "project": "p1"}}`)
}

func setupBGCP(t *testing.T, fixtures map[string]string) *bgGCPHarness {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	gcpLog := filepath.Join(stub, "gcloud-calls.log")
	realOCILog := filepath.Join(stub, "oci-calls.log")
	sshLog := filepath.Join(stub, "ssh-calls.log")

	def := func(key, val string) {
		if _, ok := fixtures[key]; !ok {
			fixtures[key] = val
		}
	}
	def("addr-list", `[]`)
	def("addr-create", `[{"name":"kampodra-active","address":"203.0.113.50","status":"RESERVED"}]`)
	def("inst-blue", `[]`)
	def("inst-green", `[]`)
	def("inst-any", `[]`)
	def("images", `[]`)

	files := map[string]string{}
	for k, v := range fixtures {
		p := filepath.Join(stub, k+".json")
		os.WriteFile(p, []byte(v), 0o600)
		files[k] = p
	}

	gcloudShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + gcpLog + "'\n" +
		"case \"$1 $2 $3\" in\n" +
		"  \"compute addresses list\") cat '" + files["addr-list"] + "' ;;\n" +
		"  \"compute addresses create\") cat '" + files["addr-create"] + "' ;;\n" +
		"  \"compute instances list\") if [[ \"$*\" == *app-blue* ]]; then cat '" + files["inst-blue"] + "'; " +
		"elif [[ \"$*\" == *app-green* ]]; then cat '" + files["inst-green"] + "'; " +
		"else cat '" + files["inst-any"] + "'; fi ;;\n" +
		"  \"compute instances create\") exit 0 ;;\n" +
		"  \"compute images list\") cat '" + files["images"] + "' ;;\n" +
		"  \"compute instances delete-access-config\") exit 0 ;;\n" +
		"  \"compute instances add-access-config\") exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "gcloud"), []byte(gcloudShim), 0o755)
	// The oci shim exists ONLY to prove it is never execed on a gcp run.
	ociShim := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> '" + realOCILog + "'\nexit 0\n"
	os.WriteFile(filepath.Join(stub, "oci"), []byte(ociShim), 0o755)
	sshShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + sshLog + "'\n" +
		"case \"$*\" in\n" +
		"  *\"rc-service app status\"*) exit 0 ;;\n" +
		"  *wget*) exit 0 ;;\n" +
		"  *\"true\"*) exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "ssh"), []byte(sshShim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	// No OCI env may leak into a gcp run: OCI_COMPARTMENT would trip the
	// OCI-only compartment fail-close.
	t.Setenv("OCI_COMPARTMENT", "")
	for _, k := range []string{"KAMPODRA_PROFILE", "OCI_PROFILE"} {
		t.Setenv(k, "")
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	h := &bgHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
		},
		stdout: stdout, stderr: stderr, ociLog: gcpLog, sshLog: sshLog,
	}
	return &bgGCPHarness{bgHarness: h, realOCILog: realOCILog}
}

// --- the gcp dispatch scenarios ---------------------------------------------

// Clean slate: status dispatches to gcloud — the reserved IP reads NONE
// and both colors read not provisioned. No refusal, no oci exec.
func TestBluegreenGCPStatusCleanSlate(t *testing.T) {
	h := setupBGCP(t, map[string]string{})
	writeBGCPProfile(t, h)
	if code := h.run(t, "status", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"reserved IP : NONE (run 'kampodra bluegreen init')",
		"app-blue : not provisioned",
		"app-green : not provisioned",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(h.stderr.String(), "bluegreen is OCI-only") {
		t.Errorf("the slice-A refusal must be gone: %q", h.stderr.String())
	}
	calls := h.ociCalls(t)
	if len(calls) != 3 { // 1x addresses list + 2x instances list
		t.Fatalf("got %d gcloud calls, want 3:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	for _, call := range calls {
		if !strings.Contains(call, "--project=p1") || strings.Contains(call, "--account") {
			t.Errorf("identity flags wrong: %s", call)
		}
		if !strings.HasPrefix(call, "compute ") {
			t.Errorf("unexpected argv: %s", call)
		}
	}
	if execs := h.ociExecs(t); len(execs) != 0 {
		t.Errorf("a gcp profile must never exec oci: %v", execs)
	}
}

// A populated pair: the reserved IP row (users[0] holder), both instance
// rows with natIP public IPs, and the health probes riding ssh.
func TestBluegreenGCPStatusPopulatedPair(t *testing.T) {
	h := setupBGCP(t, map[string]string{
		"addr-list":  bgGCPAddrList(),
		"inst-blue":  bgGCPBlueRow(),
		"inst-green": bgGCPGreenRow(),
	})
	writeBGCPProfile(t, h)
	if code := h.run(t, "status", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"reserved IP : 203.0.113.50 (kampodra-active)",
		"assigned to : app-blue",
		"app-blue : app-blue  ip=203.0.113.8  app=HEALTHY",
		"app-green : app-green  ip=203.0.113.9  app=HEALTHY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}

// init creates the DORMANT reserved address; the region derives from the
// sibling instance's zone minus its final segment (pinned argv).
func TestBluegreenGCPInitCreatesReservedAddress(t *testing.T) {
	h := setupBGCP(t, map[string]string{
		"inst-any": bgGCPBlueRow(), // the RUNNING sibling anchors the region
	})
	writeBGCPProfile(t, h)
	if code := h.run(t, "init", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "created DORMANT reserved IP: 203.0.113.50") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "compute addresses create kampodra-active --region=europe-west1 --format=json(name,address,status) --project=p1") {
		t.Errorf("create argv drift:\n%s", joined)
	}
	if !strings.Contains(joined, "compute instances list --filter=status=RUNNING") {
		t.Errorf("the region derive list is missing:\n%s", joined)
	}
}

// init is idempotent: an existing reserved address means no create call.
func TestBluegreenGCPInitIdempotent(t *testing.T) {
	h := setupBGCP(t, map[string]string{"addr-list": bgGCPAddrList()})
	writeBGCPProfile(t, h)
	if code := h.run(t, "init", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "reserved IP already exists: 203.0.113.50") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	if joined := strings.Join(h.ociCalls(t), "\n"); strings.Contains(joined, "addresses create") {
		t.Errorf("idempotent init must not create:\n%s", joined)
	}
}

// rollback on gcp: unassign IS release — the holder's access config is
// dropped (users[0] names it directly, no anchor-join) and nothing is
// attached anywhere.
func TestBluegreenGCPRollbackDetachesHolder(t *testing.T) {
	h := setupBGCP(t, map[string]string{"addr-list": bgGCPAddrList()})
	writeBGCPProfile(t, h)
	if code := h.run(t, "rollback", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "ROLLED BACK: reserved 203.0.113.50 UNASSIGNED (dormant)") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "compute instances delete-access-config app-blue --zone=europe-west1-b --project=p1") {
		t.Errorf("the holder detach argv is missing:\n%s", joined)
	}
	if strings.Contains(joined, "add-access-config") {
		t.Errorf("rollback must not attach anywhere:\n%s", joined)
	}
}

// flip fail-closed on gcp: the reserved-address handle is not an
// ocid-shaped anchor, so the command layer's anchor gate refuses BEFORE
// any write — the guest anchor watcher contract (GCE metadata external-IP
// poll) is a follow-up, and until it lands the flip must not touch the
// pair.
func TestBluegreenGCPFlipFailsClosedBeforeAnyWrite(t *testing.T) {
	h := setupBGCP(t, map[string]string{
		"addr-list":  bgGCPAddrList(),
		"inst-green": bgGCPGreenRow(),
	})
	writeBGCPProfile(t, h)
	if code := h.run(t, "flip", "--to", "green", "--force", "--profile", "gcp-prof"); code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "could not resolve/create the reserved-anchor secondary private ip on app-green") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), "bluegreen is OCI-only") {
		t.Errorf("the slice-A refusal must be gone: %q", h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	for _, write := range []string{"delete-access-config", "add-access-config", "addresses create", "instances create"} {
		if strings.Contains(joined, write) {
			t.Errorf("the refused flip must not write (%s):\n%s", write, joined)
		}
	}
	if len(h.sshCalls(t)) != 0 {
		t.Errorf("the refused flip must not touch guests: %v", h.sshCalls(t))
	}
}

// provision on gcp launches beside its sibling: the subnet handle is a
// plain NAME on gcp (the OCI ocid-shape gate is oci-leg only — live-fire
// 2026-10-16 caught the gate refusing gcp subnets), so the native route
// (--image-id) goes straight to the launch argv pinned here.
func TestBluegreenGCPProvisionLaunchesBesideSibling(t *testing.T) {
	h := setupBGCP(t, map[string]string{"inst-blue": bgGCPBlueRow(), "inst-green": `[]`})
	writeBGCPProfile(t, h)
	if code := h.run(t, "provision", "green", "--profile", "gcp-prof",
		"--image-id", "projects/debian-cloud/global/images/family/debian-12"); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	for _, want := range []string{
		"compute instances create app-green",
		"--zone=europe-west1-b", // the sibling's zone, last segment
		"--machine-type=e2-standard-2",
		"--subnet=kampodra", // the sibling's subnet, bare gcp name — no ocid gate
		"--image=projects/debian-cloud/global/images/family/debian-12",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("launch argv missing %q:\n%s", want, joined)
		}
	}
	if execs := h.ociExecs(t); len(execs) != 0 {
		t.Errorf("a gcp profile must never exec oci: %v", execs)
	}
}

// Without --image-id and with no golden image, gcp provision fails closed
// at the inject route's ocid gate (the record's image handle is gcp-native,
// not an ocid — the inject route is oci-leg only by design).
func TestBluegreenGCPProvisionInjectRouteRefuses(t *testing.T) {
	h := setupBGCP(t, map[string]string{"inst-blue": bgGCPBlueRow(), "inst-green": `[]`, "images": `[]`})
	writeBGCPProfile(t, h)
	if code := h.run(t, "provision", "green", "--profile", "gcp-prof"); code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "no resolvable image-id") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if execs := h.ociExecs(t); len(execs) != 0 {
		t.Errorf("a gcp profile must never exec oci: %v", execs)
	}
}
