package command

import (
	"regexp"
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
	// The command's OWN positional args are command-level surface (e.g.
	// `ssh [<cmd>...]`) and land under the "" key. Only the top command of
	// this snapshot contributes there — subcommand args keep their own key.
	if spec := positionalArgs(cmd); len(spec) > 0 {
		tc.Args[""] = append(tc.Args[""], spec...)
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		visit := func(f *pflag.Flag) {
			// help is cobra's universal affordance. version is NOT skipped:
			// the auto --version lives on the root (never snapshotted), so a
			// --version seen here is real shell surface (deploy --version).
			if f.Name == "help" {
				return
			}
			tc.Flags["--"+f.Name] = true
		}
		c.Flags().VisitAll(visit)
		// Persistent flags are inherited by every subcommand — real public
		// surface; cobra only merges them into Flags() at execution time.
		c.PersistentFlags().VisitAll(visit)
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
	// `provision <color>` or `download <object>`. Bracketed [--flag <value>]
	// groups in a Use string are usage help, never positional specs — strip
	// them first so `[--profile <name>]` does not read as a `<name>` arg.
	name := cmd.Name()
	rest := use
	if len(use) > len(name) && use[:len(name)] == name {
		rest = use[len(name):]
	}
	rest = flagHelpGroupRe.ReplaceAllString(rest, "")
	fields := nonEmptyFields(rest)
	args := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) > 1 && (f[0] == '<' || f[0] == '[' && len(f) > 1 && f[1] == '<') {
			args = append(args, trimBrackets(f))
		}
	}
	return args
}

// flagHelpGroupRe matches a bracketed flag group like `[--host root@<ip>]`.
var flagHelpGroupRe = regexp.MustCompile(`\[--[^\]]*\]`)

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
