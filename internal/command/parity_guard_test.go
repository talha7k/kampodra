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
// FROZEN shell spec under the explicit NOT_YET_PORTED baseline.
//   - a frozen-spec command without Go coverage fails here until it is
//     ported or consciously baselined
//   - a baseline entry whose command has landed fails (delete it)
//   - a ported command missing frozen-spec surface fails
//
// The reverse direction (kampodra-native commands/flags beyond the frozen
// spec) is review-flagged as warnings — printed here, never blocking.
func TestCommandParityGuard(t *testing.T) {
	golden, err := parity.LoadGolden(filepath.Join("..", "parity", "golden.json"))
	if err != nil {
		t.Fatalf("load golden: %v", err)
	}
	if !golden.Frozen {
		t.Fatal("golden is not marked frozen — the spec must stay pinned as final")
	}
	baseline, err := parity.LoadBaseline(filepath.Join("..", "parity", "baseline.json"))
	if err != nil {
		t.Fatalf("load baseline: %v", err)
	}
	root := command.NewRoot("test", command.Deps{})
	snapshot := command.BuildSnapshot(root)

	violations, warnings := parity.Check(golden, baseline, snapshot)
	for _, w := range warnings {
		t.Logf("parity warning (review-flagged, non-blocking): %s", w)
	}
	if len(warnings) > 0 {
		t.Logf("== %d kampodra-native addition(s) beyond the frozen spec — review each against the README changelog ==", len(warnings))
	}
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
