package project

import (
	"testing"
)

// The deploy pipeline's VM-side shape: ports, the podman network, the
// shadow probe port, and the deployed-sha stamp path — all project config
// (naming never referenced directly), never command-layer constants.

func TestLoadDefaultDeployShape(t *testing.T) {
	d := LoadDefault()
	for field, got := range map[string]string{
		"Port":            d.Port,
		"Network":         d.Network,
		"ShadowProbePort": d.ShadowProbePort,
		"DeployedShaFile": d.DeployedShaFile,
	} {
		if got == "" {
			t.Errorf("LoadDefault().%s is empty — every field must carry a named default", field)
		}
	}
	if d.Port != "8080" {
		t.Errorf("LoadDefault().Port = %q, want 8080", d.Port)
	}
}

func TestResolveDeployShapePrecedence(t *testing.T) {
	env := func(kv map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			v, ok := kv[key]
			return v, ok
		}
	}
	tests := []struct {
		name      string
		overrides string
		env       map[string]string
		field     func(Config) string
		want      string
	}{
		{
			name:  "port: env beats default",
			env:   map[string]string{"KAMPODRA_PORT": "9090"},
			field: func(c Config) string { return c.Port },
			want:  "9090",
		},
		{
			name:      "port: profile beats env",
			overrides: `{"port": "7070"}`,
			env:       map[string]string{"KAMPODRA_PORT": "9090"},
			field:     func(c Config) string { return c.Port },
			want:      "7070",
		},
		{
			name:  "network: env beats default",
			env:   map[string]string{"KAMPODRA_NETWORK": "edge"},
			field: func(c Config) string { return c.Network },
			want:  "edge",
		},
		{
			name:      "network: profile wins",
			overrides: `{"network": "appnet"}`,
			field:     func(c Config) string { return c.Network },
			want:      "appnet",
		},
		{
			name:  "shadow probe port: env beats default",
			env:   map[string]string{"KAMPODRA_SHADOW_PROBE_PORT": "18081"},
			field: func(c Config) string { return c.ShadowProbePort },
			want:  "18081",
		},
		{
			name:  "deployed sha file: env beats default",
			env:   map[string]string{"KAMPODRA_DEPLOYED_SHA_FILE": "/srv/sha"},
			field: func(c Config) string { return c.DeployedShaFile },
			want:  "/srv/sha",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve([]byte(tc.overrides), env(tc.env))
			if have := tc.field(got); have != tc.want {
				t.Errorf("Resolve() = %q, want %q", have, tc.want)
			}
		})
	}
}
