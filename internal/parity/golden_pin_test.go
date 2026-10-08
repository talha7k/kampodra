package parity

import (
	"path/filepath"
	"testing"
)

// The committed golden is an artifact: this pin locks the hand-reviewed
// surface (derived from kampodine's committed HEAD by reading the scripts)
// so a parser regression cannot silently rewrite it. When the shell moves
// and the golden is regenerated CONSCIOUSLY, update these expectations in
// the same commit — that review is the point of the guard.
func TestCommittedGoldenMatchesHandReviewedSurface(t *testing.T) {
	g, err := LoadGolden(filepath.Join("golden.json"))
	if err != nil {
		t.Fatalf("load committed golden: %v", err)
	}
	if n := len(g.Commands); n != 11 {
		t.Fatalf("committed golden has %d commands, want 11 (kampodine HEAD surface)", n)
	}
	if g.SourceShell != "github.com/talha7k/kampodine" {
		t.Errorf("source_shell_repo = %q, want the canonical repo name (never a local path)", g.SourceShell)
	}
	if len(g.SourceHead) != 40 {
		t.Errorf("source_shell_head = %q, want a full commit sha", g.SourceHead)
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
