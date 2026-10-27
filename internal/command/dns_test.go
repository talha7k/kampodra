package command_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// dns (the dns.sh port): OCI DNS records — records | add | rm. The oci CLI
// is shimmed on PATH; the no-credential static gate pins the auth rule.

type dnsHarness struct {
	deps   command.Deps
	stub   string
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	ociLog string
}

func setupDNS(t *testing.T, opts map[string]string) *dnsHarness {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	ociLog := filepath.Join(stub, "oci-calls.log")
	compartmentFixture := filepath.Join(stub, "compartments.json")
	zonesFixture := filepath.Join(stub, "zones.json")
	zoneFixture := filepath.Join(stub, "zone.json")
	rrsetFixture := filepath.Join(stub, "rrset.json")
	updateFixture := filepath.Join(stub, "update.json")

	os.WriteFile(compartmentFixture, []byte(compartmentData(opts)), 0o600)
	os.WriteFile(zonesFixture, []byte(opts["zones"]), 0o600)
	if opts["zones"] == "" {
		os.WriteFile(zonesFixture, []byte(`{"data":{"items":[{"name":"example.com","id":"ocid1.dns-zone.oc1..z1"}]}}`), 0o600)
	}
	zoneData := opts["zone"]
	if zoneData == "" {
		zoneData = `{"data":{"name":"example.com","id":"ocid1.dns-zone.oc1..z1"}}`
	}
	os.WriteFile(zoneFixture, []byte(zoneData), 0o600)
	recordsFixture := filepath.Join(stub, "records.json")
	recordsData := opts["records"]
	if recordsData == "" {
		recordsData = `{"data":{"items":[]}}`
	}
	os.WriteFile(recordsFixture, []byte(recordsData), 0o600)
	os.WriteFile(rrsetFixture, []byte(opts["rrset"]), 0o600)
	if opts["rrset"] == "" {
		os.WriteFile(rrsetFixture, []byte(`{"data":{"items":[]}}`), 0o600)
	}
	os.WriteFile(updateFixture, []byte(`{"data":{"items":[]}}`), 0o600)

	ociShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + ociLog + "'\n" +
		"case \"$1 $2\" in\n" +
		"  \"iam compartment\") cat '" + compartmentFixture + "'; exit 0 ;;\n" +
		"  \"dns zone\")\n" +
		"    if [[ \"$*\" == *\" list \"* || \"$*\" == *\" list\"* ]]; then cat '" + zonesFixture + "'; else cat '" + zoneFixture + "'; fi\n" +
		"    exit 0 ;;\n" +
		"  \"dns record\")\n" +
		"    if [[ \"$*\" == *rrset* ]]; then\n" +
		"      if [[ \"$*\" == *update* ]]; then cat '" + updateFixture + "'; else cat '" + rrsetFixture + "'; fi\n" +
		"    else\n" +
		"      cat '" + recordsFixture + "'\n" +
		"    fi\n" +
		"    exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "oci"), []byte(ociShim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	t.Setenv("OCI_COMPARTMENT", "test-compartment")
	for _, k := range []string{"KAMPODRA_PROFILE", "OCI_PROFILE"} {
		t.Setenv(k, "")
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &dnsHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
		},
		stub:   stub,
		stdout: stdout,
		stderr: stderr,
		ociLog: ociLog,
	}
}

func (h *dnsHarness) run(t *testing.T, args ...string) int {
	t.Helper()
	return command.Execute("test", h.deps, append([]string{"dns"}, args...))
}

func (h *dnsHarness) ociCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.ociLog)
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

const dnsExistingRRSet = `{"data":{"items":[{"domain":"app.example.com","rtype":"A","ttl":300,"rdata":"203.0.113.9"}]}}`

// compartmentData builds the `iam compartment list` fixture: the default
// single compute compartment, or the opts["compartments"] override for the
// split compute/DNS-scope cases.
func compartmentData(opts map[string]string) string {
	if v := opts["compartments"]; v != "" {
		return v
	}
	return `{"data":[{"id":"ocid1.compartment.oc1..aaa","name":"test-compartment"}]}`
}

