package command

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

// Snapshot mechanics: the parity guard sees the world through BuildSnapshot,
// so the flattening rules are pinned here —
//
//   - a command's OWN positional args land under the "" key (ssh <cmd>... is
//     command-level surface, no subcommand involved)
//   - bracketed [--flag <value>] groups inside a Use string are help text,
//     never positional specs
//   - the root's auto --version affordance never leaks into a subcommand's
//     surface: a subcommand's own --version flag IS shell surface
//     (kampodine deploy --version <sha7>) and must be snapshotted
func TestSnapshotOwnPositionalArgsUnderEmptyKey(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	ssh := &cobra.Command{Use: "ssh [<cmd>...]", Args: cobra.ArbitraryArgs}
	root.AddCommand(ssh)

	snap := BuildSnapshot(root)
	got := snap.Commands["ssh"].Args[""]
	if want := []string{"<cmd>..."}; !reflect.DeepEqual(got, want) {
		t.Errorf("ssh own args = %v, want %v", got, want)
	}
}

func TestSnapshotSubcommandArgsKeepSubcommandKey(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	deploy := &cobra.Command{Use: "deploy"}
	shell := &cobra.Command{Use: "shell [exec -- <cmd>...]", Args: cobra.ArbitraryArgs}
	deploy.AddCommand(shell)
	root.AddCommand(deploy)

	snap := BuildSnapshot(root)
	deploySnap := snap.Commands["deploy"]
	if got := deploySnap.Args["shell"]; !reflect.DeepEqual(got, []string{"<cmd>..."}) {
		t.Errorf("shell args = %v, want [<cmd>...]", got)
	}
	if got := deploySnap.Args[""]; got != nil {
		t.Errorf("deploy's own args = %v, want none (the Use string is all flag help)", got)
	}
}

func TestSnapshotIgnoresBracketedFlagGroupsInUse(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	status := &cobra.Command{Use: "status [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--verbose]"}
	root.AddCommand(status)

	snap := BuildSnapshot(root)
	if got := snap.Commands["status"].Args[""]; got != nil {
		t.Errorf("status own args = %v, want none — bracketed flag groups are usage text, not positionals", got)
	}
}

func TestSnapshotKeepsSubcommandVersionFlag(t *testing.T) {
	root := &cobra.Command{Use: "root", Version: "9.9.9"}
	deploy := &cobra.Command{Use: "deploy"}
	// Simulate what cobra materializes: the ROOT gets the auto --version
	// affordance (and is never snapshotted); a subcommand that declares its
	// own --version is real surface.
	deploy.Flags().String("version", "", "deploy a specific version (git sha fragment)")
	root.AddCommand(deploy)

	snap := BuildSnapshot(root)
	if !snap.Commands["deploy"].Flags["--version"] {
		t.Errorf("deploy --version missing from snapshot — shell surface dropped by the root-affordance exclusion")
	}
}

func TestSnapshotExcludesHiddenCommands(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	root.AddCommand(&cobra.Command{Use: "visible"})
	root.AddCommand(&cobra.Command{Use: "secret", Hidden: true})

	snap := BuildSnapshot(root)
	if _, ok := snap.Commands["secret"]; ok {
		t.Errorf("hidden command leaked into the snapshot")
	}
	if _, ok := snap.Commands["visible"]; !ok {
		t.Errorf("visible command missing from the snapshot")
	}
}
