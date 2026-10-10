package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The repo-level project manifest (kampodra.json): committed per-project,
// discovered upward from the working directory like package.json, and
// parsed STRICTLY — a committed config typo must fail closed, never
// silently no-op. Hosts and keys NEVER live here (profiles own those).

func writeManifest(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, ManifestFileName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverManifestFindsNearest(t *testing.T) {
	repo := t.TempDir()
	nearest := filepath.Join(repo, "apps", "api")
	if err := os.MkdirAll(nearest, 0o755); err != nil {
		t.Fatal(err)
	}
	// A manifest at the repo root AND a nearer one in apps/api — the walk
	// upward from apps/api/packages/x must find the NEAREST first.
	writeManifest(t, repo, `{"container": "root-api"}`)
	writeManifest(t, nearest, `{"container": "api-container"}`)
	deep := filepath.Join(nearest, "packages", "x")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	mf, err := DiscoverManifest(deep)
	if err != nil {
		t.Fatalf("DiscoverManifest: %v", err)
	}
	if mf.Path != filepath.Join(nearest, ManifestFileName) {
		t.Errorf("manifest path = %q, want the nearest one (%s)", mf.Path, nearest)
	}
	if mf.Fields.Container != "api-container" {
		t.Errorf("container = %q, want the nearest manifest's value", mf.Fields.Container)
	}
}

func TestDiscoverManifestWalksUpToRepoRoot(t *testing.T) {
	repo := t.TempDir()
	writeManifest(t, repo, `{"dataDir": "/srv/tenants"}`)
	deep := filepath.Join(repo, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	mf, err := DiscoverManifest(deep)
	if err != nil {
		t.Fatalf("DiscoverManifest: %v", err)
	}
	if mf.Path != filepath.Join(repo, ManifestFileName) {
		t.Errorf("manifest path = %q, want %s", mf.Path, filepath.Join(repo, ManifestFileName))
	}
	if mf.Fields.DataDir != "/srv/tenants" {
		t.Errorf("dataDir = %q, want /srv/tenants", mf.Fields.DataDir)
	}
}

func TestDiscoverManifestNoneFound(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	mf, err := DiscoverManifest(nested)
	if err != nil {
		t.Fatalf("no manifest is NOT an error, got: %v", err)
	}
	if mf.Path != "" {
		t.Errorf("path = %q, want empty when no manifest exists", mf.Path)
	}
}

func TestDiscoverManifestEmptyStartDirIsNone(t *testing.T) {
	mf, err := DiscoverManifest("")
	if err != nil {
		t.Fatalf("empty start dir must degrade to no-manifest, got: %v", err)
	}
	if mf.Path != "" {
		t.Errorf("path = %q, want empty", mf.Path)
	}
}

func TestDiscoverManifestMalformedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"container": "oops",`)

	_, err := DiscoverManifest(dir)
	if err == nil {
		t.Fatal("a malformed manifest must FAIL CLOSED, not resolve to none")
	}
	if !strings.Contains(err.Error(), ManifestFileName) {
		t.Errorf("error = %v, want it to name the offending file", err)
	}
}

func TestParseManifestFullSchema(t *testing.T) {
	raw := `{
	  "container": "my-api",
	  "shadowSuffix": "-canary",
	  "envFile": "/etc/my/env",
	  "dataDir": "/srv/tenants",
	  "bucket": "my-backups",
	  "objectPrefix": "dbs",
	  "healthPath": "/healthz",
	  "proxyHost": "app.my.example.com",
	  "services": ["my-api", "kamal-proxy"],
	  "imagePrefix": "127.0.0.1:5000/my-api",
	  "dockerfile": "deploy/Containerfile",
	  "deployedShaFile": "/etc/my/deployed-sha",
	  "envClearKeys": ["NODE_ENV", "PORT"]
	}`
	mf, err := ParseManifest(filepath.Join("/repo", ManifestFileName), []byte(raw))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if mf.Path != filepath.Join("/repo", ManifestFileName) {
		t.Errorf("path = %q", mf.Path)
	}
	f := mf.Fields
	if f.Container != "my-api" || f.ShadowSuffix != "-canary" || f.EnvFile != "/etc/my/env" ||
		f.DataDir != "/srv/tenants" || f.Bucket != "my-backups" || f.ObjectPrefix != "dbs" ||
		f.HealthPath != "/healthz" || f.ProxyHost != "app.my.example.com" || f.ImagePrefix != "127.0.0.1:5000/my-api" ||
		f.Dockerfile != "deploy/Containerfile" || f.DeployedShaFile != "/etc/my/deployed-sha" {
		t.Errorf("scalar fields did not parse: %+v", f)
	}
	if len(f.Services) != 2 || f.Services[0] != "my-api" || f.Services[1] != "kamal-proxy" {
		t.Errorf("services = %v", f.Services)
	}
	if len(f.EnvClearKeys) != 2 || f.EnvClearKeys[1] != "PORT" {
		t.Errorf("envClearKeys = %v", f.EnvClearKeys)
	}
}

func TestParseManifestMalformedJSONFailsClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"truncated":  `{"container": "x"`,
		"not-object": `["container"]`,
		"empty":      "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseManifest("/repo/kampodra.json", []byte(raw))
			if err == nil {
				t.Fatalf("malformed manifest (%s) must fail closed", name)
			}
			if !strings.Contains(err.Error(), "/repo/kampodra.json") {
				t.Errorf("error = %v, want it to name the file", err)
			}
		})
	}
}

func TestParseManifestUnknownKeyFailsClosed(t *testing.T) {
	// The manifest's schema is ITS OWN: a legacy profile-block key
	// ("envFilePath" — the manifest says "envFile") pasted into a committed
	// kampodra.json must ERROR — a silent no-op would resolve the run to
	// defaults while the operator believes the override is live.
	for _, key := range []string{"envFilePath", "container_name", "secrets"} {
		_, err := ParseManifest("/repo/kampodra.json", []byte(`{"`+key+`": "/etc/x/env"}`))
		if err == nil {
			t.Errorf("unknown key %q must fail closed", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error = %v, want it to name the unknown key %q", err, key)
		}
	}
}

func TestParseManifestEmptyObjectIsValid(t *testing.T) {
	mf, err := ParseManifest("/repo/kampodra.json", []byte(`{}`))
	if err != nil {
		t.Fatalf("empty manifest object: %v", err)
	}
	if mf.Fields.Container != "" {
		t.Errorf("container = %q, want empty (no overrides)", mf.Fields.Container)
	}
}

// TestManifestKeyHintMatchesStruct keeps the unknown-key error hint in
// lockstep with the ManifestFields schema (a new field without a hint
// update would tell users the wrong key list).
func TestManifestKeyHintMatchesStruct(t *testing.T) {
	typ := reflect.TypeOf(ManifestFields{})
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("field %s has no json tag", typ.Field(i).Name)
		}
		if strings.HasPrefix(name, "$") {
			continue // metadata keys ($schema) are not project keys
		}
		keys = append(keys, name)
	}
	want := strings.Join(keys, ", ")
	if manifestKeyHint != "container, pairInstancePrefix, shadowSuffix, envFile, dataDir, bucket, "+
		"objectPrefix, healthPath, proxyHost, services, imagePrefix, port, "+
		"network, shadowProbePort, deployedShaFile, envClearKeys, dockerfile, "+
		"migrateScript, images, binary" {
		t.Fatalf("manifestKeyHint const drifted from its own formatting")
	}
	if !strings.Contains(manifestKeyHint, want) || len(strings.Split(manifestKeyHint, ", ")) != len(keys) {
		t.Errorf("manifestKeyHint out of sync with ManifestFields:\n hint: %s\n want: %s", manifestKeyHint, want)
	}
}

// TestManifestSchemaFileInLockstep keeps docs/kampodra.schema.json in sync
// with the ManifestFields struct: every struct field must appear in the
// schema with the right JSON type, and the schema may not carry properties
// the struct does not know (besides the $schema metadata key).
func TestManifestSchemaFileInLockstep(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "kampodra.schema.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Type  string `json:"type"`
			Items *struct {
				Type string `json:"type"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	typ := reflect.TypeOf(ManifestFields{})
	seen := map[string]bool{"$schema": true}
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("field %s has no json tag", typ.Field(i).Name)
		}
		prop, ok := schema.Properties[name]
		if !ok {
			t.Errorf("schema missing property %q (add it to docs/kampodra.schema.json)", name)
			continue
		}
		seen[name] = true
		for _, e := range schemaTypeErrors(name, typ.Field(i).Name, typ.Field(i).Type.Kind(), prop.Type, prop.Items) {
			t.Error(e)
		}
	}
	for name := range schema.Properties {
		if !seen[name] {
			t.Errorf("schema has property %q with no ManifestFields counterpart", name)
		}
	}
}

