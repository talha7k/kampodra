package command_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

// TestCommandSurface is the plain command-surface smoke test: the root
// command exposes exactly the implemented command set, and every command
// identifies itself.
//
//   - the set is exact in BOTH directions — a vanished command breaks
//     users' scripts; a listed-but-unimplemented command is a promise the
//     binary cannot keep (unimplemented means unlisted)
//   - every command carries a Use and a Short — a command that cannot
//     describe itself is invisible to --help and unusable
func TestCommandSurface(t *testing.T) {
	root := command.NewRoot("test", command.Deps{})

	want := []string{
		"backup",
		"bluegreen",
		"config",
		"deploy",
		"dns",
		"env",
		"image-import",
		"metrics",
		"migrate",
		"ssh",
		"status",
		"vm-prepare",
		"vm-wipe",
	}

	var got []string
	for _, sub := range root.Commands() {
		if sub.Hidden {
			continue // not public surface
		}
		got = append(got, sub.Name())
		if sub.Use == "" {
			t.Errorf("command %q has no Use string", sub.Name())
		}
		if sub.Short == "" {
			t.Errorf("command %q has no Short description — every command must describe itself in --help", sub.Name())
		}
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("command surface drifted:\n got: %v\nwant: %v", got, want)
	}
}
