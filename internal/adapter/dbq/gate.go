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

// ValidateWrite is db exec's write gate: exactly ONE statement, and its
// first keyword must be UPDATE, INSERT, DELETE, or REPLACE. Reads are
// refused WITH the pointer to db query; DDL (CREATE/ALTER/DROP/ATTACH/
// DETACH/PRAGMA/VACUUM/REINDEX) and anything else fails with an error
// naming the gate (exit 2 on both sides of the wire — the same contract
// the helper enforces again on the VM). One gated statement per
// invocation: a write that mutates more than the operator spelled out is
// not a write this surface will make.
func ValidateWrite(stmt string) error {
	s := strings.TrimSpace(stmt)
	if s == "" {
		return errors.New("write gate: a statement is required (--sql)")
	}
	switch kw := firstKeyword(s); kw {
	case "UPDATE", "INSERT", "DELETE", "REPLACE":
		// the allowed verbs — plain data mutation, no schema, no attach
	case "SELECT", "WITH":
		return fmt.Errorf("write gate: %s is a read — db exec runs ONE write statement (UPDATE|INSERT|DELETE|REPLACE); reads belong in db query", kw)
	default:
		return fmt.Errorf("write gate: statements must start with UPDATE, INSERT, DELETE, or REPLACE — got %.40q (DDL/PRAGMA/ATTACH are refused; reads belong in db query)", s)
	}
	body := s
	if strings.HasSuffix(body, ";") {
		body = strings.TrimSpace(strings.TrimSuffix(body, ";"))
	}
	if statementTerminator(body) >= 0 {
		return errors.New("write gate: exactly ONE statement per invocation — a statement-terminating ';' was found")
	}
	return nil
}

// statementTerminator returns the index of the first `;` that actually
// terminates a statement: semicolons inside string literals, quoted
// identifiers, backticks, and comments don't count ('a;b' is a value, not
// a separator). -1 = single statement.
// lexState enumerates the SQL-scanner states for statementTerminator.
type lexState int

const (
	normal lexState = iota
	inSingle
	inDouble
	inBracket
	inBacktick
	inLineComment
	inBlockComment
)

func statementTerminator(s string) int {
	depth := 0 // block-comment nesting
	st := normal
	for i := 0; i < len(s); i++ {
		c := s[i]
		two := ""
		if i+1 < len(s) {
			two = s[i : i+2]
		}
		// statement end — only reachable in normal state (a ';' inside any
		// literal/comment is consumed by the state handlers below)
		if st == normal {
			if two == "--" {
				st = inLineComment
				i++
				continue
			}
			if two == "/*" {
				st = inBlockComment
				depth = 1
				i++
				continue
			}
			switch c {
			case '\'':
				st = inSingle
				continue
			case '"':
				st = inDouble
				continue
			case '[':
				st = inBracket
				continue
			case '`':
				st = inBacktick
				continue
			case ';':
				return i
			}
			continue
		}
		// inside a literal/comment — the state handler decides
		st = stepLexer(st, c, two, &depth, &i, s)
	}
	return -1
}

// stepLexer advances one byte inside a non-normal state and returns the
// next state. i may be advanced past a two-byte escape (” , "" , /* , */);
// depth tracks block-comment nesting.
func stepLexer(st lexState, c byte, two string, depth *int, i *int, s string) lexState {
	switch st {
	case inSingle:
		if c == '\'' {
			if strings.HasPrefix(s[*i:], "''") { // '' escapes a quote in the literal
				*i++
				return inSingle
			}
			return normal
		}
	case inDouble:
		if strings.HasPrefix(s[*i:], "\"\"") { // "" escapes a quote
			*i++
			return inDouble
		}
		if c == '"' {
			return normal
		}
	case inBracket:
		if c == ']' {
			return normal
		}
	case inBacktick:
		if c == '`' {
			return normal
		}
	case inLineComment:
		if c == '\n' {
			return normal
		}
	case inBlockComment:
		switch two {
		case "/*":
			*depth++
			*i++
		case "*/":
			*depth--
			*i++
			if *depth == 0 {
				return normal
			}
		}
	}
	return st
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

// ReadWriteDSN is db exec's open mode: an EXISTING file, read-write.
// mode=rw (not rwc) means a missing path is a driver error — a typo'd
// --db must never materialize as a fresh empty database. immutable=0
// keeps the driver honest about concurrent writers.
func ReadWriteDSN(dbPath string) string {
	return "file:" + dbPath + "?mode=rw&immutable=0"
}
