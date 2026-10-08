package project

import (
	"os"
	"path/filepath"
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
