// Package project owns the deployed project's shape: the ONE place the
// naming constants (container, env-file path, bucket, health path, image
// prefix, ...) may live. Resolution everywhere is
//
//	flags > KAMPODRA_* env > profile "project" block > kampodra.json (nearest repo root) > LoadDefault()
//
// Defaults are fine; unreachable constants are not — every consumer routes
// through a Config field, never a raw string literal (guarded by
// guard_test.go).
package project

import (
	"encoding/json"
	"strings"
)

// Config is the resolved project shape for one run.
type Config struct {
	Container       string   `json:"container"`       // the api container / service name
	ShadowSuffix    string   `json:"shadowSuffix"`    // blue/green shadow container suffix
	EnvFile         string   `json:"envFile"`         // remote env file (0600)
	DataDir         string   `json:"dataDir"`         // tenant/app data dir on the VM
	Bucket          string   `json:"bucket"`          // object-storage backup bucket
	ObjectPrefix    string   `json:"objectPrefix"`    // backup object prefix
	HealthPath      string   `json:"healthPath"`      // app health endpoint behind the proxy
	ProxyHost       string   `json:"proxyHost"`       // public TLS edge hostname
	Services        []string `json:"services"`        // the service roll call
	ImagePrefix     string   `json:"imagePrefix"`     // VM-local registry path for deploy images
	Port            string   `json:"port"`            // the container's published/listening port (health gate + proxy target)
	Network         string   `json:"network"`         // the podman network shared with kamal-proxy
	ShadowProbePort string   `json:"shadowProbePort"` // the shadow container's loopback-only probe port
	DeployedShaFile string   `json:"deployedShaFile"` // the VM's deployed-sha stamp (rollback's fallback resolution)
	EnvClearKeys    []string `json:"envClearKeys"`    // env keys OWNED by the init script — env from-schema drops them from varlock output
	Dockerfile      string   `json:"dockerfile"`      // the Containerfile/Dockerfile deploy builds (context = the repo root)
	MigrateScript   string   `json:"migrateScript"`   // the repo-relative db migrate script migrate runs on the VM
}

// LoadDefault returns today's values as NAMED DEFAULTS — the single file
// where this naming exists. Changing a default here changes every consumer
// at once; overriding per profile (config.json "project" block) or via
// KAMPODRA_* env stays available without code changes.
func LoadDefault() Config {
	return Config{
		Container:       "app",
		ShadowSuffix:    "-shadow",
		EnvFile:         "/etc/kampodra/env",
		DataDir:         "/data",
		Bucket:          "app-backups",
		ObjectPrefix:    "db",
		HealthPath:      "/up",
		ProxyHost:       "app.example.com",
		Services:        []string{"app", "kamal-proxy"},
		ImagePrefix:     "127.0.0.1:5000/app",
		Port:            "8080",
		Network:         "kamal",
		ShadowProbePort: "18080",
		DeployedShaFile: "/etc/kampodra/deployed-sha",
		EnvClearKeys:    []string{"NODE_ENV", "PORT"},
		Dockerfile:      "Dockerfile",
		MigrateScript:   "scripts/migrate-db.ts",
	}
}

// Overrides is the profile "project" block (config.json): every field
// optional, empty = "not overridden".
type Overrides struct {
	Container       string   `json:"container,omitempty"`
	ShadowSuffix    string   `json:"shadowSuffix,omitempty"`
	EnvFile         string   `json:"envFile,omitempty"` // remote env file (0600); legacy key "envFilePath" still accepted (UnmarshalJSON)
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
	MigrateScript   string   `json:"migrateScript,omitempty"`
}

// UnmarshalJSON accepts both env-file key spellings: `envFile` (canonical —
// the same key kampodra.json uses) and the profile block's legacy
// `envFilePath`, so existing config.json files keep working. When both are
// present the canonical `envFile` wins. Unknown keys stay ignored (the
// block fails open).
func (o *Overrides) UnmarshalJSON(data []byte) error {
	type plain Overrides
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		if _, canonical := raw["envFile"]; !canonical {
			if legacy, ok := raw["envFilePath"]; ok {
				var s string
				if json.Unmarshal(legacy, &s) == nil {
					p.EnvFile = s
				}
			}
		}
	}
	*o = Overrides(p)
	return nil
}

