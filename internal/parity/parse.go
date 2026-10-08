// Package parity is kampodra's command-parity guard: it derives the shell
// predecessor's command surface from committed script files (the historical
// generator mechanics), and compares the FROZEN golden against the Go
// tree under the NOT_YET_PORTED baseline ratchet. Missing frozen-spec
// coverage fails; a baseline entry whose command is already ported fails
// too, forcing deletion as ports land. Kampodra-native additions beyond the
// frozen spec are warning-only, review-flagged.
package parity

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// CommandRef is one entry of the cli.js dispatch map, in file order.
type CommandRef struct {
	Name   string
	Script string
}

// ScriptSurface is the parsed command surface of one committed bash script.
type ScriptSurface struct {
	// Flags is the sorted set of long flags any case branch accepts.
	Flags []string
	// Subcommands is the sorted dispatch set (case on $1/$SUB/$CMD/$cmd).
	Subcommands []string
	// Args maps a subcommand name to its positional argument specs as
	// documented in the script's header usage lines ("" would be
	// command-level args; none exist in the shell today).
	Args map[string][]string
	// Delegates lists scripts this one execs with the FULL arg vector
	// passthrough (`exec bash "$HERE/x.sh" "$@"` — deploy.sh ->
	// deploy-lifecycle.sh); their surfaces merge into the command's golden
	// entry.
	Delegates []string
	// TailExecs lists scripts execed with FIXED args (`exec bash
	// "$HERE/bluegreen.sh" status` — status.sh's closing pair view). Fixed-arg
	// execs do not pass the caller's surface through, so they never merge —
	// they are recorded for traceability only.
	TailExecs []string
}

var (
	// cli.js: `  deploy: "deploy.sh",` / `  "vm-prepare": "vm-prepare.sh",`
	commandsLineRe = regexp.MustCompile(`^\s+"?([A-Za-z0-9_-]+)"?\s*:\s*"([A-Za-z0-9_-]+\.sh)",?\s*$`)
	// bash case branch whose FIRST token is a long flag: `    --dry-run) DRY_RUN=1; shift ;;`
	flagBranchRe = regexp.MustCompile(`^\s+(--[a-z][a-z0-9-]*)\)`)
	// dispatch case heads: "$1", "${1:-}", "$SUB", "$CMD", "$cmd"
	caseHeadRe = regexp.MustCompile(`^(\s*)case\s+"\$(\{1:-\}|SUB|CMD|cmd|1)"\s+in\b`)
	// dispatch branch label at exactly head-indent + 2 spaces: `  status)` / `  list|prune)`
	branchRe = regexp.MustCompile(`^(\s+)([A-Za-z][A-Za-z0-9|_-]*)\)`)
	// script execs: `exec bash "$HERE/deploy-lifecycle.sh" "$@"` (surface
	// passthrough) vs `exec bash "$HERE/bluegreen.sh" status` (fixed tail)
	delegateRe = regexp.MustCompile(`exec bash "\$HERE/([a-z0-9-]+\.sh)"[^"]*"\$@"`)
	tailExecRe = regexp.MustCompile(`exec bash "\$HERE/([a-z0-9-]+\.sh)"`)
	// header usage line: `#   kampodine deploy --version <sha7>  # note`
	// (the regex matches the RETIRED shell scripts' literal text — that
	// spelling is the parsed artifact, not kampodra naming).
	usageLineRe = regexp.MustCompile(`^#   kampodine ([a-z][a-z0-9-]*)(?:\s+(.*))?$`)
	// a bracket group that OPENS with a flag: `[--host …]`, `[--ttl 300]`
	flagBracketOpenRe = regexp.MustCompile(`^\[.*--[a-z]`)
	// a placeholder token that may be a positional arg: starts with < after
	// an optional bracket, e.g. `<color>`, `<cmd>...]`
	placeholderRe = regexp.MustCompile(`^\[?\(?<[a-z0-9_-]+>`)
	knownSubRe    = regexp.MustCompile(`^[a-z][a-z-]*$`)
)

// ParseCommandsJS extracts the ordered command → script map from the
// committed cli.js dispatch object.
func ParseCommandsJS(src string) ([]CommandRef, error) {
	lines := strings.Split(src, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "const commands = {") {
			start = i + 1
			break
		}
	}
	if start == -1 {
		return nil, fmt.Errorf("cli.js: no `const commands = {` dispatch block found")
	}
	var refs []CommandRef
	for _, line := range lines[start:] {
		if strings.HasPrefix(line, "};") {
			break
		}
		if m := commandsLineRe.FindStringSubmatch(line); m != nil {
			refs = append(refs, CommandRef{Name: m[1], Script: m[2]})
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("cli.js: dispatch block parsed to zero commands")
	}
	return refs, nil
}

