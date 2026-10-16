// Provider-neutral blue/green ops: the bluegreen command surface calls
// THESE, never a provider CLI directly. Same shape as the dns/backup
// families: a CheckProvider gate, a provider switch, and a fail-closed
// default. The per-provider legs live in bgops_oci.go (byte-identical to
// the pre-gcp flow — the PATH-shim argv pins in command/bluegreen_test.go
// prove it) and bgops_gcp.go (SDK-verified gcloud argv, pinned in
// bgops_gcp_test.go).
package cloud

import (
	"context"
	"fmt"
)

// BGResolveScope resolves the pair's scope — the compartment NAME or ocid
// — to its OCID (ocids pass through untouched, the shared
// ResolveCompartment seam). gcp has no scope to resolve: the cloud
// block's project (or gcloud's active config) rides natively on every
// call, so the empty scope = native.
func BGResolveScope(ctx context.Context, auth CloudAuth, name string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", nil
	}
	return bgResolveScopeOCI(ctx, auth, name)
}

// BGColorInstance resolves one color: the newest RUNNING instance row with
// that display name plus its ephemeral public IP ("" when the VNIC has none
// yet). Zero BGInstance = not provisioned.
func BGColorInstance(ctx context.Context, auth CloudAuth, compOCID, displayName string) (BGInstance, error) {
	if err := CheckProvider(auth); err != nil {
		return BGInstance{}, err
	}
	if auth.Provider == "gcp" {
		return bgColorInstanceGCP(ctx, auth, displayName)
	}
	return bgColorInstanceOCI(ctx, auth, compOCID, displayName)
}

// BGPrimaryVnic reads an instance's primary VNIC attachment: the VNIC OCID
// and its subnet OCID. ok=false when the instance has no attachment —
// callers own their not-found rendering (lookup-only paths swallow it).
func BGPrimaryVnic(ctx context.Context, auth CloudAuth, compOCID, instanceOCID string) (vnicID, subnetID string, ok bool, err error) {
	if err := CheckProvider(auth); err != nil {
		return "", "", false, err
	}
	if auth.Provider == "gcp" {
		return bgPrimaryVnicGCP(ctx, auth, instanceOCID)
	}
	return bgPrimaryVnicOCI(ctx, auth, compOCID, instanceOCID)
}

// BGAnchorIP resolves the secondary-private-ip anchor on a VNIC (""
// when none). LOOKUP-ONLY by contract — callers that may create use
// BGPrivateIPCreate (the flip target's own path).
func BGAnchorIP(ctx context.Context, auth CloudAuth, vnicID string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return bgAnchorIPGCP(ctx, auth)
	}
	return bgAnchorIPOCI(ctx, auth, vnicID)
}

// BGReservedIPList reads the compartment's RESERVED public IP — the raw
// triple the parsers consume. ok=false when none exists (dormant pair).
func BGReservedIPList(ctx context.Context, auth CloudAuth, compOCID string) (ReservedIP, bool, error) {
	if err := CheckProvider(auth); err != nil {
		return ReservedIP{}, false, err
	}
	if auth.Provider == "gcp" {
		return bgReservedIPListGCP(ctx, auth)
	}
	return bgReservedIPListOCI(ctx, auth, compOCID)
}

// BGReservedIPCreate creates the DORMANT reserved IP (unassigned) and
// returns its address.
func BGReservedIPCreate(ctx context.Context, auth CloudAuth, compOCID, displayName string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return bgReservedIPCreateGCP(ctx, auth)
	}
	return bgReservedIPCreateOCI(ctx, auth, compOCID, displayName)
}

