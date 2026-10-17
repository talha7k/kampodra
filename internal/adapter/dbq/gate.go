// The read-only gate, stdlib-only ON PURPOSE: this file is embedded
// VERBATIM into the materialized sqlq helper module (one source of truth —
// the gate kampodra enforces client-side is byte-identical to the one the
// helper enforces on the VM). Nothing here may import anything but the
// standard library.

package dbq

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateReadOnly is the db surface's read-only gate: exactly ONE
// statement, and its first keyword must be SELECT or WITH. Everything else
// fails with an error naming the gate (the command exits 2, the helper
// exits 2 — the same contract enforced on both sides of the wire).
func ValidateReadOnly(stmt string) error {
	s := strings.TrimSpace(stmt)
	if s == "" {
		return errors.New("read-only gate: a statement is required (--sql or --file)")
	}
	if kw := firstKeyword(s); kw != "SELECT" && kw != "WITH" {
		return fmt.Errorf("read-only gate: statements must start with SELECT or WITH — got %.40q", s)
	}
	body := s
	if strings.HasSuffix(body, ";") {
		body = strings.TrimSpace(strings.TrimSuffix(body, ";"))
	}
	if statementTerminator(body) >= 0 {
		return errors.New("read-only gate: exactly ONE statement per invocation — a statement-terminating ';' was found")
	}
	return nil
}

// statementTerminator returns the index of the first `;` that actually
// terminates a statement: semicolons inside string literals, quoted
// identifiers, backticks, and comments don't count ('a;b' is a value, not
// a separator). -1 = single statement.
func statementTerminator(s string) int {
	type state int
	const (
		normal state = iota
		inSingle
		inDouble
		inBracket
		inBacktick
		inLineComment
		inBlockComment
	)
	depth := 0 // block-comment nesting
	st := normal
	for i := 0; i < len(s); i++ {
		c := s[i]
		two := ""
		if i+1 < len(s) {
			two = s[i : i+2]
		}
		switch st {
		case inSingle:
			if c == '\'' {
				// '' escapes a quote inside the literal
				if strings.HasPrefix(s[i:], "''") {
					i++
					continue
				}
				st = normal
			}
		case inDouble:
			if strings.HasPrefix(s[i:], "\"\"") {
				i++
				continue
			}
			if c == '"' {
				st = normal
			}
		case inBracket:
			if c == ']' {
				st = normal
			}
		case inBacktick:
			if c == '`' {
				st = normal
			}
		case inLineComment:
			if c == '\n' {
				st = normal
			}
		case inBlockComment:
			switch two {
			case "/*":
				depth++
				i++
			case "*/":
				depth--
				i++
				if depth == 0 {
					st = normal
				}
			}
		default:
			switch {
			case two == "--":
				st = inLineComment
				i++
			case two == "/*":
				st = inBlockComment
				depth = 1
				i++
			case c == '\'':
				st = inSingle
			case c == '"':
				st = inDouble
			case c == '[':
				st = inBracket
			case c == '`':
				st = inBacktick
			case c == ';':
				return i
			}
		}
	}
	return -1
}

// firstKeyword returns the statement's first BARE keyword (uppercased):
// leading whitespace, `--` line comments, and (nested) `/* */` block
// comments are skipped; quoted identifiers are not keywords.
func firstKeyword(stmt string) string {
	s := strings.TrimLeft(stmt, " \t\r\n")
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return "" // comment-only statement
			}
			s = strings.TrimLeft(s[i+1:], " \t\r\n")
		case strings.HasPrefix(s, "/*"):
			rest, ok := skipBlockComment(s)
			if !ok {
				return "" // unterminated comment
			}
			s = strings.TrimLeft(rest, " \t\r\n")
		default:
			i := 0
			for i < len(s) && isASCIILetter(s[i]) {
				i++
			}
			if i == 0 {
				return ""
			}
			return strings.ToUpper(s[:i])
		}
	}
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// skipBlockComment consumes one (possibly nested) /* */ comment — sqlite
// allows nesting, so a bare `*/` search would unbalance.
func skipBlockComment(s string) (string, bool) {
	depth := 1
	i := 2
	for i < len(s) && depth > 0 {
		switch {
		case strings.HasPrefix(s[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(s[i:], "*/"):
			depth--
			i += 2
		default:
			i++
		}
	}
	if depth != 0 {
		return "", false
	}
	return s[i:], true
}

// ValidateDbPath guards remote-command composition: the db path rides
// verbatim into shell commands (sqlite3 .backup, the helper's argv) and
// into the DSN — shell metacharacters, whitespace, option-leading values,
// and URI-special characters fail closed.
func ValidateDbPath(p string) error {
	if p == "" {
		return errors.New("--db is required: the sqlite file's path ON THE VM")
	}
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("--db %q must be a file path, not an option", p)
	}
	if strings.ContainsAny(p, ";|&<>$`\\\"' \t\n\r\x00?#") {
		return fmt.Errorf("--db %q must be a plain path — no shell metacharacters, whitespace, or URI '?#' characters (the remote commands compose it verbatim)", p)
	}
	return nil
}

// ReadOnlyDSN is the driver-level backstop: the helper opens the db with
// mode=ro&immutable=0 so a write fails at the driver even if both gates
// were bypassed.
func ReadOnlyDSN(dbPath string) string {
	return "file:" + dbPath + "?mode=ro&immutable=0"
}
