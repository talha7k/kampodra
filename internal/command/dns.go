package command

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
)

// dnsHelp is the shell dns.sh usage heredoc, kampodra-fied. AUTH RULE
// (hard): the oci CLI's own auth ONLY — its config file (--profile) or
// instance principal. kampodra NEVER accepts, stores, or logs credential
// material. Types are pinned to A | AAAA | CNAME; ttl range 60..172800.
const dnsHelp = `Usage:
  kampodra dns records [--zone <id-or-name>]                  # list records: domain / type / ttl / value
  kampodra dns add --name <label> --type A|AAAA|CNAME --value <target> [--ttl 300]
  kampodra dns rm --name <label> --type <A|AAAA|CNAME> --value <target>
  (all: optional --zone <id-or-name>; --instance-principal for instance auth)

Auth is the oci CLI's own — its config file (--profile) or instance
principal. kampodra never accepts, stores, or logs credential material.
Zone/compartment: --profile (default: OCI_PROFILE env > "default"),
OCI_COMPARTMENT (required — no default: compartments are account-specific).
Types are pinned to A | AAAA | CNAME. ` + "`add`" + ` merges into the existing
RRSet (round-robin records survive); ` + "`rm`" + ` filters it; both are
idempotence-aware.

Examples:
  kampodra dns records
  kampodra dns records --zone kampodine.example.com
  kampodra dns add --name app --type A --value 203.0.113.10 --zone kampodine.example.com
  kampodra dns add --name www --type CNAME --value app.example.com --ttl 3600
  kampodra dns rm --name app --type A --value 203.0.113.10
`

func newDNSCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "OCI DNS records (oci auth only — never credential material): records | add | rm",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), dnsHelp)
			return nil
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), dnsHelp)
	})
	pf := cmd.PersistentFlags()
	pf.String("zone", "", "zone id or name (default: the compartment's ONLY zone — several require this flag)")
	pf.String("name", "", "record name: a DNS label (app) or an fqdn inside the zone")
	pf.String("type", "", "record type: A, AAAA, or CNAME")
	pf.String("value", "", "record target (type-shaped: IPv4 / IPv6 / hostname)")
	pf.String("ttl", "300", "TTL seconds (60..172800)")
	pf.String("profile", "", "OCI CONFIG profile (default: OCI_PROFILE env > \"default\")")
	pf.Bool("instance-principal", false, "authenticate as the instance principal (when run ON a VM)")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "records",
			Short: "list the zone's records (domain / type / ttl / value)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runDNSRecords(d, c)
			},
		},
		&cobra.Command{
			Use:   "add",
			Short: "add a record (merges into the RRSet — round-robin survives)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runDNSAdd(d, c)
			},
		},
		&cobra.Command{
			Use:   "rm",
			Short: "remove a record from the RRSet (an emptied RRSet is removed entirely)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runDNSRm(d, c)
			},
		},
	)
	return cmd
}

// dnsContext bundles the resolved provider inputs (the shell's need_provider
// + resolve_zone sequence).
type dnsContext struct {
	compartment string
	profile     string
	ip          bool
	zoneName    string
	zoneID      string
}

func dnsNeedProvider(d Deps) error {
	if _, err := exec.LookPath("oci"); err != nil {
		return fmt.Errorf("oci CLI not found — install oci-cli (https://docs.oracle.com/en-us/iaas/Content/API/SDKDocs/cliinstall.htm), then: oci setup config")
	}
	if v, ok := d.Env("OCI_COMPARTMENT"); !ok || v == "" {
		return fmt.Errorf("set OCI_COMPARTMENT=<compartment name or ocid> — no default: compartments are account-specific")
	}
	return nil
}

