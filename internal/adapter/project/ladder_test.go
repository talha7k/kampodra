package project

import (
	"strings"
	"testing"
)

// The FULL resolution ladder with provenance:
//
//	flags > KAMPODRA_* env > profile "project" block > kampodra.json > LoadDefault()
//
// Every layer in one matrix — each field's winning source must be the
// HIGHEST layer that provides a value.

func lookupOf(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func TestLadderPrecedenceMatrix(t *testing.T) {
	manifest := &Manifest{
		Path: "/repo/kampodra.json",
		Fields: ManifestFields{
			Container:    "mf-container",
			DataDir:      "/mf/data",
			HealthPath:   "/mf/health",
			ImagePrefix:  "mf-prefix",
			EnvFile:      "/mf/env",
			ProxyHost:    "mf.example.com",
			Dockerfile:   "mf/Containerfile",
			Services:     []string{"mf-a", "mf-b"},
			EnvClearKeys: []string{"MF_KEY"},
		},
	}
	profileRaw := []byte(`{
	  "container": "pf-container",
	  "dataDir": "/pf/data",
	  "healthPath": "/pf/health",
	  "imagePrefix": "pf-prefix",
	  "services": ["pf-a"]
	}`)

	tests := []struct {
		name    string
		flags   map[string]string
		env     map[string]string
		key     string // the field's JSON key
		want    string
		wantSrc Source
	}{
		{
			name:    "default survives when no layer provides the field",
			key:     "bucket",
			want:    LoadDefault().Bucket,
			wantSrc: SourceDefault,
		},
		{
			name:    "manifest beats default",
			key:     "envFile",
			want:    "/mf/env",
			wantSrc: SourceRepoFile,
		},
		{
			name:    "profile beats manifest",
			key:     "imagePrefix",
			want:    "pf-prefix",
			wantSrc: SourceProfile,
		},
		{
			name:    "env beats profile",
			key:     "healthPath",
			env:     map[string]string{"KAMPODRA_HEALTH_PATH": "/env/health"},
			want:    "/env/health",
			wantSrc: SourceEnv,
		},
		{
			name:    "flag beats env",
			key:     "container",
			env:     map[string]string{"KAMPODRA_CONTAINER": "env-container"},
			flags:   map[string]string{"container": "flag-container"},
			want:    "flag-container",
			wantSrc: SourceFlag,
		},
		{
			name:    "flag beats every layer",
			key:     "dataDir",
			env:     map[string]string{"KAMPODRA_DATA_DIR": "/env/data"},
			flags:   map[string]string{"dataDir": "/flag/data"},
			want:    "/flag/data",
			wantSrc: SourceFlag,
		},
		{
			name:    "empty flag value falls through to env",
			key:     "container",
			env:     map[string]string{"KAMPODRA_CONTAINER": "env-container"},
			flags:   map[string]string{"container": ""},
			want:    "env-container",
			wantSrc: SourceEnv,
		},
		{
			name:    "manifest list field (envClearKeys) beats default",
			key:     "envClearKeys",
			want:    "MF_KEY",
			wantSrc: SourceRepoFile,
		},
		{
			name:    "profile list field beats manifest list field",
			key:     "services",
			want:    "pf-a",
			wantSrc: SourceProfile,
		},
		{
			name:    "env list field beats profile list field",
			key:     "services",
			env:     map[string]string{"KAMPODRA_SERVICES": "env-svc"},
			want:    "env-svc",
			wantSrc: SourceEnv,
		},
		{
			name:    "manifest envFile key maps to the env file path",
			key:     "envFile",
			want:    "/mf/env",
			wantSrc: SourceRepoFile,
		},
		{
			name:    "manifest proxyHost beats default",
			key:     "proxyHost",
			want:    "mf.example.com",
			wantSrc: SourceRepoFile,
		},
		{
			name:    "manifest dockerfile beats default",
			key:     "dockerfile",
			want:    "mf/Containerfile",
			wantSrc: SourceRepoFile,
		},
		{
			name:    "manifest envClearKeys beat default",
			key:     "envClearKeys",
			want:    "MF_KEY",
			wantSrc: SourceRepoFile,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, traces := ResolveTraced(tc.flags, lookupOf(tc.env), profileRaw, manifest)
			var view FieldView
			for _, v := range FieldViews(cfg, traces) {
				if v.Key == tc.key {
					view = v
					break
				}
			}
			if view.Value != tc.want {
				t.Errorf("%s = %q, want %q", tc.key, view.Value, tc.want)
			}
			if view.Trace.Source != tc.wantSrc {
				t.Errorf("%s source = %q, want %q", tc.key, view.Trace.Source, tc.wantSrc)
			}
		})
	}
}

