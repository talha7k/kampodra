package command_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// The dropped-key guard (live-fire 2026-10-16: a unit re-render dropped
// -e LIBSQL_TENANT_DIR — owned by the old ansible template, adopted by
// nobody — the store never opened, every wave route vanished, and
// /api/auth/ok kept the deploy smoke green).
func TestDroppedUnitEnvKeys(t *testing.T) {
	oldUnit := `ExecStart=/usr/bin/podman run --rm -e NODE_ENV=production -e PORT=8080 -e LIBSQL_TENANT_DIR=/data/tenants -e LIBSQL_API_MOUNT=/data/app --env-file /etc/esellar/env app:latest`
	newUnit := `ExecStart=/usr/bin/podman run --rm -e NODE_ENV=production -e PORT=8080 --env-file /etc/esellar/env app:latest`
	dropped := command.DroppedUnitEnvKeys(oldUnit, newUnit)
	if len(dropped) != 2 || dropped[0] != "LIBSQL_API_MOUNT" || dropped[1] != "LIBSQL_TENANT_DIR" {
		t.Errorf("dropped = %v, want [LIBSQL_API_MOUNT LIBSQL_TENANT_DIR] (sorted)", dropped)
	}
	// The fix path: the manifest unit.env re-declares the trio — nothing drops.
	fixed := `ExecStart=/usr/bin/podman run --rm -e NODE_ENV=production -e PORT=8080 -e LIBSQL_TENANT_DIR=/data/tenants --env-file /etc/esellar/env app:latest`
	if got := command.DroppedUnitEnvKeys(oldUnit, fixed); len(got) != 1 || got[0] != "LIBSQL_API_MOUNT" {
		t.Errorf("dropped = %v, want [LIBSQL_API_MOUNT]", got)
	}
	// Fresh VM (empty old unit): nothing to drop.
	if got := command.DroppedUnitEnvKeys("", newUnit); len(got) != 0 {
		t.Errorf("dropped = %v, want none on a fresh unit", got)
	}
	// Added keys are not "dropped".
	grown := strings.Replace(oldUnit, "-e NODE_ENV=production", "-e NODE_ENV=production -e EXTRA=1", 1)
	if got := command.DroppedUnitEnvKeys(oldUnit, grown); len(got) != 0 {
		t.Errorf("dropped = %v, want none (added keys are not drops)", got)
	}
}

// The anchor watcher variant rides the profile cloud block's provider:
// gcp gets the metadata OBSERVER (no ip addr, no anchor.conf), oci/empty
// gets the classic add-only watcher.
func TestCloudProviderOf(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"provider":"gcp","project":"p1"}`, "gcp"},
		{`{"compartment":"ocid1..."}`, ""}, // legacy block, no provider key
		{``, ""},                           // no cloud block
		{`not-json`, ""},                   // malformed fails later at CheckProvider
	}
	for _, tc := range cases {
		var raw json.RawMessage
		if tc.raw != "" {
			raw = json.RawMessage(tc.raw)
		}
		if got := command.CloudProviderOf(raw); got != tc.want {
			t.Errorf("cloudProviderOf(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
