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
		name     string
		baseline []string
		tree     func() *TreeSnapshot
		wantErrs []string // substrings; empty = no violations
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
		},
		{
			name:     "unported command without baseline entry fails with guidance",
			baseline: []string{},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{}}
			},
			wantErrs: []string{
				`command "deploy" is in the shell golden (scripts/deploy.sh) but neither ported nor in the NOT_YET_PORTED baseline`,
				`command "status" is in the shell golden (scripts/status.sh) but neither ported`,
				`command "dns" is in the shell golden (scripts/dns.sh) but neither ported`,
				`command "bluegreen" is in the shell golden (scripts/bluegreen.sh) but neither ported`,
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
			wantErrs: []string{`stale NOT_YET_PORTED baseline entry "metrics" — no such shell command`},
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
			name:     "ported command with extra flag fails (invented surface)",
			baseline: []string{"deploy", "dns", "bluegreen"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status": {Flags: set("--host", "--verbose", "--json")},
				}}
			},
			wantErrs: []string{`command "status": flag "--json" is not in the shell golden — inventing surface is a parity violation`},
		},
		{
			name:     "ported command missing subcommands fails",
			baseline: []string{"deploy", "bluegreen"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status": {Flags: set("--host", "--verbose")},
					"dns":    {Flags: set("--name"), Subcommands: set("add", "records")},
				}}
			},
			wantErrs: []string{`command "dns": missing subcommand "rm"`},
		},
		{
			name:     "invented Go command fails",
			baseline: []string{"deploy", "dns", "bluegreen"},
			tree: func() *TreeSnapshot {
				return &TreeSnapshot{Commands: map[string]TreeCommand{
					"status":     {Flags: set("--host", "--verbose")},
					"frobnicate": {Flags: set("--hard")},
				}}
			},
			wantErrs: []string{`command "frobnicate" exists in the Go tree but not in the shell golden — inventing surface is a parity violation`},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations := Check(goldenFixture(), &Baseline{NotYetPorted: tt.baseline}, tt.tree())
			if len(tt.wantErrs) == 0 {
				if len(violations) != 0 {
					t.Fatalf("Check() = %v, want none", violations)
				}
				return
			}
			got := make([]string, 0, len(violations))
			for _, v := range violations {
				got = append(got, fmt.Sprint(v))
			}
			for _, want := range tt.wantErrs {
				found := false
				for _, g := range got {
					if strings.Contains(g, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Check() violations %v — missing substring %q", got, want)
				}
			}
		})
	}
}

func TestCheckViolationsAreSortedAndDeterministic(t *testing.T) {
	baseline := []string{}
	tree := &TreeSnapshot{Commands: map[string]TreeCommand{}}
	a := Check(goldenFixture(), &Baseline{NotYetPorted: baseline}, tree)
	b := Check(goldenFixture(), &Baseline{NotYetPorted: baseline}, tree)
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