func TestTracedOrigins(t *testing.T) {
	manifest := &Manifest{Path: "/repo/kampodra.json", Fields: ManifestFields{DataDir: "/mf/data"}}
	cfg, traces := ResolveTraced(nil, lookupOf(map[string]string{
		"KAMPODRA_CONTAINER": "env-container",
	}), []byte(`{"bucket": "pf-bucket"}`), manifest)

	byKey := map[string]FieldTrace{}
	for _, v := range FieldViews(cfg, traces) {
		byKey[v.Key] = v.Trace
	}
	if tr := byKey["container"]; tr.Source != SourceEnv || tr.Origin != "KAMPODRA_CONTAINER" {
		t.Errorf("container trace = %+v, want env origin KAMPODRA_CONTAINER", tr)
	}
	if tr := byKey["bucket"]; tr.Source != SourceProfile || tr.Origin != "" {
		t.Errorf("bucket trace = %+v, want profile with empty origin", tr)
	}
	if tr := byKey["dataDir"]; tr.Source != SourceRepoFile || tr.Origin != "/repo/kampodra.json" {
		t.Errorf("dataDir trace = %+v, want repo-file origin naming the manifest", tr)
	}
	if tr := byKey["network"]; tr.Source != SourceDefault || tr.Origin != "" {
		t.Errorf("network trace = %+v, want default with empty origin", tr)
	}
}

func TestResolveTracedNoLayersGivesAllDefaults(t *testing.T) {
	cfg, traces := ResolveTraced(nil, nil, nil, nil)
	for _, v := range FieldViews(cfg, traces) {
		if v.Trace.Source != SourceDefault {
			t.Errorf("%s source = %q, want default (nothing set)", v.Key, v.Trace.Source)
		}
		if v.Value == "" && v.Key != "" {
			t.Errorf("%s rendered empty — every default must render", v.Key)
		}
	}
	if len(traces) != 17 {
		t.Errorf("traces = %d rows, want 17 (every Config field)", len(traces))
	}
}

func TestTracesCoverEveryConfigField(t *testing.T) {
	// The catalog must stay complete: every Config field has a binding row,
	// or config print silently drops fields when Config grows.
	_, traces := ResolveTraced(nil, nil, nil, nil)
	seen := map[string]bool{}
	for _, tr := range traces {
		seen[tr.Field] = true
	}
	for _, key := range []string{
		"container", "shadowSuffix", "envFile", "dataDir", "bucket", "objectPrefix",
		"healthPath", "proxyHost", "services", "imagePrefix", "port", "network",
		"shadowProbePort", "deployedShaFile", "envClearKeys", "dockerfile",
		"migrateScript",
	} {
		if !seen[key] {
			t.Errorf("field catalog is missing %q — config print would drop it", key)
		}
	}
}

func TestDockerfileDefaultAndEnvOverride(t *testing.T) {
	if LoadDefault().Dockerfile != "Dockerfile" {
		t.Errorf("Dockerfile default = %q, want Dockerfile", LoadDefault().Dockerfile)
	}
	cfg := Resolve(nil, nil, lookupOf(map[string]string{"KAMPODRA_DOCKERFILE": "deploy/Containerfile"}))
	if cfg.Dockerfile != "deploy/Containerfile" {
		t.Errorf("Dockerfile = %q, want the env override", cfg.Dockerfile)
	}
}

func TestManifestLayerInResolve(t *testing.T) {
	// Resolve (the compact ladder entry) honors the manifest layer too.
	mf := &Manifest{Path: "/repo/kampodra.json", Fields: ManifestFields{Container: "mf-container", ShadowSuffix: "-mf"}}
	cfg := Resolve(mf, nil, nil)
	if cfg.Container != "mf-container" || cfg.ShadowSuffix != "-mf" {
		t.Errorf("Resolve with manifest = %+v", cfg)
	}
	// profile beats manifest
	cfg = Resolve(mf, []byte(`{"container": "pf-container"}`), nil)
	if cfg.Container != "pf-container" || cfg.ShadowSuffix != "-mf" {
		t.Errorf("profile should beat the manifest per-field: %+v", cfg)
	}
}

func TestLoadDefaultDockerfileNamed(t *testing.T) {
	if strings.TrimSpace(LoadDefault().Dockerfile) == "" {
		t.Fatal("Dockerfile default is empty — every field must carry a named default")
	}
}
