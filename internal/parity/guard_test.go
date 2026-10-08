package parity

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

func goldenFixture() *Golden {
	return &Golden{
		Generator: "test",
		Commands: []Command{
			{Name: "deploy", Script: "deploy.sh", Subcommands: []string{"list", "prune"}, Flags: []string{"--dry-run", "--host", "--keep"}},
			{Name: "status", Script: "status.sh", Flags: []string{"--host", "--verbose"}},
			{Name: "dns", Script: "dns.sh", Subcommands: []string{"add", "records", "rm"}, Flags: []string{"--name"}},
			{Name: "bluegreen", Script: "bluegreen.sh", Subcommands: []string{"provision", "status"}, Flags: []string{"--to"}, Args: map[string][]string{"provision": {"<color>"}}},
		},
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name        string
		baseline    []string
		tree        func() *TreeSnapshot
		wantErrs    []string // blocking violations; empty = none
		wantWarns   []string // native-addition warnings; empty = none
		wantNoWarns bool     // assert the warning list is empty
	}{
		{
			name:     "fully covered tree passes",
			baseline: []string{"bluegreen", "deploy"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status": {Flags: set("--host", "--verbose")},
					"dns":    {Flags: set("--name"), Subcommands: set("add", "records", "rm")},
				}}
			},
			wantNoWarns: true,
		},
		{
			name:     "unported command without baseline entry fails with guidance",
			baseline: []string{},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{}}
			},
			wantErrs: []string{
				`frozen-spec command "deploy" is neither ported nor in the NOT_YET_PORTED baseline`,
				`frozen-spec command "status" is neither ported nor in the NOT_YET_PORTED baseline`,
				`frozen-spec command "dns" is neither ported nor in the NOT_YET_PORTED baseline`,
				`frozen-spec command "bluegreen" is neither ported nor in the NOT_YET_PORTED baseline`,
			},
		},
		{
			name:     "stale baseline entry (command already ported) fails",
			baseline: []string{"deploy", "status", "dns", "bluegreen"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status": {Flags: set("--host", "--verbose")},
				}}
			},
			wantErrs: []string{`stale NOT_YET_PORTED baseline entry "status" — already ported, delete it from internal/parity/baseline.json`},
		},
		{
			name:     "baseline entry missing from golden fails",
			baseline: []string{"deploy", "metrics"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{}}
			},
			wantErrs: []string{`stale NOT_YET_PORTED baseline entry "metrics" — no such frozen-spec command`},
		},
		{
			name:     "ported command with missing flag fails",
			baseline: []string{"deploy", "dns", "bluegreen"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status": {Flags: set("--host")},
				}}
			},
			wantErrs: []string{`command "status": missing flag "--verbose"`},
		},
		{
			name:     "native flag beyond the frozen spec is warning-only",
			baseline: []string{"deploy", "dns", "bluegreen"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status": {Flags: set("--host", "--verbose", "--json")},
				}}
			},
			wantWarns: []string{`kampodra-native flag "--json" on command "status" (beyond the frozen spec) — reviewed addition`},
		},
		{
			name:     "native subcommand beyond the frozen spec is warning-only",
			baseline: []string{"deploy", "bluegreen"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status": {Flags: set("--host", "--verbose")},
					"dns":    {Flags: set("--name"), Subcommands: set("add", "records", "rm", "poke")},
				}}
			},
			wantWarns: []string{`kampodra-native subcommand "poke" on command "dns" (beyond the frozen spec) — reviewed addition`},
		},
		{
			name:     "native Go command is warning-only",
			baseline: []string{"deploy", "dns", "bluegreen"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status":     {Flags: set("--host", "--verbose")},
					"frobnicate": {Flags: set("--hard")},
				}}
			},
			wantWarns: []string{`kampodra-native command "frobnicate" (beyond the frozen spec) — reviewed addition`},
		},
		{
			name:     "golden positional arg not declared fails",
			baseline: []string{"deploy", "dns", "status"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"bluegreen": {Flags: set("--to"), Subcommands: set("provision", "status")},
				}}
			},
			wantErrs: []string{`command "bluegreen": subcommand "provision" must declare positional arg "<color>"`},
		},
		{
			name:     "declared positional arg satisfies the golden",
			baseline: []string{"deploy", "dns", "status"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"bluegreen": {Flags: set("--to"), Subcommands: set("provision", "status"), Args: map[string][]string{"provision": {"<color>"}}},
				}}
			},
		},
		{
			name:     "native arg declaration is warning-only",
			baseline: []string{"deploy", "dns", "status"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"bluegreen": {Flags: set("--to"), Subcommands: set("provision", "status"), Args: map[string][]string{"provision": {"<color>"}, "status": {"<extra>"}}},
				}}
			},
			// Forward coverage satisfied; the tree-invented arg key produces
			// no warning (args flow one way, golden→tree).
			wantNoWarns: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations, warnings := Check(goldenFixture(), &Baseline{NotYetPorted: tt.baseline}, tt.tree())
			got := make([]string, 0, len(violations))
			for _, v := range violations {
				got = append(got, fmt.Sprint(v))
			}
			if len(tt.wantErrs) == 0 {
				if len(violations) != 0 {
					t.Fatalf("Check() violations = %v, want none", violations)
				}
			} else {
				for _, want := range tt.wantErrs {
					if !containsSubstring(got, want) {
						t.Errorf("Check() violations %v — missing substring %q", got, want)
					}
				}
			}
			gotWarns := make([]string, 0, len(warnings))
			for _, w := range warnings {
				gotWarns = append(gotWarns, fmt.Sprint(w))
			}
			if tt.wantNoWarns && len(warnings) != 0 {
				t.Errorf("Check() warnings = %v, want none", warnings)
			}
			for _, want := range tt.wantWarns {
				if !containsSubstring(gotWarns, want) {
					t.Errorf("Check() warnings %v — missing substring %q", gotWarns, want)
				}
			}
		})
	}
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func TestCheckWarningsAreSortedAndDeterministic(t *testing.T) {
	baseline := []string{"deploy", "dns", "bluegreen"}
	tree := &TreeSnapshot{Commands: map[string]TreeCommand{
		"status": {Flags: set("--host", "--verbose", "--json")},
		"zz":     {Flags: set("--x")},
		"aa":     {Flags: nil},
	}}
	a := warningsOf(Check(goldenFixture(), &Baseline{NotYetPorted: baseline}, tree))
	b := warningsOf(Check(goldenFixture(), &Baseline{NotYetPorted: baseline}, tree))
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("Check() warnings nondeterministic:\n%v\n%v", a, b)
	}
	sorted := append([]Warning(nil), a...)
	sort.Slice(sorted, func(i, j int) bool { return fmt.Sprint(sorted[i]) < fmt.Sprint(sorted[j]) })
	for i := range a {
		if fmt.Sprint(a[i]) != fmt.Sprint(sorted[i]) {
			t.Fatalf("Check() warnings not sorted: %v", a)
		}
	}
}

func warningsOf(_ []Violation, w []Warning) []Warning { return w }

func TestCheckViolationsAreSortedAndDeterministic(t *testing.T) {
	baseline := []string{}
	tree := &TreeSnapshot{Commands: map[string]TreeCommand{}}
	a, _ := Check(goldenFixture(), &Baseline{NotYetPorted: baseline}, tree)
	b, _ := Check(goldenFixture(), &Baseline{NotYetPorted: baseline}, tree)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("Check() is nondeterministic:\n%v\n%v", a, b)
	}
	sorted := append([]Violation(nil), a...)
	sort.Slice(sorted, func(i, j int) bool { return fmt.Sprint(sorted[i]) < fmt.Sprint(sorted[j]) })
	for i := range a {
		if fmt.Sprint(a[i]) != fmt.Sprint(sorted[i]) {
			t.Fatalf("Check() violations not sorted: %v", a)
		}
	}
}

func set(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}
