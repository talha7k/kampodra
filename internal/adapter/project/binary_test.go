package project

import (
	"strings"
	"testing"
)

// The "binary" block's per-kind contract: a wrong attempt must fail at
// PARSE time (a committed config typo errors, never resolves to a
// silently broken fast path), with the error naming the exact key.

// A typical repo: the exec-kind backend AND the static frontend side by
// side (two mounts on one unit, one exec override pinned to the backend).
const binaryStaticManifest = `"api-go":{"kind":"go","dir":"/data/app","buildDir":"apps/api-go","target":"./cmd/server","entry":"esellar-api-go","imagePath":"/usr/local/bin/esellar-api-go"},` +
	`"web-dist":{"kind":"static","dir":"/data/web","buildDir":"apps/web","artifact":"dist","imagePath":"/app/apps/web/dist","build":"pnpm build --filter web","env":"WEB_DIST_DIR"}`

func parseBinary(t *testing.T, blocks string) (Manifest, error) {
	t.Helper()
	return ParseManifest("kampodra.json", []byte(`{"binary":{`+blocks+`}}`))
}

func TestBinaryBlocksParsePerKind(t *testing.T) {
	cases := map[string]string{
		"go":                    `"api-go":{"kind":"go","dir":"/data/app","buildDir":"apps/api-go","target":"./cmd/server","entry":"esellar-api-go","imagePath":"/usr/local/bin/esellar-api-go"}`,
		"rust":                  `"api-rs":{"kind":"rust","dir":"/data/app","buildDir":"crates/api","target":"esellar-api","artifact":"target/release/esellar-api","entry":"esellar-api","imagePath":"/usr/local/bin/esellar-api"}`,
		"rust default artifact": `"api-rs":{"kind":"rust","dir":"/data/app","buildDir":"crates/api","target":"esellar-api","entry":"esellar-api","imagePath":"/usr/local/bin/esellar-api"}`,
		"node":                  `"api-ts":{"kind":"node","dir":"/data/app","buildDir":"apps/api","entry":"src/serve-node.ts","exec":"tsx","imagePath":"/app/apps/api","depsPath":"/app/node_modules"}`,
		"node custom manifest":  `"api-ts":{"kind":"node","dir":"/data/app","buildDir":"apps/api","entry":"src/serve-node.ts","exec":"node","imagePath":"/app/apps/api","depsPath":"/app/node_modules","depsManifest":["apps/api/package.json","pnpm-lock.yaml"]}`,
		"node compile step":     `"api-ts":{"kind":"node","dir":"/data/app","buildDir":"apps/api","artifact":"dist","entry":"dist/index.js","exec":"node","imagePath":"/app/apps/api/dist","depsPath":"/app/node_modules"}`,
	}
	for name, blocks := range cases {
		t.Run(name, func(t *testing.T) {
			m, err := parseBinary(t, blocks)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(m.Fields.Binary) != 1 {
				t.Fatalf("want 1 block, got %d", len(m.Fields.Binary))
			}
		})
	}
}