// SidecarImageRef is the VM-local image ref for a declared sidecar
// (kampodra.json images.sidecars): the project ImagePrefix + "-" + name
// (e.g. 127.0.0.1:5000/app-backup). Lives here so deploy's streaming and
// any future consumer can never drift from the naming.
func (c Config) SidecarImageRef(name string) string {
	return c.ImagePrefix + "-" + name
}

// Resolve layers the config, lowest layer first: LoadDefault, then the
// repo manifest (kampodra.json, nil = none), then KAMPODRA_* env, then the
// profile's raw "project" block (the command layer passes
// state.Profile.Project through untouched — this adapter owns its shape).
// A corrupt overrides block fails OPEN to manifest/env/defaults (the block
// is an override layer, not data); the manifest fails CLOSED upstream at
// discovery; a nil lookup means "no env".
func Resolve(manifest *Manifest, rawOverrides json.RawMessage, lookup func(string) (string, bool)) Config {
	cfg, _ := ResolveTraced(nil, lookup, rawOverrides, manifest)
	return cfg
}

// Source names the ladder layer a resolved field's value came from.
type Source string

const (
	SourceFlag     Source = "flag"      // an explicit per-invocation CLI flag
	SourceEnv      Source = "env"       // a KAMPODRA_* environment variable
	SourceProfile  Source = "profile"   // the profile's "project" block
	SourceRepoFile Source = "repo-file" // the repo-level kampodra.json manifest
	SourceDefault  Source = "default"   // the LoadDefault() named default
)

// FieldTrace is one field's provenance: which layer won and where
// exactly the value came from (env key, manifest path, ...).
type FieldTrace struct {
	Field  string // the manifest/profile JSON key ("container")
	Source Source
	Origin string // env var name / manifest path / flag name; "" = default
}

// fieldBinding pairs a JSON key with its Config accessor — the ladder and
// the tracer walk the SAME table, so a field added to Config must extend it
// here (the config print renderer depends on the catalog being complete).
type fieldBinding struct {
	key    string
	get    func(Config) string
	apply  func(*Config, string)
	source func(Config) []string // list-valued fields ("" = scalar)
}

// fieldBindings is the complete Config field catalog in display order.
var fieldBindings = []fieldBinding{
	{"container", func(c Config) string { return c.Container }, func(c *Config, v string) { c.Container = v }, nil},
	{"shadowSuffix", func(c Config) string { return c.ShadowSuffix }, func(c *Config, v string) { c.ShadowSuffix = v }, nil},
	{"envFile", func(c Config) string { return c.EnvFile }, func(c *Config, v string) { c.EnvFile = v }, nil},
	{"dataDir", func(c Config) string { return c.DataDir }, func(c *Config, v string) { c.DataDir = v }, nil},
	{"bucket", func(c Config) string { return c.Bucket }, func(c *Config, v string) { c.Bucket = v }, nil},
	{"objectPrefix", func(c Config) string { return c.ObjectPrefix }, func(c *Config, v string) { c.ObjectPrefix = v }, nil},
	{"healthPath", func(c Config) string { return c.HealthPath }, func(c *Config, v string) { c.HealthPath = v }, nil},
	{"proxyHost", func(c Config) string { return c.ProxyHost }, func(c *Config, v string) { c.ProxyHost = v }, nil},
	{"services", func(c Config) string { return strings.Join(c.Services, " ") }, func(c *Config, v string) { c.Services = strings.Fields(v) }, func(c Config) []string { return c.Services }},
	{"imagePrefix", func(c Config) string { return c.ImagePrefix }, func(c *Config, v string) { c.ImagePrefix = v }, nil},
	{"port", func(c Config) string { return c.Port }, func(c *Config, v string) { c.Port = v }, nil},
	{"network", func(c Config) string { return c.Network }, func(c *Config, v string) { c.Network = v }, nil},
	{"shadowProbePort", func(c Config) string { return c.ShadowProbePort }, func(c *Config, v string) { c.ShadowProbePort = v }, nil},
	{"deployedShaFile", func(c Config) string { return c.DeployedShaFile }, func(c *Config, v string) { c.DeployedShaFile = v }, nil},
	{"envClearKeys", func(c Config) string { return strings.Join(c.EnvClearKeys, " ") }, func(c *Config, v string) { c.EnvClearKeys = strings.Fields(v) }, func(c Config) []string { return c.EnvClearKeys }},
	{"dockerfile", func(c Config) string { return c.Dockerfile }, func(c *Config, v string) { c.Dockerfile = v }, nil},
	{"migrateScript", func(c Config) string { return c.MigrateScript }, func(c *Config, v string) { c.MigrateScript = v }, nil},
}

