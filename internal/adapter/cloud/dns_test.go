package cloud

import (
	"strings"
	"testing"
)

// The dns.sh port: validators (pure, run before any provider call), parsers
// for the oci CLI's JSON, and the RRSet surgery primitives.

func TestIsIPv4(t *testing.T) {
	valid := []string{"203.0.113.10", "1.2.3.4", "0.0.0.0", "255.255.255.255"}
	for _, s := range valid {
		if !IsIPv4(s) {
			t.Errorf("IsIPv4(%q) = false, want true", s)
		}
	}
	invalid := []string{"256.1.1.1", "1.2.3", "1.2.3.4.5", "a.b.c.d", "1.2.3.04x", "", "1..2.3"}
	for _, s := range invalid {
		if IsIPv4(s) {
			t.Errorf("IsIPv4(%q) = true, want false", s)
		}
	}
}

func TestIsIPv6(t *testing.T) {
	valid := []string{"fd00::1", "2001:db8::8a2e:370:7334", "::1", "fe80:0:0:0:0:0:0:1"}
	for _, s := range valid {
		if !IsIPv6(s) {
			t.Errorf("IsIPv6(%q) = false, want true", s)
		}
	}
	invalid := []string{"1.2.3.4", "::::", "fd00:::1", "ghij::1", ":", ""}
	// NOTE: "12345::" (over-wide groups) passes exactly like the shell's
	// is_ipv6 — the validator is intentionally shallow; OCI validates
	// server-side.
	for _, s := range invalid {
		if IsIPv6(s) {
			t.Errorf("IsIPv6(%q) = true, want false", s)
		}
	}
}

