package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sqlq helper's contract, tested in-process against the REAL driver:
//   - one SELECT/WITH statement on stdin → JSON rows on stdout
//   - gate violations exit 2 (the client pre-gates; this is the VM-side copy)
//   - the DSN opens mode=ro&immutable=0 — writes fail at the DRIVER even
//     if both gates were bypassed (defense in depth)

func setupDb(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (k INTEGER PRIMARY KEY, txt TEXT, r REAL, b BLOB)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t VALUES (1, 'hello', 1.5, x'00ff'), (2, NULL, NULL, NULL)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return path
}

func TestRunQueryReturnsJSONRows(t *testing.T) {
	path := setupDb(t)
	var out bytes.Buffer
	code := run(path, strings.NewReader("SELECT k, txt, r, b FROM t ORDER BY k"), &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	// The estate's tooling parses [{"col":val},...] — integers stay
	// numbers, text stays strings, REAL stays a number, blobs base64,
	// NULLs null. encoding/json sorts object keys, so this is exact.
	want := `[{"b":"AP8=","k":1,"r":1.5,"txt":"hello"},{"b":null,"k":2,"r":null,"txt":null}]` + "\n"
	if got := out.String(); got != want {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

func TestRunZeroRowsIsEmptyArray(t *testing.T) {
	path := setupDb(t)
	var out bytes.Buffer
	code := run(path, strings.NewReader("SELECT * FROM t WHERE k = 999"), &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	if got := out.String(); got != "[]\n" {
		t.Errorf("zero rows = %q, want [] (null would break the parsers)", got)
	}
}

func TestRunWithStatement(t *testing.T) {
	path := setupDb(t)
	var out bytes.Buffer
	code := run(path, strings.NewReader("WITH c AS (SELECT k*10 AS n FROM t) SELECT n FROM c ORDER BY n DESC"), &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("run code = %d, want 0", code)
	}
	want := `[{"n":20},{"n":10}]` + "\n"
	if got := out.String(); got != want {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

func TestRunGateViolationsExit2(t *testing.T) {
	path := setupDb(t)
	for _, stmt := range []string{
		"INSERT INTO t VALUES (9, 'x', 0, NULL)",
		"UPDATE t SET txt = 'owned'",
		"DELETE FROM t",
		"ATTACH DATABASE '/tmp/x.db' AS evil",
		"PRAGMA journal_mode=DELETE",
		"DROP TABLE t",
	} {
		var out, errw bytes.Buffer
		code := run(path, strings.NewReader(stmt), &out, &errw)
		if code != 2 {
			t.Errorf("run(%q) code = %d, want 2 (gate exit)", stmt, code)
		}
		if !strings.Contains(errw.String(), "read-only gate") {
			t.Errorf("run(%q) stderr %q must name the gate", stmt, errw.String())
		}
		if out.Len() != 0 {
			t.Errorf("run(%q) wrote rows %q on a gate rejection", stmt, out.String())
		}
	}
	// The table must still hold its original rows — nothing executed.
	var out bytes.Buffer
	if code := run(path, strings.NewReader("SELECT count(*) AS n FROM t"), &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("count code = %d", code)
	}
	if got := out.String(); got != `[{"n":2}]`+"\n" {
		t.Errorf("post-gate count = %q — a rejected statement mutated the fixture", got)
	}
}

func TestRunUsageErrorsExit2(t *testing.T) {
	var out, errw bytes.Buffer
	if code := run("", strings.NewReader("SELECT 1"), &out, &errw); code != 2 {
		t.Errorf("missing -db code = %d, want 2", code)
	}
	if code := run(setupDb(t), strings.NewReader("   "), &out, &errw); code != 2 {
		t.Errorf("empty stdin code = %d, want 2", code)
	}
	if code := run("/data/x.db?mode=rwc", strings.NewReader("SELECT 1"), &out, &errw); code != 2 {
		t.Errorf("URI-special db path code = %d, want 2", code)
	}
}

func TestRunMissingDbFileFailsExit1(t *testing.T) {
	var out, errw bytes.Buffer
	code := run(filepath.Join(t.TempDir(), "nope.db"), strings.NewReader("SELECT 1"), &out, &errw)
	if code != 1 {
		t.Errorf("missing db code = %d, want 1 (ran and failed, not a gate rejection)", code)
	}
	if errw.Len() == 0 {
		t.Errorf("missing db must explain itself on stderr")
	}
}

// --- the write mode (db exec) ---------------------------------------------
//
// The same helper, opened READ-WRITE for ONE gated DML statement: the
// client (kampodra db exec) pre-gates with --ack-write; this VM-side copy
// enforces the identical write gate again, opens mode=rw (never creates),
// and prints {"changes":N,"statement":"<first line>"} as the audit trail.

func TestRunWriteReturnsChangesAndStatement(t *testing.T) {
	path := setupDb(t)
	var out, errw bytes.Buffer
	code := runWrite(path, strings.NewReader("UPDATE t SET txt = 'moved' WHERE k = 1"), &out, &errw)
	if code != 0 {
		t.Fatalf("runWrite code = %d, stderr: %s", code, errw.String())
	}
	// encoding/json sorts object keys — the shape is exact.
	if got := out.String(); got != `{"changes":1,"statement":"UPDATE t SET txt = 'moved' WHERE k = 1"}`+"\n" {
		t.Errorf("result = %q, want the {changes,statement} object", got)
	}
	// the mutation really happened
	var txt string
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow("SELECT txt FROM t WHERE k = 1").Scan(&txt); err != nil {
		t.Fatal(err)
	}
	if txt != "moved" {
		t.Errorf("txt = %q, want 'moved' — the write must land", txt)
	}
}

func TestRunWriteMultiLineStatementAuditsFirstLine(t *testing.T) {
	path := setupDb(t)
	var out, errw bytes.Buffer
	code := runWrite(path, strings.NewReader("UPDATE t SET txt = 'moved'\nWHERE k = 2"), &out, &errw)
	if code != 0 {
		t.Fatalf("runWrite code = %d, stderr: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), `"statement":"UPDATE t SET txt = 'moved'"`) {
		t.Errorf("result = %q — the statement field must be the FIRST line (audit trail)", out.String())
	}
}

func TestRunWriteReportsZeroChanges(t *testing.T) {
	path := setupDb(t)
	var out, errw bytes.Buffer
	code := runWrite(path, strings.NewReader("DELETE FROM t WHERE k = 999"), &out, &errw)
	if code != 0 {
		t.Fatalf("runWrite code = %d, stderr: %s", code, errw.String())
	}
	if !strings.Contains(out.String(), `"changes":0`) {
		t.Errorf("result = %q, want changes:0 (a no-op write is still a run)", out.String())
	}
}

func TestRunWriteGateViolationsExit2(t *testing.T) {
	path := setupDb(t)
	for _, stmt := range []string{
		"SELECT k FROM t",
		"WITH c AS (SELECT 1) SELECT * FROM c",
		"CREATE TABLE owned (x)",
		"DROP TABLE t",
		"ATTACH DATABASE '/tmp/x.db' AS evil",
		"PRAGMA journal_mode=DELETE",
		"VACUUM",
		"", // empty stdin
	} {
		var out, errw bytes.Buffer
		code := runWrite(path, strings.NewReader(stmt), &out, &errw)
		if code != 2 {
			t.Errorf("runWrite(%q) code = %d, want 2 (gate exit)", stmt, code)
		}
		if !strings.Contains(errw.String(), "write gate") {
			t.Errorf("runWrite(%q) stderr %q must name the write gate", stmt, errw.String())
		}
		if out.Len() != 0 {
			t.Errorf("runWrite(%q) wrote %q on a gate rejection", stmt, out.String())
		}
	}
	// Nothing executed: the table must still hold its original rows.
	var out bytes.Buffer
	if code := run(path, strings.NewReader("SELECT count(*) AS n FROM t"), &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("count code = %d", code)
	}
	if got := out.String(); got != `[{"n":2}]`+"\n" {
		t.Errorf("post-gate count = %q — a rejected statement mutated the fixture", got)
	}
}

func TestRunWriteRejectsMultiStatement(t *testing.T) {
	path := setupDb(t)
	var out, errw bytes.Buffer
	code := runWrite(path, strings.NewReader("UPDATE t SET txt = 'owned'; DELETE FROM t"), &out, &errw)
	if code != 2 {
		t.Fatalf("runWrite code = %d, want 2 (multi-statement rejection)", code)
	}
	if !strings.Contains(errw.String(), "ONE statement") {
		t.Errorf("stderr %q must name the one-statement rule", errw.String())
	}
	// Nothing executed — neither statement.
	var count bytes.Buffer
	if code := run(path, strings.NewReader("SELECT count(*) AS n FROM t"), &count, &bytes.Buffer{}); code != 0 {
		t.Fatalf("count code = %d", code)
	}
	if got := count.String(); got != `[{"n":2}]`+"\n" {
		t.Errorf("post-gate count = %q — the smuggled second statement ran", got)
	}
}

func TestRunWriteErrorPropagatesExit1(t *testing.T) {
	path := setupDb(t)
	var out, errw bytes.Buffer
	code := runWrite(path, strings.NewReader("UPDATE no_such_table SET x = 1"), &out, &errw)
	if code != 1 {
		t.Fatalf("runWrite code = %d, want 1 (ran and failed, not a gate rejection)", code)
	}
	if errw.Len() == 0 {
		t.Errorf("sqlite errors must explain themselves on stderr")
	}
	if out.Len() != 0 {
		t.Errorf("a failed exec must not print a result: %q", out.String())
	}
}

func TestRunWriteSyntaxErrorNamesTheStatement(t *testing.T) {
	path := setupDb(t)
	var out, errw bytes.Buffer
	code := runWrite(path, strings.NewReader("UPDATE t SET x = = 1"), &out, &errw)
	if code != 1 {
		t.Fatalf("runWrite code = %d, want 1", code)
	}
	if !strings.Contains(errw.String(), "exec failed") {
		t.Errorf("stderr %q must say exec failed", errw.String())
	}
}

// mode=rw (not rwc): a missing file is a driver error — db exec must
// NEVER materialize a fresh empty db over a typo'd --db.
func TestRunWriteMissingDbFileFailsExit1(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.db")
	var out, errw bytes.Buffer
	code := runWrite(missing, strings.NewReader("UPDATE t SET x = 1"), &out, &errw)
	if code != 1 {
		t.Fatalf("runWrite code = %d, want 1", code)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("the missing db file was CREATED — mode=rw is the contract")
	}
}

// TestReadOnlyDriverBackstop is the defense-in-depth proof: the helper's
// DSN (mode=ro&immutable=0) makes the DRIVER refuse writes even when the
// gate is bypassed — an INSERT through the read-only connection errors.
func TestReadOnlyDriverBackstop(t *testing.T) {
	path := setupDb(t)
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("openReadOnly: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE t SET txt = 'owned'"); err == nil {
		t.Fatal("write through the read-only DSN succeeded — the driver backstop is dead")
	}
	// And reads still work through the same handle.
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("read through the read-only DSN failed: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	// The fixture is untouched.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if bytes.Contains(raw, []byte("owned")) {
		t.Error("fixture was mutated through the read-only connection")
	}
}