// BGReservedIPAssign attaches the reserved IP to TARGETHANDLE: on oci the
// anchor private-ip ocid implies the instance (targetHandle is accepted and
// ignored — the attach rides privateIPID); on gcp the attach is an
// access-config on the target instance itself, so the target must be named
// explicitly (live-fire 2026-10-16: the oci-shaped signature made the gcp
// leg look the anchor handle up as an instance). waitState is the
// provider's settle target ("" = fire-and-forget, the failure-rollback
// reassign never waits).
func BGReservedIPAssign(ctx context.Context, auth CloudAuth, ripHandle, targetHandle, anchorHandle, waitState string) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return bgReservedIPAssignGCP(ctx, auth, ripHandle, targetHandle)
	}
	return bgReservedIPAssignOCI(ctx, auth, ripHandle, anchorHandle, waitState)
}

// BGReservedIPRelease unassigns the reserved IP back to DORMANT (the
// documented empty-id unassign, waiting for AVAILABLE — rollback +
// explicit rollback only).
func BGReservedIPRelease(ctx context.Context, auth CloudAuth, ripOCID string) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return bgReservedIPReleaseGCP(ctx, auth)
	}
	return bgReservedIPReleaseOCI(ctx, auth, ripOCID)
}

// BGPrivateIPCreate mints the flip target's anchor secondary private IP
// and returns its OCID (flip-target creation site ONLY by contract). On
// gcp it is a deliberate no-op returning the reserved-address handle:
// the anchor IS the reserved external IP, there is no secondary private
// IP to mint.
func BGPrivateIPCreate(ctx context.Context, auth CloudAuth, vnicID, displayName string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return gcpReservedIPName, nil
	}
	return bgPrivateIPCreateOCI(ctx, auth, vnicID, displayName)
}

// BGSubnetCIDR reads one subnet's CIDR block (the anchor /prefix source).
// On gcp it fails closed: the CIDR feeds only the OCI anchor /prefix
// math, and that path is unreachable here (the flip's ocid-shape gate
// refuses gcp handles first) — fail-closed text, never dead argv.
func BGSubnetCIDR(ctx context.Context, auth CloudAuth, subnetID string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", fmt.Errorf("subnet CIDR is not applicable for provider gcp — the reserved external IP carries no guest-side /prefix (the anchor /prefix math is OCI-only)")
	}
	return bgSubnetCIDROI(ctx, auth, subnetID)
}

// BGPrivateIPAddress reads one private-ip record's address (the anchor
// address lookup).
func BGPrivateIPAddress(ctx context.Context, auth CloudAuth, privateIPID string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return bgPrivateIPAddressGCP(ctx, auth)
	}
	return bgPrivateIPAddressOCI(ctx, auth, privateIPID)
}

// BGInstanceLaunch launches a pair instance and returns its instance ID
// (the provider's handle, however its CLI formats the response).
func BGInstanceLaunch(ctx context.Context, auth CloudAuth, compOCID, ad, subnetID, imageID, displayName, sshKeyFile string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return bgInstanceLaunchGCP(ctx, auth, ad, subnetID, imageID, displayName, sshKeyFile)
	}
	return bgInstanceLaunchOCI(ctx, auth, compOCID, ad, subnetID, imageID, displayName, sshKeyFile)
}

// BGInstanceRecord fetches one instance record (launch polling, template
// image lookup).
func BGInstanceRecord(ctx context.Context, auth CloudAuth, instanceOCID string) (InstanceRecord, error) {
	if err := CheckProvider(auth); err != nil {
		return InstanceRecord{}, err
	}
	if auth.Provider == "gcp" {
		return bgInstanceRecordGCP(ctx, auth, instanceOCID)
	}
	return bgInstanceRecordOCI(ctx, auth, instanceOCID)
}

// BGGoldenImage finds the newest custom image whose display name starts
// with prefix, with its firmware verdict ("" image = none prefixed).
func BGGoldenImage(ctx context.Context, auth CloudAuth, compOCID, prefix string) (imageID, firmware string, err error) {
	if err := CheckProvider(auth); err != nil {
		return "", "", err
	}
	if auth.Provider == "gcp" {
		return bgGoldenImageGCP(ctx, auth, prefix)
	}
	return bgGoldenImageOCI(ctx, auth, compOCID, prefix)
}
