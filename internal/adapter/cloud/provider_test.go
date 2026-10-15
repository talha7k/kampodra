package cloud

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The provider gate and RunCloud (the one exec seam): oci stays the default,
// gcp slots in, everything unknown fails closed. The auth rule is unchanged:
// the provider CLI's own auth ONLY — the cloud block names identity entries
// (profile, project, account), never credentials.

func TestSupportedProviders(t *testing.T) {
	got := SupportedProviders()
	if len(got) != 2 || got[0] != "oci" || got[1] != "gcp" {
		t.Errorf("SupportedProviders() = %v, want [oci gcp]", got)
	}
}

func TestCheckProviderGateMatrix(t *testing.T) {
	for _, p := range []string{"", "oci", "gcp"} {
		if err := CheckProvider(CloudAuth{Provider: p}); err != nil {
			t.Errorf("CheckProvider(%q) = %v, want nil", p, err)
		}
	}
	err := CheckProvider(CloudAuth{Provider: "aws"})
	if err == nil || !strings.Contains(err.Error(), `"aws"`) || !strings.Contains(err.Error(), "oci, gcp") {
		t.Errorf("unknown provider must fail closed naming itself and the supported set: %v", err)
	}
}

func TestCLIMap(t *testing.T) {
	if CLIName("") != "oci" || CLIName("oci") != "oci" {
		t.Errorf("empty/oci must map to the oci CLI")
	}
	if CLIName("gcp") != "gcloud" {
		t.Errorf("gcp must map to gcloud, got %q", CLIName("gcp"))
	}
	if h := InstallHint("gcp"); !strings.Contains(h, "cloud.google.com/sdk/docs/install") || !strings.Contains(h, "gcloud init") {
		t.Errorf("gcp install hint = %q", h)
	}
	if h := InstallHint("oci"); !strings.Contains(h, "oci-cli") || !strings.Contains(h, "oci setup config") {
		t.Errorf("oci install hint = %q", h)
	}
}

func TestParseCloudAuthGCPIdentity(t *testing.T) {
	got := ParseCloudAuth(json.RawMessage(`{"provider":"gcp","project":"proj-1","account":"ops@example.iam"}`))
	if got.Provider != "gcp" || got.Project != "proj-1" || got.Account != "ops@example.iam" {
		t.Errorf("parsed = %+v", got)
	}
	// Empty project/account = gcloud resolves its active config natively.
	got = ParseCloudAuth(json.RawMessage(`{"provider":"gcp"}`))
	if got.Project != "" || got.Account != "" {
		t.Errorf("empty identity must stay empty: %+v", got)
	}
}

// withGCPAuth mirrors withProfile: the cloud block's identity names ride as
// gcloud's own config overrides, ONLY when set.
func TestWithGCPAuthOnlyWhenSet(t *testing.T) {
	if got := withGCPAuth([]string{"dns", "zone", "list"}, CloudAuth{Provider: "gcp"}); strings.Join(got, " ") != "dns zone list" {
		t.Errorf("empty project/account must stay flag-free: %v", got)
	}
	got := withGCPAuth([]string{"dns", "zone", "list"}, CloudAuth{Provider: "gcp", Project: "p1", Account: "a@b"})
	want := "dns zone list --project=p1 --account=a@b"
	if strings.Join(got, " ") != want {
		t.Errorf("got %v, want %q", got, want)
	}
}

