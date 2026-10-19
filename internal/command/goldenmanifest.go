package command

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GoldenManifest is the provenance record that travels WITH a golden
// image artifact: what the image is, where it came from, and the exact
// digest that pins it. The G1 contract: launches pin DIGESTS, never
// "latest"; publish never overwrites an existing digest directory.
type GoldenManifest struct {
	Name       string    `json:"name"`        // e.g. ubuntu-24.04-minimal
	Digest     string    `json:"digest"`      // sha256 hex of the artifact
	SizeBytes  int64     `json:"size_bytes"`  // artifact size
	SourceURL  string    `json:"source_url"`  // where the artifact came from
	ImportedAt time.Time `json:"imported_at"` // when the publish ran
	CreatedBy  string    `json:"created_by"`  // kampodra version stamp
}

// ManifestFileName is the sidecar name inside every digest directory.
const ManifestFileName = "manifest.json"

// DigestArtifact computes the sha256 hex digest of a local file.
func DigestArtifact(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("cannot digest %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// GoldenImageDir is the Object Storage prefix layout:
// golden-image/<name>-<date>-<digest8>/ — the digest IN the path makes
// "latest" unresolvable and overwrites structurally impossible.
func GoldenImageDir(name string, digest string, at time.Time) string {
	return fmt.Sprintf("golden-image/%s-%s-%s", name, at.UTC().Format("20060102"), digest[:8])
}

// EmitGoldenManifest writes manifest.json + validates the artifact
// digest in one step: the manifest ALWAYS records the real digest of the
// real bytes, never a caller-supplied one.
func EmitGoldenManifest(artifactPath, name, sourceURL, createdBy string) (GoldenManifest, string, error) {
	digest, size, err := DigestArtifact(artifactPath)
	if err != nil {
		return GoldenManifest{}, "", err
	}
	m := GoldenManifest{
		Name:       name,
		Digest:     digest,
		SizeBytes:  size,
		SourceURL:  sourceURL,
		ImportedAt: time.Now().UTC(),
		CreatedBy:  createdBy,
	}
	enc, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, "", err
	}
	return m, string(enc) + "\n", nil
}

// VerifyGoldenArtifact re-digests the artifact and compares against the
// manifest — the verify command's core: bytes match provenance or die.
func VerifyGoldenArtifact(artifactPath string, m GoldenManifest) error {
	digest, size, err := DigestArtifact(artifactPath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(digest, m.Digest) {
		return fmt.Errorf("digest mismatch: artifact %s… want %s… — the bytes are not the manifest's image", digest[:12], m.Digest[:12])
	}
	if size != m.SizeBytes {
		return fmt.Errorf("size mismatch: %d bytes, manifest says %d", size, m.SizeBytes)
	}
	return nil
}

// ParseGoldenManifest loads a manifest sidecar (remote or local).
func ParseGoldenManifest(data []byte) (GoldenManifest, error) {
	var m GoldenManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("malformed manifest: %w", err)
	}
	if m.Name == "" || len(m.Digest) != 64 {
		return m, fmt.Errorf("manifest missing name or a sha256 digest")
	}
	if _, err := hex.DecodeString(m.Digest); err != nil {
		return m, fmt.Errorf("digest is not hex: %w", err)
	}
	return m, nil
}

// ReadGoldenManifest reads the sidecar next to an artifact.
func ReadGoldenManifest(dir string) (GoldenManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		return GoldenManifest{}, fmt.Errorf("no manifest sidecar in %s: %w", dir, err)
	}
	return ParseGoldenManifest(data)
}