func TestDNSRecordsRendersTable(t *testing.T) {
	h := setupDNS(t, map[string]string{
		"records": `{"data":{"items":[
			{"domain":"app.example.com","rtype":"A","ttl":300,"rdata":"203.0.113.10"},
			{"domain":"app.example.com","rtype":"CNAME","ttl":3600,"rdata":"edge.example.com."}
		]}}`,
	})
	if code := h.run(t, "records"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"[dns] DNS records — zone example.com (ocid1.dns-zone.oc1..z1), auth profile: oci-cli default —",
		"app.example.com", "A", "300", "203.0.113.10",
		"CNAME", "3600", "edge.example.com.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("records output missing %q:\n%s", want, out)
		}
	}
	// Native resolution: no cloud block, no env → kampodra passes NO auth
	// flags and the oci CLI resolves on its own.
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "--profile") || strings.Contains(call, "--auth") {
			t.Errorf("native resolution must pass no auth flags: %s", call)
		}
	}
}

func TestDNSAddHappyPath(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	if code := h.run(t, "add", "--name", "app", "--type", "A", "--value", "203.0.113.10", "--ttl", "300"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "added: app.example.com A 203.0.113.10 (ttl 300)") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	calls := h.ociCalls(t)
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "rrset update") || !strings.Contains(joined, "--items") {
		t.Errorf("rrset update missing:\n%s", joined)
	}
	if !strings.Contains(joined, "203.0.113.10") {
		t.Errorf("the new value is not in the update items:\n%s", joined)
	}
	for _, call := range calls {
		if strings.Contains(call, "--profile") || strings.Contains(call, "--auth") {
			t.Errorf("native resolution must pass no auth flags: %s", call)
		}
	}
}

func TestDNSAddIdempotent(t *testing.T) {
	h := setupDNS(t, map[string]string{"rrset": dnsExistingRRSet})
	if code := h.run(t, "add", "--name", "app", "--type", "A", "--value", "203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "already present") || !strings.Contains(h.stdout.String(), "nothing to do") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "update") {
			t.Errorf("idempotent add must not update:\n%s", call)
		}
	}
}

func TestDNSAddMergesRoundRobin(t *testing.T) {
	h := setupDNS(t, map[string]string{"rrset": dnsExistingRRSet})
	if code := h.run(t, "add", "--name", "app", "--type", "A", "--value", "203.0.113.10"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "203.0.113.9") || !strings.Contains(joined, "203.0.113.10") {
		t.Errorf("update must MERGE (both round-robin values in items):\n%s", joined)
	}
}

func TestDNSValidationBeforeProvider(t *testing.T) {
	cases := [][]string{
		{"add", "--name", "bad name", "--type", "A", "--value", "1.2.3.4"},
		{"add", "--name", "a/b", "--type", "A", "--value", "1.2.3.4"},
		{"add", "--name", "app", "--type", "TXT", "--value", "hello"},
		{"add", "--name", "app", "--type", "a", "--value", "1.2.3.4"},
		{"add", "--name", "app", "--type", "A", "--value", "not-an-ip"},
		{"add", "--name", "app", "--type", "A", "--value", "256.1.1.1"},
		{"add", "--name", "app", "--type", "AAAA", "--value", "1.2.3.4"},
		{"add", "--name", "app", "--type", "CNAME", "--value", "not a hostname"},
		{"add", "--name", "app", "--type", "A", "--value", "1.2.3.4", "--ttl", "59"},
		{"add", "--name", "app", "--type", "A", "--value", "1.2.3.4", "--ttl", "172801"},
		{"rm", "--name", "app", "--type", "A", "--value", "1.2.3.4\""},
	}
	for _, args := range cases {
		h := setupDNS(t, map[string]string{})
		if code := h.run(t, args...); code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
		if calls := h.ociCalls(t); len(calls) != 0 {
			t.Errorf("%v: validation fired AFTER a provider call: %v", args, calls)
		}
	}
}