// ParseScript extracts the command surface of one committed bash script.
// The case statements are the authoritative accepted-flag/dispatch surface;
// the header usage lines (the same lines every usage() greps) contribute the
// positional-argument documentation.
func ParseScript(name, src string) (ScriptSurface, error) {
	flagSet := map[string]bool{}
	subSet := map[string]bool{}
	delegates := map[string]bool{}
	tailExecs := map[string]bool{}
	surface := ScriptSurface{}

	type caseBlock struct{ indent string }
	var stack []caseBlock

	lines := strings.Split(src, "\n")
	for _, line := range lines {
		// A branch line whose first token is a long flag is an accepted
		// flag — wherever it sits (every flag loop is itself a case on $1).
		if m := flagBranchRe.FindStringSubmatch(line); m != nil && m[1] != "--help" {
			flagSet[m[1]] = true
		}
		if m := tailExecRe.FindStringSubmatch(line); m != nil {
			if delegateRe.MatchString(line) {
				delegates[m[1]] = true
			} else {
				tailExecs[m[1]] = true
			}
		}
		if head := caseHeadRe.FindStringSubmatch(line); head != nil {
			stack = append(stack, caseBlock{indent: head[1]})
			continue
		}
		if len(stack) > 0 {
			top := stack[len(stack)-1]
			if line == top.indent+"esac" || strings.HasPrefix(line, top.indent+"esac ") {
				stack = stack[:len(stack)-1]
				continue
			}
			// Branch labels of the top-of-stack dispatch case only.
			if bm := branchRe.FindStringSubmatch(line); bm != nil && bm[1] == top.indent+"  " {
				for _, tok := range strings.Split(bm[2], "|") {
					if knownSubRe.MatchString(tok) && tok != "help" {
						subSet[tok] = true
					}
				}
			}
		}
	}
	surface.Flags = sortedKeys(flagSet)
	surface.Subcommands = sortedKeys(subSet)
	surface.Delegates = sortedKeys(delegates)
	surface.TailExecs = sortedKeys(tailExecs)

	args := map[string][]string{}
	parseHeaderArgs(src, surface.Subcommands, args)
	if len(args) > 0 {
		surface.Args = args
	}
	return surface, nil
}

// parseHeaderArgs applies the positional-arg state machine to the script's
// `#   kampodine <command> …` header usage lines. Rules (derived from every
// usage line in the committed v0.5 scripts):
//   - a `--flag` token documents a flag (already covered by the case parse);
//     the NEXT token is its value placeholder, never a positional
//   - a token that opens a bracket WITH a flag (`[--host root@<ip>]`)
//     consumes the rest of that bracket group as flag documentation
//   - a bare `--` (the `shell exec --` separator) is just a separator
//   - a placeholder token (`<color>`, `<cmd>...`) directly after a known
//     subcommand (or the command itself) is that subcommand's positional
func parseHeaderArgs(src string, subs []string, into map[string][]string) {
	subSet := map[string]bool{}
	for _, s := range subs {
		subSet[s] = true
	}
	for _, line := range strings.Split(src, "\n") {
		m := usageLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		command, rest := m[1], m[2]
		// The usage lines carry inline comments; cut at the first `#`.
		if idx := strings.Index(rest, "#"); idx >= 0 {
			rest = rest[:idx]
		}
		context := "" // "" = command-level
		prev := ""    // "", "flag", "sub", "other"
		flagBracket := false
		for _, tok := range strings.Fields(rest) {
			switch {
			case tok == "--":
				prev = "other"
			case flagBracketOpenRe.MatchString(tok):
				flagBracket = true // `[--host …` — the group is flag doc
				prev = "flag"
			case strings.HasPrefix(tok, "--"):
				flagBracket = false
				prev = "flag"
			case flagBracket:
				// inside a `[--host root@<ip>]` group — flag documentation;
				// the closing bracket ends it
				if strings.Contains(tok, "]") {
					flagBracket = false
				}
			case subSet[tok] && (prev == "" || prev == "sub" || prev == "other"):
				context = tok
				prev = "sub"
			case strings.Contains(tok, "<") && !strings.HasPrefix(tok, "-") &&
				placeholderRe.MatchString(tok) && (prev == "sub" || prev == "other" || prev == ""):
				if context != "" {
					into[context] = appendUnique(into[context], strings.Trim(tok, "[]()"))
				} else if command != "" {
					into[""] = appendUnique(into[""], strings.Trim(tok, "[]()"))
				}
				prev = "other"
			default:
				prev = "other"
			}
		}
	}
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
