// gcloud legs of the neutral DNS ops: everything rides the gcloud CLI's
// own auth and active config. kampodra appends ONLY the cloud block's
// identity names (--project/--account, via withGCPAuth) — never credential
// material: token flags, key files, credential env vars, and client
// secrets are forbidden forever. gcloud emits bare [] for empty lists;
// the parsers read bare arrays natively.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"os"
	"strconv"
	"strings"
)

// gcpScope names where zone ambiguity and failures live: the pinned
// project, or gcloud's active config when the cloud block leaves it native.
func gcpScope(auth CloudAuth) string {
	if auth.Project != "" {
		return auth.Project
	}
	return "gcloud active config"
}

func gcpZones(ctx context.Context, auth CloudAuth) ([]Zone, error) {
	args := []string{"dns", "managed-zones", "list", "--format=json(name,dnsName)"}
	out, err := RunCloud(ctx, auth, args) // identity flags appended at the seam
	if err != nil {
		return nil, fmt.Errorf("zone list failed (%s): %w", gcpScope(auth), err)
	}
	var rows []struct {
		Name    string `json:"name"`    // the zone slug — gcloud's --zone handle
		DNSName string `json:"dnsName"` // the fqdn, trailing dot included
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("zone list: %w", err)
	}
	zones := make([]Zone, 0, len(rows))
	for _, r := range rows {
		zones = append(zones, Zone{Name: NormalizeRData(r.DNSName), ID: r.Name})
	}
	return zones, nil
}

func dnsResolveZoneGCP(ctx context.Context, auth CloudAuth, hint string) (Zone, error) {
	zones, err := gcpZones(ctx, auth)
	if err != nil {
		return Zone{}, err
	}
	if hint == "" {
		switch len(zones) {
		case 0:
			return Zone{}, fmt.Errorf("no managed zones in %s — create one in the Google Cloud console first (or pass --zone <zone-id>)", gcpScope(auth))
		case 1:
			return zones[0], nil
		}
		return Zone{}, &ManyZonesError{Scope: gcpScope(auth), Names: zoneNames(zones)}
	}
	// gcloud's --zone takes the slug only, so a dnsName hint resolves here;
	// the slug itself passes through to the same zone.
	for _, z := range zones {
		if z.Name == NormalizeRData(hint) || z.ID == hint {
			return z, nil
		}
	}
	return Zone{}, fmt.Errorf("zone not found: %s", hint)
}

// gcpListRecords lists a zone's record sets, expanding every rrdatas[]
// entry into one neutral Record (round-robin lives flat) with trailing
// dots normalized via NormalizeRData.
func gcpListRecords(ctx context.Context, auth CloudAuth, zone string) ([]Record, error) {
	args := []string{"dns", "record-sets", "list", "--zone=" + zone, "--format=json(name,type,ttl,rrdatas)"}
	out, err := RunCloud(ctx, auth, args) // identity flags appended at the seam
	if err != nil {
		return nil, fmt.Errorf("record list failed (%s): %w", gcpScope(auth), err)
	}
	var rows []struct {
		Name    string   `json:"name"`
		Type    string   `json:"type"`
		TTL     int      `json:"ttl"`
		RRDatas []string `json:"rrdatas"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("record list: %w", err)
	}
	var records []Record
	for _, r := range rows {
		for _, data := range r.RRDatas {
			records = append(records, Record{
				Domain: NormalizeRData(r.Name),
				RType:  r.Type,
				TTL:    r.TTL,
				RData:  NormalizeRData(data),
			})
		}
	}
	return records, nil
}

func dnsGetRRSetGCP(ctx context.Context, auth CloudAuth, zone, domain, rtype string) ([]Record, error) {
	records, err := gcpListRecords(ctx, auth, zone)
	if err != nil {
		return nil, err
	}
	var items []Record
	for _, r := range records {
		if r.RType == rtype && NormalizeRData(r.Domain) == NormalizeRData(domain) {
			items = append(items, r)
		}
	}
	return items, nil
}

// absoluteDomain renders the transaction name form gcloud pins in its own
// examples (and the form record-sets list returns): trailing dot required,
// so a remove matches the live RRSet exactly.
func absoluteDomain(d string) string {
	if strings.HasSuffix(d, ".") {
		return d
	}
	return d + "."
}

// dnsUpdateRRSetGCP ports the rrset-update upsert onto gcloud's transaction
// staging: start → remove (only when the RRSet exists) → add (emptied set =
// remove only, the --items '[]' semantic) → execute. The transaction file is
// an absolute temp file that never outlives the call — never a cwd-relative
// litter path.
func dnsUpdateRRSetGCP(ctx context.Context, auth CloudAuth, zone, domain, rtype string, items []Record) error {
	// remove needs the EXACT current RRSet (ttl + every rrdata), so the
	// server's state is re-fetched here rather than trusting a stale copy.
	old, err := dnsGetRRSetGCP(ctx, auth, zone, domain, rtype)
	if err != nil {
		return err
	}
	// gcloud's `transaction start` OWNS the file's creation and refuses
	// if it already exists (live-fire 2026-10-16: os.CreateTemp here made
	// every start fail with "Transaction already exists"). Mint a unique
	// empty DIR instead — the file inside never pre-exists, and cleanup
	// removes the whole thing: no litter either way.
	dir, err := os.MkdirTemp("", "kampodra-dns-txn-")
	if err != nil {
		return fmt.Errorf("transaction dir: %w", err)
	}
	txn := filepath.Join(dir, "transaction.yaml")
	defer os.RemoveAll(dir)

	run := func(stage string, extra ...string) error {
		args := append([]string{"dns", "record-sets", "transaction", stage,
			"--zone=" + zone, "--transaction-file=" + txn}, extra...)
		_, err := RunCloud(ctx, auth, args) // identity flags appended at the seam
		return err
	}
	if err := run("start"); err != nil {
		return err
	}
	if len(old) > 0 {
		rrdatas := make([]string, 0, len(old)+3)
		rrdatas = append(rrdatas, "--name="+absoluteDomain(domain), "--type="+rtype, "--ttl="+strconv.Itoa(old[0].TTL))
		for _, r := range old {
			rrdatas = append(rrdatas, r.RData)
		}
		if err := run("remove", rrdatas...); err != nil {
			return err
		}
	}
	if len(items) > 0 {
		rrdatas := make([]string, 0, len(items)+3)
		rrdatas = append(rrdatas, "--name="+absoluteDomain(domain), "--type="+rtype, "--ttl="+strconv.Itoa(items[0].TTL))
		for _, r := range items {
			rrdatas = append(rrdatas, r.RData)
		}
		if err := run("add", rrdatas...); err != nil {
			return err
		}
	}
	return run("execute")
}
