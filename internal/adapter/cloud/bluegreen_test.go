package cloud

import (
	"strings"
	"testing"
)

func TestParseReservedIPRow(t *testing.T) {
	rip, ok := ParseReservedIPRow("ocid1.publicip.x 203.0.113.50 ocid1.privateip.y")
	if !ok || rip.OCID != "ocid1.publicip.x" || rip.Address != "203.0.113.50" ||
		rip.PrivateIP != "ocid1.privateip.y" || !rip.Assigned {
		t.Errorf("assigned row = %+v %v", rip, ok)
	}
	rip, ok = ParseReservedIPRow("ocid1.publicip.x 203.0.113.50 -")
	if !ok || rip.Assigned || rip.PrivateIP != "-" {
		t.Errorf("dormant row = %+v %v", rip, ok)
	}
	if _, ok := ParseReservedIPRow(""); ok {
		t.Error("empty output must mean no reserved IP")
	}
}

func TestPublicIPUpdateUnassignShape(t *testing.T) {
	args := PublicIPUpdateArgs(CloudAuth{}, "ocid1.publicip.x", "", "AVAILABLE")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--private-ip-id  --force") && !strings.Contains(joined, "--private-ip-id ") {
		t.Errorf("unassign must pass an empty private-ip-id: %v", args)
	}
	if !strings.Contains(joined, "--wait-for-state AVAILABLE") {
		t.Errorf("rollback must wait AVAILABLE: %v", args)
	}
	args = PublicIPUpdateArgs(CloudAuth{}, "ocid1.publicip.x", "ocid1.privateip.y", "ASSIGNED")
	if !strings.Contains(strings.Join(args, " "), "--wait-for-state ASSIGNED") {
		t.Errorf("flip must wait ASSIGNED: %v", args)
	}
}

func TestParseNewestInstance(t *testing.T) {
	out := `{"data":[
		{"id":"ocid1.instance.old","availability-domain":"AD-1","time-created":"2026-09-01T00:00:00Z"},
		{"id":"ocid1.instance.new","availability-domain":"AD-2","time-created":"2026-10-01T00:00:00Z"}]}`
	inst, ok := ParseNewestInstance(out)
	if !ok || inst.OCID != "ocid1.instance.new" || inst.AD != "AD-2" {
		t.Errorf("newest = %+v %v", inst, ok)
	}
	if _, ok := ParseNewestInstance(`{"data":[]}`); ok {
		t.Error("empty list must mean not provisioned")
	}
}

func TestParseInstanceRecord(t *testing.T) {
	rec, err := ParseInstanceRecord(`{"data":{"lifecycle-state":"RUNNING","image-id":"ocid1.image.z","availability-domain":"AD-1"}}`)
	if err != nil || rec.State != "RUNNING" || rec.ImageID != "ocid1.image.z" || rec.AD != "AD-1" {
		t.Errorf("record = %+v %v", rec, err)
	}
}

func TestFirstVnic(t *testing.T) {
	vnic, subnet, ok := FirstVnic(`{"data":[{"vnic-id":"ocid1.vnic.a","subnet-id":"ocid1.subnet.b"}]}`)
	if !ok || vnic != "ocid1.vnic.a" || subnet != "ocid1.subnet.b" {
		t.Errorf("first vnic = %q %q %v", vnic, subnet, ok)
	}
	if _, _, ok := FirstVnic(`{"data":[]}`); ok {
		t.Error("empty attachments must fail")
	}
}

func TestSecondaryPrivateIPLookupOnly(t *testing.T) {
	got := SecondaryPrivateIP(`{"data":[
		{"id":"ocid1.privateip.primary","is-primary":true},
		{"id":"ocid1.privateip.anchor","is-primary":false}]}`)
	if got != "ocid1.privateip.anchor" {
		t.Errorf("anchor = %q", got)
	}
	if got := SecondaryPrivateIP(`{"data":[{"id":"ocid1.privateip.primary","is-primary":true}]}`); got != "" {
		t.Errorf("no secondary must be empty, got %q", got)
	}
}

