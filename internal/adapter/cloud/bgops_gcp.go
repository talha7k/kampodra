// gcloud legs of the neutral blue/green ops: everything rides the gcloud
// CLI's own auth and active config. kampodra appends ONLY the cloud
// block's identity names (--project/--account, via withGCPAuth at the
// RunCloud seam) — never credential material. The compartment param is
// ignored on these legs: gcp scopes by the cloud block's project (empty =
// gcloud's active config), same posture as the dns/backup legs — no
// resolution call. gcloud emits bare [] for empty lists; the parsers read
// bare arrays natively.
//
// Flag forms verified against gcloud SDK 578: `name:` filters parse (the
// `names:` spelling is server-rejected: "Invalid list filter
// expression"); the address attach primitive is `instances
// add-access-config --address` (update-access-config lost --address — it
// now only mutates PTR records); delete/add-access-config default to the
// "external-nat" access config on nic0, so neither name flag is passed.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// gcpReservedIPName is the pair's one reserved external address — the same
// name the command layer's OCI create call uses, pinned here because gcp
// reads it back by name (init/create/assign/release all key on it).
const gcpReservedIPName = "kampodra-active"

// --- shared reads -----------------------------------------------------------

// gcpInstanceRow is one `compute instances list --format=json(...)` row
// (aggregated across zones — the zone rides as a full URL whose last
// segment is the zone gcloud's --zone flag takes). Superset struct: each
// leg projects only the fields it needs; JSON leaves the rest zero.
type gcpInstanceRow struct {
	Name       string `json:"name"`
	Zone       string `json:"zone"`
	Created    string `json:"creationTimestamp"`
	NatIP      string `json:"natIP"`
	Status     string `json:"status"`
	Subnetwork string `json:"subnetwork"`
}

func gcpInstanceListGCP(ctx context.Context, auth CloudAuth, filter, format string) ([]gcpInstanceRow, error) {
	out, err := RunCloud(ctx, auth, []string{"compute", "instances", "list",
		"--filter=" + filter, "--format=" + format}) // identity flags appended at the seam
	if err != nil {
		return nil, err
	}
	var rows []gcpInstanceRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("instances list: %w", err)
	}
	return rows, nil
}

// gcpLastSeg takes the final path segment ("the URL, or the bare name").
func gcpLastSeg(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// gcpZoneRegion derives the region from a zone name: europe-west1-b ->
// europe-west1.
func gcpZoneRegion(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// gcpUserHolder parses the addresses-list users[0] form
// (.../zones/<z>/instances/<i>) — the holder instance + its zone, no
// anchor-join needed.
func gcpUserHolder(users []string) (inst, zone string) {
	if len(users) == 0 {
		return "", ""
	}
	seg := strings.Split(users[0], "/")
	for i, s := range seg {
		switch {
		case s == "zones" && i+1 < len(seg):
			zone = seg[i+1]
		case s == "instances" && i+1 < len(seg):
			inst = seg[i+1]
		}
	}
	return inst, zone
}

// gcpAddressRow is one `compute addresses list` row for the reserved
// address: status IN_USE = assigned (users[0] names the holder), RESERVED
// = dormant.
type gcpAddressRow struct {
	Name    string   `json:"name"`
	Address string   `json:"address"`
	Status  string   `json:"status"`
	Users   []string `json:"users"`
}

func gcpReservedAddressArgs() []string {
	return []string{"compute", "addresses", "list",
		"--filter=name=" + gcpReservedIPName,
		"--format=json(name,address,status,users)"}
}

// gcpReservedAddressGCP reads the pair's reserved address (ok=false when
// none exists — the dormant-pair posture).
func gcpReservedAddressGCP(ctx context.Context, auth CloudAuth) (gcpAddressRow, bool, error) {
	out, err := RunCloud(ctx, auth, gcpReservedAddressArgs()) // identity flags appended at the seam
	if err != nil {
		return gcpAddressRow{}, false, err
	}
	var rows []gcpAddressRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return gcpAddressRow{}, false, fmt.Errorf("addresses list: %w", err)
	}
	if len(rows) == 0 {
		return gcpAddressRow{}, false, nil
	}
	return rows[0], true, nil
}

// gcpInstanceZone resolves one instance's zone via the aggregated list
// (describe would need a zone the neutral signatures do not carry).
func gcpInstanceZone(ctx context.Context, auth CloudAuth, inst string) (string, error) {
	rows, err := gcpInstanceListGCP(ctx, auth, "name ~ ^"+inst+"$", "json(name,zone)")
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("instance not found: %s", inst)
	}
	return gcpLastSeg(rows[0].Zone), nil
}

// --- the legs ---------------------------------------------------------------

