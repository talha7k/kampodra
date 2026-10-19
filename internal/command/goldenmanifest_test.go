package command

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeArtifact makes a deterministic fake golden image (the content is
// the test's control over the digest).
func writeArtifact(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "image.qcow2")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEmitGoldenManifestDigestsRealBytes(t *testing.T) {
	p := writeArtifact(t, "golden-bytes-v1")
	m, enc, err := EmitGoldenManifest(p, "ubuntu-24.04-minimal", "https://cloud-images.ubuntu.com/x", "kampodra-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Digest) != 64 || m.SizeBytes != int64(len("golden-bytes-v1")) {
		t.Fatalf("manifest digest/size wrong: %v %+v", err, m)
	}
	// the digest MUST match a direct computation — the manifest always
	// records the real bytes' digest, never a caller's claim
	d, _, err := DigestArtifact(p)
	if err != nil || d != m.Digest {
		t.Fatalf("digest mismatch: %v", err)
	}
	var back GoldenManifest
	if err := json.Unmarshal([]byte(enc), &back); err != nil || back.Name != m.Name {
		t.Fatalf("emitted JSON does not round-trip: %v", err)
	}
}

func TestGoldenImageDirPinsDigestNeverLatest(t *testing.T) {
	at := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	dir := GoldenImageDir("ubuntu-24.04-minimal", strings.Repeat("ab", 32), at)
	if !strings.HasPrefix(dir, "golden-image/") {
		t.Fatalf("dir must live under the golden-image prefix: %s", dir)
	}
	if !strings.Contains(dir, "abababab") {
		t.Fatalf("the digest head must be IN the path (overwrites structurally impossible): %s", dir)
	}
	if strings.Contains(strings.ToLower(dir), "latest") {
		t.Fatal("the layout must never encode 'latest'")
	}
}

func TestVerifyGoldenArtifactDetectsSwap(t *testing.T) {
	p := writeArtifact(t, "golden-bytes-v1")
	m, _, err := EmitGoldenManifest(p, "n", "s", "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyGoldenArtifact(p, m); err != nil {
		t.Fatalf("the original bytes must verify: %v", err)
	}
	// swap the bytes behind the manifest's back — verify MUST die
	p2 := writeArtifact(t, "TAMPERED")
	if err := VerifyGoldenArtifact(p2, m); err == nil {
		t.Fatal("tampered bytes must fail verification")
	} else if !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("the failure must name the digest: %v", err)
	}
}

func TestParseGoldenManifestFailClosed(t *testing.T) {
	if _, err := ParseGoldenManifest([]byte(`{"name":"x"}`)); err == nil {
		t.Fatal("missing digest must fail")
	}
	if _, err := ParseGoldenManifest([]byte(`{"name":"x","digest":"zzzz"}`)); err == nil {
		t.Fatal("non-hex digest must fail")
	}
	good, _, _ := EmitGoldenManifest(writeArtifact(t, "b"), "n", "s", "t")
	enc, _ := json.Marshal(good)
	if _, err := ParseGoldenManifest(enc); err != nil {
		t.Fatalf("a valid manifest must parse: %v", err)
	}
}

func TestReadGoldenManifestSidecar(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadGoldenManifest(dir); err == nil {
		t.Fatal("a missing sidecar must fail (fail-closed verify)")
	}
	m, enc, _ := EmitGoldenManifest(writeArtifact(t, "sidecar"), "n", "s", "t")
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), []byte(enc), 0o644); err != nil {
		t.Fatal(err)
	}
	back, err := ReadGoldenManifest(dir)
	if err != nil || back.Digest != m.Digest {
		t.Fatalf("sidecar round-trip failed: %v", err)
	}
}
