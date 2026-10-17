// Hrana v2 response normalization for the libsql leg — pure functions,
// Go-side (this file is NOT embedded into the sqlq helper; only gate.go
// carries the stdlib-only constraint). The envelope flattens to the same
// row shape the sqlite helper prints, so agents parse one format per
// engine-class, not one per engine.

package dbq

import (
	crand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// jsonString encodes a Go string as a JSON string literal (the stdlib's
// escaping, not a shell quoting exercise).
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		// Marshal on a string cannot fail; belt for the impossible.
		return `""`
	}
	return string(b)
}

type hranaPipelineResponse struct {
	Results []struct {
		Type  string `json:"type"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Response *struct {
			Result struct {
				Cols []struct {
					Name string `json:"name"`
				} `json:"cols"`
				Rows [][]json.RawMessage `json:"rows"`
			} `json:"result"`
		} `json:"response"`
	} `json:"results"`
}

type hranaValue struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// normalizeHrana flattens the pipeline envelope: results[0] error → Go
// error with the server's message; otherwise [{col:val},...] with
// integers/floats as numbers, text as strings, blobs base64 (the sqlite
// leg's convention), nulls null. Non-JSON input (connection refused HTML,
// empty body) is an error quoting the raw prefix — never silently empty.
func normalizeHrana(raw string) (string, error) {
	var resp hranaPipelineResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		prefix := raw
		if len(prefix) > 120 {
			prefix = prefix[:120]
		}
		return "", fmt.Errorf("sqld returned a non-Hrana response (%v): %.120q", err, prefix)
	}
	if len(resp.Results) == 0 {
		return "", fmt.Errorf("sqld returned an empty pipeline result")
	}
	r := resp.Results[0]
	if r.Type == "error" || r.Error != nil {
		msg := "unknown sqld error"
		if r.Error != nil && r.Error.Message != "" {
			msg = r.Error.Message
		}
		return "", fmt.Errorf("sqld rejected the statement: %s", msg)
	}
	cols := r.Response.Result.Cols
	rows := r.Response.Result.Rows
	out := make([]byte, 0, 64*len(rows)+32)
	out = append(out, '[')
	for i, row := range rows {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '{')
		for j, cell := range row {
			if j > 0 {
				out = append(out, ',')
			}
			name := ""
			if j < len(cols) {
				name = cols[j].Name
			}
			out = append(out, jsonString(name)...)
			out = append(out, ':')
			out = append(out, decodeHranaValue(cell)...)
		}
		out = append(out, '}')
	}
	out = append(out, ']')
	return string(out), nil
}

// isJSONInteger reports whether s is a bare signed integer (the only
// shape safely re-emitted as a JSON number).
func isJSONInteger(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i = 1
	}
	if i >= len(s) {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// decodeHranaValue maps the typed envelope value to plain JSON: numbers
// pass through, text/blob are strings (blob already arrives base64),
// null is null. Unknown types surface as their raw envelope (visible,
// never invented).
func decodeHranaValue(cell json.RawMessage) []byte {
	var v hranaValue
	if err := json.Unmarshal(cell, &v); err != nil {
		return cell // pass through; visibility over invention
	}
	switch v.Type {
	case "null":
		return []byte("null")
	case "integer":
		// Hrana encodes integers as DIGIT STRINGS (JS float-precision
		// convention — real sqld sends {"type":"integer","value":"7"}).
		// Emit the digits raw as a JSON number: precision-preserving
		// (no float round-trip) and type-consistent with the sqlite leg.
		var digits string
		if json.Unmarshal(v.Value, &digits) == nil && isJSONInteger(digits) {
			return []byte(digits)
		}
		return v.Value
	case "float":
		return v.Value
	case "text":
		var s string
		if json.Unmarshal(v.Value, &s) == nil {
			return []byte(jsonString(s))
		}
		return v.Value
	case "blob":
		var b64 string
		if json.Unmarshal(v.Value, &b64) == nil {
			// verify it IS base64; re-emit canonically
			if _, err := base64.StdEncoding.DecodeString(b64); err == nil {
				return []byte(jsonString(b64))
			}
		}
		return v.Value
	default:
		return cell
	}
}

// GeneratePassword mints an agent-role password: 24 crypto-random bytes,
// hex — 48 chars, no quoting hazards in any SQL dialect or env file.
func GeneratePassword() (string, error) {
	var b [24]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