func bindingIndex(key string) int {
	for i, b := range fieldBindings {
		if b.key == key {
			return i
		}
	}
	return -1
}

// envBindingKeys maps each field's JSON key to its KAMPODRA_* env override
// (key order matches fieldBindings).
var envBindingKeys = map[string]string{
	"container":       "KAMPODRA_CONTAINER",
	"shadowSuffix":    "KAMPODRA_SHADOW_SUFFIX",
	"envFile":         "KAMPODRA_ENV_FILE",
	"dataDir":         "KAMPODRA_DATA_DIR",
	"bucket":          "KAMPODRA_BUCKET",
	"objectPrefix":    "KAMPODRA_OBJECT_PREFIX",
	"healthPath":      "KAMPODRA_HEALTH_PATH",
	"proxyHost":       "KAMPODRA_PROXY_HOST",
	"services":        "KAMPODRA_SERVICES",
	"imagePrefix":     "KAMPODRA_IMAGE_PREFIX",
	"port":            "KAMPODRA_PORT",
	"network":         "KAMPODRA_NETWORK",
	"shadowProbePort": "KAMPODRA_SHADOW_PROBE_PORT",
	"deployedShaFile": "KAMPODRA_DEPLOYED_SHA_FILE",
	"envClearKeys":    "KAMPODRA_ENV_CLEAR_KEYS",
	"dockerfile":      "KAMPODRA_DOCKERFILE",
	"migrateScript":   "KAMPODRA_MIGRATE_SCRIPT",
}

// ResolveTraced applies the FULL ladder with per-field provenance:
//
//	flags > KAMPODRA_* env > profile "project" block > kampodra.json > LoadDefault()
//
// flags maps a field's JSON key to an explicit per-invocation value (only
// callers whose command actually exposes flags pass entries). Each layer
// overrides a field only when it PROVIDES a value (non-empty string /
// non-empty list), so the returned traces always point at the winning
// layer. config print renders these; other callers use Resolve.
func ResolveTraced(flags map[string]string, lookup func(string) (string, bool), rawOverrides json.RawMessage, manifest *Manifest) (Config, []FieldTrace) {
	cfg := LoadDefault()
	traces := make(map[string]FieldTrace, len(fieldBindings))
	set := func(key string, src Source, origin string, value string, list []string) {
		i := bindingIndex(key)
		if i < 0 {
			return
		}
		b := fieldBindings[i]
		if list != nil {
			b.apply(&cfg, strings.Join(list, " "))
		} else {
			b.apply(&cfg, value)
		}
		traces[key] = FieldTrace{Field: key, Source: src, Origin: origin}
	}

	// 1. repo manifest (lowest override layer; parse already failed closed)
	if manifest != nil {
		for _, b := range fieldBindings {
			if b.source != nil {
				if v := manifestListField(manifest.Fields, b.key); len(v) > 0 {
					set(b.key, SourceRepoFile, manifest.Path, "", v)
				}
				continue
			}
			if v := manifestStringField(manifest.Fields, b.key); v != "" {
				set(b.key, SourceRepoFile, manifest.Path, v, nil)
			}
		}
	}

	// 2. the profile's raw "project" block (corrupt block fails open)
	if len(rawOverrides) > 0 {
		var o Overrides
		if json.Unmarshal(rawOverrides, &o) == nil {
			for _, b := range fieldBindings {
				if b.source != nil {
					if v := overridesListField(o, b.key); len(v) > 0 {
						set(b.key, SourceProfile, "", "", v)
					}
					continue
				}
				if v := overridesStringField(o, b.key); v != "" {
					set(b.key, SourceProfile, "", v, nil)
				}
			}
		}
	}

	// 3. KAMPODRA_* env (session-scoped beats the persisted layers)
	if lookup != nil {
		for _, b := range fieldBindings {
			if key, ok := envBindingKeys[b.key]; ok {
				if v, ok := lookup(key); ok && v != "" {
					set(b.key, SourceEnv, key, v, nil)
				}
			}
		}
	}

	// 4. explicit flags (highest layer)
	for key, v := range flags {
		if v != "" {
			set(key, SourceFlag, key, v, nil)
		}
	}

	out := make([]FieldTrace, 0, len(fieldBindings))
	for _, b := range fieldBindings {
		if tr, ok := traces[b.key]; ok {
			out = append(out, tr)
		} else {
			out = append(out, FieldTrace{Field: b.key, Source: SourceDefault})
		}
	}
	return cfg, out
}

