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
