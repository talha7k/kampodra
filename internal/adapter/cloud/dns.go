// DNS family of the oci CLI surface (the dns.sh port): pure validators,
// zone/record/rrset parsing, and RRSet surgery primitives. AUTH RULE (hard):
// the oci CLI's own auth ONLY — its config file (--profile) or instance
// principal. kampodra NEVER accepts, stores, or logs credential material:
// no key flags, no credential env vars, nothing token-shaped in args or
// output. The OCI config file stays the single credential store.
package cloud

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// DNS arg vectors — every one carries --profile (the auth-rule contract).
func DNSCompartmentListArgs(auth CloudAuth) []string {
	args := []string{"iam", "compartment", "list", "--all"}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

func DNSZoneGetArgs(zone, compartment, profile string, instancePrincipal bool) []string {
	args := []string{"dns", "zone", "get", "--zone-name-or-id", zone, "-c", compartment}
	return withProfile(args, profile, instancePrincipal)
}

func DNSZoneListArgs(compartment, profile string, instancePrincipal bool) []string {
	args := []string{"dns", "zone", "list", "--all", "-c", compartment}
	return withProfile(args, profile, instancePrincipal)
}

func DNSRecordsArgs(compartment, zone, profile string, instancePrincipal bool) []string {
	args := []string{"dns", "record", "zone", "get", "-c", compartment, "--zone-name-or-id", zone}
	return withProfile(args, profile, instancePrincipal)
}

func DNSRRSetGetArgs(compartment, zone, domain, rtype, profile string, instancePrincipal bool) []string {
	args := []string{"dns", "record", "rrset", "get", "-c", compartment, "--zone-name-or-id", zone, "--domain", domain, "--rtype", rtype}
	return withProfile(args, profile, instancePrincipal)
}

func DNSRRSetUpdateArgs(compartment, zone, domain, rtype, items, profile string, instancePrincipal bool) []string {
	args := []string{"dns", "record", "rrset", "update", "-c", compartment, "--zone-name-or-id", zone,
		"--domain", domain, "--rtype", rtype, "--items", items, "--force"}
	return withProfile(args, profile, instancePrincipal)
}

// --- pure validators (no side effects, run before any provider call) ------

// IsIPv4 ports is_ipv4: dotted quad, every octet <= 255.
func IsIPv4(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
		n := 0
		for _, r := range p {
			n = n*10 + int(r-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// IsIPv6 ports is_ipv6: hex digits + colons only, at least one hex digit,
// no triple colons, at most 8 groups (≤7 colons), at most one "::".
func IsIPv6(ip string) bool {
	for _, r := range ip {
		if r != ':' && (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	if !strings.ContainsAny(ip, "0123456789abcdefABCDEF") {
		return false // at least one hex digit
	}
	if strings.Contains(ip, ":::") {
		return false
	}
	colons := strings.Count(ip, ":")
	if colons > 7 {
		return false
	}
	withoutFirst := strings.Replace(ip, "::", "", 1)
	return !strings.Contains(withoutFirst, "::")
}

// hostnameRe ports is_hostname.
var hostnameRe = regexp.MustCompile(`^([A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.)+[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.?$`)

func IsHostname(s string) bool { return hostnameRe.MatchString(s) }

// dnsLabelRe ports validate_name_shape's label branch.
var dnsLabelRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)

func IsDNSLabel(s string) bool { return dnsLabelRe.MatchString(s) }

// ValidateRecordType pins the supported types: A | AAAA | CNAME.
func ValidateRecordType(rtype string) error {
	switch rtype {
	case "A", "AAAA", "CNAME":
		return nil
	case "":
		return fmt.Errorf("--type is required: A, AAAA, or CNAME (--help)")
	default:
		return fmt.Errorf("--type must be one of: A, AAAA, CNAME (got: %s)", rtype)
	}
}

// ValidateRecordValue ports validate_value: type-shaped values only; quotes
// and backslashes are never valid in a record value.
func ValidateRecordValue(rtype, value string) error {
	if value == "" {
		return fmt.Errorf("--value is required (--help)")
	}
	if strings.ContainsAny(value, "\"\\") {
		return fmt.Errorf("invalid --value — quotes/backslashes are never valid in a record value")
	}
	switch rtype {
	case "A":
		if !IsIPv4(value) {
			return fmt.Errorf("invalid --value for A — need dotted-quad IPv4 (203.0.113.10)")
		}
	case "AAAA":
		if !IsIPv6(value) {
			return fmt.Errorf("invalid --value for AAAA — need an IPv6 literal (fd00::1)")
		}
	case "CNAME":
		if !IsHostname(value) {
			return fmt.Errorf("invalid --value for CNAME — need a hostname (app.example.com)")
		}
	}
	return nil
}

// ValidateTTL ports validate_ttl: OCI allows 60..172800 seconds.
func ValidateTTL(ttl string) error {
	n, err := parsePositiveInt(ttl)
	if err != nil {
		return fmt.Errorf("invalid --ttl '%s' — seconds (60..172800)", ttl)
	}
	if n < 60 || n > 172800 {
		return fmt.Errorf("invalid --ttl %d — OCI allows 60..172800 seconds", n)
	}
	return nil
}

// NormalizeRData strips a trailing dot for value comparison (CNAMEs and
// OC-fetched rdata carry absolute forms).
func NormalizeRData(v string) string { return strings.TrimSuffix(v, ".") }

// ValidateDomainShape ports validate_name_shape: a plain label or an fqdn.
func ValidateDomainShape(name string) error {
	if IsDNSLabel(name) || IsHostname(name) {
		return nil
	}
	return fmt.Errorf("invalid --name — use a DNS label (app) or an fqdn inside the zone (app.example.com)")
}

// ValidateDomain ports resolve_domain's guard: an fqdn must live inside the
// zone (never write into someone else's zone by accident).
func ValidateDomain(name, zoneName string) error {
	if IsDNSLabel(name) {
		return nil
	}
	domain := NormalizeRData(name)
	if domain == zoneName || strings.HasSuffix(domain, "."+zoneName) {
		return nil
	}
	return fmt.Errorf("--name '%s' is outside zone '%s' — pass --zone <that-zone> explicitly if intended", name, zoneName)
}

// ResolveDomain turns --name into the record domain: a plain label becomes
// label.zone; an fqdn passes through (trailing dot stripped).
func ResolveDomain(name, zoneName string) string {
	if IsDNSLabel(name) {
		return name + "." + zoneName
	}
	return NormalizeRData(name)
}

func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// --- oci response parsing (native — the shell needed jq for this) ---------

// CompartmentOCID finds the named compartment in `iam compartment list`
// output ({"data":[…]}, the oci CLI's wrapper — a bare array is tolerated),
// refusing non-compartment ocids.
func CompartmentOCID(listJSON, name string) (string, error) {
	var wrapped struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	var rows []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(listJSON), &wrapped); err == nil && wrapped.Data != nil {
		rows = append(rows, wrapped.Data...)
	} else if err := json.Unmarshal([]byte(listJSON), &rows); err != nil {
		return "", fmt.Errorf("compartment list: %w", err)
	}
	for _, r := range rows {
		if r.Name == name {
			if !strings.HasPrefix(r.ID, "ocid1.compartment") {
				return "", fmt.Errorf("compartment %q resolved to something that is not an ocid1.compartment ocid: %q", name, r.ID)
			}
			return r.ID, nil
		}
	}
	return "", fmt.Errorf("compartment %q not found", name)
}

// Zone is one parsed zone row.
type Zone struct {
	Name string
	ID   string
}

// ParseZones parses `dns zone list` output.
func ParseZones(listJSON string) ([]Zone, error) {
	var parsed struct {
		Data struct {
			Items []struct {
				Name string `json:"name"`
				ID   string `json:"id"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listJSON), &parsed); err != nil {
		return nil, fmt.Errorf("zone list: %w", err)
	}
	zones := make([]Zone, 0, len(parsed.Data.Items))
	for _, z := range parsed.Data.Items {
		zones = append(zones, Zone{Name: z.Name, ID: z.ID})
	}
	return zones, nil
}

// Record is one DNS record row.
type Record struct {
	Domain string `json:"domain"`
	RType  string `json:"rtype"`
	TTL    int    `json:"ttl"`
	RData  string `json:"rdata"`
}

// ParseRecords parses `dns record zone get` output.
func ParseRecords(zoneJSON string) ([]Record, error) {
	var parsed struct {
		Data struct {
			Items []Record `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(zoneJSON), &parsed); err != nil {
		return nil, fmt.Errorf("zone records: %w", err)
	}
	return parsed.Data.Items, nil
}

// ParseRRSet parses `dns record rrset get` output; a missing RRSet reads as
// empty (the first add must not 404).
func ParseRRSet(rrsetJSON string) ([]Record, error) {
	if strings.TrimSpace(rrsetJSON) == "" {
		return nil, nil
	}
	var parsed struct {
		Data struct {
			Items []Record `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(rrsetJSON), &parsed); err != nil {
		return nil, fmt.Errorf("rrset: %w", err)
	}
	return parsed.Data.Items, nil
}

// RRSetContains reports whether the RRSet already carries the value
// (trailing-dot normalized).
func RRSetContains(items []Record, value string) bool {
	norm := NormalizeRData(value)
	for _, r := range items {
		if NormalizeRData(r.RData) == norm {
			return true
		}
	}
	return false
}

// MergeRecord appends a new record to the RRSet (round-robin records
// survive — `add` MERGES, never replaces).
func MergeRecord(items []Record, domain, rtype string, ttl int, value string) []Record {
	out := append([]Record(nil), items...)
	out = append(out, Record{Domain: domain, RType: rtype, TTL: ttl, RData: value})
	return out
}

// FilterRecord removes every record whose rdata matches the value
// (trailing-dot normalized).
func FilterRecord(items []Record, value string) []Record {
	norm := NormalizeRData(value)
	var out []Record
	for _, r := range items {
		if NormalizeRData(r.RData) != norm {
			out = append(out, r)
		}
	}
	return out
}

// MarshalItems canonicalizes an RRSet for `rrset update --items`.
func MarshalItems(items []Record) (string, error) {
	if items == nil {
		items = []Record{}
	}
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
