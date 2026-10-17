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

// GOLDEN: captured verbatim from real `mariadb --batch` inside the
// estate-shaped podman container (mariadb:11) — O'Neill apostrophe, a
// batch-escaped \n\t pair, a bare NULL, and — pre --binary-as-hex — the
// trap this test exists for: a RAW 0xDEADBEEF blob arriving as invalid
// UTF-8 bytes (json.Marshal would corrupt them to U+FFFD).
func TestNormalizeTSVGoldenRealMaria(t *testing.T) {
	raw := "id\tname\tbalance\tdata\n" +
		"1\tO'Neill\t-5.50\tNULL\n" +
		"2\tline\\nbreak\\ttabbed\t0.00\t" + string([]byte{0xDE, 0xAD, 0xBE, 0xEF}) + "\n"
	got, err := NormalizeTSV(raw)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("invalid JSON (raw blob leaked through?): %v\n%s", err, got)
	}
	if rows[0]["balance"] != "-5.50" || rows[0]["data"] != nil {
		t.Errorf("row 1: %s", got)
	}
	if rows[1]["name"] != "line\nbreak\ttabbed" {
		t.Errorf("batch escapes must unreverse: %s", got)
	}
	blob, ok := rows[1]["data"].(string)
	if !ok {
		t.Fatalf("raw-blob cell must become a base64 STRING, got %T", rows[1]["data"])
	}
	if dec, err := base64.StdEncoding.DecodeString(blob); err != nil || string(dec) != "\xde\xad\xbe\xef" {
		t.Errorf("raw blob must survive as base64: %q (%v)", blob, err)
	}
}

// GOLDEN: captured verbatim from real `psql --csv` in the estate-shaped
// podman container (postgres:16) — the quoted multi-line value proves CSV
// quoting (the reason postgres uses CSV, not TSV), and NULL visibility
// comes from --set=null=NULL.
func TestNormalizeCSVGoldenRealPsql(t *testing.T) {
	raw := "id,name\n" +
		"1,pg-pod\n" +
		"2,\"line\ntab\"\n" +
		"3,NULL\n"
	got, err := NormalizeCSV(raw)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(got), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, got)
	}
	if rows[1]["name"] != "line\ntab" {
		t.Errorf("CSV quoting must carry embedded newlines: %s", got)
	}
	if rows[2]["name"] != nil {
		t.Errorf("--set=null=NULL must surface NULLs: %s", got)
	}
}
