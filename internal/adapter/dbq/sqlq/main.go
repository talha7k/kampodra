// Command sqlq is kampodra's on-VM sqlite helper. Read mode: ONE
// SELECT/WITH statement on stdin, JSON rows on stdout (db query). Write
// mode (-write): ONE UPDATE/INSERT/DELETE/REPLACE statement on stdin, a
// {"changes":N,"statement":"…"} JSON result on stdout (db exec). kampodra
// cross-builds it (linux, CGO off), streams it to the VM's /tmp once, and
// pipes statements through it — agents never hand-roll sqlite binaries
// on the VM.
//
// Contract: exit 0 = data printed (rows / write result); exit 2 = gate/
// usage rejection (the same gates kampodra enforces client-side); exit 1
// = ran and failed. The db opens with mode=ro&immutable=0 in read mode
// and mode=rw&immutable=0 in write mode — a read can never write, and a
// write can never CREATE a database.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/talha7k/kampodra/internal/adapter/dbq"

	// The pure-Go sqlite driver (CGO off, static-linux build). Imported
	// for its side effect: it registers the "sqlite" database/sql driver.
	_ "modernc.org/sqlite"
)

// sqlqVersion stamps the helper's content — it must equal dbq.Version
// (the VM cache path's invalidator); a pin test keeps them together.
// v2: the write mode (-write, db exec) — existing /tmp caches re-stream.
const sqlqVersion = "2"

func main() {
	dbPath := flag.String("db", "", "sqlite file to open (read mode: mode=ro; write mode: mode=rw — never creates)")
	write := flag.Bool("write", false, "write mode: execute ONE gated UPDATE/INSERT/DELETE/REPLACE from stdin, print {\"changes\":N,\"statement\":\"…\"}")
	showVersion := flag.Bool("version", false, "print the helper version and exit")
	flag.Parse()
	switch {
	case *showVersion:
		fmt.Printf("kampodra-sqlq v%s\n", sqlqVersion)
	case *write:
		os.Exit(runWrite(*dbPath, os.Stdin, os.Stdout, os.Stderr))
	default:
		os.Exit(run(*dbPath, os.Stdin, os.Stdout, os.Stderr))
	}
}

func run(dbPath string, stmtIn io.Reader, out, errw io.Writer) int {
	raw, err := io.ReadAll(stmtIn)
	if err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: cannot read the statement: %v\n", err)
		return 1
	}
	stmt := strings.TrimSpace(string(raw))
	if err := dbq.ValidateReadOnly(stmt); err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: %v\n", err)
		return 2
	}
	if dbPath == "" {
		fmt.Fprintln(errw, "kampodra-sqlq: -db <path> is required")
		return 2
	}
	if err := dbq.ValidateDbPath(dbPath); err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: %v\n", err)
		return 2
	}

	db, err := openReadOnly(dbPath)
	if err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: cannot open %s read-only: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(stmt)
	if err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: query failed: %v\n", err)
		return 1
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: %v\n", err)
		return 1
	}
	// [{"col":val},...] — the estate's tooling parses this shape. Zero
	// rows print [] (never null); integers/reals stay numbers, text stays
	// strings, blobs base64, NULLs null.
	result := make([]map[string]any, 0)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fmt.Fprintf(errw, "kampodra-sqlq: %v\n", err)
			return 1
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			m[c] = vals[i]
		}
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: %v\n", err)
		return 1
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: cannot encode rows: %v\n", err)
		return 1
	}
	return 0
}

func openReadOnly(path string) (*sql.DB, error) {
	return sql.Open("sqlite", dbq.ReadOnlyDSN(path))
}

// writeResult is write mode's stdout contract: rows affected plus the
// statement's first line — the audit trail the operator quotes back.
type writeResult struct {
	Changes   int64  `json:"changes"`
	Statement string `json:"statement"`
}

// runWrite is the write mode (db exec's helper leg): ONE gated
// UPDATE/INSERT/DELETE/REPLACE statement on stdin, executed through
// mode=rw (an existing file — never created), {"changes":N} JSON on
// stdout. The gate here is the VM-side copy of kampodra's — both sides
// refuse multi-statement strings and non-DML verbs BEFORE the db opens.
func runWrite(dbPath string, stmtIn io.Reader, out, errw io.Writer) int {
	raw, err := io.ReadAll(stmtIn)
	if err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: cannot read the statement: %v\n", err)
		return 1
	}
	stmt := strings.TrimSpace(string(raw))
	if err := dbq.ValidateWrite(stmt); err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: %v\n", err)
		return 2
	}
	if dbPath == "" {
		fmt.Fprintln(errw, "kampodra-sqlq: -db <path> is required")
		return 2
	}
	if err := dbq.ValidateDbPath(dbPath); err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: %v\n", err)
		return 2
	}

	db, err := sql.Open("sqlite", dbq.ReadWriteDSN(dbPath))
	if err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: cannot open %s read-write: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = db.Close() }()

	res, err := db.Exec(stmt)
	if err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: exec failed: %v\n", err)
		return 1
	}
	changes, err := res.RowsAffected()
	if err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: cannot read rows-affected: %v\n", err)
		return 1
	}
	firstLine := stmt
	if i := strings.IndexByte(stmt, '\n'); i >= 0 {
		firstLine = strings.TrimSpace(stmt[:i])
	}
	if err := json.NewEncoder(out).Encode(writeResult{Changes: changes, Statement: firstLine}); err != nil {
		fmt.Fprintf(errw, "kampodra-sqlq: cannot encode the result: %v\n", err)
		return 1
	}
	return 0
}
