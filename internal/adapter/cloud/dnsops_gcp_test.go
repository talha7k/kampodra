package cloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gcloud legs of the neutral DNS ops: exact argv pins for zones /
// records / the transaction sequence, rrdatas expansion, trailing-dot
// normalization, and gcloud's bare-[] empty lists. The gcloud CLI is
// PATH-shimmed; kampodra passes only identity flags (--project/--account),
// never credential material.

type gcpShim struct {
	dir string
	log string
}

// setupGCPShim installs a gcloud shim dispatching on "$1 $2 $3" with
// fixture files, and pins TMPDIR so transaction files land (and are
// verifiably cleaned up) in a known absolute dir.
func setupGCPShim(t *testing.T, zonesJSON, recordsJSON string) *gcpShim {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	zones := filepath.Join(dir, "zones.json")
	records := filepath.Join(dir, "records.json")
	log := filepath.Join(dir, "gcloud-calls.log")
	if err := os.WriteFile(zones, []byte(zonesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(records, []byte(recordsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"case \"$1 $2 $3\" in\n" +
		"  \"dns managed-zones list\") cat '" + zones + "' ;;\n" +
		"  \"dns record-sets list\") cat '" + records + "' ;;\n" +
		"  \"dns record-sets transaction\") ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim shells out to cat — keep the core dirs on PATH like the
	// command-level harness does.
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return &gcpShim{dir: dir, log: log}
}

func (s *gcpShim) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(s.log)
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

const gcpTwoZones = `[{"name":"example-com","dnsName":"example.com."},{"name":"other-net","dnsName":"other.net."}]`
const gcpAppRecords = `[
	{"name":"app.example.com.","type":"A","ttl":300,"rrdatas":["203.0.113.9","203.0.113.10"]},
	{"name":"www.example.com.","type":"CNAME","ttl":3600,"rrdatas":["edge.example.com."]},
	{"name":"example.com.","type":"NS","ttl":86400,"rrdatas":["ns-cloud-b1.googledomains.com."]}
]`

func TestGCPResolveZoneArgvAndMatch(t *testing.T) {
	shim := setupGCPShim(t, gcpTwoZones, gcpAppRecords)
	ctx := context.Background()
	auth := CloudAuth{Provider: "gcp", Project: "p1"}

	// dnsName match, trailing dot stripped; the slug is the ID.
	z, err := DNSResolveZone(ctx, auth, "example.com.")
	if err != nil {
		t.Fatalf("dnsName hint: %v", err)
	}
	if z.Name != "example.com" || z.ID != "example-com" {
		t.Errorf("dnsName match = %+v", z)
	}
	// Same zone via the bare dnsName and via the slug passthrough.
	for _, hint := range []string{"example.com", "example-com"} {
		if z, err := DNSResolveZone(ctx, auth, hint); err != nil || z.ID != "example-com" {
			t.Errorf("hint %q = %+v, %v", hint, z, err)
		}
	}
	// Unknown hint fails naming it.
	if _, err := DNSResolveZone(ctx, auth, "nope.io"); err == nil || !strings.Contains(err.Error(), "zone not found: nope.io") {
		t.Errorf("unknown hint = %v", err)
	}
	// Ambiguity is a ManyZonesError carrying every candidate — never a guess.
	_, err = DNSResolveZone(ctx, auth, "")
	var mz *ManyZonesError
	if !errors.As(err, &mz) || mz.Scope != "p1" || strings.Join(mz.Names, ",") != "example.com,other.net" {
		t.Errorf("empty hint = %v (%+v), want ManyZonesError{p1 [example.com other.net]}", err, mz)
	}

	// One argv shape for every resolve: the managed-zones list with the
	// project identity flag appended exactly once.
	calls := shim.calls(t)
	if len(calls) != 5 {
		t.Fatalf("got %d gcloud calls, want 5: %v", len(calls), calls)
	}
	for _, c := range calls {
		if c != "dns managed-zones list --format=json(name,dnsName) --project=p1" {
			t.Errorf("argv drift: %s", c)
		}
	}
}

func TestGCPResolveZoneSingleZoneAuto(t *testing.T) {
	setupGCPShim(t, `[{"name":"example-com","dnsName":"example.com."}]`, gcpAppRecords)
	// One zone and a native (empty) project: auto-resolve, no identity flags.
	z, err := DNSResolveZone(context.Background(), CloudAuth{Provider: "gcp"}, "")
	if err != nil || z.Name != "example.com" || z.ID != "example-com" {
		t.Fatalf("auto-resolve = %+v, %v", z, err)
	}
	// No zones at all is an actionable error, not an empty success.
	setupGCPShim(t, `[]`, "[]")
	if _, err := DNSResolveZone(context.Background(), CloudAuth{Provider: "gcp"}, ""); err == nil || !strings.Contains(err.Error(), "no managed zones") {
		t.Errorf("no zones = %v", err)
	}
}

func TestGCPListRecordsArgvExpansion(t *testing.T) {
	shim := setupGCPShim(t, gcpTwoZones, gcpAppRecords)
	records, err := DNSListRecords(context.Background(), CloudAuth{Provider: "gcp"}, "example-com")
	if err != nil {
		t.Fatalf("DNSListRecords: %v", err)
	}
	// Every rrdatas[] entry expands to one neutral Record, trailing dots
	// normalized via NormalizeRData.
	if len(records) != 4 {
		t.Fatalf("got %d records (rrdatas must expand flat), want 4: %+v", len(records), records)
	}
	if records[0].Domain != "app.example.com" || records[0].RType != "A" || records[0].TTL != 300 || records[0].RData != "203.0.113.9" {
		t.Errorf("records[0] = %+v", records[0])
	}
	if records[1].RData != "203.0.113.10" {
		t.Errorf("round-robin expansion lost the second rdata: %+v", records[1])
	}
	if records[2].RType != "CNAME" || records[2].RData != "edge.example.com" {
		t.Errorf("CNAME trailing dot must normalize: %+v", records[2])
	}
	if calls := shim.calls(t); len(calls) != 1 || calls[0] != "dns record-sets list --zone=example-com --format=json(name,type,ttl,rrdatas)" {
		t.Errorf("argv drift: %v", calls)
	}
}

// gcloud emits bare [] for empty lists — the parser reads them natively.
func TestGCPListRecordsBareEmptyArray(t *testing.T) {
	setupGCPShim(t, gcpTwoZones, `[]`)
	records, err := DNSListRecords(context.Background(), CloudAuth{Provider: "gcp"}, "example-com")
	if err != nil || len(records) != 0 {
		t.Errorf("bare [] = %d records, %v", len(records), err)
	}
}

func TestGCPGetRRSetFiltersNameAndType(t *testing.T) {
	setupGCPShim(t, gcpTwoZones, gcpAppRecords)
	ctx := context.Background()
	auth := CloudAuth{Provider: "gcp"}
	items, err := DNSGetRRSet(ctx, auth, "example-com", "app.example.com", "A")
	if err != nil || len(items) != 2 {
		t.Fatalf("A rrset = %d items, %v", len(items), err)
	}
	if items, err := DNSGetRRSet(ctx, auth, "example-com", "www.example.com.", "CNAME"); err != nil || len(items) != 1 || items[0].RData != "edge.example.com" {
		t.Errorf("CNAME rrset = %+v, %v (trailing-dot domain must match)", items, err)
	}
	if items, err := DNSGetRRSet(ctx, auth, "example-com", "app.example.com", "CNAME"); err != nil || len(items) != 0 {
		t.Errorf("type mismatch must read empty: %+v, %v", items, err)
	}
}

func TestGCPUpdateTransactionSequence(t *testing.T) {
	shim := setupGCPShim(t, gcpTwoZones, gcpAppRecords)
	ctx := context.Background()
	auth := CloudAuth{Provider: "gcp"}
	items := []Record{
		{Domain: "app.example.com", RType: "A", TTL: 300, RData: "203.0.113.9"},
		{Domain: "app.example.com", RType: "A", TTL: 300, RData: "203.0.113.10"},
		{Domain: "app.example.com", RType: "A", TTL: 300, RData: "203.0.113.11"},
	}
	if err := DNSUpdateRRSet(ctx, auth, "example-com", "app.example.com", "A", items); err != nil {
		t.Fatalf("DNSUpdateRRSet: %v", err)
	}
	calls := shim.calls(t)
	if len(calls) != 5 { // 1x list (current rrset for remove) + 4x transaction
		t.Fatalf("got %d calls, want 5:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	if !strings.HasPrefix(calls[0], "dns record-sets list ") {
		t.Errorf("update must fetch the current rrset first: %s", calls[0])
	}
	// start → remove (exact current rrset) → add (merged) → execute, sharing
	// ONE absolute transaction file (never a cwd-relative litter path). The
	// --transaction-file value is dynamic, so it is extracted, then the
	// remaining argv is compared exactly.
	txn := ""
	want := []struct{ stage, rest string }{
		{"start", "--zone=example-com"},
		{"remove", "--zone=example-com --name=app.example.com. --type=A --ttl=300 203.0.113.9 203.0.113.10"},
		{"add", "--zone=example-com --name=app.example.com. --type=A --ttl=300 203.0.113.9 203.0.113.10 203.0.113.11"},
		{"execute", "--zone=example-com"},
	}
	for i, w := range want {
		got := calls[i+1]
		prefix := "dns record-sets transaction " + w.stage + " "
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("call %d = %s, want prefix %q", i+1, got, prefix)
			continue
		}
		rest := strings.TrimPrefix(got, prefix)
		file := ""
		var kept []string
		for _, fld := range strings.Fields(rest) {
			if v, ok := strings.CutPrefix(fld, "--transaction-file="); ok {
				file = v
				continue
			}
			kept = append(kept, fld)
		}
		if exact := strings.Join(kept, " "); exact != w.rest {
			t.Errorf("%s argv drift:\n got: %s\nwant: %s", w.stage, exact, w.rest)
		}
		if !filepath.IsAbs(file) {
			t.Errorf("%s: transaction file must be absolute, got %q", w.stage, file)
		}
		if txn == "" {
			txn = file
		} else if file != txn {
			t.Errorf("%s: transaction file changed mid-transaction: %q vs %q", w.stage, file, txn)
		}
	}
	if !strings.HasPrefix(txn, shim.dir) {
		// TMPDIR was pinned to the shim dir — a different root would mean
		// cwd-relative litter.
		t.Errorf("transaction file %q escaped the pinned dir %q", txn, shim.dir)
	}
	if _, err := os.Stat(txn); !os.IsNotExist(err) {
		t.Errorf("transaction file must be cleaned up, stat = %v", err)
	}
}

func TestGCPUpdateSkipsRemoveWhenAbsent(t *testing.T) {
	shim := setupGCPShim(t, gcpTwoZones, `[{"name":"example.com.","type":"NS","ttl":86400,"rrdatas":["ns-cloud-b1.googledomains.com."]}]`)
	items := []Record{{Domain: "app.example.com", RType: "A", TTL: 300, RData: "203.0.113.9"}}
	if err := DNSUpdateRRSet(context.Background(), CloudAuth{Provider: "gcp"}, "example-com", "app.example.com", "A", items); err != nil {
		t.Fatalf("DNSUpdateRRSet: %v", err)
	}
	joined := strings.Join(shim.calls(t), "\n")
	if strings.Contains(joined, "transaction remove") {
		t.Errorf("a missing RRSet must not remove:\n%s", joined)
	}
	if !strings.Contains(joined, "transaction start") || !strings.Contains(joined, "transaction add") || !strings.Contains(joined, "transaction execute") {
		t.Errorf("start/add/execute sequence incomplete:\n%s", joined)
	}
}

// Compartments are OCI-only: a gcp profile carrying one fails closed with
// the scoping remedy — never an improvised argv against the wrong cloud.
func TestDNSResolveScopeGCPFailsClosed(t *testing.T) {
	shim := setupGCPShim(t, gcpTwoZones, gcpAppRecords)
	_, err := DNSResolveScope(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "some-comp")
	if err == nil || !strings.Contains(err.Error(), "OCI-only") {
		t.Fatalf("gcp compartment = %v, want an OCI-only refusal", err)
	}
	if calls := shim.calls(t); len(calls) != 0 {
		t.Errorf("the refusal must not exec gcloud: %v", calls)
	}
}

// An emptied set is remove-only — the port of OCI's `--items '[]'` semantic.
func TestGCPUpdateRemoveOnlyWhenEmptied(t *testing.T) {
	shim := setupGCPShim(t, gcpTwoZones, gcpAppRecords)
	if err := DNSUpdateRRSet(context.Background(), CloudAuth{Provider: "gcp"}, "example-com", "app.example.com", "A", nil); err != nil {
		t.Fatalf("DNSUpdateRRSet: %v", err)
	}
	joined := strings.Join(shim.calls(t), "\n")
	if strings.Contains(joined, "transaction add") {
		t.Errorf("an emptied RRSet must be remove-only:\n%s", joined)
	}
	if !strings.Contains(joined, "transaction remove") || !strings.Contains(joined, "203.0.113.9 203.0.113.10") {
		t.Errorf("remove leg must carry the exact current rrset:\n%s", joined)
	}
}
