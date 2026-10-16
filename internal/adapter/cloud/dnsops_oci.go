// OCI legs of the neutral DNS ops: TODAY's exported builders + parsers,
// composed verbatim — the oci argv is byte-identical to the pre-gcp flow
// (the PATH-shim argv pins in command/dns_test.go prove it).
package cloud

import (
	"context"
	"fmt"
	"strings"
)

// dnsCompartment is the OCI scope the DNS ops search: the dedicated DNS
// compartment (--dns-compartment flag / cloud block "dnsCompartment", both
// riding auth.DnsCompartment) when set — DNS zones routinely live outside
// the compute compartment — falling back to the compartment itself (the
// byte-identical default). gcp has no compartments and ignores the field.
func dnsCompartment(auth CloudAuth) string {
	if auth.DnsCompartment != "" {
		return auth.DnsCompartment
	}
	return auth.Compartment
}

// dnsScopeHint is the not-found remedy for a DNS scope miss: the compartment
// the ops searched may simply not be the one holding the zones.
func dnsScopeHint(compartment string) string {
	return fmt.Sprintf("is your DNS zone in another compartment? pass --dns-compartment or set the cloud block's \"dnsCompartment\" (searched: %s)", compartment)
}

func dnsResolveZoneOCI(ctx context.Context, auth CloudAuth, hint string) (Zone, error) {
	scope := dnsCompartment(auth)
	if hint != "" {
		// An ocid resolves through zone get; a plain name passes through
		// (the API accepts name-or-id everywhere downstream).
		if strings.HasPrefix(hint, "ocid1.dns-zone") {
			out, err := RunOCI(ctx, DNSZoneGetArgs(hint, scope, auth.Profile, auth.InstancePrincipal))
			if err != nil {
				return Zone{}, fmt.Errorf("zone not found: %s in compartment %s — %s", hint, scope, dnsScopeHint(scope))
			}
			zones, err := ParseZones(out)
			if err != nil || len(zones) == 0 {
				return Zone{}, fmt.Errorf("zone not found: %s in compartment %s — %s", hint, scope, dnsScopeHint(scope))
			}
			return Zone{Name: zones[0].Name, ID: hint}, nil
		}
		return Zone{Name: strings.TrimSuffix(hint, "."), ID: hint}, nil
	}
	out, err := RunOCI(ctx, DNSZoneListArgs(scope, auth.Profile, auth.InstancePrincipal))
	if err != nil {
		return Zone{}, fmt.Errorf("zone list failed (profile: %s, compartment: %s): %w", auth.Profile, scope, err)
	}
	zones, err := ParseZones(out)
	if err != nil {
		return Zone{}, err
	}
	if len(zones) == 0 {
		return Zone{}, fmt.Errorf("no managed zones in compartment %s — %s", scope, dnsScopeHint(scope))
	}
	if len(zones) > 1 {
		return Zone{}, &ManyZonesError{Scope: scope, Names: zoneNames(zones)}
	}
	return zones[0], nil
}

func dnsListRecordsOCI(ctx context.Context, auth CloudAuth, zone string) ([]Record, error) {
	out, err := RunOCI(ctx, DNSRecordsArgs(dnsCompartment(auth), zone, auth.Profile, auth.InstancePrincipal))
	if err != nil {
		return nil, err
	}
	return ParseRecords(out)
}

func dnsGetRRSetOCI(ctx context.Context, auth CloudAuth, zone, domain, rtype string) ([]Record, error) {
	// The get error is swallowed (today's semantics): a missing RRSet reads
	// as empty — the first add must not 404.
	out, _ := RunOCI(ctx, DNSRRSetGetArgs(dnsCompartment(auth), zone, domain, rtype, auth.Profile, auth.InstancePrincipal))
	return ParseRRSet(out)
}

func dnsUpdateRRSetOCI(ctx context.Context, auth CloudAuth, zone, domain, rtype string, items []Record) error {
	itemsJSON, err := MarshalItems(items)
	if err != nil {
		return err
	}
	_, err = RunOCI(ctx, DNSRRSetUpdateArgs(dnsCompartment(auth), zone, domain, rtype, itemsJSON, auth.Profile, auth.InstancePrincipal))
	return err
}

func dnsResolveScopeOCI(ctx context.Context, auth CloudAuth, name string) (string, error) {
	out, err := RunOCI(ctx, DNSCompartmentListArgs(auth))
	if err != nil {
		return "", err
	}
	return CompartmentOCID(out, name)
}