func TestBinaryBlockValidationErrors(t *testing.T) {
	cases := []struct{ name, blocks, want string }{
		{"unknown kind", `"x":{"kind":"deno","dir":"/data/app","buildDir":"a","entry":"e","imagePath":"/i"}`, `unknown kind "deno" (valid: go, rust, node, static)`},
		{"missing kind", `"x":{"dir":"/data/app","buildDir":"a","entry":"e","imagePath":"/i"}`, `"kind" is required`},
		{"missing dir", `"x":{"kind":"go","buildDir":"a","target":"./cmd/x","entry":"e","imagePath":"/i"}`, `"dir" is required`},
		{"missing entry", `"x":{"kind":"go","dir":"/data/app","buildDir":"a","target":"./cmd/x","imagePath":"/i"}`, `"entry" is required`},
		{"missing imagePath", `"x":{"kind":"go","dir":"/data/app","buildDir":"a","target":"./cmd/x","entry":"e"}`, `"imagePath" is required`},
		{"relative dir", `"x":{"kind":"go","dir":"data/app","buildDir":"a","target":"./cmd/x","entry":"e","imagePath":"/i"}`, `"dir" must be an absolute path`},
		{"absolute buildDir", `"x":{"kind":"go","dir":"/data/app","buildDir":"/a","target":"./cmd/x","entry":"e","imagePath":"/i"}`, `"buildDir" must be repo-relative`},
		{"entry escapes the repo", `"x":{"kind":"go","dir":"/data/app","buildDir":"a","target":"./cmd/x","entry":"../e","imagePath":"/i"}`, `"entry" must be a repo-relative path`},
		{"go without target", `"x":{"kind":"go","dir":"/data/app","buildDir":"a","entry":"e","imagePath":"/i"}`, `"target" is required for kind "go"`},
		{"go with exec", `"x":{"kind":"go","dir":"/data/app","buildDir":"a","target":"./cmd/x","entry":"e","exec":"bash","imagePath":"/i"}`, `"exec" must be empty for kind "go"`},
		{"rust without target", `"x":{"kind":"rust","dir":"/data/app","buildDir":"a","entry":"e","imagePath":"/i"}`, `"target" is required for kind "rust"`},
		{"node without exec", `"x":{"kind":"node","dir":"/data/app","buildDir":"a","entry":"e","imagePath":"/i","depsPath":"/app/node_modules"}`, `"exec" is required for kind "node"`},
		{"node without depsPath", `"x":{"kind":"node","dir":"/data/app","buildDir":"a","entry":"e","exec":"tsx","imagePath":"/app/apps/api"}`, `"depsPath" is required for kind "node"`},
		{"depsManifest escapes the repo", `"x":{"kind":"node","dir":"/data/app","buildDir":"a","entry":"e","exec":"tsx","imagePath":"/app/apps/api","depsPath":"/app/node_modules","depsManifest":["/etc/passwd"]}`, `"depsManifest" entries must be repo-relative`},
		{"static without build", `"x":{"kind":"static","dir":"/data/web","buildDir":"a","artifact":"dist","imagePath":"/i"}`, `"build" is required for kind "static"`},
		{"static without artifact", `"x":{"kind":"static","dir":"/data/web","buildDir":"a","imagePath":"/i","build":"pnpm build"}`, `"artifact" is required for kind "static"`},
		{"static with exec", `"x":{"kind":"static","dir":"/data/web","buildDir":"a","artifact":"dist","imagePath":"/i","build":"pnpm build","exec":"tsx"}`, `"exec" must be empty for kind "static"`},
		{"static bad env name", `"x":{"kind":"static","dir":"/data/web","buildDir":"a","artifact":"dist","imagePath":"/i","build":"pnpm build","env":"web-dist"}`, `"env" must be an environment variable name`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBinary(t, tc.blocks)
			if err == nil {
				t.Fatalf("want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestBinaryDerivedSurfaces(t *testing.T) {
	goBin := ManifestBinary{Kind: BinaryKindGo, Dir: "/data/app", Entry: "esellar-api-go"}
	nodeBin := ManifestBinary{Kind: BinaryKindNode, Dir: "/data/app", Entry: "src/serve-node.ts", Exec: "tsx",
		BuildDir: "apps/api", DepsPath: "/app/node_modules"}
	rustBin := ManifestBinary{Kind: BinaryKindRust, Target: "esellar-api", BuildDir: "crates/api"}

	if goBin.IsDirShape() || rustBin.IsDirShape() {
		t.Error("go/rust must be file-shape")
	}
	if !nodeBin.IsDirShape() {
		t.Error("node must be dir-shape")
	}
	if got := goBin.RunExec(); got != "--entrypoint /data/app/esellar-api-go" {
		t.Errorf("go RunExec = %q", got)
	}
	if got := goBin.EntrypointFlag(); got != "--entrypoint /data/app/esellar-api-go" {
		t.Errorf("go EntrypointFlag = %q", got)
	}
	if got := goBin.CmdOverride(); got != "" {
		t.Errorf("go must not render a post-image CMD override, got %q", got)
	}
	if got := nodeBin.EntrypointFlag(); got != "" {
		t.Errorf("node must not render a --entrypoint flag, got %q", got)
	}
	if got := nodeBin.CmdOverride(); got != "tsx /data/app/src/serve-node.ts" {
		t.Errorf("node CmdOverride = %q", got)
	}
	if got := goBin.MountArg(); got != "-v /data/app:/data/app" {
		t.Errorf("MountArg = %q", got)
	}
	if got := rustBin.RustTriple(); got != "aarch64-unknown-linux-musl" {
		t.Errorf("default rust triple = %q", got)
	}
	if got := rustBin.LocalArtifactDir(); got != "aarch64-unknown-linux-musl/release/esellar-api" {
		t.Errorf("default rust artifact dir = %q", got)
	}
	if got := nodeBin.DepsManifestFiles(); len(got) != 2 || got[0] != "apps/api/package.json" || got[1] != "pnpm-lock.yaml" {
		t.Errorf("default deps manifest = %v", got)
	}
	if got := (ManifestBinary{Kind: BinaryKindGo}).LocalArtifactDir(); got != "" {
		t.Errorf("go builds directly to staging, LocalArtifactDir = %q", got)
	}
}

// The static kind: a self-contained dist tree that swaps in WITHOUT a
// restart (the app serves the mount per-request), gated on its
// /build-id.txt freshness signal. It carries NO exec override — the
// exec-kind block keeps the unit's entrypoint.
func TestBinaryStaticKindSurfaces(t *testing.T) {
	staticBin := ManifestBinary{Kind: BinaryKindStatic, Dir: "/data/web", BuildDir: "apps/web",
		Artifact: "dist", ImagePath: "/app/apps/web/dist", Build: "pnpm build --filter web", Env: "WEB_DIST_DIR"}

	if !staticBin.IsDirShape() {
		t.Error("static must be dir-shape (tar + atomic dir swap)")
	}
	if got := staticBin.RunExec(); got != "" {
		t.Errorf("static must render NO exec override, RunExec = %q", got)
	}
	if got := staticBin.EntrypointFlag(); got != "" {
		t.Errorf("static must render no --entrypoint flag, got %q", got)
	}
	if got := staticBin.CmdOverride(); got != "" {
		t.Errorf("static must render no post-image CMD, got %q", got)
	}
	if got := staticBin.LocalArtifactDir(); got != "dist" {
		t.Errorf("static LocalArtifactDir = %q, want the artifact dir", got)
	}
	if got := staticBin.MountArg(); got != "-v /data/web:/data/web" {
		t.Errorf("static MountArg = %q", got)
	}
	if got := staticBin.StaticEnvArg(); got != "-e WEB_DIST_DIR=/data/web/current" {
		t.Errorf("static StaticEnvArg = %q", got)
	}
	if got := (ManifestBinary{Kind: BinaryKindStatic, Dir: "/data/web"}).StaticEnvArg(); got != "" {
		t.Errorf("static without env must render no -e, got %q", got)
	}
}

// An exec-kind block and a static block coexist in one manifest: each
// parses, keeps its own shape, and the static one never steals the exec.
func TestBinaryStaticCoexistsWithExecKind(t *testing.T) {
	m, err := parseBinary(t, binaryStaticManifest)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(m.Fields.Binary) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(m.Fields.Binary))
	}
	web := m.Fields.Binary["web-dist"]
	if web.Kind != BinaryKindStatic || web.Build == "" || web.Env != "WEB_DIST_DIR" {
		t.Errorf("static block parsed wrong: %+v", web)
	}
	api := m.Fields.Binary["api-go"]
	if api.EntrypointFlag() != "--entrypoint /data/app/esellar-api-go" {
		t.Errorf("the exec-kind block must keep its --entrypoint flag: %q", api.EntrypointFlag())
	}
	if web.EntrypointFlag() != "" {
		t.Errorf("the static block must not render an --entrypoint flag: %q", web.EntrypointFlag())
	}
}
