package cloud

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gcloud legs of the neutral blue/green ops: exact argv pins and the
// mapping into the same neutral types the OCI legs return. The gcloud CLI
// is PATH-shimmed with fixture files; kampodra passes only identity flags
// (--project/--account via withGCPAuth), never credential material.
// Flags verified against gcloud SDK 578: `name:` filters parse (the
// `names:` spelling is server-rejected), the address attach primitive is
// `add-access-config --address` (update-access-config lost --address),
// and delete/add-access-config default to the "external-nat" config on
// nic0 — so neither name flag is passed.

type bgGCPShim struct {
	dir string
	log string
}

// setupBGGCPShim installs a gcloud shim dispatching on "$1 $2 $3" with
// fixture files. instances-list calls route on the name filter (the
// aggregated list is the only instances read — no zone fan-out).
func setupBGGCPShim(t *testing.T, fixtures map[string]string) *bgGCPShim {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "gcloud-calls.log")
	def := func(key, val string) {
		if _, ok := fixtures[key]; !ok {
			fixtures[key] = val
		}
	}
	def("addr-list", `[]`)
	def("addr-create", `[{"name":"kampodra-active","address":"203.0.113.50","status":"RESERVED"}]`)
	def("inst-blue", `[]`)
	def("inst-green", `[]`)
	def("inst-any", `[]`)
	def("images", `[]`)

	files := map[string]string{}
	for k, v := range fixtures {
		p := filepath.Join(dir, k+".json")
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		files[k] = p
	}
	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"case \"$1 $2 $3\" in\n" +
		"  \"compute addresses list\") cat '" + files["addr-list"] + "' ;;\n" +
		"  \"compute addresses create\") cat '" + files["addr-create"] + "' ;;\n" +
		"  \"compute instances list\") if [[ \"$*\" == *app-blue* ]]; then cat '" + files["inst-blue"] + "'; " +
		"elif [[ \"$*\" == *app-green* ]]; then cat '" + files["inst-green"] + "'; " +
		"else cat '" + files["inst-any"] + "'; fi ;;\n" +
		"  \"compute instances create\") exit 0 ;;\n" +
		"  \"compute images list\") cat '" + files["images"] + "' ;;\n" +
		"  \"compute instances delete-access-config\") exit 0 ;;\n" +
		"  \"compute instances add-access-config\") exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return &bgGCPShim{dir: dir, log: log}
}

const (
	// gcpBGZone is the aggregated-list zone form: a full URL whose last
	// segment is the zone gcloud's --zone flag takes.
	bgGCPZone = "https://www.googleapis.com/compute/v1/projects/p1/zones/europe-west1-b"
	// bgGCPUser is the addresses-list users[0] form: the holder path.
	bgGCPUser = "https://www.googleapis.com/compute/v1/projects/p1/zones/europe-west1-b/instances/app-blue"
)

func TestBGResolveScopeGCPNative(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{})
	scope, err := BGResolveScope(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "some-comp")
	if err != nil || scope != "" {
		t.Fatalf("BGResolveScope(gcp) = %q, %v — want native (\"\", nil)", scope, err)
	}
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 0 {
		t.Errorf("native scope must not exec gcloud: %v", calls)
	}
}

func TestBGColorInstanceGCPArgvAndMapping(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"inst-green": `[
			{"name":"app-green","zone":"` + bgGCPZone + `","creationTimestamp":"2026-10-01T00:00:00Z","status":"RUNNING","networkInterfaces":[{"accessConfigs":[{"natIP":"203.0.113.1"}]}]},
			{"name":"app-green","zone":"` + bgGCPZone + `","creationTimestamp":"2026-10-02T00:00:00Z","status":"RUNNING","networkInterfaces":[{"accessConfigs":[{"natIP":"203.0.113.9"}]}]}
		]`,
	})
	inst, err := BGColorInstance(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "", "app-green")
	if err != nil {
		t.Fatalf("BGColorInstance: %v", err)
	}
	// Newest row wins; zone rides the AD slot (dual meaning, like the OCI
	// leg's availability domain) and natIP the public IP.
	if inst.OCID != "app-green" || inst.PublicIP != "203.0.113.9" || inst.AD != "europe-west1-b" {
		t.Errorf("BGInstance = %+v", inst)
	}
	calls := bgOpShimCalls(t, shim.log)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1: %v", len(calls), calls)
	}
	want := "compute instances list --filter=name ~ ^app-green$ AND status=RUNNING" +
		" --format=json(name,zone,creationTimestamp,networkInterfaces[0].accessConfigs[0].natIP) --project=p1"
	if calls[0] != want {
		t.Errorf("argv drift:\n got: %s\nwant: %s", calls[0], want)
	}
}

