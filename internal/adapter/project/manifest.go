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
	"regexp"
	"strings"
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
// fail closed at parse time. The optional "$schema" key (JSON Schema
// reference for editor tooling) is accepted and ignored — it is metadata,
// not a project field, and never enters the resolution ladder.
type ManifestFields struct {
	Schema             string          `json:"$schema,omitempty"`
	Container          string          `json:"container,omitempty"`
	PairInstancePrefix string          `json:"pairInstancePrefix,omitempty"`
	ShadowSuffix       string          `json:"shadowSuffix,omitempty"`
	EnvFile            string          `json:"envFile,omitempty"`
	DataDir            string          `json:"dataDir,omitempty"`
	Bucket             string          `json:"bucket,omitempty"`
	ObjectPrefix       string          `json:"objectPrefix,omitempty"`
	HealthPath         string          `json:"healthPath,omitempty"`
	ProxyHost          string          `json:"proxyHost,omitempty"`
	Services           []string        `json:"services,omitempty"`
	ImagePrefix        string          `json:"imagePrefix,omitempty"`
	Port               string          `json:"port,omitempty"`
	Network            string          `json:"network,omitempty"`
	ShadowProbePort    string          `json:"shadowProbePort,omitempty"`
	DeployedShaFile    string          `json:"deployedShaFile,omitempty"`
	EnvClearKeys       []string        `json:"envClearKeys,omitempty"`
	Dockerfile         string          `json:"dockerfile,omitempty"`
	MigrateScript      string          `json:"migrateScript,omitempty"`
	Images             *ManifestImages `json:"images,omitempty"`
}

// ManifestImages is the kampodra.json "images" block: secondary (sidecar)
// images deploy builds+streams+tags alongside the primary app image
// (backup daemons, shippers — anything the VM needs without a manual
// `podman build | save | load`). A repo property: declared once here, not
// per host profile.
type ManifestImages struct {
	Sidecars []ManifestSidecar `json:"sidecars,omitempty"`
}

// ManifestSidecar is one declared secondary image: its NAME (the image
// ref derives from the project ImagePrefix: <ImagePrefix>-<name>) and the
// repo-relative Dockerfile deploy builds it from.
type ManifestSidecar struct {
	Name       string `json:"name"`
	Dockerfile string `json:"dockerfile"`
}

// sidecarNameRe constrains a sidecar name to a safe image-tag fragment
// (it becomes `<ImagePrefix>-<name>` verbatim — no shell/registry
// surprises).
var sidecarNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// manifestKeyHint lists the valid kampodra.json keys for parse errors.
// Kept in lockstep with ManifestFields by TestManifestKeyHintMatchesStruct.
const manifestKeyHint = "container, pairInstancePrefix, shadowSuffix, envFile, dataDir, bucket, " +
	"objectPrefix, healthPath, proxyHost, services, imagePrefix, port, " +
	"network, shadowProbePort, deployedShaFile, envClearKeys, dockerfile, " +
	"migrateScript, images"

// ParseManifest parses raw manifest bytes strictly: malformed JSON and
// unknown keys FAIL CLOSED (error names the file and the offending
// problem; unknown keys also list the valid keys). An empty object is
// valid (no overrides).
func ParseManifest(path string, raw []byte) (Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var fields ManifestFields
	if err := dec.Decode(&fields); err != nil {
		if strings.Contains(err.Error(), "unknown field ") {
			return Manifest{}, fmt.Errorf("%s: %w\n  valid kampodra.json keys: %s", path, err, manifestKeyHint)
		}
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateSidecars(fields.Images); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return Manifest{Path: path, Fields: fields}, nil
}

// validateSidecars fails closed on a malformed images block: every sidecar
// needs a safe name and a dockerfile, and names must be unique (they
// become image ref fragments — a collision would silently overwrite).
func validateSidecars(images *ManifestImages) error {
	if images == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, sc := range images.Sidecars {
		if sc.Name == "" {
			return fmt.Errorf("images.sidecars: every sidecar needs a non-empty \"name\"")
		}
		if !sidecarNameRe.MatchString(sc.Name) {
			return fmt.Errorf("images.sidecars: sidecar name %q is invalid (must match %s — it becomes the <imagePrefix>-%s image ref)", sc.Name, sidecarNameRe, sc.Name)
		}
		if sc.Dockerfile == "" {
			return fmt.Errorf("images.sidecars: sidecar %q needs a non-empty \"dockerfile\" (repo-relative)", sc.Name)
		}
		if seen[sc.Name] {
			return fmt.Errorf("images.sidecars: duplicate sidecar name %q", sc.Name)
		}
		seen[sc.Name] = true
	}
	return nil
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
