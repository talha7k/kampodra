// Provider-neutral DNS ops: the dns.go command surface calls THESE, never a
// provider CLI directly. Every op dispatches on auth.Provider with a
// fail-closed default, so a provider that half-lands can only refuse, never
// improvise an argv. The per-provider legs live in dnsops_oci.go (byte-
// identical to the pre-gcp flow) and dnsops_gcp.go.
package cloud

import (
	"context"
	"fmt"
	"strings"
)

// ManyZonesError reports an ambiguous zone: the candidates must reach the
// operator, never a guess between several. Scope names where the ambiguity
// lives (the resolved compartment / the pinned project).
type ManyZonesError struct {
	Scope string
	Names []string
}

func (e *ManyZonesError) Error() string {
	return fmt.Sprintf("multiple zones in %s: %s", e.Scope, strings.Join(e.Names, ", "))
}

func zoneNames(zones []Zone) []string {
	names := make([]string, 0, len(zones))
	for _, z := range zones {
		names = append(names, z.Name)
	}
	return names
}

// DNSResolveZone resolves a --zone hint ("": the exactly-one-zone rule) to
// a zone — Name for domain math, ID as the provider's API handle.
func DNSResolveZone(ctx context.Context, auth CloudAuth, hint string) (Zone, error) {
	if err := CheckProvider(auth); err != nil {
		return Zone{}, err
	}
	if auth.Provider == "gcp" {
		return dnsResolveZoneGCP(ctx, auth, hint)
	}
	return dnsResolveZoneOCI(ctx, auth, hint)
}

// DNSListRecords lists a zone's records, one neutral Record per rdata.
func DNSListRecords(ctx context.Context, auth CloudAuth, zone string) ([]Record, error) {
	if err := CheckProvider(auth); err != nil {
		return nil, err
	}
	if auth.Provider == "gcp" {
		return gcpListRecords(ctx, auth, zone)
	}
	return dnsListRecordsOCI(ctx, auth, zone)
}

// DNSGetRRSet reads one RRSet (name+type) — a missing RRSet reads as empty
// so the first add never 404s.
func DNSGetRRSet(ctx context.Context, auth CloudAuth, zone, domain, rtype string) ([]Record, error) {
	if err := CheckProvider(auth); err != nil {
		return nil, err
	}
	if auth.Provider == "gcp" {
		return dnsGetRRSetGCP(ctx, auth, zone, domain, rtype)
	}
	return dnsGetRRSetOCI(ctx, auth, zone, domain, rtype)
}

// DNSUpdateRRSet replaces one RRSet: add merges, an emptied set removes.
func DNSUpdateRRSet(ctx context.Context, auth CloudAuth, zone, domain, rtype string, items []Record) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return dnsUpdateRRSetGCP(ctx, auth, zone, domain, rtype, items)
	}
	return dnsUpdateRRSetOCI(ctx, auth, zone, domain, rtype, items)
}

// DNSResolveScope resolves a compartment NAME to its ocid through the
// provider's identity API. OCI-only by definition: gcp has no compartments,
// so a gcp profile carrying one fails closed — scope there is the cloud
// block's project (or gcloud's active config).
func DNSResolveScope(ctx context.Context, auth CloudAuth, name string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", fmt.Errorf("compartment %q is OCI-only — a gcp cloud block scopes with {\"project\": \"…\"} (empty = gcloud's active config)", name)
	}
	return dnsResolveScopeOCI(ctx, auth, name)
}