// Zero rows = not provisioned — the caller's semantics depend on the zero
// BGInstance surviving the leg.
func TestBGColorInstanceGCPNotProvisioned(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{})
	inst, err := BGColorInstance(context.Background(), CloudAuth{Provider: "gcp"}, "", "app-blue")
	if err != nil || inst != (BGInstance{}) {
		t.Errorf("empty list = %+v, %v — want the zero BGInstance", inst, err)
	}
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 1 {
		t.Errorf("want exactly the one list call: %v", calls)
	}
}

func TestBGPrimaryVnicGCPDerivesFromInstance(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"inst-green": `[{"name":"app-green","zone":"` + bgGCPZone + `","networkInterfaces":[{"subnetwork":"https://www.googleapis.com/compute/v1/projects/p1/regions/europe-west1/subnetworks/kampodra"}]}]`,
	})
	vnic, subnet, ok, err := BGPrimaryVnic(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "", "app-green")
	if err != nil || !ok {
		t.Fatalf("BGPrimaryVnic = %q %q %v %v", vnic, subnet, ok, err)
	}
	// No vnic-attachment dance on gcp: the instance IS the NIC handle.
	if vnic != "app-green" || subnet != "kampodra" {
		t.Errorf("vnic/subnet = %q/%q", vnic, subnet)
	}
	want := "compute instances list --filter=name ~ ^app-green$" +
		" --format=json(name,zone,networkInterfaces[0].subnetwork) --project=p1"
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 1 || calls[0] != want {
		t.Errorf("argv drift: %v", calls)
	}
	setupBGGCPShim(t, map[string]string{}) // fresh shim, no rows
	if _, _, ok, _ := BGPrimaryVnic(context.Background(), CloudAuth{Provider: "gcp"}, "", "app-green"); ok {
		t.Error("missing instance must read ok=false")
	}
}

func TestBGReservedIPListGCPMapping(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"addr-list": `[{"name":"kampodra-active","address":"203.0.113.50","status":"IN_USE","users":["` + bgGCPUser + `"]}]`,
	})
	rip, ok, err := BGReservedIPList(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "")
	if err != nil || !ok {
		t.Fatalf("BGReservedIPList = %+v %v %v", rip, ok, err)
	}
	// users[0] names the holder directly (no anchor-join) — the last
	// segment is the holder instance.
	if rip.OCID != "kampodra-active" || rip.Address != "203.0.113.50" || !rip.Assigned || rip.PrivateIP != "app-blue" {
		t.Errorf("ReservedIP = %+v", rip)
	}
	want := "compute addresses list --filter=name=kampodra-active --format=json(name,address,status,users) --project=p1"
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 1 || calls[0] != want {
		t.Errorf("argv drift: %v", calls)
	}
}

// RESERVED (no users) = dormant, mirroring the OCI leg's `-` holder.
func TestBGReservedIPListGCPDormant(t *testing.T) {
	setupBGGCPShim(t, map[string]string{
		"addr-list": `[{"name":"kampodra-active","address":"203.0.113.50","status":"RESERVED","users":[]}]`,
	})
	rip, ok, err := BGReservedIPList(context.Background(), CloudAuth{Provider: "gcp"}, "")
	if err != nil || !ok {
		t.Fatalf("BGReservedIPList = %+v %v %v", rip, ok, err)
	}
	if rip.Assigned || rip.PrivateIP != "-" {
		t.Errorf("dormant mapping = %+v", rip)
	}
	setupBGGCPShim(t, map[string]string{"addr-list": `[]`}) // bare []
	if rip, ok, _ := BGReservedIPList(context.Background(), CloudAuth{Provider: "gcp"}, ""); ok || rip != (ReservedIP{}) {
		t.Errorf("bare [] must read none: %+v %v", rip, ok)
	}
}

