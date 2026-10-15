// Provider-neutral blue/green ops: the bluegreen command surface calls
// THESE, never a provider CLI directly. Same shape as the dns/backup
// families: a CheckProvider gate, a provider switch, and a fail-closed
// default. The per-provider legs live in bgops_oci.go (byte-identical to
// the pre-gcp flow — the PATH-shim argv pins in command/bluegreen_test.go
// prove it). The gcp legs land in slice 4, so every gcp case fails closed
// for now with the same refusal the command surface's interim gate raises.
package cloud

import (
	"context"
	"fmt"
)

// bgGCPRefusal is the interim fail-closed every bg op's gcp case hits
// until the gcp legs land (slice 4). The command surface's
// gcpOCIOnlyRefusal already refuses gcp profiles at resolveBGPair, so
// this is defense-in-depth at the op seam: same message, zero argv.
func bgGCPRefusal() error {
	return fmt.Errorf("bluegreen is OCI-only today — the gcp leg hasn't landed; run it with an oci profile")
}

// BGResolveScope resolves the pair's scope — the compartment NAME or ocid —
// to its OCID (ocids pass through untouched, the shared ResolveCompartment
// seam).
func BGResolveScope(ctx context.Context, auth CloudAuth, name string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", bgGCPRefusal()
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
		return BGInstance{}, bgGCPRefusal()
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
		return "", "", false, bgGCPRefusal()
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
		return "", bgGCPRefusal()
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
		return ReservedIP{}, false, bgGCPRefusal()
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
		return "", bgGCPRefusal()
	}
	return bgReservedIPCreateOCI(ctx, auth, compOCID, displayName)
}

// BGReservedIPAssign attaches privateIPID to the reserved IP; waitState is
// the provider's settle target ("" = fire-and-forget, the failure-rollback
// reassign never waits).
func BGReservedIPAssign(ctx context.Context, auth CloudAuth, ripOCID, privateIPID, waitState string) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return bgGCPRefusal()
	}
	return bgReservedIPAssignOCI(ctx, auth, ripOCID, privateIPID, waitState)
}

// BGReservedIPRelease unassigns the reserved IP back to DORMANT (the
// documented empty-id unassign, waiting for AVAILABLE — rollback +
// explicit rollback only).
func BGReservedIPRelease(ctx context.Context, auth CloudAuth, ripOCID string) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return bgGCPRefusal()
	}
	return bgReservedIPReleaseOCI(ctx, auth, ripOCID)
}

// BGPrivateIPCreate mints the flip target's anchor secondary private IP
// and returns its OCID (flip-target creation site ONLY by contract).
func BGPrivateIPCreate(ctx context.Context, auth CloudAuth, vnicID, displayName string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", bgGCPRefusal()
	}
	return bgPrivateIPCreateOCI(ctx, auth, vnicID, displayName)
}

// BGSubnetCIDR reads one subnet's CIDR block (the anchor /prefix source).
func BGSubnetCIDR(ctx context.Context, auth CloudAuth, subnetID string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", bgGCPRefusal()
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
		return "", bgGCPRefusal()
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
		return "", bgGCPRefusal()
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
		return InstanceRecord{}, bgGCPRefusal()
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
		return "", "", bgGCPRefusal()
	}
	return bgGoldenImageOCI(ctx, auth, compOCID, prefix)
}
