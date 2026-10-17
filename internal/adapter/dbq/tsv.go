// TSV output normalization for the server-engine legs (MariaDB --batch,
// psql --no-align --field-separator tab): flattens header+rows into the
// same [{col:val},...] shape the file engines print. Handles the clients'
// batch-mode escapes (\n \t \\ in values) and --binary-as-hex blobs
// (0x-prefixed → base64, the JSON engines' blob convention).

package dbq

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// NormalizeTSV flattens client batch output: the FIRST line is the header
// row; every following line is one record split on tabs. Values are
// unescaped per the batch-mode rules (\\ → \, \n → newline, \t → tab, \0
// → NUL byte as \u0000) and hex blobs (0x…/0X…, even-length) become
// base64 strings. NULL (the bare literal) becomes JSON null. Rows with a
// different column count than the header are a hard error — a torn or
// misparsed table must not masquerade as data.
func NormalizeTSV(raw string) (string, error) {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return "", fmt.Errorf("empty TSV payload — the client produced no table")
	}
	cols := strings.Split(lines[0], "\t")
	out := make([]byte, 0, 64*len(lines)+16)
	out = append(out, '[')
	for i, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		cells := strings.Split(line, "\t")
		if len(cells) != len(cols) {
			return "", fmt.Errorf("TSV row %d has %d fields, header has %d — refusing to guess the mapping", i+1, len(cells), len(cols))
		}
		if i > 0 {
			out = append(out, ',')
		} else {
			out = out[:0]
			out = append(out, '[')
		}
		out = append(out, '{')
		for j, cell := range cells {
			if j > 0 {
				out = append(out, ',')
			}
			out = append(out, jsonString(cols[j])...)
			out = append(out, ':')
			out = append(out, tsvValue(cell)...)
		}
		out = append(out, '}')
	}
	out = append(out, ']')
	return string(out), nil
}

// tsvValue maps one cell to a JSON scalar: bare NULL → null, hex blob →
// base64 string, everything else → unescaped string.
func tsvValue(cell string) []byte {
	if cell == "NULL" {
		return []byte("null")
	}
	if len(cell) >= 3 && (strings.HasPrefix(cell, "0x") || strings.HasPrefix(cell, "0X")) {
		if raw, err := hex.DecodeString(cell[2:]); err == nil && len(raw) > 0 {
			return []byte(jsonString(base64.StdEncoding.EncodeToString(raw)))
		}
	}
	return []byte(jsonString(unescapeBatch(cell)))
}

// unescapeBatch reverses the clients' batch-mode escapes. Unknown
// backslash pairs keep the backslash literally (never invent content).
func unescapeBatch(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '0':
			b.WriteString("\\u0000")
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
