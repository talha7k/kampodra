package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
	"github.com/talha7k/kampodra/internal/command"
)

// The gcp twin of the backup harness: a gcloud shim on PATH dispatching on
// "$1 $2" (ls-vs-head split on the trailing ** wildcard), fixtures mirroring
// gcloud's native shapes (bare arrays, base64 md5Hash, string sizes), and an
// invocation log. Auth rides the instance profile's cloud block
// {"provider": "gcp", ...}; the namespace flag/env are inert under gcp —
// GCS scopes by bucket+project.

// setupBackupGCP is setupBackup's gcloud twin: same harness struct, the
// gcloud log riding ociLog (the reader serves both CLIs).
func setupBackupGCP(t *testing.T) *backupHarness {
	t.Helper()
	home := t.TempDir()
	stubDir := t.TempDir()
	fixtures := t.TempDir()
	gcpLog := filepath.Join(stubDir, "gcloud-calls.log")
	body := filepath.Join(fixtures, "body")
	os.WriteFile(body, []byte(backupFixtureBody), 0o600)

	gcloudShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + gcpLog + "'\n" +
		"case \"$1 $2\" in\n" +
		"  \"storage ls\")\n" +
		"    case \"$4\" in\n" +
		"      *\"**\") [ -f \"" + fixtures + "/list.json\" ] && cat \"" + fixtures + "/list.json\" || echo '[]' ;;\n" +
		"      *) [ -f \"" + fixtures + "/head.json\" ] && cat \"" + fixtures + "/head.json\" || echo '[]' ;;\n" +
		"    esac ;;\n" +
		"  \"storage cp\") cp '" + body + "' \"$4\" ;;\n" +
		"esac\nexit 0\n"
	os.WriteFile(filepath.Join(stubDir, "gcloud"), []byte(gcloudShim), 0o755)

	t.Setenv("PATH", stubDir+":/usr/bin:/bin")
	// No OCI env may leak into a gcp run: OCI_NAMESPACE must stay inert (the
	// gcp leg has no namespace concept) and OCI_COMPARTMENT/OCI_PROFILE are
	// meaningless here.
	t.Setenv("OCI_NAMESPACE", "")
	t.Setenv("OCI_PROFILE", "")
	t.Setenv("OCI_COMPARTMENT", "")
	for _, k := range []string{"KAMPODRA_HOST", "KAMPODRA_SSH_KEY", "KAMPODRA_PROFILE", "KAMPODRA_BUCKET"} {
		t.Setenv(k, "")
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &backupHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
		},
		fixtures: fixtures,
		stdout:   stdout,
		stderr:   stderr,
		ociLog:   gcpLog,
	}
}

