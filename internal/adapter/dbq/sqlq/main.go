// Command sqlq is kampodra's on-VM read-only sqlite helper: ONE
// SELECT/WITH statement on stdin, JSON rows on stdout. kampodra `db
// query` cross-builds it (linux, CGO off), streams it to the VM's /tmp
// once, and pipes statements through it — read-only agents never need a
// hand-rolled sqlite binary on the VM.
//
// Contract: exit 0 = JSON rows printed; exit 2 = gate/usage rejection
// (the same gate kampodra enforces client-side); exit 1 = ran and failed.
// The db opens with mode=ro&immutable=0 — writes fail at the driver even
// if the gate were bypassed.
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
const sqlqVersion = "1"

func main() {
	dbPath := flag.String("db", "", "sqlite file to open READ-ONLY (mode=ro&immutable=0)")
	showVersion := flag.Bool("version", false, "print the helper version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("kampodra-sqlq v%s\n", sqlqVersion)
		return
	}
	os.Exit(run(*dbPath, os.Stdin, os.Stdout, os.Stderr))
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
