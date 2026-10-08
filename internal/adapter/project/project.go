// Package project owns the deployed project's shape: the ONE place the
// naming constants (container, env-file path, bucket, health path, image
// prefix, ...) may live. Resolution everywhere is
//
//	profile "project" block > KAMPODRA_* env > LoadDefault()
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
	EnvFilePath     string   `json:"envFilePath"`     // remote env file (0600)
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
}

// LoadDefault returns today's values as NAMED DEFAULTS — the single file
// where this naming exists. Changing a default here changes every consumer
// at once; overriding per profile (config.json "project" block) or via
// KAMPODRA_* env stays available without code changes.
func LoadDefault() Config {
	return Config{
		Container:       "kampodine-api",
		ShadowSuffix:    "-shadow",
		EnvFilePath:     "/etc/kampodine/env",
		DataDir:         "/data/tenants",
		Bucket:          "esellar-libsql-backups",
		ObjectPrefix:    "db",
		HealthPath:      "/api/auth/ok",
		ProxyHost:       "app.example.com",
		Services:        []string{"kampodine-api", "kamal-proxy", "walshipper"},
		ImagePrefix:     "127.0.0.1:5000/kampodine-api",
		Port:            "8080",
		Network:         "kamal",
		ShadowProbePort: "18080",
		DeployedShaFile: "/etc/kampodine/deployed-sha",
		EnvClearKeys:    []string{"NODE_ENV", "PORT", "LIBSQL_TENANT_DIR", "LIBSQL_API_MOUNT", "STATIC_SPA_MOUNT"},
	}
}

// Overrides is the profile "project" block (config.json): every field
// optional, empty = "not overridden".
type Overrides struct {
	Container       string   `json:"container,omitempty"`
	ShadowSuffix    string   `json:"shadowSuffix,omitempty"`
	EnvFilePath     string   `json:"envFilePath,omitempty"`
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
}

// envBindings maps every Config field to its KAMPODRA_* env override.
var envBindings = []struct {
	key   string
	apply func(*Config, string)
}{
	{"KAMPODRA_CONTAINER", func(c *Config, v string) { c.Container = v }},
	{"KAMPODRA_SHADOW_SUFFIX", func(c *Config, v string) { c.ShadowSuffix = v }},
	{"KAMPODRA_ENV_FILE", func(c *Config, v string) { c.EnvFilePath = v }},
	{"KAMPODRA_DATA_DIR", func(c *Config, v string) { c.DataDir = v }},
	{"KAMPODRA_BUCKET", func(c *Config, v string) { c.Bucket = v }},
	{"KAMPODRA_OBJECT_PREFIX", func(c *Config, v string) { c.ObjectPrefix = v }},
	{"KAMPODRA_HEALTH_PATH", func(c *Config, v string) { c.HealthPath = v }},
	{"KAMPODRA_PROXY_HOST", func(c *Config, v string) { c.ProxyHost = v }},
	{"KAMPODRA_SERVICES", func(c *Config, v string) {
		c.Services = strings.Fields(v)
	}},
	{"KAMPODRA_IMAGE_PREFIX", func(c *Config, v string) { c.ImagePrefix = v }},
	{"KAMPODRA_PORT", func(c *Config, v string) { c.Port = v }},
	{"KAMPODRA_NETWORK", func(c *Config, v string) { c.Network = v }},
	{"KAMPODRA_SHADOW_PROBE_PORT", func(c *Config, v string) { c.ShadowProbePort = v }},
	{"KAMPODRA_DEPLOYED_SHA_FILE", func(c *Config, v string) { c.DeployedShaFile = v }},
	{"KAMPODRA_ENV_CLEAR_KEYS", func(c *Config, v string) { c.EnvClearKeys = strings.Fields(v) }},
}

// Resolve layers the config: LoadDefault, then KAMPODRA_* env, then the
// profile's raw "project" block (the command layer passes
// state.Profile.Project through untouched — this adapter owns its shape).
// A corrupt overrides block fails OPEN to env/defaults (the block is an
// override layer, not data); a nil lookup means "no env".
func Resolve(rawOverrides json.RawMessage, lookup func(string) (string, bool)) Config {
	cfg := LoadDefault()
	if lookup != nil {
		for _, b := range envBindings {
			if v, ok := lookup(b.key); ok && v != "" {
				b.apply(&cfg, v)
			}
		}
	}
	if len(rawOverrides) > 0 {
		var o Overrides
		if json.Unmarshal(rawOverrides, &o) == nil {
			if o.Container != "" {
				cfg.Container = o.Container
			}
			if o.ShadowSuffix != "" {
				cfg.ShadowSuffix = o.ShadowSuffix
			}
			if o.EnvFilePath != "" {
				cfg.EnvFilePath = o.EnvFilePath
			}
			if o.DataDir != "" {
				cfg.DataDir = o.DataDir
			}
			if o.Bucket != "" {
				cfg.Bucket = o.Bucket
			}
			if o.ObjectPrefix != "" {
				cfg.ObjectPrefix = o.ObjectPrefix
			}
			if o.HealthPath != "" {
				cfg.HealthPath = o.HealthPath
			}
			if o.ProxyHost != "" {
				cfg.ProxyHost = o.ProxyHost
			}
			if len(o.Services) > 0 {
				cfg.Services = o.Services
			}
			if o.ImagePrefix != "" {
				cfg.ImagePrefix = o.ImagePrefix
			}
			if o.Port != "" {
				cfg.Port = o.Port
			}
			if o.Network != "" {
				cfg.Network = o.Network
			}
			if o.ShadowProbePort != "" {
				cfg.ShadowProbePort = o.ShadowProbePort
			}
			if o.DeployedShaFile != "" {
				cfg.DeployedShaFile = o.DeployedShaFile
			}
			if len(o.EnvClearKeys) > 0 {
				cfg.EnvClearKeys = o.EnvClearKeys
			}
		}
	}
	return cfg
}
