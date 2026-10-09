package cloud

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpectedDigest(t *testing.T) {
	cases := []struct {
		name, head, want string
	}{
		{
			name: "opc-meta-sha256 wins",
			head: `{"data":{"opc-meta-sha256":"abc123","opc-content-md5":"zzz"}}`,
			want: "sha256:abc123",
		},
		{
			name: "content-sha256 is the sha256 fallback",
			head: `{"data":{"content-sha256":"def456"}}`,
			want: "sha256:def456",
		},
		{
			name: "opc-content-md5 is the last resort",
			head: `{"data":{"opc-content-md5":"c78fq7YevMcFIZZ2F4h5dg=="}}`,
			want: "md5:c78fq7YevMcFIZZ2F4h5dg==",
		},
		{
			name: "no digest metadata means verification skipped",
			head: `{"data":{"etag":"x"}}`,
			want: "",
		},
		{name: "empty head", head: "", want: ""},
		{name: "garbage head", head: "not json", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpectedDigest(tc.head); got != tc.want {
				t.Errorf("ExpectedDigest() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseObjectList(t *testing.T) {
	fixture := `{"data":{"objects":[
		{"name":"db/tenants/acme/20261008T050000Z.db","size":"1048576","timeCreated":"2026-10-08T05:00:05.000Z"},
		{"name":"db/tenants/acme/20261009T050000Z.db","size":"2048","timeCreated":null}
	]}}`
	objs, err := ParseObjectList(fixture)
	if err != nil {
		t.Fatalf("ParseObjectList: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objects", len(objs))
	}
	if objs[0].Name != "db/tenants/acme/20261008T050000Z.db" || objs[0].Size != 1048576 {
		t.Errorf("objs[0] = %+v", objs[0])
	}
	if objs[1].TimeCreated != "-" {
		t.Errorf("missing timeCreated must render as -, got %q", objs[1].TimeCreated)
	}
	// No objects / empty payload reads as an empty list, not an error.
	if objs, err := ParseObjectList(`{"data":{}}`); err != nil || len(objs) != 0 {
		t.Errorf("empty list: objs=%v err=%v", objs, err)
	}
}

// The live `oci os object list` CLI wraps rows DIRECTLY in an array under
// .data (the raw-API {data:{objects:[]}} shape is the fixture above) and
// often omits timeCreated entirely — 2026-10-09 live fire against bucket
// esellar-libsql-backups returned exactly this shape.
func TestParseObjectListCLIShape(t *testing.T) {
	fixture := `{"data":[
		{"name":"tenants/20261009_040000.tgz","size":"111260252"},
		{"name":"tenants/20261008_194630.tgz","size":"107491215","timeCreated":null}
	]}`
	objs, err := ParseObjectList(fixture)
	if err != nil {
		t.Fatalf("ParseObjectList: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want 2", len(objs))
	}
	if objs[0].Name != "tenants/20261009_040000.tgz" || objs[0].Size != 111260252 {
		t.Errorf("objs[0] = %+v", objs[0])
	}
	if objs[0].TimeCreated != "-" || objs[1].TimeCreated != "-" {
		t.Errorf("absent/null timeCreated must render as -, got %q / %q", objs[0].TimeCreated, objs[1].TimeCreated)
	}
}

func TestIsAuthDenied(t *testing.T) {
	denied := []string{
		"ServiceError: NotAuthorizedOrNotFound",
		"NotAuthorized",
		"NotAuthenticatedOrAuthorized",
		"Authorization failed",
	}
	for _, s := range denied {
		if !IsAuthDenied(s) {
			t.Errorf("IsAuthDenied(%q) = false", s)
		}
	}
	clear := []string{"connection refused", "timeout", "404 Not Found (bucket missing)"}
	for _, s := range clear {
		if IsAuthDenied(s) {
			t.Errorf("IsAuthDenied(%q) = true (over-matched)", s)
		}
	}
}

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	content := []byte("kampodra backup integrity fixture\n")
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	shaSum := sha256.Sum256(content)
	got, err := HashFile(p, "sha256")
	if err != nil {
		t.Fatalf("HashFile sha256: %v", err)
	}
	if want := hex.EncodeToString(shaSum[:]); got != want {
		t.Errorf("sha256 = %q, want %q", got, want)
	}
	md5Sum := md5.Sum(content)
	got, err = HashFile(p, "md5")
	if err != nil {
		t.Fatalf("HashFile md5: %v", err)
	}
	if want := base64.StdEncoding.EncodeToString(md5Sum[:]); got != want {
		t.Errorf("md5 = %q, want %q (OCI reports base64)", got, want)
	}
	if _, err := HashFile(p, "crc32c"); err == nil {
		t.Error("unsupported algorithm must error, not silently pass")
	}
}

// The auth rule is structural: config-file (--profile) or instance principal
// ONLY. Every arg vector carries --profile; instance principal adds --auth.
func TestArgBuildersCarryProfile(t *testing.T) {
	vecs := [][]string{
		ObjectListArgs("bkt", "db/", "myprof", false),
		ObjectHeadArgs("bkt", "db/x.db", "myprof", false),
		ObjectGetArgs("bkt", "db/x.db", "/tmp/out", "myprof", false),
	}
	for i, v := range vecs {
		if !contains(v, "--profile") {
			t.Errorf("vec %d missing --profile: %v", i, v)
		}
	}
	ip := ObjectListArgs("bkt", "", "myprof", true)
	if !contains(ip, "--auth") || !contains(ip, "instance_principal") {
		t.Errorf("instance-principal auth args missing: %v", ip)
	}
	noIP := ObjectListArgs("bkt", "", "myprof", false)
	for _, a := range noIP {
		if a == "instance_principal" {
			t.Errorf("--auth leaked into config-file auth vector: %v", noIP)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RunOCI execs the oci CLI; the adapter never invents credential flags.
func TestRunOCIUsesLookPathAndCapturesStderr(t *testing.T) {
	dir := t.TempDir()
	shim := "#!/bin/sh\nif [ \"$1\" = fail ]; then echo 'boom: NotAuthorized' >&2; exit 1; fi\necho ok\n"
	if err := os.WriteFile(filepath.Join(dir, "oci"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	out, err := RunOCI(context.Background(), []string{"ping"})
	if err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("RunOCI = %q, %v", out, err)
	}
	if _, err := RunOCI(context.Background(), []string{"fail"}); err == nil || !strings.Contains(err.Error(), "NotAuthorized") {
		t.Errorf("RunOCI error = %v, want stderr attached", err)
	}
}