// bgColorInstanceGCP maps the newest RUNNING row onto BGInstance: the
// instance name rides the OCID slot (the provider handle), the zone the
// AD slot (dual meaning — the OCI leg puts the availability domain
// there), and natIP the public IP: one aggregated list row carries what
// the OCI leg needs a 3-call VNIC dance for. Zero rows = not provisioned.
func bgColorInstanceGCP(ctx context.Context, auth CloudAuth, displayName string) (BGInstance, error) {
	rows, err := gcpInstanceListGCP(ctx, auth,
		"name ~ ^"+displayName+"$ AND status=RUNNING",
		"json(name,zone,creationTimestamp,networkInterfaces[0].accessConfigs[0].natIP)")
	if err != nil {
		return BGInstance{}, err
	}
	best, bestCreated := -1, ""
	for i, r := range rows {
		if best < 0 || r.Created >= bestCreated {
			best, bestCreated = i, r.Created
		}
	}
	if best < 0 {
		return BGInstance{}, nil
	}
	return BGInstance{OCID: rows[best].Name, PublicIP: rows[best].NatIP, AD: gcpLastSeg(rows[best].Zone)}, nil
}

// bgPrimaryVnicGCP: gcp has no vnic-attachment dance — the instance IS
// its primary NIC's handle. The subnet name (last segment of the
// subnetwork URL) rides the subnet slot. ok=false when no such instance.
func bgPrimaryVnicGCP(ctx context.Context, auth CloudAuth, inst string) (vnicID, subnetID string, ok bool, err error) {
	rows, err := gcpInstanceListGCP(ctx, auth, "name ~ ^"+inst+"$",
		"json(name,zone,networkInterfaces[0].subnetwork)")
	if err != nil {
		return "", "", false, err
	}
	if len(rows) == 0 || rows[0].Subnetwork == "" {
		return "", "", false, nil
	}
	return rows[0].Name, gcpLastSeg(rows[0].Subnetwork), true, nil
}

// bgAnchorIPGCP: the anchor concept IS the reserved external IP (OCI's
// secondary-private-IP anchor has no gcp counterpart — the reserved IP is
// the anchor). It reads as the address while IN_USE, "" otherwise.
// FOLLOW-UP: the guest-side watcher contract changes with it — the
// anchor.conf feed becomes a GCE metadata external-IP poll (owner-approved
// design; vmbootstrap work, out of scope for this slice).
func bgAnchorIPGCP(ctx context.Context, auth CloudAuth) (string, error) {
	row, ok, err := gcpReservedAddressGCP(ctx, auth)
	if err != nil || !ok || row.Status != "IN_USE" {
		return "", err
	}
	return row.Address, nil
}

// bgReservedIPListGCP maps the reserved-address row onto the OCI parser's
// ReservedIP shape: the name rides the OCID slot (the handle every
// assign/release passes back), users[0]'s instance name the private-ip
// slot ("-" when dormant, mirroring ParseReservedIPRow).
func bgReservedIPListGCP(ctx context.Context, auth CloudAuth) (ReservedIP, bool, error) {
	row, ok, err := gcpReservedAddressGCP(ctx, auth)
	if err != nil || !ok {
		return ReservedIP{}, false, err
	}
	holder, _ := gcpUserHolder(row.Users)
	if holder == "" {
		holder = "-"
	}
	return ReservedIP{OCID: row.Name, Address: row.Address, PrivateIP: holder, Assigned: row.Status == "IN_USE"}, true, nil
}

// bgReservedIPCreateGCP creates the DORMANT reserved address. gcp
// addresses are regional, and the neutral signature carries no region —
// it derives from a RUNNING pair instance's zone (the pair shares one
// region); with none, fail closed before the create. The returned shape
// reuses the OCI create leg's inline parse semantics (JSON when the CLI
// emitted it, trimmed raw otherwise).
func bgReservedIPCreateGCP(ctx context.Context, auth CloudAuth) (string, error) {
	rows, err := gcpInstanceListGCP(ctx, auth, "status=RUNNING", "json(name,zone)")
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("could not derive the region for the reserved address — no RUNNING pair instance to anchor it (gcp addresses are regional); provision a color first")
	}
	out, err := RunCloud(ctx, auth, []string{"compute", "addresses", "create", gcpReservedIPName,
		"--region=" + gcpZoneRegion(gcpLastSeg(rows[0].Zone)),
		"--format=json(name,address,status)"}) // identity flags appended at the seam
	if err != nil {
		return "", err
	}
	var created []struct {
		Address string `json:"address"`
	}
	addr := strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(out), &created); err == nil && len(created) > 0 && created[0].Address != "" {
		addr = created[0].Address
	}
	return addr, nil
}

// gcpDetachAccessConfig drops the holder's external-IP access config; the
// address falls back to RESERVED on its own. The documented defaults
// apply ("external-nat" access config on nic0) — no name flags passed.
func gcpDetachAccessConfig(ctx context.Context, auth CloudAuth, inst, zone string) error {
	_, err := RunCloud(ctx, auth, []string{"compute", "instances", "delete-access-config",
		inst, "--zone=" + zone}) // identity flags appended at the seam
	return err
}