func TestBGReservedIPCreateGCPRegionFromSibling(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"inst-any": `[{"name":"app-blue","zone":"` + bgGCPZone + `","status":"RUNNING"}]`,
	})
	addr, err := BGReservedIPCreate(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "", "")
	if err != nil || addr != "203.0.113.50" {
		t.Fatalf("BGReservedIPCreate = %q, %v", addr, err)
	}
	calls := bgOpShimCalls(t, shim.log)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 (region derive + create): %v", len(calls), calls)
	}
	// The region derives from the sibling's zone minus its final segment.
	if !strings.HasPrefix(calls[0], "compute instances list --filter=status=RUNNING --format=json(name,zone) ") {
		t.Errorf("region derive drift: %s", calls[0])
	}
	want := "compute addresses create kampodra-active --region=europe-west1 --format=json(name,address,status) --project=p1"
	if calls[1] != want {
		t.Errorf("create argv drift:\n got: %s\nwant: %s", calls[1], want)
	}
}

// No RUNNING sibling = no region to anchor the regional address — fail
// closed before the create.
func TestBGReservedIPCreateGCPNoRegion(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{})
	if _, err := BGReservedIPCreate(context.Background(), CloudAuth{Provider: "gcp"}, "", ""); err == nil ||
		!strings.Contains(err.Error(), "region") {
		t.Fatalf("no sibling = %v, want a region failure", err)
	}
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 1 {
		t.Errorf("the create must not fire without a region: %v", calls)
	}
}

// THE FLIP: detach the reserved address from its current holder, then
// attach it to the target — pinned argv, pinned order. The attach is
// `add-access-config --address` (SDK 578 removed --address from
// update-access-config); the detach relies on the documented
// "external-nat" access-config default.
func TestBGReservedIPAssignGCPOrdering(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"addr-list":  `[{"name":"kampodra-active","address":"203.0.113.50","status":"IN_USE","users":["` + bgGCPUser + `"]}]`,
		"inst-green": `[{"name":"app-green","zone":"` + bgGCPZone + `","status":"RUNNING"}]`,
	})
	err := BGReservedIPAssign(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "kampodra-active", "app-green", "203.0.113.50", "ASSIGNED")
	if err != nil {
		t.Fatalf("BGReservedIPAssign: %v", err)
	}
	calls := bgOpShimCalls(t, shim.log)
	want := []string{
		"compute addresses list --filter=name=kampodra-active --format=json(name,address,status,users) --project=p1",
		"compute instances delete-access-config app-blue --zone=europe-west1-b --project=p1",
		"compute instances list --filter=name ~ ^app-green$ --format=json(name,zone) --project=p1",
		// GCP allows at most ONE access config per NIC (live-fire
		// 2026-10-16): the target's ephemeral IP is deleted by design —
		// it was never the serving address once the reserved one attaches.
		"compute instances delete-access-config app-green --zone=europe-west1-b --project=p1",
		"compute instances add-access-config app-green --zone=europe-west1-b --address=203.0.113.50 --project=p1",
	}
	if len(calls) != len(want) {
		t.Fatalf("got %d calls, want %d:\n%s", len(calls), len(want), strings.Join(calls, "\n"))
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d drift:\n got: %s\nwant: %s", i, calls[i], want[i])
		}
	}
}

// Failure-rollback reverse: re-attaching to the old holder detaches the
// flip target first (the holder IS the assign target here, so no detach
// of it — the reserved address rides to the old holder in one add).
func TestBGReservedIPAssignGCPRestoreOldHolder(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"addr-list": `[{"name":"kampodra-active","address":"203.0.113.50","status":"IN_USE","users":["https://www.googleapis.com/compute/v1/projects/p1/zones/europe-west1-b/instances/app-green"]}]`,
		"inst-blue": `[{"name":"app-blue","zone":"` + bgGCPZone + `","status":"RUNNING"}]`,
	})
	if err := BGReservedIPAssign(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "kampodra-active", "app-blue", "203.0.113.50", ""); err != nil {
		t.Fatalf("BGReservedIPAssign(rollback): %v", err)
	}
	joined := strings.Join(bgOpShimCalls(t, shim.log), "\n")
	if !strings.Contains(joined, "delete-access-config app-green --zone=europe-west1-b") {
		t.Errorf("the failed target must be detached first:\n%s", joined)
	}
	if !strings.Contains(joined, "add-access-config app-blue --zone=europe-west1-b --address=203.0.113.50") {
		t.Errorf("the old holder must be re-attached:\n%s", joined)
	}
	if strings.Contains(joined, "delete-access-config app-blue") {
		t.Errorf("the restore target must not be detached:\n%s", joined)
	}
}

