// OCI legs of the neutral DNS ops: TODAY's exported builders + parsers,
// composed verbatim — the oci argv is byte-identical to the pre-gcp flow
// (the PATH-shim argv pins in command/dns_test.go prove it).
package cloud

import (
	"context"
	"fmt"
	"strings"
)

func dnsResolveZoneOCI(ctx context.Context, auth CloudAuth, hint string) (Zone, error) {
	if hint != "" {
		// An ocid resolves through zone get; a plain name passes through
		// (the API accepts name-or-id everywhere downstream).
		if strings.HasPrefix(hint, "ocid1.dns-zone") {
			out, err := RunOCI(ctx, DNSZoneGetArgs(hint, auth.Compartment, auth.Profile, auth.InstancePrincipal))
			if err != nil {
				return Zone{}, fmt.Errorf("zone not found: %s", hint)
			}
			zones, err := ParseZones(out)
			if err != nil || len(zones) == 0 {
				return Zone{}, fmt.Errorf("zone not found: %s", hint)
			}
			return Zone{Name: zones[0].Name, ID: hint}, nil
		}
		return Zone{Name: strings.TrimSuffix(hint, "."), ID: hint}, nil
	}
	out, err := RunOCI(ctx, DNSZoneListArgs(auth.Compartment, auth.Profile, auth.InstancePrincipal))
	if err != nil {
		return Zone{}, fmt.Errorf("zone list failed (profile: %s, compartment: %s): %w", auth.Profile, auth.Compartment, err)
	}
	zones, err := ParseZones(out)
	if err != nil {
		return Zone{}, err
	}
	if len(zones) == 0 {
		return Zone{}, fmt.Errorf("no DNS zones in compartment '%s' — create one in the OCI console first (or pass --zone <id-or-name>)", auth.Compartment)
	}
	if len(zones) > 1 {
		return Zone{}, &ManyZonesError{Scope: auth.Compartment, Names: zoneNames(zones)}
	}
	return zones[0], nil
}

func dnsListRecordsOCI(ctx context.Context, auth CloudAuth, zone string) ([]Record, error) {
	out, err := RunOCI(ctx, DNSRecordsArgs(auth.Compartment, zone, auth.Profile, auth.InstancePrincipal))
	if err != nil {
		return nil, err
	}
	return ParseRecords(out)
}

func dnsGetRRSetOCI(ctx context.Context, auth CloudAuth, zone, domain, rtype string) ([]Record, error) {
	// The get error is swallowed (today's semantics): a missing RRSet reads
	// as empty — the first add must not 404.
	out, _ := RunOCI(ctx, DNSRRSetGetArgs(auth.Compartment, zone, domain, rtype, auth.Profile, auth.InstancePrincipal))
	return ParseRRSet(out)
}

func dnsUpdateRRSetOCI(ctx context.Context, auth CloudAuth, zone, domain, rtype string, items []Record) error {
	itemsJSON, err := MarshalItems(items)
	if err != nil {
		return err
	}
	_, err = RunOCI(ctx, DNSRRSetUpdateArgs(auth.Compartment, zone, domain, rtype, itemsJSON, auth.Profile, auth.InstancePrincipal))
	return err
}

func dnsResolveScopeOCI(ctx context.Context, auth CloudAuth, name string) (string, error) {
	out, err := RunOCI(ctx, DNSCompartmentListArgs(auth))
	if err != nil {
		return "", err
	}
	return CompartmentOCID(out, name)
}