func TestIsHostname(t *testing.T) {
	valid := []string{"app.example.com", "example.com.", "xn--80ak6aa92e.com", "a.b", "_foo.example.com"}
	for _, s := range valid {
		if !IsHostname(s) {
			t.Errorf("IsHostname(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "-lead.example.com", "app..example.com", "app example.com", "ex ample.com"}
	// NOTE: "app.example" is a VALID hostname per the shell's is_hostname
	// (label + tld; internal/short names are legal).
	for _, s := range invalid {
		if IsHostname(s) {
			t.Errorf("IsHostname(%q) = true, want false", s)
		}
	}
}

func TestIsDNSLabel(t *testing.T) {
	if !IsDNSLabel("app") || !IsDNSLabel("my-app_2") {
		t.Error("plain labels must pass")
	}
	if IsDNSLabel("app.example.com") || IsDNSLabel("") || IsDNSLabel(strings.Repeat("a", 64)) {
		t.Error("fqdn/empty/too-long must fail the label check")
	}
}

func TestResolveDomain(t *testing.T) {
	// label -> label.zone
	if got := ResolveDomain("app", "example.com"); got != "app.example.com" {
		t.Errorf("ResolveDomain(label) = %q", got)
	}
	// fqdn inside the zone passes through (trailing dot stripped)
	if got := ResolveDomain("app.example.com.", "example.com"); got != "app.example.com" {
		t.Errorf("ResolveDomain(fqdn.) = %q", got)
	}
	// the apex itself is legal
	if got := ResolveDomain("example.com", "example.com"); got != "example.com" {
		t.Errorf("ResolveDomain(apex) = %q", got)
	}
	// outside the zone is refused
	if err := ValidateDomain("www.other.com", "example.com"); err == nil {
		t.Error("name outside the zone must be refused (never write into someone else's zone by accident)")
	}
}

func TestValidateRecordType(t *testing.T) {
	for _, rt := range []string{"A", "AAAA", "CNAME"} {
		if err := ValidateRecordType(rt); err != nil {
			t.Errorf("ValidateRecordType(%q) = %v", rt, err)
		}
	}
	for _, rt := range []string{"", "TXT", "a", "MX"} {
		if err := ValidateRecordType(rt); err == nil {
			t.Errorf("ValidateRecordType(%q) = nil, want error", rt)
		}
	}
}

func TestValidateRecordValue(t *testing.T) {
	if err := ValidateRecordValue("A", "203.0.113.10"); err != nil {
		t.Errorf("A value: %v", err)
	}
	if err := ValidateRecordValue("AAAA", "fd00::1"); err != nil {
		t.Errorf("AAAA value: %v", err)
	}
	if err := ValidateRecordValue("CNAME", "app.example.com"); err != nil {
		t.Errorf("CNAME value: %v", err)
	}
	if err := ValidateRecordValue("A", "not-an-ip"); err == nil {
		t.Error("bad A value must fail")
	}
	if err := ValidateRecordValue("A", "1.2.3.4\nrm -rf"); err == nil {
		t.Error("injection-shaped value must fail")
	}
}

func TestValidateTTL(t *testing.T) {
	if err := ValidateTTL("300"); err != nil {
		t.Errorf("300: %v", err)
	}
	for _, bad := range []string{"59", "172801", "abc", ""} {
		if err := ValidateTTL(bad); err == nil {
			t.Errorf("ttl %q = nil, want error", bad)
		}
	}
}

func TestParseCompartments(t *testing.T) {
	fixture := `[{"id":"ocid1.compartment.oc1..aaa","name":"other"},{"id":"ocid1.compartment.oc1..bbb","name":"test-compartment"}]`
	ocid, err := CompartmentOCID(fixture, "test-compartment")
	if err != nil {
		t.Fatalf("CompartmentOCID: %v", err)
	}
	if ocid != "ocid1.compartment.oc1..bbb" {
		t.Errorf("ocid = %q", ocid)
	}
	if _, err := CompartmentOCID(fixture, "missing"); err == nil {
		t.Error("missing compartment must fail")
	}
	if _, err := CompartmentOCID(`[{"id":"ocid1.tenancy.oc1..xyz","name":"odd"}]`, "odd"); err == nil || !strings.Contains(err.Error(), "ocid1.compartment") {
		t.Error("non-compartment ocids must be rejected (the shell checked the ocid1.compartment prefix)")
	}
}

func TestParseZones(t *testing.T) {
	fixture := `{"data":{"items":[{"name":"example.com","id":"ocid1.dns-zone.oc1..z1"},{"name":"other.net","id":"ocid1.dns-zone.oc1..z2"}]}}`
	zones, err := ParseZones(fixture)
	if err != nil {
		t.Fatalf("ParseZones: %v", err)
	}
	if len(zones) != 2 || zones[0].Name != "example.com" || zones[0].ID != "ocid1.dns-zone.oc1..z1" {
		t.Errorf("zones = %+v", zones)
	}
}

func TestParseRecords(t *testing.T) {
	fixture := `{"data":{"items":[
		{"domain":"app.example.com","rtype":"A","ttl":300,"rdata":"203.0.113.10"},
		{"domain":"example.com","rtype":"NS","ttl":86400,"rdata":"ns1.pch.net."}
	]}}`
	records, err := ParseRecords(fixture)
	if err != nil {
		t.Fatalf("ParseRecords: %v", err)
	}
	if len(records) != 2 || records[0].TTL != 300 || records[0].RData != "203.0.113.10" {
		t.Errorf("records = %+v", records)
	}
}

func TestParseRRSet(t *testing.T) {
	fixture := `{"data":{"items":[{"domain":"app.example.com","rtype":"A","ttl":300,"rdata":"203.0.113.10."},{"domain":"app.example.com","rtype":"A","ttl":300,"rdata":"203.0.113.11"}]}}`
	items, err := ParseRRSet(fixture)
	if err != nil {
		t.Fatalf("ParseRRSet: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	// RRSet surgery: merge keeps round-robin records; filter drops the match.
	merged := MergeRecord(items, "app.example.com", "A", 300, "203.0.113.12")
	if len(merged) != 3 {
		t.Errorf("merge lost records: %+v", merged)
	}
	remaining := FilterRecord(items, "203.0.113.10")
	if len(remaining) != 1 || remaining[0].RData != "203.0.113.11" {
		t.Errorf("filter wrong: %+v", remaining)
	}
	if !RRSetContains(items, "203.0.113.11") {
		t.Error("contains failed for a matching rdata (trailing dot normalized)")
	}
	if RRSetContains(items, "203.0.113.99") {
		t.Error("contains true for an absent rdata")
	}
}

func TestNormalizeRData(t *testing.T) {
	if NormalizeRData("app.example.com.") != "app.example.com" {
		t.Error("trailing dot must strip for comparison")
	}
	if NormalizeRData("app.example.com") != "app.example.com" {
		t.Error("plain value unchanged")
	}
}
