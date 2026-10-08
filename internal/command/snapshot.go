package command

import (
	"sort"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/talha7k/kampodra/internal/parity"
)

// BuildSnapshot flattens a cobra tree into the parity guard's view: every
// top-level command with the UNION of its own and all descendants' flags
// (the shell parses flags flat per script, so the union is the parity
// semantic), subcommand names, and positional arg specs by subcommand.
// Hidden commands are excluded — they are not public surface.
func BuildSnapshot(root *cobra.Command) *parity.TreeSnapshot {
	snap := &parity.TreeSnapshot{Commands: map[string]parity.TreeCommand{}}
	for _, sub := range root.Commands() {
		if sub.Hidden {
			continue
		}
		snap.Commands[sub.Name()] = snapshotCommand(sub)
	}
	return snap
}

func snapshotCommand(cmd *cobra.Command) parity.TreeCommand {
	tc := parity.TreeCommand{
		Flags:       map[string]bool{},
		Subcommands: map[string]bool{},
		Args:        map[string][]string{},
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" || f.Name == "version" {
				return // universal cobra affordances, not shell surface
			}
			tc.Flags["--"+f.Name] = true
		})
		for _, sub := range c.Commands() {
			if sub.Hidden {
				continue
			}
			tc.Subcommands[sub.Name()] = true
			if spec := positionalArgs(sub); len(spec) > 0 {
				tc.Args[sub.Name()] = append(tc.Args[sub.Name()], spec...)
			}
			walk(sub)
		}
	}
	walk(cmd)
	return tc
}

func positionalArgs(cmd *cobra.Command) []string {
	use := cmd.Use
	// cobra Use strings carry the positional shape after the name, e.g.
	// `provision <color>` or `download <object>`.
	name := cmd.Name()
	rest := use
	if len(use) > len(name) && use[:len(name)] == name {
		rest = use[len(name):]
	}
	fields := nonEmptyFields(rest)
	args := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) > 1 && (f[0] == '<' || f[0] == '[' && len(f) > 1 && f[1] == '<') {
			args = append(args, trimBrackets(f))
		}
	}
	return args
}

func nonEmptyFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		} else if start == -1 {
			start = i
		}
	}
	return out
}

func trimBrackets(s string) string {
	for len(s) > 0 && (s[0] == '[' || s[0] == '(') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ']' || s[len(s)-1] == ')') {
		s = s[:len(s)-1]
	}
	return s
}

// sortedNames is a tiny helper used by tests of the snapshot shape.
func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
