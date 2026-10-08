// Package envfile is the env-file adapter: the env.sh grammar (parse),
// the masking contract (fingerprints NEVER leak values — KEY + value length
// + first 2 characters), and the fingerprint-level local-vs-remote diff.
// Everything here is pure; the command layer owns every side effect (file
// reads, ssh fetches, stdout).
package envfile

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Var is one KEY=VALUE pair (quotes already unformatted).
type Var struct {
	Key   string
	Value string
}

// Kind classifies one diff entry.
type Kind string

const (
	KindAdded     Kind = "added"
	KindRemoved   Kind = "removed"
	KindChanged   Kind = "changed"
	KindUnchanged Kind = "unchanged"
)

// Entry is one diff row for a key in the union of both sides.
type Entry struct {
	Key string
	// Kind is the local-vs-remote verdict (compare VALUES — a fingerprint
	// collision must never hide drift; fingerprints are display-only).
	Kind      Kind
	Local     Var // the effective (last-occurrence) local var when HasLocal
	HasLocal  bool
	Remote    Var
	HasRemote bool
}

// keyRe is the shell's key grammar: ^[A-Za-z_][A-Za-z0-9_]*$.
func isValidKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			// fine anywhere
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Parse ports env_fp_lines' line grammar: blanks, comments (#), non-pairs,
// and invalid key names are skipped; the FIRST '=' splits; ONE pair of
// matching surrounding quotes is formatting, not content (podman --env-file
// does not unquote — the deploy pipeline strips them). Order is preserved.
func Parse(content string) []Var {
	var out []Var
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || !isValidKey(key) {
			continue
		}
		out = append(out, Var{Key: key, Value: stripMatchingQuotes(value)})
	}
	return out
}

// stripMatchingQuotes removes one pair of matching surrounding quotes (only
// when at least 2 chars — the shell's ${#value} -ge 2 guard).
func stripMatchingQuotes(value string) string {
	if utf8.RuneCountInString(value) < 2 {
		return value
	}
	if strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) ||
		strings.HasPrefix(value, `'`) && strings.HasSuffix(value, `'`) {
		return value[1 : len(value)-1]
	}
	return value
}

// fingerprintTail is the value half of the masking contract (everything
// after the key column): `len=<n> <first-2>…` or `len=0      (empty)`.
func fingerprintTail(v Var) string {
	if len(v.Value) == 0 {
		return "len=0      (empty)"
	}
	// ${value:0:2} takes the first two RUNES in bash, not bytes.
	first2 := string([]rune(v.Value)[:minInt(2, utf8.RuneCountInString(v.Value))])
	return fmt.Sprintf("len=%-7d %s…", len(v.Value), first2)
}

// Fingerprint ports THE masking contract: `KEY<len=32> <len + first-2-chars>…`
// (or the "(empty)" shape for len=0). The raw value is never printed.
func Fingerprint(v Var) string {
	return fmt.Sprintf("%-32s %s", v.Key, fingerprintTail(v))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Table renders one fingerprint line per parsed var (the shell's
// env_fp_lines output shape, trailing newline included).
func Table(content string) string {
	var sb strings.Builder
	for _, v := range Parse(content) {
		sb.WriteString(Fingerprint(v))
		sb.WriteString("\n")
	}
	return sb.String()
}

// CountKeyLines ports push's gate (grep -cE '^[A-Za-z_][A-Za-z0-9_]*='):
// the number of lines starting with a valid KEY= — used to refuse pushing a
// file with nothing to push.
func CountKeyLines(content string) int {
	n := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSuffix(line, "\r")
		key, _, found := strings.Cut(line, "=")
		if found && isValidKey(key) {
			n++
		}
	}
	return n
}

// Diff classifies the union of both sides' keys (each side's LAST
// occurrence wins — podman env-file semantics), sorted by key.
func Diff(local, remote []Var) []Entry {
	last := func(vars []Var) map[string]Var {
		m := map[string]Var{}
		for _, v := range vars {
			m[v.Key] = v
		}
		return m
	}
	lm, rm := last(local), last(remote)

	keys := map[string]bool{}
	for k := range lm {
		keys[k] = true
	}
	for k := range rm {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	entries := make([]Entry, 0, len(sorted))
	for _, k := range sorted {
		lv, lok := lm[k]
		rv, rok := rm[k]
		e := Entry{Key: k, Local: lv, HasLocal: lok, Remote: rv, HasRemote: rok}
		switch {
		case lok && !rok:
			e.Kind = KindAdded
		case !lok && rok:
			e.Kind = KindRemoved
		case lv.Value != rv.Value:
			e.Kind = KindChanged
		default:
			e.Kind = KindUnchanged
		}
		entries = append(entries, e)
	}
	return entries
}

// RenderDiff renders the diff body: one +/~/- line per non-unchanged entry
// (changed entries carry BOTH sides' fingerprints) and a summary footer.
// Fingerprint-level only — raw values never appear.
func RenderDiff(entries []Entry) string {
	var sb strings.Builder
	counts := map[Kind]int{}
	for _, e := range entries {
		counts[e.Kind]++
		switch e.Kind {
		case KindAdded:
			fmt.Fprintf(&sb, "+ %-24s %-8s (local: %s)\n", e.Key, e.Kind, fingerprintTail(e.Local))
		case KindRemoved:
			fmt.Fprintf(&sb, "- %-24s %-8s (remote: %s)\n", e.Key, e.Kind, fingerprintTail(e.Remote))
		case KindChanged:
			fmt.Fprintf(&sb, "~ %-24s %-8s (local: %s)\n", e.Key, e.Kind, fingerprintTail(e.Local))
			fmt.Fprintf(&sb, "    (remote: %s)\n", fingerprintTail(e.Remote))
		}
	}
	fmt.Fprintf(&sb, "== env diff summary: %d added, %d removed, %d changed, %d unchanged ==\n",
		counts[KindAdded], counts[KindRemoved], counts[KindChanged], counts[KindUnchanged])
	return sb.String()
}