// bgReservedIPAssignGCP is THE FLIP: detach the reserved address from its
// current holder (users[0]), then attach it to the target instance. The
// attach is `add-access-config --address=<reserved name>` (SDK 578
// removed --address from update-access-config). Restore = the same call
// with the old holder as the target — the failed flip target detaches
// first, the old holder re-attaches. waitState is dropped on this leg:
// gcloud calls are synchronous, the OCI settle-state semantic has no
// counterpart.
func bgReservedIPAssignGCP(ctx context.Context, auth CloudAuth, ripName, targetInst string) error {
	row, ok, err := gcpReservedAddressGCP(ctx, auth)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("reserved address %s not found", ripName)
	}
	if inst, zone := gcpUserHolder(row.Users); inst != "" && inst != targetInst {
		if err := gcpDetachAccessConfig(ctx, auth, inst, zone); err != nil {
			return err
		}
	}
	zone, err := gcpInstanceZone(ctx, auth, targetInst)
	if err != nil {
		return err
	}
	_, err = RunCloud(ctx, auth, []string{"compute", "instances", "add-access-config",
		targetInst, "--zone=" + zone, "--address=" + ripName}) // identity flags appended at the seam
	return err
}

// bgReservedIPReleaseGCP: on gcp unassign IS release — drop the holder's
// access config and the address is RESERVED (dormant) again. Already
// dormant or missing = a no-op beyond the lookup.
func bgReservedIPReleaseGCP(ctx context.Context, auth CloudAuth) error {
	row, ok, err := gcpReservedAddressGCP(ctx, auth)
	if err != nil || !ok {
		return err
	}
	inst, zone := gcpUserHolder(row.Users)
	if inst == "" {
		return nil // RESERVED — nothing to detach
	}
	return gcpDetachAccessConfig(ctx, auth, inst, zone)
}

// bgPrivateIPAddressGCP: the anchor address lookup reads the reserved
// external address (the privateIPID param is the OCI anchor handle — no
// gcp counterpart).
func bgPrivateIPAddressGCP(ctx context.Context, auth CloudAuth) (string, error) {
	row, ok, err := gcpReservedAddressGCP(ctx, auth)
	if err != nil || !ok {
		return "", err
	}
	return row.Address, nil
}

// bgInstanceLaunchGCP launches the pair instance pinned to
// e2-standard-2 (the OCI leg pins VM.Standard.A1.Flex; t2a-standard-2 is
// the Arm alternative). The handle IS the name — kampodra chose it. The
// ssh key rides --metadata-from-file ("" omits it: the golden image
// carries the ops key).
func bgInstanceLaunchGCP(ctx context.Context, auth CloudAuth, ad, subnetID, imageID, displayName, sshKeyFile string) (string, error) {
	args := []string{"compute", "instances", "create", displayName,
		"--zone=" + ad,
		"--machine-type=e2-standard-2",
		"--subnet=" + subnetID,
		"--image=" + imageID}
	if sshKeyFile != "" {
		args = append(args, "--metadata-from-file", "ssh-keys="+sshKeyFile)
	}
	if _, err := RunCloud(ctx, auth, args); err != nil { // identity flags appended at the seam
		return "", fmt.Errorf("instance launch failed: %w", err)
	}
	return displayName, nil
}

// bgInstanceRecordGCP reads the instance record via the aggregated list —
// the zone-free describe (a describe would need a zone the neutral
// signature does not carry). The caller polls State to RUNNING. ImageID
// stays empty: list rows carry no image, and its only consumer (the
// template-image inject route) is refused upstream on gcp by the
// ocid-shape gate.
func bgInstanceRecordGCP(ctx context.Context, auth CloudAuth, inst string) (InstanceRecord, error) {
	rows, err := gcpInstanceListGCP(ctx, auth, "name ~ ^"+inst+"$", "json(name,zone,status)")
	if err != nil {
		return InstanceRecord{}, err
	}
	if len(rows) == 0 {
		return InstanceRecord{}, fmt.Errorf("instance not found: %s", inst)
	}
	return InstanceRecord{State: rows[0].Status, AD: gcpLastSeg(rows[0].Zone)}, nil
}

// bgGoldenImageGCP finds the newest image whose name starts with prefix.
// The filter narrows server-side (`name:` — the `names:` spelling is
// rejected), the prefix + newest pick stay client-side. No match = ("",
// "") — the command's own fallback path then runs. The firmware verdict
// is always the native-route one: GCE images boot UEFI/BIOS-flexible, the
// A1 UEFI_64 gate is OCI-only.
func bgGoldenImageGCP(ctx context.Context, auth CloudAuth, prefix string) (imageID, firmware string, err error) {
	out, err := RunCloud(ctx, auth, []string{"compute", "images", "list",
		"--filter=name:" + prefix,
		"--format=json(name,creationTimestamp)"}) // identity flags appended at the seam
	if err != nil {
		return "", "", err
	}
	var rows []struct {
		Name    string `json:"name"`
		Created string `json:"creationTimestamp"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return "", "", fmt.Errorf("images list: %w", err)
	}
	best, bestCreated := "", ""
	for _, r := range rows {
		if !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		if r.Created >= bestCreated {
			best, bestCreated = r.Name, r.Created
		}
	}
	if best == "" {
		return "", "", nil // none prefixed — the command's fallback path runs
	}
	return best, "UEFI_64", nil
}
