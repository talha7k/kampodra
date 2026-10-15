package cloud

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gcloud legs of the neutral backup ops: exact argv pins for the flat
// `storage ls` (incl. the empty-prefix `**` wildcard), gcloud's bare-[]
// listings, md5-base64 parity with the HashFile digest path, the composite
// (empty-md5Hash) digest-skip, and download file placement. The gcloud CLI
// is PATH-shimmed; kampodra passes only identity flags (--project/--account),
// never credential material. The namespace param is OCI-only — GCS scopes by
// bucket+project — so no gcp argv carries it.

const gcpBackupBody = "kampodra gcp backup fixture payload\n"

const gcpBackupListFixture = `[
	{"name":"db/tenants/acme/20261008T050000Z.db","size":"1048576","timeCreated":"2026-10-08T05:00:05.000Z","md5Hash":"CY9rzUYh03PK3k6DJie09g=="},
	{"name":"gs://my-bkt/db/tenants/beta/latest.db","size":"42","updated":"2026-10-09T10:00:00.000Z"},
	{"name":"db/tenants/gamma/none.db","size":"7"}
]`

type backupGCPShim struct {
	dir string
	log string
}

// setupBackupGCPShim installs a gcloud shim dispatching on "$1 $2" (with the
// ls-vs-head split on the trailing ** wildcard), fixture files, and an
// invocation log. `storage cp` places the fixture body at the destination
// argument.
func setupBackupGCPShim(t *testing.T, listJSON, headJSON string) *backupGCPShim {
	t.Helper()
	dir := t.TempDir()
	list := filepath.Join(dir, "list.json")
	head := filepath.Join(dir, "head.json")
	body := filepath.Join(dir, "body.bin")
	log := filepath.Join(dir, "gcloud-calls.log")
	if err := os.WriteFile(list, []byte(listJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(head, []byte(headJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(body, []byte(gcpBackupBody), 0o600); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/bash\nprintf '%s\\n' \"$*\" >> '" + log + "'\n" +
		"case \"$1 $2\" in\n" +
		"  \"storage ls\")\n" +
		"    case \"$4\" in\n" +
		"      *\"**\") cat '" + list + "' ;;\n" +
		"      *) cat '" + head + "' ;;\n" +
		"    esac ;;\n" +
		"  \"storage cp\") cp '" + body + "' \"$4\" ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gcloud"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim shells out to cat/cp — keep the core dirs on PATH.
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	return &backupGCPShim{dir: dir, log: log}
}

func (s *backupGCPShim) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(s.log)
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

func TestGCPBackupListArgvAndParse(t *testing.T) {
	shim := setupBackupGCPShim(t, gcpBackupListFixture, `[]`)
	// The namespace param rides along to prove the gcp leg ignores it.
	objs, err := BackupList(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "my-bkt", "db/tenants/", "test-ns")
	if err != nil {
		t.Fatalf("BackupList: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("got %d objects, want 3: %+v", len(objs), objs)
	}
	if objs[0].Name != "db/tenants/acme/20261008T050000Z.db" || objs[0].Size != 1048576 || objs[0].TimeCreated != "2026-10-08T05:00:05.000Z" {
		t.Errorf("objs[0] = %+v", objs[0])
	}
	// A gs://bucket/ name prefix is tolerated and stripped; the updated
	// stamp falls into timeCreated's slot.
	if objs[1].Name != "db/tenants/beta/latest.db" || objs[1].Size != 42 || objs[1].TimeCreated != "2026-10-09T10:00:00.000Z" {
		t.Errorf("objs[1] = %+v (gs:// prefix must strip, updated must fill the time slot)", objs[1])
	}
	// No timestamp at all reads as the same "-" placeholder the oci parser emits.
	if objs[2].TimeCreated != "-" {
		t.Errorf("objs[2].TimeCreated = %q, want \"-\"", objs[2].TimeCreated)
	}
	// One flat recursive call, the identity flag appended exactly once, and
	// NO namespace anywhere (GCS scopes by bucket+project).
	calls := shim.calls(t)
	if len(calls) != 1 || calls[0] != "storage ls --json gs://my-bkt/db/tenants/** --project=p1" {
		t.Errorf("argv drift: %v", calls)
	}
}

func TestGCPBackupListEmptyPrefixWildcardAndBareEmptyArray(t *testing.T) {
	// Empty prefix lists the whole bucket: `**` directly after the slash.
	shim := setupBackupGCPShim(t, gcpBackupListFixture, `[]`)
	if _, err := BackupList(context.Background(), CloudAuth{Provider: "gcp"}, "my-bkt", "", ""); err != nil {
		t.Fatalf("BackupList: %v", err)
	}
	calls := shim.calls(t)
	if len(calls) != 1 || calls[0] != "storage ls --json gs://my-bkt/**" {
		t.Fatalf("empty-prefix argv drift: %v", calls)
	}
	// gcloud emits bare [] for an empty listing — zero objects, no error.
	shim = setupBackupGCPShim(t, `[]`, `[]`)
	objs, err := BackupList(context.Background(), CloudAuth{Provider: "gcp"}, "my-bkt", "db/", "")
	if err != nil || len(objs) != 0 {
		t.Errorf("bare [] = %d objects, %v", len(objs), err)
	}
}

// md5Hash is base64 MD5 — it must map onto the EXISTING digest semantics:
// "md5:<base64>", exactly the shape ExpectedDigest emits for
// opc-content-md5 and exactly what HashFile("md5") reproduces, so the
// command's verify compares like-for-like unchanged.
func TestGCPBackupHeadMD5Parity(t *testing.T) {
	parity := filepath.Join(t.TempDir(), "parity.bin")
	if err := os.WriteFile(parity, []byte(gcpBackupBody), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := HashFile(parity, "md5") // base64 — the existing digest path
	if err != nil {
		t.Fatal(err)
	}
	shim := setupBackupGCPShim(t, `[]`,
		`[{"name":"db/x.db","size":"39","md5Hash":"`+want+`","timeCreated":"2026-10-08T05:00:05.000Z"}]`)
	obj, err := BackupHead(context.Background(), CloudAuth{Provider: "gcp", Project: "p1"}, "my-bkt", "db/x.db", "test-ns")
	if err != nil {
		t.Fatalf("BackupHead: %v", err)
	}
	if obj.Name != "db/x.db" || obj.Size != 39 {
		t.Errorf("head = %+v", obj)
	}
	if obj.Digest != "md5:"+want {
		t.Errorf("Digest = %q, want md5:%s (the HashFile(\"md5\") form)", obj.Digest, want)
	}
	// Single-object argv shape: no wildcard, same ls op.
	if calls := shim.calls(t); len(calls) != 1 || calls[0] != "storage ls --json gs://my-bkt/db/x.db --project=p1" {
		t.Errorf("argv drift: %v", calls)
	}
}

// Composite objects carry no md5Hash — digest-skips, preserving today's
// "no digest metadata" semantic. A failed head does the same.
func TestGCPBackupHeadCompositeAndFailureDigestSkip(t *testing.T) {
	setupBackupGCPShim(t, `[]`, `[{"name":"db/comp","size":"7","md5Hash":""}]`)
	obj, err := BackupHead(context.Background(), CloudAuth{Provider: "gcp"}, "my-bkt", "db/comp", "")
	if err != nil || obj.Digest != "" {
		t.Errorf("composite head = %+v, %v — empty md5Hash must digest-skip", obj, err)
	}
	// A failed ls reads as zero (the caller's HEAD-must-not-break rule).
	setupBackupGCPShim(t, `[]`, `[]`)
	if obj, err := BackupHead(context.Background(), CloudAuth{Provider: "gcp"}, "my-bkt", "db/absent", ""); err != nil || obj.Digest != "" {
		t.Errorf("absent head = %+v, %v — must digest-skip", obj, err)
	}
	// The name tolerates a gs://bucket/ prefix here too.
	setupBackupGCPShim(t, `[]`, `[{"name":"gs://my-bkt/db/prefixed.db","size":"1","md5Hash":"CY9rzUYh03PK3k6DJie09g=="}]`)
	if obj, err := BackupHead(context.Background(), CloudAuth{Provider: "gcp"}, "my-bkt", "db/prefixed.db", ""); err != nil || obj.Name != "db/prefixed.db" || obj.Digest != "md5:CY9rzUYh03PK3k6DJie09g==" {
		t.Errorf("prefixed head = %+v, %v", obj, err)
	}
}

func TestGCPBackupDownloadArgvAndPlacement(t *testing.T) {
	shim := setupBackupGCPShim(t, `[]`, `[]`)
	out := filepath.Join(shim.dir, "restore.db")
	auth := CloudAuth{Provider: "gcp", Project: "p1"}
	if err := BackupDownload(context.Background(), auth, "my-bkt", "db/x.db", out, "test-ns"); err != nil {
		t.Fatalf("BackupDownload: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != gcpBackupBody {
		t.Fatalf("downloaded file wrong: %v %q", err, data)
	}
	calls := shim.calls(t)
	if len(calls) != 1 || calls[0] != "storage cp gs://my-bkt/db/x.db "+out+" --project=p1" {
		t.Errorf("argv drift: %v", calls)
	}
}

// An unknown provider refuses before any argv is improvised — the
// CheckProvider gate fires first, the shim never runs.
func TestGCPBackupUnknownProviderFailsClosed(t *testing.T) {
	shim := setupBackupGCPShim(t, `[]`, `[]`)
	auth := CloudAuth{Provider: "aws"}
	if _, err := BackupList(context.Background(), auth, "b", "p", "ns"); err == nil || !strings.Contains(err.Error(), "unsupported cloud provider") {
		t.Errorf("BackupList = %v, want a fail-closed refusal", err)
	}
	if _, err := BackupHead(context.Background(), auth, "b", "o", "ns"); err == nil || !strings.Contains(err.Error(), "unsupported cloud provider") {
		t.Errorf("BackupHead = %v, want a fail-closed refusal", err)
	}
	if err := BackupDownload(context.Background(), auth, "b", "o", "out", "ns"); err == nil || !strings.Contains(err.Error(), "unsupported cloud provider") {
		t.Errorf("BackupDownload = %v, want a fail-closed refusal", err)
	}
	if calls := shim.calls(t); len(calls) != 0 {
		t.Errorf("the refusal must not exec gcloud: %v", calls)
	}
}
