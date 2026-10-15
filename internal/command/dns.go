package command

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
)

// dnsHelp is the shell dns.sh usage heredoc, kampodra-fied. AUTH RULE
// (hard): the provider CLI's own auth ONLY — oci's config file / instance
// principal, gcloud's active config. kampodra NEVER accepts, stores, or
// logs credential material — identity names only, forever. Types are
// pinned to A | AAAA | CNAME; ttl range 60..172800.
const dnsHelp = `Usage:
  kampodra dns records [--zone <id-or-name>]                  # list records: domain / type / ttl / value
  kampodra dns add --name <label> --type A|AAAA|CNAME --value <target> [--ttl 300]
  kampodra dns rm --name <label> --type <A|AAAA|CNAME> --value <target>
  (all: optional --zone <id-or-name>)

Auth is the provider CLI's own — kampodra passes no auth flags unless the
instance profile's "cloud" block overrides it:
  oci: {"profile": "…", "instancePrincipal": true, "compartment": "…"} —
       OCI_PROFILE / OCI_COMPARTMENT env are the escape hatch.
  gcp: {"provider": "gcp", "project": "…", "account": "…"} — an empty
       project/account = gcloud's active config; CLOUDSDK_CORE_PROJECT /
       CLOUDSDK_CORE_ACCOUNT / CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT
       are gcloud's own env escape hatch (kampodra never reads them).
Identity names only: access-token flags, key files, credential env vars,
and client secrets are never accepted, stored, or logged. Types are pinned
to A | AAAA | CNAME. ` + "`add`" + ` merges into the existing RRSet (round-robin
records survive); ` + "`rm`" + ` filters it; both are idempotence-aware.

Examples:
  kampodra dns records
  kampodra dns records --zone example.com
  kampodra dns add --name app --type A --value 203.0.113.10 --zone example.com
  kampodra dns add --name www --type CNAME --value app.example.com --ttl 3600
  kampodra dns rm --name app --type A --value 203.0.113.10
`

func newDNSCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "DNS records via the provider CLI's own auth (never credential material): records | add | rm",
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
	pf.String("profile", "", "kampodra instance profile — carries the cloud block (provider CLI auth overrides: profile/compartment/project/account)")
	pf.String("zone", "", "zone id or name (default: the compartment's ONLY zone — several require this flag)")
	pf.String("name", "", "record name: a DNS label (app) or an fqdn inside the zone")
	pf.String("type", "", "record type: A, AAAA, or CNAME")
	pf.String("value", "", "record target (type-shaped: IPv4 / IPv6 / hostname)")
	pf.String("ttl", "300", "TTL seconds (60..172800)")

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
// + resolve_zone sequence). The compartment's resolved ocid rides back on
// auth.Compartment — the neutral ops read it from there.
type dnsContext struct {
	auth     cloud.CloudAuth
	zoneName string
	zoneID   string
}

// dnsCheckCLI is the pre-flight guard: the provider's own CLI must be on
// PATH — the fail-closed remedy is the install hint, never a credential
// workaround.
func dnsCheckCLI(auth cloud.CloudAuth) error {
	cli := cloud.CLIName(auth.Provider)
	if _, err := exec.LookPath(cli); err != nil {
		return fmt.Errorf("%s CLI not found — %s", cli, cloud.InstallHint(auth.Provider))
	}
	return nil
}

// dnsNeedProvider gates on the provider CLI and (OCI only) a compartment —
// gcp scopes natively via the cloud block's project, so an empty project
// resolving through gcloud's active config is NOT an error.
func dnsNeedProvider(d Deps, auth cloud.CloudAuth) error {
	if err := dnsCheckCLI(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return nil
	}
	if v, ok := d.Env("OCI_COMPARTMENT"); !ok || v == "" {
		return fmt.Errorf("set OCI_COMPARTMENT=<compartment name or ocid> — no default: compartments are account-specific")
	}
	return nil
}

// dnsResolveZone resolves the --zone hint through the neutral op (ocid →
// get the name; name → as-is; empty → the exactly-one-zone rule — never a
// guess between several).
func dnsResolveZone(d Deps, c *cobra.Command, dc *dnsContext) error {
	zone, err := cloud.DNSResolveZone(c.Context(), dc.auth, flagString(c, "zone"))
	if err != nil {
		var mz *cloud.ManyZonesError
		if errors.As(err, &mz) {
			// the candidates must reach the operator, never a guess
			fmt.Fprintf(d.Stderr, "[dns] FAIL: multiple zones in %s — pass --zone with one of:\n", mz.Scope)
			for _, name := range mz.Names {
				fmt.Fprintf(d.Stderr, "  %s\n", name)
			}
			return &exitError{code: 1}
		}
		return err
	}
	dc.zoneName, dc.zoneID = zone.Name, zone.ID
	return nil
}