// dnsResolveZone ports resolve_zone: explicit --zone wins (ocid → get the
// name; name → as-is); otherwise the compartment must hold exactly one
// zone — never guess between several.
func dnsResolveZone(d Deps, c *cobra.Command, dc *dnsContext) error {
	zoneArg := flagString(c, "zone")
	ctx := c.Context()
	if zoneArg != "" {
		if strings.HasPrefix(zoneArg, "ocid1.dns-zone") {
			out, err := cloud.RunOCI(ctx, cloud.DNSZoneGetArgs(zoneArg, dc.compartment, dc.profile, dc.ip))
			if err != nil {
				return fmt.Errorf("zone not found: %s", zoneArg)
			}
			zones, err := cloud.ParseZones(out)
			if err != nil || len(zones) == 0 {
				return fmt.Errorf("zone not found: %s", zoneArg)
			}
			dc.zoneName, dc.zoneID = zones[0].Name, zoneArg
			return nil
		}
		dc.zoneName = strings.TrimSuffix(zoneArg, ".")
		dc.zoneID = zoneArg
		return nil
	}
	out, err := cloud.RunOCI(ctx, cloud.DNSZoneListArgs(dc.compartment, dc.profile, dc.ip))
	if err != nil {
		return fmt.Errorf("zone list failed (profile %s, compartment %s): %w", dc.profile, dc.compartment, err)
	}
	zones, err := cloud.ParseZones(out)
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return fmt.Errorf("no DNS zones in compartment '%s' — create one in the OCI console first (or pass --zone <id-or-name>)", dc.compartment)
	}
	if len(zones) > 1 {
		fmt.Fprintf(d.Stderr, "[dns] FAIL: multiple zones in %s — pass --zone with one of:\n", dc.compartment)
		for _, z := range zones {
			fmt.Fprintf(d.Stderr, "  %s\n", z.Name)
		}
		return &exitError{code: 1}
	}
	dc.zoneName, dc.zoneID = zones[0].Name, zones[0].ID
	return nil
}

func dnsResolveCompartment(d Deps, c *cobra.Command, dc *dnsContext) error {
	compartmentEnv, ok := d.Env("OCI_COMPARTMENT")
	if !ok || compartmentEnv == "" {
		return fmt.Errorf("set OCI_COMPARTMENT=<compartment name or ocid> — no default: compartments are account-specific")
	}
	dc.compartment = compartmentEnv
	dc.profile = ociProfile(d, c)
	dc.ip = flagBool(c, "instance-principal")

	// A name (not an ocid) resolves through iam compartment list.
	if !strings.HasPrefix(compartmentEnv, "ocid1.") {
		out, err := cloud.RunOCI(c.Context(), cloud.DNSCompartmentListArgs(dc.profile, dc.ip))
		if err != nil {
			return err
		}
		ocid, err := cloud.CompartmentOCID(out, compartmentEnv)
		if err != nil {
			return fmt.Errorf("compartment '%s' not found (profile %s)", compartmentEnv, dc.profile)
		}
		dc.compartment = ocid
	}
	return nil
}

// dnsValidateArgs runs the pure validators BEFORE any provider call (bad
// args die locally, no oci invocation).
func dnsValidateArgs(c *cobra.Command, needValue bool) (name, rtype, value, ttl string, err error) {
	name = flagString(c, "name")
	rtype = flagString(c, "type")
	value = flagString(c, "value")
	ttl = flagString(c, "ttl")
	if name == "" {
		return "", "", "", "", fmt.Errorf("--name is required (--help)")
	}
	if err := cloud.ValidateRecordType(rtype); err != nil {
		return "", "", "", "", err
	}
	if needValue {
		if err := cloud.ValidateRecordValue(rtype, value); err != nil {
			return "", "", "", "", err
		}
	}
	if needValue {
		if err := cloud.ValidateTTL(ttl); err != nil {
			return "", "", "", "", err
		}
	}
	if err := cloud.ValidateDomainShape(name); err != nil {
		return "", "", "", "", err
	}
	return name, rtype, value, ttl, nil
}

// dnsPrepare chains validation -> provider check -> compartment -> zone ->
// domain guard (the add/rm prelude). All validation is local; no provider
// call happens on a validation failure.
func dnsPrepare(d Deps, c *cobra.Command, needValue bool) (dc *dnsContext, name, rtype, value, ttl string, err error) {
	name, rtype, value, ttl, err = dnsValidateArgs(c, needValue)
	if err != nil {
		return nil, "", "", "", "", err
	}
	dc = &dnsContext{}
	if err = dnsNeedProvider(d); err != nil {
		return nil, "", "", "", "", err
	}
	if err = dnsResolveCompartment(d, c, dc); err != nil {
		return nil, "", "", "", "", err
	}
	if err = dnsResolveZone(d, c, dc); err != nil {
		return nil, "", "", "", "", err
	}
	if err = cloud.ValidateDomain(name, dc.zoneName); err != nil {
		return nil, "", "", "", "", err
	}
	return dc, name, rtype, value, ttl, nil
}

