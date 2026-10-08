package project

import (
	"strings"
	"testing"
)

// LoadDefault is the ONE place the deployed project's naming lives. The
// values are assertions about today's shape — change them consciously.
func TestLoadDefaultNamedValues(t *testing.T) {
	d := LoadDefault()
	want := map[string]string{
		"Container":    d.Container,
		"ShadowSuffix": d.ShadowSuffix,
		"EnvFilePath":  d.EnvFilePath,
		"DataDir":      d.DataDir,
		"Bucket":       d.Bucket,
		"ObjectPrefix": d.ObjectPrefix,
		"HealthPath":   d.HealthPath,
		"ProxyHost":    d.ProxyHost,
		"ImagePrefix":  d.ImagePrefix,
	}
	for _, field := range []string{"Container", "ShadowSuffix", "EnvFilePath", "DataDir", "Bucket", "ObjectPrefix", "HealthPath", "ProxyHost", "ImagePrefix"} {
		if want[field] == "" {
			t.Errorf("LoadDefault().%s is empty — every field must carry a named default", field)
		}
	}
	if len(d.Services) == 0 {
		t.Errorf("LoadDefault().Services is empty — every field must carry a named default")
	}
}

// Resolution everywhere: profile field > KAMPODRA_* env > LoadDefault().
func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		overrides string // raw profile "project" block JSON ("" = none)
		env       map[string]string
		field     func(Config) string
		want      string
	}{
		{
			name:  "default wins when nothing else set",
			field: func(c Config) string { return c.Container },
			want:  LoadDefault().Container,
		},
		{
			name:  "env beats default",
			env:   map[string]string{"KAMPODRA_CONTAINER": "from-env"},
			field: func(c Config) string { return c.Container },
			want:  "from-env",
		},
		{
			name:      "env beats profile (0.7 ladder: env above the profile block)",
			overrides: `{"container": "from-profile"}`,
			env:       map[string]string{"KAMPODRA_CONTAINER": "from-env"},
			field:     func(c Config) string { return c.Container },
			want:      "from-env",
		},
		{
			name:      "profile still beats the repo manifest + default",
			overrides: `{"container": "from-profile"}`,
			env:       map[string]string{"KAMPODRA_UNRELATED": "x"},
			field:     func(c Config) string { return c.Container },
			want:      "from-profile",
		},
		{
			name:  "env file path",
			env:   map[string]string{"KAMPODRA_ENV_FILE": "/srv/app/env"},
			field: func(c Config) string { return c.EnvFilePath },
			want:  "/srv/app/env",
		},
		{
			name:      "env file path from profile",
			overrides: `{"envFilePath": "/srv/other/env"}`,
			field:     func(c Config) string { return c.EnvFilePath },
			want:      "/srv/other/env",
		},
		{
			name:  "health path",
			env:   map[string]string{"KAMPODRA_HEALTH_PATH": "/healthz"},
			field: func(c Config) string { return c.HealthPath },
			want:  "/healthz",
		},
		{
			name:  "bucket",
			env:   map[string]string{"KAMPODRA_BUCKET": "other-bucket"},
			field: func(c Config) string { return c.Bucket },
			want:  "other-bucket",
		},
		{
			name:  "proxy host",
			env:   map[string]string{"KAMPODRA_PROXY_HOST": "edge.other.tld"},
			field: func(c Config) string { return c.ProxyHost },
			want:  "edge.other.tld",
		},
		{
			name:      "env beats profile proxy host (0.7 ladder)",
			overrides: `{"proxyHost": "edge.profile.tld"}`,
			env:       map[string]string{"KAMPODRA_PROXY_HOST": "edge.other.tld"},
			field:     func(c Config) string { return c.ProxyHost },
			want:      "edge.other.tld",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(nil, []byte(tt.overrides), func(k string) (string, bool) {
				v, ok := tt.env[k]
				return v, ok
			})
			if have := tt.field(got); have != tt.want {
				t.Errorf("Resolve() field = %q, want %q", have, tt.want)
			}
		})
	}
}

func TestResolveServicesEnvIsSpaceSeparated(t *testing.T) {
	got := Resolve(nil, nil, func(k string) (string, bool) {
		if k == "KAMPODRA_SERVICES" {
			return "api web worker", true
		}
		return "", false
	})
	if strings.Join(got.Services, ",") != "api,web,worker" {
		t.Errorf("Services = %v, want [api web worker]", got.Services)
	}
}

func TestResolveProfileServicesOverride(t *testing.T) {
	got := Resolve(nil, []byte(`{"services": ["solo"]}`), func(string) (string, bool) { return "", false })
	if len(got.Services) != 1 || got.Services[0] != "solo" {
		t.Errorf("Services = %v, want [solo]", got.Services)
	}
}

func TestResolveCorruptOverridesFallOpenToEnvAndDefaults(t *testing.T) {
	// A corrupt profile project block must not crash the CLI: env/defaults
	// still apply (the block is an override layer, not a data file).
	got := Resolve(nil, []byte(`{"container": `), func(k string) (string, bool) {
		if k == "KAMPODRA_CONTAINER" {
			return "env-value", true
		}
		return "", false
	})
	if got.Container != "env-value" {
		t.Errorf("Container = %q, want the env value (corrupt overrides ignored)", got.Container)
	}
}

func TestResolveNilLookupAndNilRaw(t *testing.T) {
	got := Resolve(nil, nil, nil)
	if got.Container != LoadDefault().Container {
		t.Errorf("Resolve(nil, nil, nil) = %+v, want the defaults", got)
	}
}