// Unassign IS release on gcp: drop the holder's access config; the
// address falls back to RESERVED on its own.
func TestBGReservedIPReleaseGCPDetachesHolder(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"addr-list": `[{"name":"kampodra-active","address":"203.0.113.50","status":"IN_USE","users":["` + bgGCPUser + `"]}]`,
	})
	if err := BGReservedIPRelease(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "kampodra-active"); err != nil {
		t.Fatalf("BGReservedIPRelease: %v", err)
	}
	calls := bgOpShimCalls(t, shim.log)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2: %v", len(calls), calls)
	}
	if !strings.Contains(calls[1], "delete-access-config app-blue --zone=europe-west1-b") {
		t.Errorf("release must detach the holder only:\n%s", strings.Join(calls, "\n"))
	}
	if strings.Contains(strings.Join(calls, "\n"), "add-access-config") {
		t.Errorf("release must not attach anywhere:\n%s", strings.Join(calls, "\n"))
	}
}

// Already dormant = a no-op beyond the lookup.
func TestBGReservedIPReleaseGCPDormantNoop(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"addr-list": `[{"name":"kampodra-active","address":"203.0.113.50","status":"RESERVED","users":[]}]`,
	})
	if err := BGReservedIPRelease(context.Background(), CloudAuth{Provider: "gcp"}, "kampodra-active"); err != nil {
		t.Fatalf("BGReservedIPRelease(dormant): %v", err)
	}
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 1 {
		t.Errorf("dormant release must not detach: %v", calls)
	}
}

// The anchor IS the reserved external IP on gcp (no secondary-private-ip
// mechanics) — it reads as the address while IN_USE, "" otherwise.
func TestBGAnchorIPGCPReadsReservedAddress(t *testing.T) {
	setupBGGCPShim(t, map[string]string{
		"addr-list": `[{"name":"kampodra-active","address":"203.0.113.50","status":"IN_USE","users":["` + bgGCPUser + `"]}]`,
	})
	addr, err := BGAnchorIP(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "")
	if err != nil || addr != "203.0.113.50" {
		t.Fatalf("BGAnchorIP = %q, %v", addr, err)
	}
	setupBGGCPShim(t, map[string]string{
		"addr-list": `[{"name":"kampodra-active","address":"203.0.113.50","status":"RESERVED","users":[]}]`,
	})
	if addr, _ := BGAnchorIP(context.Background(), CloudAuth{Provider: "gcp"}, ""); addr != "" {
		t.Errorf("dormant anchor = %q, want empty", addr)
	}
}

// BGPrivateIPCreate is a no-op on gcp (the anchor IS the reserved
// external IP — there is no secondary private IP to mint) returning the
// reserved address handle; BGSubnetCIDR fails closed (the CIDR feeds only
// the OCI anchor /prefix math, unreachable on this leg). Neither execs.
func TestBGPrivateIPCreateAndSubnetCIDRGCP(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"addr-list": `[{"name":"kampodra-active","address":"203.0.113.50","status":"IN_USE","users":["` + bgGCPUser + `"]}]`,
	})
	handle, err := BGPrivateIPCreate(context.Background(), CloudAuth{Provider: "gcp"}, "", "")
	if err != nil || handle != "kampodra-active" {
		t.Fatalf("BGPrivateIPCreate(gcp) = %q, %v", handle, err)
	}
	_, err = BGSubnetCIDR(context.Background(), CloudAuth{Provider: "gcp"}, "kampodra")
	if err == nil || !strings.Contains(err.Error(), "not applicable for provider gcp") {
		t.Fatalf("BGSubnetCIDR(gcp) = %v, want the fail-closed", err)
	}
	addr, err := BGPrivateIPAddress(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "")
	if err != nil || addr != "203.0.113.50" {
		t.Fatalf("BGPrivateIPAddress(gcp) = %q, %v", addr, err)
	}
	// The create + CIDR legs never exec; the address read is the one list.
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 1 || !strings.Contains(calls[0], "addresses list") {
		t.Errorf("want only the addresses list: %v", calls)
	}
}

