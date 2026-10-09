// Blue/green + image-import OCI surface: reserved public IPs, compute
// instances, VNICs/private IPs, subnets, custom images, and the object
// staging for qcow2 imports. Same rules as the dns/backup families: the
// oci CLI's own auth ONLY (auth comes from withProfile — an empty profile
// resolves natively), response parsing is native Go over --query
// raw-output rows (never jq), and kampodra never accepts, stores, or logs
// credential material.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// --- reserved public IPs ---------------------------------------------------

// ResolveCompartment maps a compartment name-or-ocid to its OCID (ocids
// pass through untouched). The single shared seam — dns, bluegreen, and
// image-import all resolve through here instead of repeating the
// list+parse sequence.
func ResolveCompartment(ctx context.Context, auth CloudAuth, name string) (string, error) {
	if IsOCID(name, "compartment") {
		return name, nil
	}
	out, err := RunOCI(ctx, DNSCompartmentListArgs(auth))
	if err != nil {
		return "", err
	}
	return CompartmentOCID(out, name)
}

// IsOCID reports whether id looks like an OCI resource OCID of the given
// kind ("instance", "subnet", "image", "privateip", "compartment",
// "vnic", "publicip"). The ONLY place OCID shapes are known — command
// code asks this instead of inlining "ocid1." prefixes, so the provider
// seam stays inside this package.
func IsOCID(id, kind string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), "ocid1."+kind)
}

// ReservedIP is one `network public-ip list` row: the reserved address, its
// OCID, and the anchored private-ip OCID ("-" when unassigned/dormant).
type ReservedIP struct {
	OCID      string
	Address   string
	PrivateIP string // "-" when unassigned
	Assigned  bool
}

// ReservedIPListArgs lists the compartment's RESERVED public IPs.
func ReservedIPListArgs(auth CloudAuth, compartment string) []string {
	args := []string{"network", "public-ip", "list", "-c", compartment,
		"--scope", "REGION", "--lifetime", "RESERVED", "--all"}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// ParseReservedIPRow parses one `data[0] | [id, ip-address, private-ip-id]`
// raw-output triple. Empty input = no reserved IP (caller treats as none).
func ParseReservedIPRow(out string) (ReservedIP, bool) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) < 2 {
		return ReservedIP{}, false
	}
	holder := "-"
	if len(f) >= 3 {
		holder = f[2]
	}
	return ReservedIP{OCID: f[0], Address: f[1], PrivateIP: holder, Assigned: holder != "-"}, true
}

