// bluegreen status: pair rendering with the reserved IP present, and the
// no-reserved-IP error naming the init path.

package command_test

import (
	"strings"
	"testing"
)

func TestBluegreenStatusRendersPair(t *testing.T) {
	h := setupBG(t, map[string]string{
		"reserved":        "ocid1.publicip.r1 203.0.113.50 ocid1.privateip.anchor",
		"instances-green": greenRow(),
	}, map[string]string{"HEALTHY": "0"})
	if code := h.run(t, "status"); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{
		"== app blue/green pair (compartment test-compartment) ==",
		"reserved IP : 203.0.113.50 (ocid1.publicip.r1)",
		"assigned to : ocid1.privateip.anchor",
		"app-blue : not provisioned",
		"app=HEALTHY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestBluegreenStatusNoReservedIP(t *testing.T) {
	h := setupBG(t, map[string]string{"reserved": ""}, nil)
	if code := h.run(t, "status"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "reserved IP : NONE") || !strings.Contains(out, "not provisioned") {
		t.Errorf("status missing NONE/not-provisioned:\n%s", out)
	}
}