// The zone-membership guard inherently needs the zone (resolved AFTER the
// compartment — the shell's resolve_domain runs there too), so one provider
// call before the refusal is faithful; the refusal itself is what's pinned.
func TestDNSZoneMembershipGuard(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	if code := h.run(t, "add", "--name", "outside.example.org", "--type", "A", "--value", "1.2.3.4", "--zone", "example.com"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "outside zone 'example.com'") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	// and no WRITE happens
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "update") {
			t.Errorf("zone-guard refusal must not update:\n%s", call)
		}
	}
}

func TestDNSRequiresNameTypeValue(t *testing.T) {
	for _, args := range [][]string{
		{"add", "--type", "A", "--value", "1.2.3.4"},
		{"rm", "--value", "1.2.3.4", "--type", "A"},
	} {
		h := setupDNS(t, map[string]string{})
		if code := h.run(t, args...); code != 1 {
			t.Errorf("%v: exit = %d, want 1", args, code)
		}
	}
}

func TestDNSRmRemovesAndReports(t *testing.T) {
	h := setupDNS(t, map[string]string{"rrset": dnsExistingRRSet})
	if code := h.run(t, "rm", "--name", "app", "--type", "A", "--value", "203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "removed: app.example.com A 203.0.113.9") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
}

func TestDNSRmNoMatchFails(t *testing.T) {
	h := setupDNS(t, map[string]string{"rrset": dnsExistingRRSet})
	if code := h.run(t, "rm", "--name", "app", "--type", "A", "--value", "203.0.113.99"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "no matching record") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestDNSRmCanEmptyTheRRSet(t *testing.T) {
	h := setupDNS(t, map[string]string{"rrset": dnsExistingRRSet})
	if code := h.run(t, "rm", "--name", "app", "--type", "A", "--value", "203.0.113.9"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	// an emptied RRSet is legal: --items '[]' removes it entirely
	if !strings.Contains(joined, `"--items", "[]"`) && !strings.Contains(joined, "--items []") {
		t.Errorf("emptied RRSet update missing:\n%s", joined)
	}
}

func TestDNSMultipleZonesNeedExplicitZone(t *testing.T) {
	h := setupDNS(t, map[string]string{
		"zones": `{"data":{"items":[{"name":"example.com","id":"ocid1.dns-zone.oc1..z1"},{"name":"other.net","id":"ocid1.dns-zone.oc1..z2"}]}}`,
	})
	if code := h.run(t, "records"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "multiple zones") || !strings.Contains(h.stderr.String(), "example.com") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

// Cloud auth rides the instance profile's `cloud` block (or the provider
// env): instance principal via the block, compartment via block or env.
func TestDNSCloudBlockDrivesInstancePrincipal(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	writeDNSProfile(t, h.deps.Home, "oci-vm", `{"host": "root@198.51.100.7", "cloud": {"instancePrincipal": true}}`)
	if code := h.run(t, "records", "--profile", "oci-vm"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	for _, call := range h.ociCalls(t) {
		if !strings.Contains(call, "--auth instance_principal") {
			t.Errorf("cloud-block instancePrincipal missing --auth: %s", call)
		}
	}
}

func TestDNSCloudBlockProfileSelectsConfigProfile(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	writeDNSProfile(t, h.deps.Home, "tenancy", `{"host": "root@198.51.100.7", "cloud": {"profile": "myprof"}}`)
	if code := h.run(t, "records", "--profile", "tenancy"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "--profile myprof") {
		t.Errorf("cloud-block profile did not reach the oci CLI as its config profile:\n%s", joined)
	}
}

// With no cloud block and no env, the oci CLI resolves natively — kampodra
// passes NO --profile flag (its config's default/DEFAULT/first precedence).
func TestDNSNativeResolutionOmitsProfileFlag(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	t.Setenv("OCI_PROFILE", "")
	if code := h.run(t, "records"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "--profile") || strings.Contains(call, "--auth") {
			t.Errorf("native resolution must pass no auth flags: %s", call)
		}
	}
}

func TestDNSRequiresCompartment(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	t.Setenv("OCI_COMPARTMENT", "")
	if code := h.run(t, "records"); code != 1 || !strings.Contains(h.stderr.String(), "OCI_COMPARTMENT") {
		t.Errorf("exit=%d stderr=%q", code, h.stderr.String())
	}
}

// The no-credential static gate (the archived dns-validation.test.ts,
// ported): no dns-surface source references credential material tokens,
// and the dns surface supports the sanctioned auth modes (config-file
// --profile + instance principal) across its files.
func TestDNSNoCredentialMaterialStaticGate(t *testing.T) {
	// Forbidden forever (never read, stored, or passed): token flags, key
	// files, impersonation env, client secrets — for BOTH providers.
	forbidden := `(?i)--auth[-_]token|api[-_]key|private[-_]key|password|security[-_]token-file\s*=|OCI_API_KEY|--access-token|GOOGLE_APPLICATION_CREDENTIALS|client[-_]secret|key[-_]file`
	sources := []string{
		filepath.Join("..", "command", "dns.go"),
		filepath.Join("..", "adapter", "cloud", "dns.go"),
		// AuthArgs — the --auth instance_principal arg builder — lives in the
		// cloud adapter's shared file; the gate covers it too.
		filepath.Join("..", "adapter", "cloud", "backup.go"),
		filepath.Join("..", "adapter", "cloud", "provider.go"),
		filepath.Join("..", "adapter", "cloud", "dnsops.go"),
		filepath.Join("..", "adapter", "cloud", "dnsops_oci.go"),
		filepath.Join("..", "adapter", "cloud", "dnsops_gcp.go"),
		// The backup family's neutral ops — same auth rule, same gate.
		filepath.Join("..", "adapter", "cloud", "backupops.go"),
		filepath.Join("..", "adapter", "cloud", "backupops_oci.go"),
		filepath.Join("..", "adapter", "cloud", "backupops_gcp.go"),
	}
	combined := ""
	for _, src := range sources {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		combined += string(data)
		if matchesForbidden(forbidden, string(data)) {
			t.Errorf("%s references credential material — auth is the oci config file / instance principal ONLY", src)
		}
	}
	if !strings.Contains(combined, "instance_principal") {
		t.Error("the dns surface does not support instance principal auth")
	}
	if !strings.Contains(combined, "--profile") {
		t.Error("the dns surface does not support --profile (config-file auth)")
	}
	// The command carries NO cloud flags: auth comes from the instance
	// profile's `cloud` block or the provider env, and the provider CLI
	// resolves natively otherwise (gcp scope rides the block's project,
	// never a command flag).
	cmdSrc, err := os.ReadFile(filepath.Join("..", "command", "dns.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"--instance-principal", "--oci-profile", "--project", "--account", "--provider"} {
		if strings.Contains(string(cmdSrc), banned) {
			t.Errorf("the dns command must not expose %s", banned)
		}
	}
}

func matchesForbidden(pattern, s string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

// writeDNSProfile writes one profile into the harness's config.json
// (cloud blocks ride ordinary profiles — no special harness wiring).
func writeDNSProfile(t *testing.T, home, name, profileJSON string) {
	t.Helper()
	dir := filepath.Join(home, ".kampodra")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	cfg := map[string]any{"profiles": map[string]any{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &cfg)
	}
	if cfg["profiles"] == nil {
		cfg["profiles"] = map[string]any{}
	}
	var p any
	if err := json.Unmarshal([]byte(profileJSON), &p); err != nil {
		t.Fatal(err)
	}
	cfg["profiles"].(map[string]any)[name] = p
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- gcp: the gcloud twin of the oci shim harness --------------------------

const dnsGCPAppRecords = `[
	{"name":"app.example.com.","type":"A","ttl":300,"rrdatas":["203.0.113.9","203.0.113.10"]},
	{"name":"www.example.com.","type":"CNAME","ttl":3600,"rrdatas":["edge.example.com."]},
	{"name":"example.com.","type":"NS","ttl":86400,"rrdatas":["ns-cloud-b1.googledomains.com."]}
]`

// setupDNSGCP is setupDNS's gcloud twin: a temp-dir shim dispatching on
// "$1 $2", fixture files, and an invocation log. Fixtures mirror gcloud's
// native shapes: bare arrays, dnsName with trailing dot, rrdatas arrays.
func setupDNSGCP(t *testing.T, opts map[string]string) *dnsHarness {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	gcpLog := filepath.Join(stub, "gcloud-calls.log")
	zonesFixture := filepath.Join(stub, "zones.json")
	recordsFixture := filepath.Join(stub, "records.json")

	zonesData := opts["zones"]
	if zonesData == "" {
		zonesData = `[{"name":"example-com","dnsName":"example.com."}]`
	}
	os.WriteFile(zonesFixture, []byte(zonesData), 0o600)
	recordsData := opts["records"]
	if recordsData == "" {
		recordsData = `[]` // gcloud emits bare [] for empty lists
	}
	os.WriteFile(recordsFixture, []byte(recordsData), 0o600)

	gcloudShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + gcpLog + "'\n" +
		"case \"$1 $2\" in\n" +
		"  \"dns managed-zones\") cat '" + zonesFixture + "' ;;\n" +
		"  \"dns record-sets\")\n" +
		"    if [[ \"$3\" == \"transaction\" ]]; then\n" +
		// Real gcloud refuses `transaction start` when the file already
		// exists — it owns the file's creation. Model that so the suite
		// catches pre-created temp files (live-fire bug 2026-10-16).
		"      if [[ \"$3 $4\" == \"transaction start\" ]]; then\n" +
		"        f=\"\"\n" +
		"        for a in \"$@\"; do case \"$a\" in --transaction-file=*) f=\"${a#--transaction-file=}\";; esac; done\n" +
		"        if [[ -n \"$f\" && -e \"$f\" ]]; then echo \"Transaction already exists at [$f]\" >&2; exit 1; fi\n" +
		"      fi\n" +
		"      exit 0\n" +
		"    fi\n" +
		"    cat '" + recordsFixture + "'\n" +
		"    exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "gcloud"), []byte(gcloudShim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	// No OCI env may leak into a gcp run: OCI_COMPARTMENT would trip the
	// OCI-only compartment fail-close.
	t.Setenv("OCI_COMPARTMENT", "")
	for _, k := range []string{"KAMPODRA_PROFILE", "OCI_PROFILE"} {
		t.Setenv(k, "")
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &dnsHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
		},
		stub:   stub,
		stdout: stdout,
		stderr: stderr,
		ociLog: gcpLog,
	}
}

// gcpCalls reads the shim invocation log — the same reader serves both CLIs.
func (h *dnsHarness) gcpCalls(t *testing.T) []string {
	return h.ociCalls(t)
}

func TestDNSGCPRecordsExpandRRDatas(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{"records": dnsGCPAppRecords})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp", "project": "p1"}}`)
	if code := h.run(t, "records", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	// Every rrdatas[] entry expands to one row; trailing dots normalize.
	for _, want := range []string{
		"[dns] DNS records — zone example.com",
		"gcloud active config",
		"app.example.com\tA\t300\t203.0.113.9",
		"app.example.com\tA\t300\t203.0.113.10",
		"www.example.com\tCNAME\t3600\tedge.example.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("records output missing %q:\n%s", want, out)
		}
	}
	// One argv shape per op, the identity flag appended exactly once —
	// never an --account (the block names none).
	for _, call := range h.gcpCalls(t) {
		if !strings.Contains(call, "--project=p1") || strings.Contains(call, "--account") {
			t.Errorf("identity flags wrong: %s", call)
		}
	}
	if joined := strings.Join(h.gcpCalls(t), "\n"); !strings.Contains(joined,
		"dns record-sets list --zone=example-com --format=json(name,type,ttl,rrdatas) --project=p1") {
		t.Errorf("record list argv drift:\n%s", joined)
	}
}

// A dnsName hint (gcloud's list form, trailing dot included) resolves to
// the zone slug gcloud's --zone actually takes.
func TestDNSGCPZoneResolveByDNSName(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	if code := h.run(t, "records", "--zone", "example.com.", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	// Native block (no project): NO identity flags — gcloud's active config.
	for _, call := range h.gcpCalls(t) {
		if strings.Contains(call, "--project") || strings.Contains(call, "--account") {
			t.Errorf("native resolution must pass no identity flags: %s", call)
		}
	}
	if joined := strings.Join(h.gcpCalls(t), "\n"); !strings.Contains(joined, "--zone=example-com ") {
		t.Errorf("dnsName hint must resolve to the slug:\n%s", joined)
	}
}

func TestDNSGCPAddIdempotent(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{"records": dnsGCPAppRecords})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	if code := h.run(t, "add", "--name", "app", "--type", "A", "--value", "203.0.113.9", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "already present") || !strings.Contains(h.stdout.String(), "nothing to do") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	if joined := strings.Join(h.gcpCalls(t), "\n"); strings.Contains(joined, "transaction") {
		t.Errorf("idempotent add must not stage a transaction:\n%s", joined)
	}
}

func TestDNSGCPAddMergesAndExecutesTransaction(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{"records": dnsGCPAppRecords})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	if code := h.run(t, "add", "--name", "app", "--type", "A", "--value", "203.0.113.11", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "added: app.example.com A 203.0.113.11 (ttl 300)") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	joined := strings.Join(h.gcpCalls(t), "\n")
	// remove stages the EXACT current rrset; add stages the merge.
	if !strings.Contains(joined, "transaction start") ||
		!strings.Contains(joined, "transaction remove") || !strings.Contains(joined, "203.0.113.9 203.0.113.10") ||
		!strings.Contains(joined, "transaction add") || !strings.Contains(joined, "203.0.113.9 203.0.113.10 203.0.113.11") ||
		!strings.Contains(joined, "transaction execute") {
		t.Errorf("transaction sequence incomplete:\n%s", joined)
	}
}

func TestDNSGCPRmFiltersAndExecutesTransaction(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{"records": dnsGCPAppRecords})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	if code := h.run(t, "rm", "--name", "app", "--type", "A", "--value", "203.0.113.9", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "removed: app.example.com A 203.0.113.9") {
		t.Errorf("stdout = %q", h.stdout.String())
	}
	joined := strings.Join(h.gcpCalls(t), "\n")
	if !strings.Contains(joined, "transaction add") || !strings.Contains(joined, "203.0.113.10") {
		t.Errorf("rm must keep the surviving rdata:\n%s", joined)
	}
	if strings.Contains(joined, "203.0.113.9 203.0.113.10 203.0.113.10") {
		t.Errorf("rm must not duplicate the surviving rdata:\n%s", joined)
	}
}

func TestDNSGCPRmNoMatchFails(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{"records": dnsGCPAppRecords})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	if code := h.run(t, "rm", "--name", "app", "--type", "A", "--value", "203.0.113.99", "--profile", "gcp-prof"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "no matching record") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if joined := strings.Join(h.gcpCalls(t), "\n"); strings.Contains(joined, "transaction") {
		t.Errorf("a refused rm must not stage a transaction:\n%s", joined)
	}
}

func TestDNSGCPMissingGCLOUDFailsClosed(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	// Empty PATH dir, NOT /usr/bin:/bin — GitHub runners ship gcloud in
	// /usr/bin; see TestBackupGCPMissingGCLOUDFailsClosed.
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	if code := h.run(t, "records", "--profile", "gcp-prof"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{"gcloud CLI not found", "gcloud init"} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, h.stderr.String())
		}
	}
	if calls := h.gcpCalls(t); len(calls) != 0 {
		t.Errorf("no CLI means no calls: %v", calls)
	}
}