func dnsResolveCompartment(d Deps, ctx context.Context, target Target, dc *dnsContext) error {
	dc.auth = cloudAuthFor(d, target)
	if err := cloud.CheckProvider(dc.auth); err != nil {
		return err
	}
	if dc.auth.Provider == "gcp" {
		// gcp has no compartments: scope is the block's project (the legs
		// read it directly); carrying one is a config error the neutral op
		// refuses.
		if dc.auth.Compartment != "" {
			_, err := cloud.DNSResolveScope(ctx, dc.auth, dc.auth.Compartment)
			return err
		}
		return nil
	}
	if dc.auth.Compartment == "" {
		return fmt.Errorf("no OCI compartment configured — set the instance profile's \"cloud\" block ({\"compartment\": \"<name or ocid>\"}) in ~/.kampodra/config.json, or export OCI_COMPARTMENT=<compartment name or ocid> — compartments are account-specific")
	}
	// A name (not an ocid) resolves through the neutral scope op; the ocid
	// rides back on auth.Compartment for the ops downstream.
	if !strings.HasPrefix(dc.auth.Compartment, "ocid1.") {
		ocid, err := cloud.DNSResolveScope(ctx, dc.auth, dc.auth.Compartment)
		if err != nil {
			return fmt.Errorf("compartment '%s' not found (auth profile: %s)", dc.auth.Compartment, profileLabel(dc.auth))
		}
		dc.auth.Compartment = ocid
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
// call happens on a validation failure. Auth resolves BEFORE the pre-flight
// guard — which provider's CLI to demand is itself provider-derived.
func dnsPrepare(d Deps, c *cobra.Command, ctx context.Context, target Target, needValue bool) (dc *dnsContext, name, rtype, value, ttl string, err error) {
	name, rtype, value, ttl, err = dnsValidateArgs(c, needValue)
	if err != nil {
		return nil, "", "", "", "", err
	}
	dc = &dnsContext{auth: cloudAuthFor(d, target)}
	if err = dnsNeedProvider(d, dc.auth); err != nil {
		return nil, "", "", "", "", err
	}
	if err = dnsResolveCompartment(d, ctx, target, dc); err != nil {
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
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	dc := &dnsContext{auth: cloudAuthFor(d, target)}
	if err := dnsNeedProvider(d, dc.auth); err != nil {
		return err
	}
	if err := dnsResolveCompartment(d, c.Context(), target, dc); err != nil {
		return err
	}
	if err := dnsResolveZone(d, c, dc); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[dns] DNS records — zone %s (%s), auth profile: %s —\n", dc.zoneName, dc.zoneID, profileLabel(dc.auth))
	records, err := cloud.DNSListRecords(c.Context(), dc.auth, dc.zoneID)
	if err != nil {
		return err
	}
	for _, r := range records {
		fmt.Fprintf(d.Stdout, "%s\t%s\t%d\t%s\n", r.Domain, r.RType, r.TTL, r.RData)
	}
	return nil
}

func runDNSAdd(d Deps, c *cobra.Command) error {
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	dc, name, rtype, value, ttl, err := dnsPrepare(d, c, c.Context(), target, true)
	if err != nil {
		return err
	}
	domain := cloud.ResolveDomain(name, dc.zoneName)
	items, err := cloud.DNSGetRRSet(c.Context(), dc.auth, dc.zoneID, domain, rtype)
	if err != nil {
		return err
	}
	if cloud.RRSetContains(items, value) {
		fmt.Fprintf(d.Stdout, "[dns] already present: %s %s %s — nothing to do\n", domain, rtype, value)
		return nil
	}
	ttlN, _ := strconv.Atoi(ttl)
	merged := cloud.MergeRecord(items, domain, rtype, ttlN, value)
	if err := cloud.DNSUpdateRRSet(c.Context(), dc.auth, dc.zoneID, domain, rtype, merged); err != nil {
		return fmt.Errorf("RRSet update failed for %s %s (zone %s): %w", domain, rtype, dc.zoneName, err)
	}
	fmt.Fprintf(d.Stdout, "[dns] added: %s %s %s (ttl %s)\n", domain, rtype, value, ttl)
	return nil
}

func runDNSRm(d Deps, c *cobra.Command) error {
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	dc, name, rtype, value, _, err := dnsPrepare(d, c, c.Context(), target, true)
	if err != nil {
		return err
	}
	domain := cloud.ResolveDomain(name, dc.zoneName)
	items, err := cloud.DNSGetRRSet(c.Context(), dc.auth, dc.zoneID, domain, rtype)
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
	// an emptied RRSet is legal: the OCI leg marshals [] itself
	if err := cloud.DNSUpdateRRSet(c.Context(), dc.auth, dc.zoneID, domain, rtype, remaining); err != nil {
		return fmt.Errorf("RRSet update failed for %s %s (zone %s): %w", domain, rtype, dc.zoneName, err)
	}
	fmt.Fprintf(d.Stdout, "[dns] removed: %s %s %s\n", domain, rtype, value)
	return nil
}
