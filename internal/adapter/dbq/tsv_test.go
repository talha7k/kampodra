package dbq

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// TSV normalization: header + tab rows → flat JSON, batch escapes
// reversed, hex blobs → base64, bare NULL → null, torn rows refuse.
func TestNormalizeTSV(t *testing.T) {
	got, err := NormalizeTSV("id\tname\tbalance\n1\tO'Neill\t-5.5\n2\tline\\nbreak\tNULL\n3\tbin\t0xDEADBEEF")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, got)
	}
	if len(rows) != 3 || rows[0]["name"] != "O'Neill" || rows[1]["balance"] != nil {
		t.Errorf("rows malformed: %s", got)
	}
	if rows[1]["name"] != "line\nbreak" {
		t.Errorf("batch \\n escape must unreverse: %s", got)
	}
	blob := rows[2]["balance"]
	s, _ := blob.(string)
	if dec, err := base64.StdEncoding.DecodeString(s); err != nil || string(dec) != "\xde\xad\xbe\xef" {
		t.Errorf("hex blob must become base64: %v (%s)", blob, got)
	}
	// header-only → empty array, not an error (a schema with zero rows)
	empty, err := NormalizeTSV("id\tname\n")
	if err != nil || empty != "[]" {
		t.Errorf("header-only TSV = []: %q, %v", empty, err)
	}
	// torn rows refuse
	if _, err := NormalizeTSV("a\tb\n1\n"); err == nil {
		t.Errorf("a row with a missing field must refuse")
	}
	if _, err := NormalizeTSV(""); err == nil {
		t.Errorf("empty payload must refuse")
	}
}

// The JSON engines and the TSV engines agree on the row STRUCTURE — flat
// [{col:val},...] with the same column names. Values differ by protocol
// honesty: batch text has no types, so TSV values are strings; typed
// protocols (Hrana) carry native numbers. Guessing types from text would
// corrupt string codes ("007" → 7) — never done.
func TestTSVAndHranaShapesAgree(t *testing.T) {
	tsv, err := NormalizeTSV("id\tname\n7\tseven")
	if err != nil {
		t.Fatal(err)
	}
	hr, err := NormalizeHranaResponse(`{"results":[{"type":"ok","response":{"result":{"cols":[{"name":"id"},{"name":"name"}],"rows":[[{"type":"integer","value":7},{"type":"text","value":"seven"}]]}}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var tsvRows, hrRows []map[string]any
	if err := json.Unmarshal([]byte(tsv), &tsvRows); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(hr), &hrRows); err != nil {
		t.Fatal(err)
	}
	if len(tsvRows) != 1 || len(hrRows) != 1 {
		t.Fatalf("row counts diverge: %d vs %d", len(tsvRows), len(hrRows))
	}
	for col := range hrRows[0] {
		if _, ok := tsvRows[0][col]; !ok {
			t.Errorf("TSV row missing column %q from the Hrana shape", col)
		}
	}
	if tsvRows[0]["name"] != "seven" || hrRows[0]["name"] != "seven" {
		t.Errorf("text values must match across engines: %s vs %s", tsv, hr)
	}
	if tsvRows[0]["id"] != "7" {
		t.Errorf("TSV values are strings (the wire has no types): %v", tsvRows[0]["id"])
	}
	if hrRows[0]["id"] != float64(7) {
		t.Errorf("Hrana values are typed: %v", hrRows[0]["id"])
	}
}
