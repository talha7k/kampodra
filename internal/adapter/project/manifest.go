// The repo-level project manifest: kampodra.json, committed per-project
// (the vercel.json pattern). Discovered upward from the working directory
// like package.json — the NEAREST file wins. Schema = the project Config
// fields (the same JSON keys as the profile "project" block, camelCase).
//
// It carries project NAMING only: hosts, ssh keys, and any secret material
// stay in ~/.kampodra profiles (0600) — never in a committed file. A
// malformed manifest FAILS CLOSED (it is a committed config: a typo must
// error, not silently resolve to defaults), unlike the profile block which
// fails open.
package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ManifestFileName is the repo-level project manifest's file name.
const ManifestFileName = "kampodra.json"

// Manifest is one discovered repo manifest: where it was found and the
// (validated) override fields it carries. The manifest's JSON schema is the
// ProjectConfig field set under its OWN keys — camelCase as documented,
// `envFile` (the profile block's legacy `envFilePath` key is NOT valid
// here; the committed file uses the clean name).
type Manifest struct {
	Path   string
	Fields ManifestFields
}

// ManifestFields is the kampodra.json schema: every ProjectConfig field,
// all optional (empty = "not overridden"). Keys are camelCase; unknown keys
// fail closed at parse time.
type ManifestFields struct {
	Container       string   `json:"container,omitempty"`
	ShadowSuffix    string   `json:"shadowSuffix,omitempty"`
	EnvFile         string   `json:"envFile,omitempty"`
	DataDir         string   `json:"dataDir,omitempty"`
	Bucket          string   `json:"bucket,omitempty"`
	ObjectPrefix    string   `json:"objectPrefix,omitempty"`
	HealthPath      string   `json:"healthPath,omitempty"`
	ProxyHost       string   `json:"proxyHost,omitempty"`
	Services        []string `json:"services,omitempty"`
	ImagePrefix     string   `json:"imagePrefix,omitempty"`
	Port            string   `json:"port,omitempty"`
	Network         string   `json:"network,omitempty"`
	ShadowProbePort string   `json:"shadowProbePort,omitempty"`
	DeployedShaFile string   `json:"deployedShaFile,omitempty"`
	EnvClearKeys    []string `json:"envClearKeys,omitempty"`
	Dockerfile      string   `json:"dockerfile,omitempty"`
}

// ParseManifest parses raw manifest bytes strictly: malformed JSON and
// unknown keys FAIL CLOSED (error names the file and the offending
// problem). An empty object is valid (no overrides).
func ParseManifest(path string, raw []byte) (Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fields ManifestFields
	if err := dec.Decode(&fields); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return Manifest{Path: path, Fields: fields}, nil
}

// DiscoverManifest walks up from startDir (toward the filesystem root)
// looking for kampodra.json; the NEAREST one wins. (zero Manifest, nil) is
// returned when none exists — manifest-less runs are the norm. An existing
// but unreadable/malformed manifest fails closed.
func DiscoverManifest(startDir string) (Manifest, error) {
	if startDir == "" {
		return Manifest{}, nil
	}
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return Manifest{}, nil // unreachable cwd — degrade to no manifest
	}
	for {
		path := filepath.Join(dir, ManifestFileName)
		if _, err := os.Stat(path); err == nil {
			raw, err := os.ReadFile(path)
			if err != nil {
				return Manifest{}, fmt.Errorf("%s: %w", path, err)
			}
			return ParseManifest(path, raw)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Manifest{}, nil // filesystem root — none found
		}
		dir = parent
	}
}