// manifestStringField reads one scalar field off a parsed manifest without
// reflection: the switch IS the manifest schema (a new ManifestFields field
// extends it).
func manifestStringField(o ManifestFields, key string) string {
	switch key {
	case "container":
		return o.Container
	case "shadowSuffix":
		return o.ShadowSuffix
	case "envFile":
		return o.EnvFile
	case "dataDir":
		return o.DataDir
	case "bucket":
		return o.Bucket
	case "objectPrefix":
		return o.ObjectPrefix
	case "healthPath":
		return o.HealthPath
	case "proxyHost":
		return o.ProxyHost
	case "imagePrefix":
		return o.ImagePrefix
	case "port":
		return o.Port
	case "network":
		return o.Network
	case "shadowProbePort":
		return o.ShadowProbePort
	case "deployedShaFile":
		return o.DeployedShaFile
	case "dockerfile":
		return o.Dockerfile
	case "migrateScript":
		return o.MigrateScript
	}
	return ""
}

// manifestListField reads one list-valued field off a parsed manifest.
func manifestListField(o ManifestFields, key string) []string {
	switch key {
	case "services":
		return o.Services
	case "envClearKeys":
		return o.EnvClearKeys
	}
	return nil
}

// overridesStringField/overridesListField read the same field keys off the
// profile's "project" block — same camelCase keys as the manifest/env
// ladder (envFile included; the legacy envFilePath spelling is accepted at
// unmarshal time only).
func overridesStringField(o Overrides, key string) string {
	switch key {
	case "container":
		return o.Container
	case "shadowSuffix":
		return o.ShadowSuffix
	case "envFile":
		return o.EnvFile
	case "dataDir":
		return o.DataDir
	case "bucket":
		return o.Bucket
	case "objectPrefix":
		return o.ObjectPrefix
	case "healthPath":
		return o.HealthPath
	case "proxyHost":
		return o.ProxyHost
	case "imagePrefix":
		return o.ImagePrefix
	case "port":
		return o.Port
	case "network":
		return o.Network
	case "shadowProbePort":
		return o.ShadowProbePort
	case "deployedShaFile":
		return o.DeployedShaFile
	case "dockerfile":
		return o.Dockerfile
	case "migrateScript":
		return o.MigrateScript
	}
	return ""
}

func overridesListField(o Overrides, key string) []string {
	switch key {
	case "services":
		return o.Services
	case "envClearKeys":
		return o.EnvClearKeys
	}
	return nil
}

// FieldView is one field's rendered row: key, resolved value (lists
// space-joined), and the winning trace.
type FieldView struct {
	Key   string
	Value string
	Trace FieldTrace
}

// FieldViews pairs the resolved config with its traces in catalog order —
// the data behind `config print` (formatting stays in the command layer).
func FieldViews(cfg Config, traces []FieldTrace) []FieldView {
	byField := make(map[string]FieldTrace, len(traces))
	for _, tr := range traces {
		byField[tr.Field] = tr
	}
	views := make([]FieldView, 0, len(fieldBindings))
	for _, b := range fieldBindings {
		tr, ok := byField[b.key]
		if !ok {
			tr = FieldTrace{Field: b.key, Source: SourceDefault}
		}
		views = append(views, FieldView{Key: b.key, Value: b.get(cfg), Trace: tr})
	}
	return views
}