func TestParseManifestImagesSidecars(t *testing.T) {
	raw := `{
	  "images": {
	    "sidecars": [
	      {"name": "backup", "dockerfile": "deploy/backup.Dockerfile"},
	      {"name": "shipper", "dockerfile": "deploy/shipper.Containerfile"}
	    ]
	  }
	}`
	mf, err := ParseManifest(filepath.Join("/repo", ManifestFileName), []byte(raw))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if mf.Fields.Images == nil || len(mf.Fields.Images.Sidecars) != 2 {
		t.Fatalf("images.sidecars = %+v, want 2 sidecars", mf.Fields.Images)
	}
	if mf.Fields.Images.Sidecars[0].Name != "backup" || mf.Fields.Images.Sidecars[0].Dockerfile != "deploy/backup.Dockerfile" {
		t.Errorf("sidecar[0] = %+v", mf.Fields.Images.Sidecars[0])
	}
}

func TestParseManifestImagesAbsentIsNil(t *testing.T) {
	mf, err := ParseManifest("/repo/kampodra.json", []byte(`{}`))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if mf.Fields.Images != nil {
		t.Errorf("Images = %+v, want nil (no images block)", mf.Fields.Images)
	}
}

func TestParseManifestSidecarFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // error must contain
	}{
		{"empty name", `{"images":{"sidecars":[{"name":"","dockerfile":"d.Dockerfile"}]}}`, "name"},
		{"empty dockerfile", `{"images":{"sidecars":[{"name":"backup","dockerfile":""}]}}`, "dockerfile"},
		{"missing dockerfile", `{"images":{"sidecars":[{"name":"backup"}]}}`, "dockerfile"},
		{"bad name charset", `{"images":{"sidecars":[{"name":"Back Up!","dockerfile":"d.Dockerfile"}]}}`, "name"},
		{"duplicate names", `{"images":{"sidecars":[{"name":"backup","dockerfile":"a.Dockerfile"},{"name":"backup","dockerfile":"b.Dockerfile"}]}}`, "duplicate"},
		{"nested unknown key", `{"images":{"sidecars":[{"name":"backup","dockerfile":"d.Dockerfile","context":"."}]}}`, "context"},
	}
	for _, tc := range cases {
		_, err := ParseManifest("/repo/kampodra.json", []byte(tc.raw))
		if err == nil {
			t.Errorf("%s: must fail closed", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

// TestParseManifestPairInstancePrefix: the blue/green pair's instance
// display-name prefix is a valid manifest key (the 2026-10-09 live fire
// hit the legacy-name estate: instances predate the tool's
// container-derived naming). Unknown keys elsewhere still fail closed.
func TestParseManifestPairInstancePrefix(t *testing.T) {
	m, err := ParseManifest("/repo/kampodra.json", []byte(`{"pairInstancePrefix": "esellar"}`))
	if err != nil {
		t.Fatalf("pairInstancePrefix must parse: %v", err)
	}
	if m.Fields.PairInstancePrefix != "esellar" {
		t.Errorf("PairInstancePrefix = %q, want esellar", m.Fields.PairInstancePrefix)
	}
	if _, err := ParseManifest("/repo/kampodra.json", []byte(`{"pairInstancePrefix": 17}`)); err == nil {
		t.Error("non-string pairInstancePrefix must fail closed")
	}
}

// schemaTypeErrors checks one ManifestFields field against its schema
// property: the Go kind maps to the JSON type (string, array-of-string,
// object, or a keyed set described by additionalProperties).
func schemaTypeErrors(key, fieldName string, kind reflect.Kind, propType string, items *struct {
	Type string `json:"type"`
}) []string {
	switch kind {
	case reflect.String:
		if propType != "string" {
			return []string{fmt.Sprintf("schema type of %q = %q, want string", key, propType)}
		}
	case reflect.Slice:
		if propType != "array" || items == nil || items.Type != "string" {
			return []string{fmt.Sprintf("schema type of %q must be array of string", key)}
		}
	case reflect.Pointer, reflect.Map:
		// A keyed set (e.g. binary blocks by name): an object whose
		// members are described by additionalProperties.
		if propType != "object" {
			return []string{fmt.Sprintf("schema type of %q = %q, want object", key, propType)}
		}
	default:
		return []string{fmt.Sprintf("unexpected field kind %s on %s", kind, fieldName)}
	}
	return nil
}
