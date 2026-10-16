package command

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// The resolution provenance line: ONE terse line at the top of every
// host-aware command run — which profile/host/provider the invocation
// resolved to, before any network or exec (2026-10-16 incident: commands
// never said which profile they resolved to).
func TestSayProvenanceOutputShape(t *testing.T) {
	cases := []struct {
		name    string
		cmdName string
		target  Target
		want    string
	}{
		{
			name:    "provider set in the cloud block",
			cmdName: "dns",
			target: Target{
				ProfileName: "prod",
				HostSpec:    transport.HostSpec{Host: "root@203.0.113.9"},
				Cloud:       json.RawMessage(`{"provider":"gcp","project":"my-prj"}`),
			},
			want: "[dns] profile prod · host root@203.0.113.9 · provider gcp\n",
		},
		{
			name:    "provider empty = the oci default (CheckProvider convention)",
			cmdName: "backup",
			target: Target{
				ProfileName: "prod",
				HostSpec:    transport.HostSpec{Host: "root@203.0.113.9"},
				Cloud:       json.RawMessage(`{"compartment":"ocid1.compartment.oc1..aaa"}`),
			},
			want: "[backup] profile prod · host root@203.0.113.9 · provider oci\n",
		},
		{
			name:    "no cloud block at all also resolves the oci default",
			cmdName: "metrics",
			target: Target{
				ProfileName: "staging",
				HostSpec:    transport.HostSpec{Host: "root@203.0.113.8"},
			},
			want: "[metrics] profile staging · host root@203.0.113.8 · provider oci\n",
		},
		{
			name:    "no profile renders the dash placeholder",
			cmdName: "status",
			target:  Target{HostSpec: transport.HostSpec{Host: "root@203.0.113.10"}},
			want:    "[status] profile - · host root@203.0.113.10 · provider oci\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			sayProvenance(Deps{Stdout: out}, tc.cmdName, tc.target)
			if got := out.String(); got != tc.want {
				t.Errorf("sayProvenance(%q) = %q, want %q", tc.cmdName, got, tc.want)
			}
		})
	}
}