func TestBackupGCPListRendersTable(t *testing.T) {
	h := setupBackupGCP(t)
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp", "project": "p1"}}`)
	os.WriteFile(filepath.Join(h.fixtures, "list.json"), []byte(`[
		{"url":"gs://my-bkt/db/tenants/acme/20261008T050000Z.db#1","type":"cloud_object","metadata":{"bucket":"my-bkt","name":"db/tenants/acme/20261008T050000Z.db","size":"1048576","timeCreated":"2026-10-08T05:00:05.000Z"}},
		{"url":"gs://my-bkt/db/tenants/beta/latest.db#2","type":"cloud_object","metadata":{"bucket":"my-bkt","name":"db/tenants/beta/latest.db","size":"42","updated":"2026-10-09T10:00:00.000Z"}}
	]`), 0o600)

	if code := h.run(t, "list", "--bucket", "my-bkt", "--prefix", "db/tenants/", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"[backup] objects in bucket my-bkt (prefix: db/tenants/) (profile: gcloud active config):",
		"db/tenants/acme/20261008T050000Z.db", "1048576", "2026-10-08T05:00:05.000Z",
		"db/tenants/beta/latest.db", "42", "2026-10-09T10:00:00.000Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	// One flat recursive call, the identity flag appended exactly once, and
	// no namespace anywhere.
	calls := h.ociInvocations(t)
	if calls != "storage ls --json gs://my-bkt/db/tenants/** --project=p1\n" {
		t.Errorf("argv drift:\n%s", calls)
	}
	if strings.Contains(calls, "namespace") || strings.Contains(calls, "os ns") {
		t.Errorf("gcp must not carry or resolve a namespace:\n%s", calls)
	}
}

// The md5 digest path must verify end-to-end: the fixture's md5Hash is
// exactly what HashFile("md5") produces for the downloaded bytes, so the
// parser's "md5:<base64>" mapping lands byte-identical on the existing
// verify comparison.
func TestBackupGCPDownloadVerifiedByMD5(t *testing.T) {
	h := setupBackupGCP(t)
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp", "project": "p1"}}`)
	md5Hash, err := cloud.HashFile(filepath.Join(h.fixtures, "body"), "md5")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h.fixtures, "head.json"), []byte(
		`[{"url":"gs://my-bkt/db/x.db#1","type":"cloud_object","metadata":{"bucket":"my-bkt","name":"db/x.db","size":"39","md5Hash":"`+md5Hash+`","timeCreated":"2026-10-08T05:00:05.000Z"}}]`), 0o600)
	out := filepath.Join(t.TempDir(), "restore.db")

	if code := h.run(t, "download", "db/x.db", "--out", out, "--bucket", "my-bkt", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != backupFixtureBody {
		t.Fatalf("downloaded file wrong: %v %q", err, data)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Errorf("downloaded perm = %o, want 600", fi.Mode().Perm())
	}
	outStr := h.stdout.String()
	if !strings.Contains(outStr, "integrity: verified (md5)") {
		t.Errorf("md5 verify missing:\n%s", outStr)
	}
	// head argv (single-object ls) then cp argv — identity flag once each.
	joined := h.ociInvocations(t)
	for _, want := range []string{
		"storage ls --json gs://my-bkt/db/x.db --project=p1",
		"storage cp gs://my-bkt/db/x.db " + out + " --project=p1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q:\n%s", want, joined)
		}
	}
}

func TestBackupGCPMissingGCLOUDFailsClosed(t *testing.T) {
	h := setupBackupGCP(t)
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	t.Setenv("PATH", "/usr/bin:/bin") // no gcloud
	if code := h.run(t, "list", "--bucket", "b", "--profile", "gcp-prof"); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	errOut := h.stderr.String()
	for _, want := range []string{"gcloud CLI not found", "install Google Cloud SDK"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
	if calls := h.ociInvocations(t); calls != "" {
		t.Errorf("no CLI means no calls: %s", calls)
	}
}

// The --namespace flag and the OCI_NAMESPACE env are OCI-scoped: under gcp
// they ride nothing, resolve nothing, and fail nothing.
func TestBackupGCPNamespaceFlagIgnored(t *testing.T) {
	h := setupBackupGCP(t)
	writeDNSProfile(t, h.deps.Home, "gcp-prof", `{"host": "root@198.51.100.7", "cloud": {"provider": "gcp"}}`)
	t.Setenv("OCI_NAMESPACE", "env-ns")
	if code := h.run(t, "list", "--bucket", "my-bkt", "--namespace", "given-ns", "--profile", "gcp-prof"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "[backup] (no objects)") {
		t.Errorf("expected the bare-[] empty listing:\n%s", h.stdout.String())
	}
	calls := h.ociInvocations(t)
	if calls != "storage ls --json gs://my-bkt/**\n" {
		t.Errorf("argv drift:\n%s", calls)
	}
	for _, banned := range []string{"namespace", "os ns", "env-ns", "given-ns"} {
		if strings.Contains(calls, banned) {
			t.Errorf("gcp leaked %q into the argv:\n%s", banned, calls)
		}
	}
}
