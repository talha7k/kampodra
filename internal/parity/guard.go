package parity

import (
	"fmt"
	"sort"
	"strings"
)

// Violation is one parity failure, worded so the fix is obvious.
type Violation string

func (v Violation) Error() string { return string(v) }

// Warning is one non-blocking parity observation: kampodra-native surface
// (commands/flags/subcommands beyond the frozen shell spec). Native
// additions are review-flagged, never blocked — the frozen spec governs
// what must EXIST, not what may be added.
type Warning string

func (w Warning) Error() string { return string(w) }

// Check compares the frozen golden (the shell 0.6.0 spec) against the Go
// tree snapshot under the NOT_YET_PORTED baseline ratchet:
//
// Blocking violations:
//   - every golden command must be ported (in the tree) or baselined;
//     neither = violation (frozen-spec coverage gap)
//   - a baseline entry whose command IS ported = stale = violation (delete
//     the baseline entry as the port lands)
//   - a baseline entry with no such golden command = stale = violation
//   - a ported command's flags/subcommands/args must COVER the golden —
//     missing surface violates
//
// Warnings (review-flagged, non-blocking):
//   - a Go command with no golden entry = kampodra-native addition
//   - a ported command with surface beyond the golden = kampodra-native
//     addition (flag/subcommand/arg)
func Check(golden *Golden, baseline *Baseline, tree *TreeSnapshot) ([]Violation, []Warning) {
	var violations []Violation
	var warnings []Warning
	add := func(format string, args ...any) {
		violations = append(violations, Violation(fmt.Sprintf(format, args...)))
	}
	warn := func(format string, args ...any) {
		warnings = append(warnings, Warning(fmt.Sprintf(format, args...)))
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
			add("stale NOT_YET_PORTED baseline entry %q — no such frozen-spec command; delete it from internal/parity/baseline.json", name)
		}
	}

	for _, cmd := range golden.Commands {
		tc, ported := tree.Commands[cmd.Name]
		switch {
		case baselined[cmd.Name] && ported:
			add("stale NOT_YET_PORTED baseline entry %q — already ported, delete it from internal/parity/baseline.json", cmd.Name)
		case !baselined[cmd.Name] && !ported:
			add("frozen-spec command %q is neither ported nor in the NOT_YET_PORTED baseline — port it or add it to internal/parity/baseline.json", cmd.Name)
		case !ported:
			continue // deliberately not yet ported
		default:
			vs, ws := checkSurface(cmd, tc)
			violations = append(violations, vs...)
			warnings = append(warnings, ws...)
		}
	}

	treeNames := make([]string, 0, len(tree.Commands))
	for name := range tree.Commands {
		treeNames = append(treeNames, name)
	}
	sort.Strings(treeNames)
	for _, name := range treeNames {
		if !goldenNames[name] {
			warn("kampodra-native command %q (beyond the frozen spec) — reviewed addition, untracked by the golden", name)
		}
	}

	sort.Slice(violations, func(i, j int) bool {
		return strings.Compare(string(violations[i]), string(violations[j])) < 0
	})
	sort.Slice(warnings, func(i, j int) bool {
		return strings.Compare(string(warnings[i]), string(warnings[j])) < 0
	})
	return violations, warnings
}

// checkSurface: the forward direction (frozen spec coverage) is blocking;
// the reverse direction (native additions) is warning-only.
func checkSurface(cmd Command, tc TreeCommand) ([]Violation, []Warning) {
	var violations []Violation
	var warnings []Warning
	add := func(format string, args ...any) {
		violations = append(violations, Violation(fmt.Sprintf(format, args...)))
	}
	note := func(format string, args ...any) {
		warnings = append(warnings, Warning(fmt.Sprintf(format, args...)))
	}

	for _, flag := range cmd.Flags {
		if !tc.Flags[flag] {
			add("command %q: missing flag %q", cmd.Name, flag)
		}
	}
	for flag := range tc.Flags {
		if !contains(cmd.Flags, flag) {
			note("kampodra-native flag %q on command %q (beyond the frozen spec) — reviewed addition", flag, cmd.Name)
		}
	}
	for _, sub := range cmd.Subcommands {
		if !tc.Subcommands[sub] {
			add("command %q: missing subcommand %q", cmd.Name, sub)
		}
	}
	for sub := range tc.Subcommands {
		if !contains(cmd.Subcommands, sub) {
			note("kampodra-native subcommand %q on command %q (beyond the frozen spec) — reviewed addition", sub, cmd.Name)
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
	return violations, warnings
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
