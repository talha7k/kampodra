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
	"sort"
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
	Schema             string                    `json:"$schema,omitempty"`
	Container          string                    `json:"container,omitempty"`
	PairInstancePrefix string                    `json:"pairInstancePrefix,omitempty"`
	ShadowSuffix       string                    `json:"shadowSuffix,omitempty"`
	EnvFile            string                    `json:"envFile,omitempty"`
	DataDir            string                    `json:"dataDir,omitempty"`
	Bucket             string                    `json:"bucket,omitempty"`
	ObjectPrefix       string                    `json:"objectPrefix,omitempty"`
	HealthPath         string                    `json:"healthPath,omitempty"`
	ProxyHost          string                    `json:"proxyHost,omitempty"`
	Services           []string                  `json:"services,omitempty"`
	ImagePrefix        string                    `json:"imagePrefix,omitempty"`
	Port               string                    `json:"port,omitempty"`
	Network            string                    `json:"network,omitempty"`
	ShadowProbePort    string                    `json:"shadowProbePort,omitempty"`
	DeployedShaFile    string                    `json:"deployedShaFile,omitempty"`
	EnvClearKeys       []string                  `json:"envClearKeys,omitempty"`
	Dockerfile         string                    `json:"dockerfile,omitempty"`
	MigrateScript      string                    `json:"migrateScript,omitempty"`
	Images             *ManifestImages           `json:"images,omitempty"`
	Binary             map[string]ManifestBinary `json:"binary,omitempty"`
}

// BinaryKinds are the artifact kinds `deploy --binary` supports. The kind
// selects the artifact SHAPE and therefore the build/transfer/exec
// semantics:
//
//	go/rust — a single native executable: local cross-build, scp the file,
//	          atomic swap, the container execs it directly.
//	node     — a source tree run through a runtime (tsx/node): no compile
//	           (or one), tar the tree, swap the directory, the container
//	           execs `<runtime> <dir>/<entry>`; deps stay baked in the
//	           image (depsPath symlink) — the mount never carries them.
const (
	BinaryKindGo   = "go"
	BinaryKindRust = "rust"
	BinaryKindNode = "node"

	// defaultRustTriple is the Alpine/arm64 static triple: the golden
	// images are musl-based, so the gnu triple's glibc dependency would
	// not resolve at runtime.
	defaultRustTriple = "aarch64-unknown-linux-musl"
)

// ManifestBinary is one declared binary artifact (kampodra.json "binary"
// block, keyed by name — a repo ships at most one per stack, e.g. the Go
// api AND the TS api during a cutover). `deploy --binary <name>` pushes
// one; a lone block needs no name. Every field is validated fail-closed at
// parse time (see validateBinaryBlock) — a typo must error, never resolve
// to a silently broken fast path.
type ManifestBinary struct {
	Kind         string   `json:"kind"`                   // go | rust | node
	Dir          string   `json:"dir"`                    // VM bind-mount dir (host path == in-container path)
	BuildDir     string   `json:"buildDir"`               // repo-relative local build cwd
	Target       string   `json:"target"`                 // go: package · rust: bin name · node: "" (source-run)
	Artifact     string   `json:"artifact"`               // artifact path under BuildDir ("" = kind default)
	Entry        string   `json:"entry"`                  // the exec'd file under Dir: go/rust the binary, node the entry script
	Exec         string   `json:"exec"`                   // "" = direct exec · node: the runtime (tsx/node)
	ImagePath    string   `json:"imagePath"`              // the artifact's path INSIDE the image (mount sync + parity)
	DepsPath     string   `json:"depsPath"`               // node: in-image deps dir — the mount symlinks node_modules at it
	DepsManifest []string `json:"depsManifest,omitempty"` // node: repo-relative dep-manifest files hashed for drift detection ("" = [<buildDir>/package.json, pnpm-lock.yaml])
	Triple       string   `json:"triple,omitempty"`       // rust: target triple ("" = defaultRustTriple)
}

// DepsManifestFiles is the effective dependency-manifest set: the files
// whose content pins the artifact's dependency closure. A node fast push
// hashes these and refuses on drift (the mount never carries deps — the
// image does); go/rust rebuild every push and have no such closure.
func (b ManifestBinary) DepsManifestFiles() []string {
	if len(b.DepsManifest) > 0 {
		return b.DepsManifest
	}
	return []string{b.BuildDir + "/package.json", "pnpm-lock.yaml"}
}

// IsDirShape reports whether the artifact is a directory tree (node) or a
// single executable file (go/rust).
func (b ManifestBinary) IsDirShape() bool { return b.Kind == BinaryKindNode }

// RustTriple is the effective cargo target triple.
func (b ManifestBinary) RustTriple() string {
	if b.Triple != "" {
		return b.Triple
	}
	return defaultRustTriple
}

// RunExec is the in-container exec override the app unit renders after
// the image ref: the binary itself (go/rust) or `<exec> <dir>/<entry>`
// (node).
func (b ManifestBinary) RunExec() string {
	if b.Kind == BinaryKindNode {
		return b.Exec + " " + b.Dir + "/" + b.Entry
	}
	return b.Dir + "/" + b.Entry
}

