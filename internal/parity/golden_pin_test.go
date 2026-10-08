package parity

import (
	"path/filepath"
	"strings"
	"testing"
)

// The committed golden is the FROZEN SPEC of the shell 0.6.0 surface (the
// shell line is retired; nothing regenerates this file). This pin locks the
// hand-reviewed surface so a parser regression cannot silently rewrite the
// spec. The golden's kampodra-native additions (ssh, --all-profiles,
// --follow, env diff) are NOT in here — the guard flags them as
// warning-only native additions instead of spec entries.
func TestCommittedGoldenMatchesHandReviewedSurface(t *testing.T) {
	g, err := LoadGolden(filepath.Join("golden.json"))
	if err != nil {
		t.Fatalf("load committed golden: %v", err)
	}
	if n := len(g.Commands); n != 11 {
		t.Fatalf("committed golden has %d commands, want 11 (the frozen shell 0.6.0 surface)", n)
	}
	if g.SourceShell != "github.com/talha7k/kampodine" {
		t.Errorf("source_shell_repo = %q, want the canonical repo name (never a local path)", g.SourceShell)
	}
	if len(g.SourceHead) != 40 {
		t.Errorf("source_shell_head = %q, want a full commit sha", g.SourceHead)
	}
	if !g.Frozen {
		t.Errorf("golden is not marked frozen — the spec must be pinned as final")
	}
	for _, fragment := range []string{"v0.6.0", "5bf07fe", "retired 2026-10-08"} {
		if !strings.Contains(g.FrozenNote, fragment) {
			t.Errorf("frozen_note %q missing %q", g.FrozenNote, fragment)
		}
	}

	byName := map[string]Command{}
	for _, c := range g.Commands {
		byName[c.Name] = c
	}

	status, ok := byName["status"]
	if !ok {
		t.Fatalf("golden has no status command")
	}
	if eqStrings(status.Flags, "--host", "--profile", "--ssh-key", "--verbose") == false {
		t.Errorf("status flags = %v, want [--host --profile --ssh-key --verbose]", status.Flags)
	}
	if len(status.Subcommands) != 0 {
		t.Errorf("status subcommands = %v, want none (the tail exec is bluegreen's surface, not status's)", status.Subcommands)
	}
	if len(status.TailExecs) != 1 || status.TailExecs[0] != "bluegreen.sh" {
		t.Errorf("status tail execs = %v, want [bluegreen.sh]", status.TailExecs)
	}

	deploy := byName["deploy"]
	if eqStrings(deploy.Subcommands, "list", "logs", "prune", "restart", "shell") == false {
		t.Errorf("deploy subcommands = %v", deploy.Subcommands)
	}
	if len(deploy.Flags) != 12 {
		t.Errorf("deploy flags = %v, want 12 (deploy.sh + deploy-lifecycle.sh union)", deploy.Flags)
	}
	if deploy.Args["shell"] == nil || deploy.Args["shell"][0] != "<cmd>..." {
		t.Errorf("deploy shell args = %v, want [<cmd>...]", deploy.Args["shell"])
	}

	bluegreen := byName["bluegreen"]
	if eqStrings(bluegreen.Subcommands, "flip", "init", "provision", "rollback", "status") == false {
		t.Errorf("bluegreen subcommands = %v", bluegreen.Subcommands)
	}
	if eqStrings(bluegreen.Flags, "--force", "--profile", "--to") == false {
		t.Errorf("bluegreen flags = %v", bluegreen.Flags)
	}
	if got := bluegreen.Args["provision"]; len(got) != 1 || got[0] != "<color>" {
		t.Errorf("bluegreen provision args = %v, want [<color>]", got)
	}

	dns := byName["dns"]
	if eqStrings(dns.Subcommands, "add", "records", "rm") == false {
		t.Errorf("dns subcommands = %v", dns.Subcommands)
	}
	if eqStrings(dns.Flags, "--instance-principal", "--name", "--profile", "--ttl", "--type", "--value", "--zone") == false {
		t.Errorf("dns flags = %v", dns.Flags)
	}

	env := byName["env"]
	if eqStrings(env.Subcommands, "fingerprint", "list", "pull", "push") == false {
		t.Errorf("env subcommands = %v", env.Subcommands)
	}

	migrate := byName["migrate"]
	if eqStrings(migrate.Flags, "--allow-running", "--profile") == false {
		t.Errorf("migrate flags = %v", migrate.Flags)
	}

	vmPrepare := byName["vm-prepare"]
	if eqStrings(vmPrepare.Flags, "--host", "--profile", "--pull-images", "--ssh-key") == false {
		t.Errorf("vm-prepare flags = %v", vmPrepare.Flags)
	}

	imageImport := byName["image-import"]
	if eqStrings(imageImport.Flags, "--bucket", "--compartment", "--from-pass", "--image", "--keep-object", "--name-prefix", "--profile") == false {
		t.Errorf("image-import flags = %v", imageImport.Flags)
	}
}

func eqStrings(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
