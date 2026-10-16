package cloud

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The neutral bg ops' provider gates. Since slice 4 a gcp profile
// DISPATCHES to the gcloud legs (bgops_gcp.go): these pins hold the
// dispatch contract — no bluegreen refusal, zero oci execs (the wrong-CLI
// exec is the failure slice A closed), and gcloud execs for the read
// legs. An unknown provider still fails closed via CheckProvider, before
// any exec, forever (same protocol as backupops_gcp_test.go).

func setupBGOpsShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "oci-calls.log")
	shim := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "oci"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return log
}

// setupBGDispatchShims puts BOTH provider CLIs on PATH as logging shims —
// the gcp dispatch must exec gcloud and never oci.
func setupBGDispatchShims(t *testing.T) (gcpLog, ociLog string) {
	t.Helper()
	dir := t.TempDir()
	gcpLog = filepath.Join(dir, "gcloud-calls.log")
	ociLog = filepath.Join(dir, "oci-calls.log")
	for _, s := range []struct{ name, log string }{
		{"gcloud", gcpLog},
		{"oci", ociLog},
	} {
		shim := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> '" + s.log + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, s.name), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return gcpLog, ociLog
}

func bgOpShimCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// bgOpsAll maps every neutral bg op to a minimal invocation — the shared
// driver for the gcp-dispatch and unknown-provider pins.
func bgOpsAll() map[string]func(context.Context, CloudAuth) error {
	comp := "ocid1.compartment.test"
	return map[string]func(context.Context, CloudAuth) error{
		"BGResolveScope": func(ctx context.Context, a CloudAuth) error {
			_, err := BGResolveScope(ctx, a, comp)
			return err
		},
		"BGColorInstance": func(ctx context.Context, a CloudAuth) error {
			_, err := BGColorInstance(ctx, a, comp, "app-blue")
			return err
		},
		"BGAnchorIP": func(ctx context.Context, a CloudAuth) error {
			_, err := BGAnchorIP(ctx, a, "ocid1.vnic.v1")
			return err
		},
		"BGReservedIPList": func(ctx context.Context, a CloudAuth) error {
			_, _, err := BGReservedIPList(ctx, a, comp)
			return err
		},
		"BGReservedIPCreate": func(ctx context.Context, a CloudAuth) error {
			_, err := BGReservedIPCreate(ctx, a, comp, "kampodra-active")
			return err
		},
		"BGReservedIPAssign": func(ctx context.Context, a CloudAuth) error {
			return BGReservedIPAssign(ctx, a, "ocid1.publicip.r1", "app-green", "ocid1.privateip.p", "ASSIGNED")
		},
		"BGReservedIPRelease": func(ctx context.Context, a CloudAuth) error {
			return BGReservedIPRelease(ctx, a, "ocid1.publicip.r1")
		},
		"BGPrimaryVnic": func(ctx context.Context, a CloudAuth) error {
			_, _, _, err := BGPrimaryVnic(ctx, a, comp, "ocid1.instance.i")
			return err
		},
		"BGPrivateIPCreate": func(ctx context.Context, a CloudAuth) error {
			_, err := BGPrivateIPCreate(ctx, a, "ocid1.vnic.v1", "kampodra-reserved-anchor")
			return err
		},
		"BGSubnetCIDR": func(ctx context.Context, a CloudAuth) error {
			_, err := BGSubnetCIDR(ctx, a, "ocid1.subnet.s1")
			return err
		},
		"BGPrivateIPAddress": func(ctx context.Context, a CloudAuth) error {
			_, err := BGPrivateIPAddress(ctx, a, "ocid1.privateip.p")
			return err
		},
		"BGInstanceLaunch": func(ctx context.Context, a CloudAuth) error {
			_, err := BGInstanceLaunch(ctx, a, comp, "AD-1", "ocid1.subnet.s1", "ocid1.image.i", "app-blue", "")
			return err
		},
		"BGInstanceRecord": func(ctx context.Context, a CloudAuth) error {
			_, err := BGInstanceRecord(ctx, a, "ocid1.instance.i")
			return err
		},
		"BGGoldenImage": func(ctx context.Context, a CloudAuth) error {
			_, _, err := BGGoldenImage(ctx, a, comp, "app-alpine")
			return err
		},
	}
}

// A gcp profile dispatches to gcloud: no op raises the bluegreen refusal,
// not one call lands on the oci CLI, and the read legs exec gcloud.
func TestBGOpsGCPDispatchesToGCloud(t *testing.T) {
	gcpLog, ociLog := setupBGDispatchShims(t)
	for name, op := range bgOpsAll() {
		if err := op(context.Background(), CloudAuth{Provider: "gcp"}); err != nil &&
			strings.Contains(err.Error(), "bluegreen is OCI-only today") {
			t.Errorf("%s(gcp) = %v — the slice-A refusal must be gone (slice 4 dispatches)", name, err)
		}
	}
	if calls := bgOpShimCalls(t, ociLog); len(calls) != 0 {
		t.Errorf("a gcp profile must never exec oci: %v", calls)
	}
	if calls := bgOpShimCalls(t, gcpLog); len(calls) == 0 {
		t.Error("the gcp dispatch must exec gcloud (the read legs), got zero calls")
	}
}

func TestBGOpsUnknownProviderFailsClosed(t *testing.T) {
	log := setupBGOpsShim(t)
	for name, op := range bgOpsAll() {
		err := op(context.Background(), CloudAuth{Provider: "aws"})
		if err == nil || !strings.Contains(err.Error(), "unsupported cloud provider") {
			t.Errorf("%s(aws) = %v, want the CheckProvider refusal", name, err)
		}
	}
	if calls := bgOpShimCalls(t, log); len(calls) != 0 {
		t.Errorf("the refusals must not exec oci: %v", calls)
	}
}
