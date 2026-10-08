package parity

import (
	"fmt"
	"sort"
	"strings"
)

// Violation is one parity failure, worded so the fix is obvious.
type Violation string

func (v Violation) Error() string { return string(v) }

// Check compares the shell golden against the Go tree snapshot under the
// NOT_YET_PORTED baseline ratchet:
//
//   - every golden command must be ported (in the tree) or baselined;
//     neither = violation (a new shell command landed without Go coverage)
//   - a baseline entry whose command IS ported = stale = violation (delete
//     the baseline entry as the port lands)
//   - a baseline entry with no such shell command = stale = violation
//   - a ported command's flags/subcommands/args must cover the golden
//     exactly — missing AND invented surface both violate
//   - a Go command with no golden entry = invented surface = violation
func Check(golden *Golden, baseline *Baseline, tree *TreeSnapshot) []Violation {
	var violations []Violation
	add := func(format string, args ...any) {
		violations = append(violations, Violation(fmt.Sprintf(format, args...)))
	}

	baselined := map[string]bool{}
	for _, name := range baseline.NotYetPorted {
		baselined[name] = true
	}
	goldenNames := map[string]bool{}
	for _, cmd := range golden.Commands {
		goldenNames[cmd.Name] = true
	}
	for _, name := range baseline.NotYetPorted {
		if !goldenNames[name] {
			add("stale NOT_YET_PORTED baseline entry %q — no such shell command (kampodine HEAD); delete it from internal/parity/baseline.json", name)
		}
	}

	for _, cmd := range golden.Commands {
		tc, ported := tree.Commands[cmd.Name]
		switch {
		case baselined[cmd.Name] && ported:
			add("stale NOT_YET_PORTED baseline entry %q — already ported, delete it from internal/parity/baseline.json", cmd.Name)
		case !baselined[cmd.Name] && !ported:
			add("command %q is in the shell golden (scripts/%s) but neither ported nor in the NOT_YET_PORTED baseline — port it or add it to internal/parity/baseline.json", cmd.Name, cmd.Script)
		case !ported:
			continue // deliberately not yet ported
		default:
			violations = append(violations, checkSurface(cmd, tc)...)
		}
	}

	treeNames := make([]string, 0, len(tree.Commands))
	for name := range tree.Commands {
		treeNames = append(treeNames, name)
	}
	sort.Strings(treeNames)
	for _, name := range treeNames {
		if !goldenNames[name] {
			add("command %q exists in the Go tree but not in the shell golden — inventing surface is a parity violation", name)
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		return strings.Compare(string(violations[i]), string(violations[j])) < 0
	})
	return violations
}

func checkSurface(cmd Command, tc TreeCommand) []Violation {
	var violations []Violation
	add := func(format string, args ...any) {
		violations = append(violations, Violation(fmt.Sprintf(format, args...)))
	}

	for _, flag := range cmd.Flags {
		if !tc.Flags[flag] {
			add("command %q: missing flag %q", cmd.Name, flag)
		}
	}
	for flag := range tc.Flags {
		if !contains(cmd.Flags, flag) {
			add("command %q: flag %q is not in the shell golden — inventing surface is a parity violation", cmd.Name, flag)
		}
	}
	for _, sub := range cmd.Subcommands {
		if !tc.Subcommands[sub] {
			add("command %q: missing subcommand %q", cmd.Name, sub)
		}
	}
	for sub := range tc.Subcommands {
		if !contains(cmd.Subcommands, sub) {
			add("command %q: subcommand %q is not in the shell golden — inventing surface is a parity violation", cmd.Name, sub)
		}
	}
	for sub, args := range cmd.Args {
		for _, arg := range args {
			declared := false
			for _, got := range tc.Args[sub] {
				if got == arg {
					declared = true
					break
				}
			}
			if !declared {
				add("command %q: subcommand %q must declare positional arg %q", cmd.Name, sub, arg)
			}
		}
	}
	return violations
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
