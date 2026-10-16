// The tenancy namespace ladder for the backup object calls: --namespace >
// cloud block / OCI_NAMESPACE > `oci os ns get` (the oci CLI's own internal
// resolution fails on user-principal laptop configs, 2026-10-10 live fire).

package command_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The 2026-10-10 laptop-path contract: backup list resolves the tenancy
// namespace FIRST (`oci os ns get` — the same call the oci CLI wraps for
// its internal resolution) and feeds it to the object list as --namespace.
// The CLI's internal resolution fails on user-principal laptop configs
// ("Unable to retrieve namespace internally"); the instance-principal VM
// path keeps working because ns get works there too.
func TestBackupListResolvesNamespaceAndFeedsItToList(t *testing.T) {
	h := setupBackup(t)
	if code := h.run(t, "list", "--bucket", "my-bkt"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.ociInvocations(t)
	if !strings.Contains(calls, "os ns get") {
		t.Errorf("list must resolve the namespace via `oci os ns get`:\n%s", calls)
	}
	if !strings.Contains(calls, "--namespace test-ns") {
		t.Errorf("resolved namespace must ride the list call:\n%s", calls)
	}
	if i, j := strings.Index(calls, "os ns get"), strings.Index(calls, "object list"); i > j {
		t.Errorf("ns get must run before the list call:\n%s", calls)
	}
}

// An explicit --namespace flag is authoritative: it rides the list call and
// the resolution call is skipped entirely.
func TestBackupListExplicitNamespaceFlagSkipsResolution(t *testing.T) {
	h := setupBackup(t)
	if code := h.run(t, "list", "--bucket", "b", "--namespace", "given-ns"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.ociInvocations(t)
	if strings.Contains(calls, "os ns") {
		t.Errorf("explicit --namespace must skip the ns get call:\n%s", calls)
	}
	if !strings.Contains(calls, "--namespace given-ns") {
		t.Errorf("explicit namespace must reach the list call:\n%s", calls)
	}
}

// namespaceEscapeHatchCase is the SHARED body for both env-var escape
// hatch tests (the KAMPODRA_CLOUD_* primary and the deprecated OCI_*
// alias — same behavior, one source): the env rides the list call
// without an ns get, and the explicit flag beats it.
func namespaceEscapeHatchCase(t *testing.T, envKey string) {
	h := setupBackup(t)
	t.Setenv(envKey, "env-ns")
	if code := h.run(t, "list", "--bucket", "b"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.ociInvocations(t)
	if strings.Contains(calls, "os ns") || !strings.Contains(calls, "--namespace env-ns") {
		t.Errorf("%s must ride the list call without an ns get:\n%s", envKey, calls)
	}

	h = setupBackup(t)
	t.Setenv(envKey, "env-ns")
	if code := h.run(t, "list", "--bucket", "b", "--namespace", "flag-ns"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if calls := h.ociInvocations(t); !strings.Contains(calls, "--namespace flag-ns") || strings.Contains(calls, "env-ns") {
		t.Errorf("--namespace flag must beat %s:\n%s", envKey, calls)
	}
}

// OCI_NAMESPACE is the deprecated escape hatch (matching
// OCI_PROFILE / OCI_COMPARTMENT); the explicit flag still beats it.
func TestBackupNamespaceOCIEnvEscapeHatch(t *testing.T) {
	namespaceEscapeHatchCase(t, "OCI_NAMESPACE")
}

// The renamed kampodra-namespaced env is the primary namespace escape hatch
// (the deprecated OCI_NAMESPACE alias keeps resolving one release); the
// explicit flag still beats it.
func TestBackupNamespaceCloudEnvEscapeHatch(t *testing.T) {
	namespaceEscapeHatchCase(t, "KAMPODRA_CLOUD_NAMESPACE")
}

// The instance profile's cloud block "namespace" field is the persistent
// config surface; it beats the env escape hatch.
func TestBackupCloudBlockNamespaceBeatsEnv(t *testing.T) {
	h := setupBackup(t)
	t.Setenv("OCI_NAMESPACE", "env-ns")
	os.MkdirAll(filepath.Join(h.deps.Home, ".kampodra"), 0o700)
	cfg := `{"defaultProfile":"","profiles":{"inst-prof":{"host":"root@203.0.113.9","sshKey":"/keys/p","cloud":{"namespace":"cloud-ns"}}}}`
	os.WriteFile(filepath.Join(h.deps.Home, ".kampodra", "config.json"), []byte(cfg), 0o600)
	if code := h.run(t, "list", "--bucket", "b", "--profile", "inst-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.ociInvocations(t)
	if strings.Contains(calls, "os ns") || !strings.Contains(calls, "--namespace cloud-ns") || strings.Contains(calls, "env-ns") {
		t.Errorf("cloud-block namespace must beat env and skip resolution:\n%s", calls)
	}
}

// When the namespace cannot be resolved, the failure names the fix and no
// object call happens.
func TestBackupListNamespaceResolutionFailureNamesTheFix(t *testing.T) {
	h := setupBackup(t)
	os.WriteFile(filepath.Join(h.fixtures, "deny-ns"), []byte("denied"), 0o600)
	if code := h.run(t, "list", "--bucket", "b"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	errOut := h.stderr.String()
	for _, want := range []string{"namespace", "--namespace", "OCI_NAMESPACE"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("resolution failure must name the fix %q:\n%s", want, errOut)
		}
	}
	if calls := h.ociInvocations(t); strings.Contains(calls, "object list") {
		t.Errorf("list must not run when the namespace is unresolved:\n%s", calls)
	}
}

// Download resolves the namespace too — the head (digest) and get legs ride
// it; on the laptop both would hit the same internal-resolution failure.
func TestBackupDownloadFeedsResolvedNamespaceToHeadAndGet(t *testing.T) {
	h := setupBackup(t)
	out := filepath.Join(t.TempDir(), "restore.db")
	if code := h.run(t, "download", "db/x.db", "--out", out, "--bucket", "b"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	calls := h.ociInvocations(t)
	if got := strings.Count(calls, "--namespace test-ns"); got != 2 {
		t.Errorf("--namespace on head+get = %d, want 2:\n%s", got, calls)
	}
}
