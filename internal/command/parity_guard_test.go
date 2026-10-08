package command_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
	"github.com/talha7k/kampodra/internal/parity"
)

// TestCommandParityGuard is THE ratchet: the Go cobra tree must cover the
// committed shell golden under the explicit NOT_YET_PORTED baseline.
//   - a command the shell gains without Go coverage fails here until it is
//     ported or consciously baselined
//   - a baseline entry whose command has landed fails (delete it)
//   - a ported command with missing or invented flags/subcommands fails
func TestCommandParityGuard(t *testing.T) {
	golden, err := parity.LoadGolden(filepath.Join("..", "parity", "golden.json"))
	if err != nil {
		t.Fatalf("load golden: %v", err)
	}
	baseline, err := parity.LoadBaseline(filepath.Join("..", "parity", "baseline.json"))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	root := command.NewRoot("test", command.Deps{})
	snapshot := command.BuildSnapshot(root)

	violations := parity.Check(golden, baseline, snapshot)
	if len(violations) != 0 {
		var sb strings.Builder
		sb.WriteString("command-parity guard failed:\n")
		for _, v := range violations {
			sb.WriteString("  - " + string(v) + "\n")
		}
		t.Fatal(sb.String())
	}
}

// TestCommittedBaselineShape pins the baseline artifact's ratchet semantics.
func TestCommittedBaselineShape(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "parity", "baseline.json"))
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var b parity.Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	if len(b.NotYetPorted) == 0 {
		t.Fatal("baseline is empty — either everything is ported (delete the file) or the ratchet is being bypassed")
	}
	seen := map[string]bool{}
	for _, name := range b.NotYetPorted {
		if seen[name] {
			t.Errorf("baseline has duplicate entry %q", name)
		}
		seen[name] = true
	}
}