// fakeCLI drops a logging shim for one binary on a PATH-isolated dir.
func fakeCLI(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// RunCloud dispatches on the cloud block: oci (and empty) → the oci CLI,
// gcp → gcloud — each argv byte-identical, auth flags only when set.
func TestRunCloudDispatch(t *testing.T) {
	dir := t.TempDir()
	ociLog := filepath.Join(dir, "oci-calls.log")
	gcloudLog := filepath.Join(dir, "gcloud-calls.log")
	for _, c := range []struct{ bin, log string }{
		{"oci", ociLog}, {"gcloud", gcloudLog},
	} {
		shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + c.log + "'\necho " + c.bin + "-ok\n"
		if err := os.WriteFile(filepath.Join(dir, c.bin), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	ctx := context.Background()

	out, err := RunCloud(ctx, CloudAuth{Provider: "oci"}, []string{"dns", "zone", "list", "-c", "ocid1.x"})
	if err != nil || strings.TrimSpace(out) != "oci-ok" {
		t.Fatalf("RunCloud(oci) = %q, %v", out, err)
	}
	// Empty provider IS oci — the byte-identical default path.
	out, err = RunCloud(ctx, CloudAuth{}, []string{"iam", "compartment", "list", "--all"})
	if err != nil || strings.TrimSpace(out) != "oci-ok" {
		t.Fatalf("RunCloud(empty) = %q, %v", out, err)
	}
	out, err = RunCloud(ctx, CloudAuth{Provider: "gcp", Project: "p1", Account: "a@b"}, []string{"dns", "managed-zones", "list"})
	if err != nil || strings.TrimSpace(out) != "gcloud-ok" {
		t.Fatalf("RunCloud(gcp) = %q, %v", out, err)
	}
	out, err = RunCloud(ctx, CloudAuth{Provider: "gcp"}, []string{"dns", "record-sets", "list", "--zone=z"})
	if err != nil || strings.TrimSpace(out) != "gcloud-ok" {
		t.Fatalf("RunCloud(gcp native) = %q, %v", out, err)
	}

	// Exact argv pins: the identity flags ride ONLY the gcp call that set
	// them, appended after the command words; oci never sees them.
	wantLines := map[string]string{
		ociLog:    "dns zone list -c ocid1.x\niam compartment list --all",
		gcloudLog: "dns managed-zones list --project=p1 --account=a@b\ndns record-sets list --zone=z",
	}
	for log, want := range wantLines {
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatalf("read %s: %v", log, err)
		}
		if strings.TrimSpace(string(data)) != want {
			t.Errorf("%s argv drift:\n got: %q\nwant: %q", log, strings.TrimSpace(string(data)), want)
		}
	}
}

func TestRunCloudUnknownFailsClosed(t *testing.T) {
	dir := t.TempDir()
	shim := "#!/bin/sh\nexit 0\n"
	for _, bin := range []string{"oci", "gcloud"} {
		if err := os.WriteFile(filepath.Join(dir, bin), []byte(shim), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	_, err := RunCloud(context.Background(), CloudAuth{Provider: "aws"}, []string{"whatever"})
	if err == nil || !strings.Contains(err.Error(), "unsupported cloud provider") || !strings.Contains(err.Error(), "oci, gcp") {
		t.Errorf("unknown provider must fail closed: %v", err)
	}
}

func TestRunCloudMissingCLINamesInstallHint(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no provider CLI anywhere
	_, err := RunCloud(context.Background(), CloudAuth{Provider: "gcp"}, []string{"dns", "managed-zones", "list"})
	if err == nil || !strings.Contains(err.Error(), "gcloud CLI not found") || !strings.Contains(err.Error(), "install Google Cloud SDK") {
		t.Errorf("missing gcloud must fail closed naming the install hint: %v", err)
	}
}

// gcloud failures ride stderr exactly like the oci seam: the message names
// the CLI + first arg, and the CLI's own stderr is attached for classifying.
func TestRunCloudGCloudCapturesStderr(t *testing.T) {
	dir := fakeCLI(t, "gcloud", "#!/bin/sh\nif [ \"$1\" = fail ]; then echo 'boom: permission denied' >&2; exit 1; fi\necho ok\n")
	t.Setenv("PATH", dir)
	_, err := RunCloud(context.Background(), CloudAuth{Provider: "gcp"}, []string{"fail", "--zone=z"})
	if err == nil || !strings.Contains(err.Error(), "gcloud fail:") || !strings.Contains(err.Error(), "boom: permission denied") {
		t.Errorf("gcloud error = %v, want stderr attached", err)
	}
}