// MountArg is the podman run bind-mount argument for the artifact dir.
func (b ManifestBinary) MountArg() string { return "-v " + b.Dir + ":" + b.Dir }

// LocalArtifactDir is where the artifact lives locally after the build,
// relative to BuildDir ("" for go, whose build writes staging directly;
// rust: <triple>/release/<target>; node: Artifact, "" = BuildDir itself).
func (b ManifestBinary) LocalArtifactDir() string {
	switch b.Kind {
	case BinaryKindRust:
		if b.Artifact != "" {
			return b.Artifact
		}
		return filepath.Join(b.RustTriple(), "release", b.Target)
	case BinaryKindNode:
		return b.Artifact
	}
	return ""
}

// validateBinaryBlocks fails closed on a malformed binary block set:
// every entry is validated (validateBinaryBlock) and names must be safe
// (they become the `--binary <name>` selector).
func validateBinaryBlocks(blocks map[string]ManifestBinary) error {
	names := make([]string, 0, len(blocks))
	for name := range blocks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" {
			return fmt.Errorf("binary: a block needs a non-empty name (it becomes the --binary <name> selector)")
		}
		if err := validateBinaryBlock(name, blocks[name]); err != nil {
			return err
		}
	}
	return nil
}

// validateBinaryBlock enforces the per-kind contract with errors that
// name the exact key to fix (a wrong attempt must fail HERE, at parse
// time, not at the VM's health gate). Split by concern to keep each
// leg readable and gocognit-clean.
func validateBinaryBlock(name string, b ManifestBinary) error {
	if err := validateBinaryKind(name, b); err != nil {
		return err
	}
	if err := validateBinaryRequiredKeys(name, b); err != nil {
		return err
	}
	return validateBinaryPathShapes(name, b)
}

func validateBinaryKind(name string, b ManifestBinary) error {
	switch b.Kind {
	case "":
		return fmt.Errorf("binary.%s: \"kind\" is required (go, rust, or node)", name)
	case BinaryKindGo, BinaryKindRust, BinaryKindNode:
		return nil
	}
	return fmt.Errorf("binary.%s: unknown kind %q (valid: go, rust, node)", name, b.Kind)
}

func validateBinaryRequiredKeys(name string, b ManifestBinary) error {
	for _, kv := range [][2]string{{"dir", b.Dir}, {"buildDir", b.BuildDir}, {"entry", b.Entry}, {"imagePath", b.ImagePath}} {
		if kv[1] == "" {
			return fmt.Errorf("binary.%s: \"%s\" is required", name, kv[0])
		}
	}
	switch b.Kind {
	case BinaryKindGo, BinaryKindRust:
		if b.Target == "" {
			return fmt.Errorf("binary.%s: \"target\" is required for kind %q (go: the package, e.g. ./cmd/server; rust: the bin name)", name, b.Kind)
		}
		if b.Exec != "" {
			return fmt.Errorf("binary.%s: \"exec\" must be empty for kind %q (the artifact IS the executable)", name, b.Kind)
		}
	case BinaryKindNode:
		if b.Exec == "" {
			return fmt.Errorf("binary.%s: \"exec\" is required for kind \"node\" (the runtime: tsx or node)", name)
		}
		if b.DepsPath == "" {
			return fmt.Errorf("binary.%s: \"depsPath\" is required for kind \"node\" (the in-image node_modules the mount symlinks at — the mount never carries dependencies)", name)
		}
		if !filepath.IsAbs(b.DepsPath) {
			return fmt.Errorf("binary.%s: \"depsPath\" must be absolute (in-image), got %q", name, b.DepsPath)
		}
	}
	return nil
}

func validateBinaryPathShapes(name string, b ManifestBinary) error {
	if !filepath.IsAbs(b.Dir) {
		return fmt.Errorf("binary.%s: \"dir\" must be an absolute path (the VM bind mount), got %q", name, b.Dir)
	}
	if !filepath.IsAbs(b.ImagePath) {
		return fmt.Errorf("binary.%s: \"imagePath\" must be absolute (the artifact's path inside the image), got %q", name, b.ImagePath)
	}
	if filepath.IsAbs(b.BuildDir) {
		return fmt.Errorf("binary.%s: \"buildDir\" must be repo-relative, got %q", name, b.BuildDir)
	}
	for _, kv := range [][2]string{{"buildDir", b.BuildDir}, {"entry", b.Entry}, {"artifact", b.Artifact}} {
		if kv[1] != "" && !safeRepoRelPath(kv[1]) {
			return fmt.Errorf("binary.%s: \"%s\" must be a repo-relative path (no leading /, no ..), got %q", name, kv[0], kv[1])
		}
	}
	for _, dep := range b.DepsManifest {
		if !safeRepoRelPath(dep) {
			return fmt.Errorf("binary.%s: \"depsManifest\" entries must be repo-relative paths (no leading /, no ..), got %q", name, dep)
		}
	}
	return nil
}

// safeRepoRelPath rejects absolute paths and any parent-directory escape.
func safeRepoRelPath(p string) bool {
	if filepath.IsAbs(p) || p == "" {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
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
	"migrateScript, images, binary"

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
	if err := validateBinaryBlocks(fields.Binary); err != nil {
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
