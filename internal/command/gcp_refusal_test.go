package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Interim fail-closed guards (slice 5 lands the image-import gcp leg):
// image-import still execs the oci CLI directly, but CheckProvider passes
// a gcp profile — without the guard it would exec the wrong CLI. The
// image-import refusal is pinned in the dns_test.go
// TestDNSGCPCompartmentIsRefused style: a {"provider":"gcp"} profile fails
// closed with the message, zero CLI execs of either CLI. bluegreen
// DISPATCHES since slice 4 — its gcp pin lives in bluegreen_gcp_test.go
// (gcloud shim harness, no oci exec, no refusal).

// addGCLOUDShim puts a logging gcloud on PATH (the bg/import harnesses only
// shim oci): any gcloud exec during a refusal is proof of a leak.
func addGCLOUDShim(t *testing.T, logPath string) {
	t.Helper()
	dir := t.TempDir()
	shim := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func gcpShimCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
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

func TestImageImportGCPProfileFailsClosed(t *testing.T) {
	h, qcow2 := setupImport(t, "AVAILABLE", "UEFI_64")
	gcpLog := filepath.Join(t.TempDir(), "gcloud-calls.log")
	addGCLOUDShim(t, gcpLog)
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	if code := h.run(t, "--image", qcow2, "--profile", "gcp-prof"); code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr: %s)", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "image-import is OCI-only today — gcloud compute images import requires the Cloud Build API; run it directly or use an oci profile") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if calls := gcpShimCalls(t, gcpLog); len(calls) != 0 {
		t.Errorf("the refusal must not exec gcloud: %v", calls)
	}
	if calls := h.ociCalls(t); len(calls) != 0 {
		t.Errorf("the refusal must not exec oci: %v", calls)
	}
}