func runDNSRecords(d Deps, c *cobra.Command) error {
	dc := &dnsContext{}
	if _, err := exec.LookPath("oci"); err != nil {
		return fmt.Errorf("oci CLI not found — install oci-cli (https://docs.oracle.com/en-us/iaas/Content/API/SDKDocs/cliinstall.htm), then: oci setup config")
	}
	if err := dnsResolveCompartment(d, c, dc); err != nil {
		return err
	}
	if err := dnsResolveZone(d, c, dc); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[dns] DNS records — zone %s (%s), profile %s:\n", dc.zoneName, dc.zoneID, dc.profile)
	out, err := cloud.RunOCI(c.Context(), cloud.DNSRecordsArgs(dc.compartment, dc.zoneID, dc.profile, dc.ip))
	if err != nil {
		return err
	}
	records, err := cloud.ParseRecords(out)
	if err != nil {
		return err
	}
	for _, r := range records {
		fmt.Fprintf(d.Stdout, "%s\t%s\t%d\t%s\n", r.Domain, r.RType, r.TTL, r.RData)
	}
	return nil
}

func runDNSAdd(d Deps, c *cobra.Command) error {
	dc, name, rtype, value, ttl, err := dnsPrepare(d, c, true)
	if err != nil {
		return err
	}
	domain := cloud.ResolveDomain(name, dc.zoneName)
	rrsetOut, _ := cloud.RunOCI(c.Context(), cloud.DNSRRSetGetArgs(dc.compartment, dc.zoneID, domain, rtype, dc.profile, dc.ip))
	items, err := cloud.ParseRRSet(rrsetOut)
	if err != nil {
		return err
	}
	if cloud.RRSetContains(items, value) {
		fmt.Fprintf(d.Stdout, "[dns] already present: %s %s %s — nothing to do\n", domain, rtype, value)
		return nil
	}
	ttlN, _ := strconv.Atoi(ttl)
	merged := cloud.MergeRecord(items, domain, rtype, ttlN, value)
	itemsJSON, err := cloud.MarshalItems(merged)
	if err != nil {
		return err
	}
	if _, err := cloud.RunOCI(c.Context(), cloud.DNSRRSetUpdateArgs(dc.compartment, dc.zoneID, domain, rtype, itemsJSON, dc.profile, dc.ip)); err != nil {
		return fmt.Errorf("RRSet update failed for %s %s (zone %s): %w", domain, rtype, dc.zoneName, err)
	}
	fmt.Fprintf(d.Stdout, "[dns] added: %s %s %s (ttl %s)\n", domain, rtype, value, ttl)
	return nil
}

func runDNSRm(d Deps, c *cobra.Command) error {
	dc, name, rtype, value, _, err := dnsPrepare(d, c, true)
	if err != nil {
		return err
	}
	domain := cloud.ResolveDomain(name, dc.zoneName)
	rrsetOut, _ := cloud.RunOCI(c.Context(), cloud.DNSRRSetGetArgs(dc.compartment, dc.zoneID, domain, rtype, dc.profile, dc.ip))
	items, err := cloud.ParseRRSet(rrsetOut)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return fmt.Errorf("no matching record: %s %s %s (see: kampodra dns records)", domain, rtype, value)
	}
	remaining := cloud.FilterRecord(items, value)
	if len(remaining) == len(items) {
		return fmt.Errorf("no matching record: %s %s %s (see: kampodra dns records)", domain, rtype, value)
	}
	itemsJSON, err := cloud.MarshalItems(remaining) // an emptied RRSet is legal: --items '[]' removes it entirely
	if err != nil {
		return err
	}
	if _, err := cloud.RunOCI(c.Context(), cloud.DNSRRSetUpdateArgs(dc.compartment, dc.zoneID, domain, rtype, itemsJSON, dc.profile, dc.ip)); err != nil {
		return fmt.Errorf("RRSet update failed for %s %s (zone %s): %w", domain, rtype, dc.zoneName, err)
	}
	fmt.Fprintf(d.Stdout, "[dns] removed: %s %s %s\n", domain, rtype, value)
	return nil
}