func TestNewestPrefixedImageSkipsNonMatches(t *testing.T) {
	out := `{"data":[
		{"id":"ocid1.image.old","display-name":"app-alpine-3.21","time-created":"2026-08-01T00:00:00Z"},
		{"id":"ocid1.image.new","display-name":"app-alpine-3.22","time-created":"2026-09-01T00:00:00Z"},
		{"id":"ocid1.image.ubuntu","display-name":"Canonical-Ubuntu","time-created":"2026-10-01T00:00:00Z"}]}`
	id, ok := NewestPrefixedImage(out, "app-alpine")
	if !ok || id != "ocid1.image.new" {
		t.Errorf("newest prefixed = %q %v", id, ok)
	}
	if _, ok := NewestPrefixedImage(out, "nope-"); ok {
		t.Error("no match must fail")
	}
}

func TestImageFirmwareGate(t *testing.T) {
	if got := ImageFirmware(`{"data":{"launch-options":{"firmware":"UEFI_64"}}}`); got != "UEFI_64" {
		t.Errorf("firmware = %q", got)
	}
	if got := ImageFirmware(`{"data":{"launch-options":{"firmware":"BIOS"}}}`); got != "BIOS" {
		t.Errorf("firmware = %q", got)
	}
}

func TestImportImageOCIDValidates(t *testing.T) {
	id, ok := ImportImageOCID(`{"data":{"id":"ocid1.image.imported"}}`)
	if !ok || id != "ocid1.image.imported" {
		t.Errorf("import = %q %v", id, ok)
	}
	if _, ok := ImportImageOCID(`{"data":{"id":"junk"}}`); ok {
		t.Error("non-image id must fail")
	}
}

func TestEmptyProfileOmitsAuthFlag(t *testing.T) {
	vecs := [][]string{
		ReservedIPListArgs(CloudAuth{}, "ocid1.compartment.x"),
		InstanceListArgs(CloudAuth{}, "ocid1.compartment.x", "app-blue"),
		ImageImportArgs(CloudAuth{}, "ocid1.compartment.x", "bkt", "ns", "obj", "disp"),
	}
	for i, v := range vecs {
		if contains(v, "--profile") {
			t.Errorf("vec %d must not pass --profile (native resolution): %v", i, v)
		}
	}
}

func TestParseLaunchID(t *testing.T) {
	if id, ok := ParseLaunchID("ocid1.instance.xyz \n"); !ok || id != "ocid1.instance.xyz" {
		t.Errorf("bare ocid = %q %v", id, ok)
	}
	if id, ok := ParseLaunchID(`{"data":{"id":"ocid1.instance.xyz"}}`); !ok || id != "ocid1.instance.xyz" {
		t.Errorf("wrapped = %q %v", id, ok)
	}
	if _, ok := ParseLaunchID("garbage"); ok {
		t.Error("garbage must fail")
	}
}

func TestIsOCIDCentralizesShapeKnowledge(t *testing.T) {
	if !IsOCID("ocid1.instance.xyz ", "instance") || IsOCID("ocid1.image.xyz", "instance") || IsOCID("", "instance") {
		t.Error("IsOCID misjudges")
	}
}

func TestCheckProviderDefaultsAndRejects(t *testing.T) {
	if err := CheckProvider(CloudAuth{}); err != nil {
		t.Errorf("empty provider must default-accept: %v", err)
	}
	if err := CheckProvider(CloudAuth{Provider: "oci"}); err != nil {
		t.Errorf("oci must accept: %v", err)
	}
	err := CheckProvider(CloudAuth{Provider: "aws"})
	if err == nil || !strings.Contains(err.Error(), `"aws"`) || !strings.Contains(err.Error(), "oci") {
		t.Errorf("unknown provider must fail naming itself and the supported set: %v", err)
	}
}