// A compartment under gcp is a config error: the neutral scope op fails
// closed BEFORE any gcloud call.
func TestDNSGCPCompartmentIsRefused(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	t.Setenv("OCI_COMPARTMENT", "some-comp")
	if code := h.run(t, "records", "--profile", "gcp-prof"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(h.stderr.String(), "OCI-only") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if calls := h.gcpCalls(t); len(calls) != 0 {
		t.Errorf("the refusal must not exec gcloud: %v", calls)
	}
}

// --- dns compartment scoping (incident 2026-10-16): the DNS zones may live
// --- in a different OCI compartment than compute. Order: --dns-compartment
// --- flag > the cloud block's "dnsCompartment" > compartment (byte-identical
// --- default); gcp ignores the whole thing.

const dnsTwoCompartments = `{"data":[
	{"id":"ocid1.compartment.oc1..aaa","name":"test-compartment"},
	{"id":"ocid1.compartment.oc1..bbb","name":"dns-zone-comp"}]}`

// --dns-compartment reaches the scope resolution: every dns argv searches the
// flag's compartment ocid, and the compute compartment never appears.
func TestDNSDNSCompartmentFlagReachesArgv(t *testing.T) {
	h := setupDNS(t, map[string]string{"compartments": dnsTwoCompartments})
	writeDNSProfile(t, h.deps.Home, "split", `{"host": "root@198.51.100.7", "cloud": {"compartment": "test-compartment"}}`)
	if code := h.run(t, "records", "--profile", "split", "--dns-compartment", "dns-zone-comp"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.ociCalls(t)
	want := []string{
		"iam compartment list --all",
		"dns zone list --all -c ocid1.compartment.oc1..bbb",
		"dns record zone get -c ocid1.compartment.oc1..bbb --zone-name-or-id ocid1.dns-zone.oc1..z1",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("argv drift:\n got: %q\nwant: %q", calls, want)
	}
}

// The cloud block's "dnsCompartment" alone (no flag) scopes dns the same way.
func TestDNSCloudBlockDNSCompartment(t *testing.T) {
	h := setupDNS(t, map[string]string{"compartments": dnsTwoCompartments})
	writeDNSProfile(t, h.deps.Home, "split", `{"host": "root@198.51.100.7", "cloud": {"compartment": "test-compartment", "dnsCompartment": "dns-zone-comp"}}`)
	if code := h.run(t, "records", "--profile", "split"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.ociCalls(t)
	want := []string{
		"iam compartment list --all",
		"dns zone list --all -c ocid1.compartment.oc1..bbb",
		"dns record zone get -c ocid1.compartment.oc1..bbb --zone-name-or-id ocid1.dns-zone.oc1..z1",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("argv drift:\n got: %q\nwant: %q", calls, want)
	}
}

// The flag outranks the cloud block's dnsCompartment.
func TestDNSDNSCompartmentFlagBeatsBlock(t *testing.T) {
	h := setupDNS(t, map[string]string{"compartments": `{"data":[
		{"id":"ocid1.compartment.oc1..aaa","name":"test-compartment"},
		{"id":"ocid1.compartment.oc1..bbb","name":"block-comp"},
		{"id":"ocid1.compartment.oc1..ccc","name":"flag-comp"}]}`})
	writeDNSProfile(t, h.deps.Home, "split", `{"host": "root@198.51.100.7", "cloud": {"compartment": "test-compartment", "dnsCompartment": "block-comp"}}`)
	if code := h.run(t, "records", "--profile", "split", "--dns-compartment", "flag-comp"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "dns zone list --all -c ocid1.compartment.oc1..ccc") {
		t.Errorf("the flag must outrank the block's dnsCompartment:\n%s", joined)
	}
	if strings.Contains(joined, "ocid1.compartment.oc1..bbb") {
		t.Errorf("the block's dnsCompartment must lose to the flag:\n%s", joined)
	}
}

// Unset flag + unset block: today's behavior, byte-identical — the compute
// compartment resolves exactly as before.
func TestDNSDNSCompartmentUnsetArgvByteIdentical(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	if code := h.run(t, "records"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.ociCalls(t)
	want := []string{
		"iam compartment list --all",
		"dns zone list --all -c ocid1.compartment.oc1..aaa",
		"dns record zone get -c ocid1.compartment.oc1..aaa --zone-name-or-id ocid1.dns-zone.oc1..z1",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("unset scope must stay byte-identical:\n got: %q\nwant: %q", calls, want)
	}
}

// The no-zones failure names the compartment it searched and hints the
// override — the incident's "no managed zones" dead end must not repeat.
func TestDNSNoZonesNamesCompartmentAndHintsOverride(t *testing.T) {
	h := setupDNS(t, map[string]string{"zones": `{"data":{"items":[]}}`})
	if code := h.run(t, "records"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	for _, want := range []string{
		"no managed zones in compartment ocid1.compartment.oc1..aaa",
		"is your DNS zone in another compartment?",
		"--dns-compartment",
		`"dnsCompartment"`,
	} {
		if !strings.Contains(h.stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, h.stderr.String())
		}
	}
}

// gcp has no compartments: DnsCompartment (block or flag) is meaningless —
// ignored, failing nothing.
func TestDNSGCPDNSCompartmentIgnored(t *testing.T) {
	h := setupDNSGCP(t, map[string]string{})
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp", "dnsCompartment": "some-comp"}}`)
	if code := h.run(t, "records", "--dns-compartment", "flag-comp", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	for _, call := range h.gcpCalls(t) {
		if strings.Contains(call, "some-comp") || strings.Contains(call, "flag-comp") {
			t.Errorf("dnsCompartment values must never reach a gcp argv: %s", call)
		}
	}
}

// --- cloud env rename: KAMPODRA_CLOUD_* is the primary escape hatch, the
// --- vendor-prefixed OCI_* names stay as read-but-deprecated aliases.

// The renamed env name alone satisfies the oci compartment gate and drives
// the scope resolution end to end.
func TestDNSCloudEnvCompartmentSatisfiesGate(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	t.Setenv("OCI_COMPARTMENT", "")
	t.Setenv("KAMPODRA_CLOUD_COMPARTMENT", "test-compartment")
	if code := h.run(t, "records"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "-c ocid1.compartment.oc1..aaa") {
		t.Errorf("the new env name must drive the scope resolution:\n%s", joined)
	}
}

// The missing-compartment gate error names the NEW env name first and keeps
// the deprecated alias for existing scripts.
func TestDNSCompartmentGateErrorNamesNewEnv(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	t.Setenv("OCI_COMPARTMENT", "")
	if code := h.run(t, "records"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	errOut := h.stderr.String()
	for _, want := range []string{"KAMPODRA_CLOUD_COMPARTMENT", "OCI_COMPARTMENT"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("gate error must name %q:\n%s", want, errOut)
		}
	}
}

// Precedence at the command surface: when both names are set, the
// KAMPODRA_CLOUD_* value wins and the deprecated value never reaches the
// provider argv.
func TestDNSNewCloudEnvBeatsDeprecatedOCIEnv(t *testing.T) {
	h := setupDNS(t, map[string]string{})
	t.Setenv("OCI_COMPARTMENT", "ocid1.compartment.oc1..old")
	t.Setenv("KAMPODRA_CLOUD_COMPARTMENT", "ocid1.compartment.oc1..new")
	if code := h.run(t, "records"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	if !strings.Contains(joined, "-c ocid1.compartment.oc1..new") {
		t.Errorf("the new env name must win:\n%s", joined)
	}
	if strings.Contains(joined, "ocid1.compartment.oc1..old") {
		t.Errorf("the deprecated value must lose entirely:\n%s", joined)
	}
}