// ReservedIPCreateArgs creates the DORMANT reserved IP (unassigned).
func ReservedIPCreateArgs(auth CloudAuth, compartment, displayName string) []string {
	args := []string{"network", "public-ip", "create", "-c", compartment,
		"--lifetime", "RESERVED", "--display-name", displayName}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// PublicIPUpdateArgs assigns (privateIPID set) or unassigns ("" — the
// documented empty-id unassign) the reserved IP, waiting for waitState
// ("ASSIGNED" on flip, "AVAILABLE" on rollback-to-dormant).
func PublicIPUpdateArgs(auth CloudAuth, reservedOCID, privateIPID, waitState string) []string {
	args := []string{"network", "public-ip", "update", "--public-ip-id", reservedOCID,
		"--private-ip-id", privateIPID, "--force"}
	if waitState != "" {
		args = append(args, "--wait-for-state", waitState)
	}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// --- compute instances -----------------------------------------------------

// BGInstance is one RUNNING instance row: OCID, ephemeral public IP
// ("" when none yet), and availability domain.
type BGInstance struct {
	OCID     string
	PublicIP string
	AD       string
}

// InstanceListArgs lists RUNNING instances with the exact display name.
func InstanceListArgs(auth CloudAuth, compartment, displayName string) []string {
	args := []string{"compute", "instance", "list", "-c", compartment,
		"--display-name", displayName, "--lifecycle-state", "RUNNING", "--all"}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// ParseNewestInstance picks the newest instance row from
// `instance list` JSON (data[{id, availability-domain, public-ip?,
// time-created}] — the ephemeral IP comes from the VNIC, so rows here
// carry the OCID + AD; the public IP is resolved separately).
func ParseNewestInstance(listJSON string) (BGInstance, bool) {
	var wrapped struct {
		Data []struct {
			ID          string `json:"id"`
			AD          string `json:"availability-domain"`
			TimeCreated string `json:"time-created"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listJSON), &wrapped); err != nil || len(wrapped.Data) == 0 {
		return BGInstance{}, false
	}
	rows := append([]struct {
		ID          string `json:"id"`
		AD          string `json:"availability-domain"`
		TimeCreated string `json:"time-created"`
	}{}, wrapped.Data...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TimeCreated < rows[j].TimeCreated })
	last := rows[len(rows)-1]
	return BGInstance{OCID: last.ID, AD: last.AD}, true
}

// InstanceGetArgs fetches one instance record.
func InstanceGetArgs(auth CloudAuth, instanceID string) []string {
	args := []string{"compute", "instance", "get", "--instance-id", instanceID}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// InstanceRecord is the parsed subset kampodra needs from `instance get`.
type InstanceRecord struct {
	State   string
	ImageID string
	AD      string
}

// ParseInstanceRecord parses `instance get` JSON (data.lifecycle-state,
// data.image-id, data.availability-domain).
func ParseInstanceRecord(getJSON string) (InstanceRecord, error) {
	var wrapped struct {
		Data struct {
			State   string `json:"lifecycle-state"`
			ImageID string `json:"image-id"`
			AD      string `json:"availability-domain"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(getJSON), &wrapped); err != nil {
		return InstanceRecord{}, fmt.Errorf("instance record: %w", err)
	}
	return InstanceRecord{State: wrapped.Data.State, ImageID: wrapped.Data.ImageID, AD: wrapped.Data.AD}, nil
}

// InstanceLaunchArgs launches the pair instance. shapeConfig is the
// JSON shape-config (ocpus/memory); sshKeyFile "" omits the key file
// (native-image route — the golden image carries the ops key).
func InstanceLaunchArgs(auth CloudAuth, compartment, ad, subnetID, imageID, displayName, sshKeyFile string) []string {
	args := []string{"compute", "instance", "launch",
		"-c", compartment,
		"--availability-domain", ad,
		"--subnet-id", subnetID,
		"--image-id", imageID,
		"--shape", "VM.Standard.A1.Flex",
		"--shape-config", `{"ocpus":2,"memoryInGBs":12}`,
		"--assign-public-ip", "true",
		"--display-name", displayName}
	if sshKeyFile != "" {
		args = append(args, "--ssh-authorized-keys-file", sshKeyFile)
	}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// ParseLaunchID parses `instance launch` output — the call sites pass
// --query 'data.id', tolerated here as either a bare ocid or the JSON
// wrapper.
func ParseLaunchID(launchOut string) (string, bool) {
	trimmed := strings.TrimSpace(launchOut)
	if strings.HasPrefix(trimmed, "ocid1.instance") {
		return strings.Fields(trimmed)[0], true
	}
	var wrapped struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(launchOut), &wrapped); err != nil {
		return "", false
	}
	id := strings.TrimSpace(wrapped.Data.ID)
	return id, strings.HasPrefix(id, "ocid1.instance")
}

// --- VNICs / private IPs / subnets -----------------------------------------

// FirstVnic parses `vnic-attachment list` JSON (data[{vnic-id,
// subnet-id}]) — the primary attachment is data[0].
func FirstVnic(attachJSON string) (vnicID, subnetID string, ok bool) {
	var wrapped struct {
		Data []struct {
			VnicID   string `json:"vnic-id"`
			SubnetID string `json:"subnet-id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(attachJSON), &wrapped); err != nil || len(wrapped.Data) == 0 {
		return "", "", false
	}
	return wrapped.Data[0].VnicID, wrapped.Data[0].SubnetID, true
}

// VnicAttachmentsArgs lists an instance's VNIC attachments (the query
// takes the compartment flag; private-ip list takes the reverse — both
// pinned by contract).
func VnicAttachmentsArgs(auth CloudAuth, compartment, instanceID string) []string {
	args := []string{"compute", "vnic-attachment", "list", "-c", compartment, "--instance-id", instanceID}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// VnicPublicIP parses `network vnic get` JSON (data.public-ip).
func VnicPublicIP(vnicJSON string) string {
	var wrapped struct {
		Data struct {
			PublicIP string `json:"public-ip"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(vnicJSON), &wrapped); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapped.Data.PublicIP)
}

// VnicGetArgs fetches one VNIC record.
func VnicGetArgs(auth CloudAuth, vnicID string) []string {
	args := []string{"network", "vnic", "get", "--vnic-id", vnicID}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// SecondaryPrivateIP parses `network private-ip list` JSON — the FIRST
// non-primary private-ip id, "" when none. LOOKUP-ONLY by contract:
// rollback paths must never mint anchors on colors they only point at.
func SecondaryPrivateIP(listJSON string) string {
	var wrapped struct {
		Data []struct {
			ID        string `json:"id"`
			IsPrimary bool   `json:"is-primary"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listJSON), &wrapped); err != nil {
		return ""
	}
	for _, row := range wrapped.Data {
		if !row.IsPrimary && row.ID != "" {
			return row.ID
		}
	}
	return ""
}

// PrivateIPListArgs lists a VNIC's private IPs (no compartment flag —
// the CLI rejects it there).
func PrivateIPListArgs(auth CloudAuth, vnicID string) []string {
	args := []string{"network", "private-ip", "list", "--vnic-id", vnicID}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// PrivateIPCreateArgs mints the flip target's anchor secondary private IP
// (flip-target creation site ONLY by contract).
func PrivateIPCreateArgs(auth CloudAuth, vnicID, displayName string) []string {
	args := []string{"network", "private-ip", "create", "--vnic-id", vnicID,
		"--display-name", displayName}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// PrivateIPID parses `private-ip create` JSON (data.id).
func PrivateIPID(createJSON string) string {
	var wrapped struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(createJSON), &wrapped); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapped.Data.ID)
}

// PrivateIPGetArgs fetches one private-ip record (anchor address lookup).
func PrivateIPGetArgs(auth CloudAuth, privateIPID string) []string {
	args := []string{"network", "private-ip", "get", "--private-ip-id", privateIPID}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// PrivateIPAddress parses `private-ip get` JSON (data.ip-address).
func PrivateIPAddress(getJSON string) string {
	var wrapped struct {
		Data struct {
			IPAddress string `json:"ip-address"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(getJSON), &wrapped); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapped.Data.IPAddress)
}

// SubnetGetArgs fetches one subnet record (CIDR lookup for the anchor /prefix).
func SubnetGetArgs(auth CloudAuth, subnetID string) []string {
	args := []string{"network", "subnet", "get", "--subnet-id", subnetID}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// SubnetCIDR parses `subnet get` JSON (data.cidr-block).
func SubnetCIDR(getJSON string) string {
	var wrapped struct {
		Data struct {
			CIDR string `json:"cidr-block"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(getJSON), &wrapped); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapped.Data.CIDR)
}

// --- custom images ---------------------------------------------------------

// ImageListArgs lists the compartment's custom images (provision picks the
// newest whose display name starts with namePrefix).
func ImageListArgs(auth CloudAuth, compartment string) []string {
	args := []string{"compute", "image", "list", "-c", compartment, "--all", "--sort-by", "TIMECREATED"}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// NewestPrefixedImage picks the newest image whose display name starts
// with prefix from `image list` JSON (data[{id, display-name,
// time-created}]).
func NewestPrefixedImage(listJSON, prefix string) (string, bool) {
	var wrapped struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display-name"`
			TimeCreated string `json:"time-created"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listJSON), &wrapped); err != nil {
		return "", false
	}
	best, bestTime := "", ""
	for _, row := range wrapped.Data {
		if !strings.HasPrefix(row.DisplayName, prefix) {
			continue
		}
		if row.TimeCreated >= bestTime {
			best, bestTime = row.ID, row.TimeCreated
		}
	}
	return best, best != ""
}

// ImageGetArgs fetches one image record (firmware verdict).
func ImageGetArgs(auth CloudAuth, imageID string) []string {
	args := []string{"compute", "image", "get", "--image-id", imageID}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// ImageFirmware parses `image get` JSON
// (data.launch-options.firmware) — "UEFI_64" launches on A1, anything
// else (imports pin BIOS) is rejected there.
func ImageFirmware(getJSON string) string {
	var wrapped struct {
		Data struct {
			LaunchOptions struct {
				Firmware string `json:"firmware"`
			} `json:"launch-options"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(getJSON), &wrapped); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapped.Data.LaunchOptions.Firmware)
}

// --- image import (qcow2 staging + custom-image import) --------------------

// OSNamespaceArgs resolves the tenancy object-storage namespace.
func OSNamespaceArgs(auth CloudAuth) []string {
	args := []string{"os", "ns", "get"}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// OSNamespace parses `os ns get` JSON ({"data": "<namespace>"}).
func OSNamespace(nsOut string) string {
	var wrapped struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal([]byte(nsOut), &wrapped); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapped.Data)
}

// OSObjectPutArgs stages the qcow2 (`os object put --force`).
func OSObjectPutArgs(auth CloudAuth, bucket, file, object string) []string {
	args := []string{"os", "object", "put", "-bn", bucket, "--file", file, "--name", object, "--force"}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// OSObjectDeleteArgs removes the staged object after a kept import.
func OSObjectDeleteArgs(auth CloudAuth, bucket, object string) []string {
	args := []string{"os", "object", "delete", "-bn", bucket, "--name", object, "--force"}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// SupportedImportOSes lists the guest OSes the golden-image pipeline
// (image-import + provision) accepts. One source of truth for the flag
// validation, the import metadata, and the error messages.
func SupportedImportOSes() []string {
	return []string{"alpine", "ubuntu"}
}

// ImportOSMetadata maps a validated guest OS to the OCI import OS
// metadata. Fail-closed like CheckProvider — an unknown OS name must
// never reach the import call silently defaulted. Alpine is not in OCI's
// supported import list (generic self-supported metadata); Ubuntu 24.04
// LTS imports as Canonical Ubuntu.
func ImportOSMetadata(os string) (osName, osVersion string, err error) {
	switch os {
	case "alpine":
		return "Linux", "Alpine (self-supported)", nil
	case "ubuntu":
		return "Canonical Ubuntu", "Ubuntu 24.04", nil
	}
	return "", "", fmt.Errorf("unsupported import OS %q (supported: %s)", os, strings.Join(SupportedImportOSes(), ", "))
}

// ImageImportArgs imports the staged object as a custom image with the
// caller-resolved OS metadata (ImportOSMetadata; validated upstream —
// this stays a pure string-vector builder like every other Args builder).
// PARAVIRTUALIZED — Ampere shapes are UEFI-only; OCI pins imports to
// firmware=BIOS, so the result warns when it is not UEFI_64.
func ImageImportArgs(auth CloudAuth, compartment, bucket, namespace, object, displayName, osName, osVersion string) []string {
	args := []string{"compute", "image", "import", "from-object",
		"-c", compartment,
		"--bucket-name", bucket,
		"--namespace", namespace,
		"--name", object,
		"--display-name", displayName,
		"--source-image-type", "QCOW2",
		"--operating-system", osName,
		"--operating-system-version", osVersion,
		"--launch-mode", "PARAVIRTUALIZED"}
	return withProfile(args, auth.Profile, auth.InstancePrincipal)
}

// ImportImageOCID parses the import response (data.id, an ocid1.image).
func ImportImageOCID(importJSON string) (string, bool) {
	var wrapped struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(importJSON), &wrapped); err != nil {
		return "", false
	}
	id := strings.TrimSpace(wrapped.Data.ID)
	return id, strings.HasPrefix(id, "ocid1.image")
}

// ImageState parses `image get` JSON (data.lifecycle-state).
func ImageState(getJSON string) string {
	var wrapped struct {
		Data struct {
			State string `json:"lifecycle-state"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(getJSON), &wrapped); err != nil {
		return ""
	}
	return strings.TrimSpace(wrapped.Data.State)
}
