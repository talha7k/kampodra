// OCI legs of the neutral blue/green ops: TODAY's exported builders +
// parsers, composed verbatim — the oci argv is byte-identical to the
// pre-gcp flow (the PATH-shim argv pins in command/bluegreen_test.go and
// the per-area suites prove it). Error text paths are byte-identical too:
// where the old command bodies wrapped an exec error, the wrap lives here
// (the dnsops_oci.go precedent for leg-owned rendering).
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func bgResolveScopeOCI(ctx context.Context, auth CloudAuth, name string) (string, error) {
	return ResolveCompartment(ctx, auth, name)
}

func bgColorInstanceOCI(ctx context.Context, auth CloudAuth, compOCID, displayName string) (BGInstance, error) {
	out, err := RunOCI(ctx, InstanceListArgs(auth, compOCID, displayName))
	if err != nil {
		return BGInstance{}, err
	}
	inst, ok := ParseNewestInstance(out)
	if !ok {
		return BGInstance{}, nil
	}
	att, err := RunOCI(ctx, VnicAttachmentsArgs(auth, compOCID, inst.OCID))
	if err != nil {
		return BGInstance{}, err
	}
	vnic, _, ok := FirstVnic(att)
	if !ok {
		return inst, nil
	}
	vnicJSON, err := RunOCI(ctx, VnicGetArgs(auth, vnic))
	if err != nil {
		return BGInstance{}, err
	}
	inst.PublicIP = VnicPublicIP(vnicJSON)
	return inst, nil
}

func bgPrimaryVnicOCI(ctx context.Context, auth CloudAuth, compOCID, instanceOCID string) (vnicID, subnetID string, ok bool, err error) {
	att, err := RunOCI(ctx, VnicAttachmentsArgs(auth, compOCID, instanceOCID))
	if err != nil {
		return "", "", false, err
	}
	vnicID, subnetID, ok = FirstVnic(att)
	return vnicID, subnetID, ok, nil
}

func bgAnchorIPOCI(ctx context.Context, auth CloudAuth, vnicID string) (string, error) {
	out, err := RunOCI(ctx, PrivateIPListArgs(auth, vnicID))
	if err != nil {
		return "", err
	}
	return SecondaryPrivateIP(out), nil
}

func bgReservedIPListOCI(ctx context.Context, auth CloudAuth, compOCID string) (ReservedIP, bool, error) {
	rpOut, err := RunOCI(ctx, ReservedIPListArgs(auth, compOCID))
	if err != nil {
		return ReservedIP{}, false, err
	}
	rip, ok := ParseReservedIPRow(rpOut)
	return rip, ok, nil
}

func bgReservedIPCreateOCI(ctx context.Context, auth CloudAuth, compOCID, displayName string) (string, error) {
	out, err := RunOCI(ctx, ReservedIPCreateArgs(auth, compOCID, displayName))
	if err != nil {
		return "", err
	}
	var created struct {
		Data struct {
			IPAddress string `json:"ip-address"`
		} `json:"data"`
	}
	addr := strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(out), &created); err == nil && created.Data.IPAddress != "" {
		addr = created.Data.IPAddress
	}
	return addr, nil
}

func bgReservedIPAssignOCI(ctx context.Context, auth CloudAuth, ripOCID, privateIPID, waitState string) error {
	_ = ripOCID // the attach rides privateIPID; the target is implied by it

	_, err := RunOCI(ctx, PublicIPUpdateArgs(auth, ripOCID, privateIPID, waitState))
	return err
}

func bgReservedIPReleaseOCI(ctx context.Context, auth CloudAuth, ripOCID string) error {
	_, err := RunOCI(ctx, PublicIPUpdateArgs(auth, ripOCID, "", "AVAILABLE"))
	return err
}

func bgPrivateIPCreateOCI(ctx context.Context, auth CloudAuth, vnicID, displayName string) (string, error) {
	cout, err := RunOCI(ctx, PrivateIPCreateArgs(auth, vnicID, displayName))
	if err != nil {
		return "", err
	}
	return PrivateIPID(cout), nil
}

func bgSubnetCIDROI(ctx context.Context, auth CloudAuth, subnetID string) (string, error) {
	cidrOut, err := RunOCI(ctx, SubnetGetArgs(auth, subnetID))
	if err != nil {
		return "", err
	}
	return SubnetCIDR(cidrOut), nil
}

func bgPrivateIPAddressOCI(ctx context.Context, auth CloudAuth, privateIPID string) (string, error) {
	pipOut, err := RunOCI(ctx, PrivateIPGetArgs(auth, privateIPID))
	if err != nil {
		return "", err
	}
	return PrivateIPAddress(pipOut), nil
}

func bgInstanceLaunchOCI(ctx context.Context, auth CloudAuth, compOCID, ad, subnetID, imageID, displayName, sshKeyFile string) (string, error) {
	launchOut, err := RunOCI(ctx, InstanceLaunchArgs(auth, compOCID, ad, subnetID, imageID, displayName, sshKeyFile))
	if err != nil {
		return "", fmt.Errorf("instance launch failed: %w", err)
	}
	iid := strings.TrimSpace(launchOut)
	if !IsOCID(iid, "instance") {
		if parsed, ok := ParseLaunchID(launchOut); ok {
			iid = parsed
		} else {
			return "", fmt.Errorf("unexpected launch response: %s", launchOut)
		}
	}
	return iid, nil
}

func bgInstanceRecordOCI(ctx context.Context, auth CloudAuth, instanceOCID string) (InstanceRecord, error) {
	out, err := RunOCI(ctx, InstanceGetArgs(auth, instanceOCID))
	if err != nil {
		return InstanceRecord{}, err
	}
	return ParseInstanceRecord(out)
}

func bgGoldenImageOCI(ctx context.Context, auth CloudAuth, compOCID, prefix string) (imageID, firmware string, err error) {
	imgOut, err := RunOCI(ctx, ImageListArgs(auth, compOCID))
	if err != nil {
		return "", "", err
	}
	newest, ok := NewestPrefixedImage(imgOut, prefix)
	if !ok {
		return "", "", nil
	}
	fwOut, err := RunOCI(ctx, ImageGetArgs(auth, newest))
	if err != nil {
		return "", "", err
	}
	return newest, ImageFirmware(fwOut), nil
}