// The machine-type pin mirrors the OCI A1.Flex pin; t2a-standard-2 is the
// Arm alternative. The ssh key rides --metadata-from-file.
func TestBGInstanceLaunchGCPArgv(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{})
	iid, err := BGInstanceLaunch(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"},
		"", "europe-west1-b", "kampodra", "app-alpine-20261010", "app-green", "")
	if err != nil || iid != "app-green" {
		t.Fatalf("BGInstanceLaunch = %q, %v", iid, err)
	}
	want := "compute instances create app-green --zone=europe-west1-b --machine-type=e2-standard-2" +
		" --subnet=kampodra --image=app-alpine-20261010 --project=p1"
	calls := bgOpShimCalls(t, shim.log)
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("argv drift: %v", calls)
	}
	shim2 := setupBGGCPShim(t, map[string]string{}) // fresh shim for the ssh-keys variant
	if _, err := BGInstanceLaunch(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"},
		"", "europe-west1-b", "kampodra", "app-alpine-20261010", "app-green", "/tmp/ops.pub"); err != nil {
		t.Fatal(err)
	}
	wantKey := "compute instances create app-green --zone=europe-west1-b --machine-type=e2-standard-2" +
		" --subnet=kampodra --image=app-alpine-20261010 --metadata-from-file ssh-keys=/tmp/ops.pub --project=p1"
	if calls := bgOpShimCalls(t, shim2.log); len(calls) != 1 || calls[0] != wantKey {
		t.Errorf("ssh-keys argv drift: %v", calls)
	}
}

// Launch failure carries the same wrap text as the OCI leg.
func TestBGInstanceLaunchGCPFailureWrap(t *testing.T) {
	setupBGGCPShim(t, map[string]string{})
	dir := t.TempDir()
	shim := "#!/bin/bash\nprintf 'boom' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	_, err := BGInstanceLaunch(context.Background(), CloudAuth{Provider: "gcp"}, "", "z", "s", "i", "app-green", "")
	if err == nil || !strings.Contains(err.Error(), "instance launch failed") {
		t.Fatalf("launch failure = %v, want the OCI-leg wrap text", err)
	}
}

// The caller polls the record to RUNNING. The aggregated list is the
// zone-free record read (describe would need a zone the neutral
// signature does not carry).
func TestBGInstanceRecordGCPPollsToRunning(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"inst-green": `[{"name":"app-green","zone":"` + bgGCPZone + `","status":"RUNNING"}]`,
	})
	rec, err := BGInstanceRecord(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "app-green")
	if err != nil {
		t.Fatalf("BGInstanceRecord: %v", err)
	}
	if rec.State != "RUNNING" || rec.AD != "europe-west1-b" {
		t.Errorf("record = %+v", rec)
	}
	want := "compute instances list --filter=name ~ ^app-green$ --format=json(name,zone,status) --project=p1"
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 1 || calls[0] != want {
		t.Errorf("argv drift: %v", calls)
	}
	setupBGGCPShim(t, map[string]string{})
	if _, err := BGInstanceRecord(context.Background(), CloudAuth{Provider: "gcp"}, "app-green"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("missing instance = %v, want not-found", err)
	}
}

// Golden image: newest name-prefixed image wins; none prefixed = ("","")
// so the command's own fallback path runs. The filter narrows server-side
// (`name:` — the `names:` spelling is rejected by the API), the prefix +
// newest pick stay client-side.
func TestBGGoldenImageGCPNewestPrefixed(t *testing.T) {
	shim := setupBGGCPShim(t, map[string]string{
		"images": `[
			{"name":"app-alpine-20261001","creationTimestamp":"2026-10-01T00:00:00Z"},
			{"name":"app-alpine-20261010","creationTimestamp":"2026-10-10T00:00:00Z"},
			{"name":"wrapapp-alpine-20261011","creationTimestamp":"2026-10-11T00:00:00Z"}
		]`,
	})
	image, firmware, err := BGGoldenImage(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "", "app-alpine")
	if err != nil {
		t.Fatalf("BGGoldenImage: %v", err)
	}
	if image != "app-alpine-20261010" {
		t.Errorf("image = %q, want the newest PREFIXED row (substring matches excluded)", image)
	}
	if firmware != "UEFI_64" {
		t.Errorf("firmware = %q, want the native-route verdict", firmware)
	}
	want := "compute images list --filter=name:app-alpine --format=json(name,creationTimestamp) --project=p1"
	if calls := bgOpShimCalls(t, shim.log); len(calls) != 1 || calls[0] != want {
		t.Errorf("argv drift: %v", calls)
	}
	setupBGGCPShim(t, map[string]string{"images": `[{"name":"other-thing","creationTimestamp":"2026-10-11T00:00:00Z"}]`})
	image, firmware, err = BGGoldenImage(context.Background(), CloudAuth{Provider: "gcp"}, "", "app-alpine")
	if err != nil || image != "" || firmware != "" {
		t.Errorf("no prefixed image = %q/%q, %v — want (\"\",\"\") for the fallback path", image, firmware, err)
	}
}
