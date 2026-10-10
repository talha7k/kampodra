package project

import (
	"strings"
	"testing"
)

// The "binary" block's per-kind contract: a wrong attempt must fail at
// PARSE time (a committed config typo errors, never resolves to a
// silently broken fast path), with the error naming the exact key.

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
		{"unknown kind", `"x":{"kind":"deno","dir":"/data/app","buildDir":"a","entry":"e","imagePath":"/i"}`, `unknown kind "deno" (valid: go, rust, node)`},
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
	if got := goBin.RunExec(); got != "/data/app/esellar-api-go" {
		t.Errorf("go RunExec = %q", got)
	}
	if got := nodeBin.RunExec(); got != "tsx /data/app/src/serve-node.ts" {
		t.Errorf("node RunExec = %q", got)
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
