package state

import (
	"path/filepath"
	"strings"
	"testing"
)

func writeLedger2(t *testing.T, content string) string {
	t.Helper()
	home := writeLedger(t, content)
	return LedgerPath(home)
}

func TestLedgerEntriesParsesWellFormedLines(t *testing.T) {
	path := writeLedger2(t, `{"ts":"2026-10-08T10:05:00Z","host":"root@203.0.113.9","sha":"aaa1111000000000000000000000000000000000","tag":"aaa1111","result":"success","duration_ms":182000,"subject":"feat: ledger merge"}
{"ts":"2026-10-08T11:00:00Z","host":"root@203.0.113.9","sha":"ccc3333000000000000000000000000000000000","tag":"ccc3333","result":"rollback","duration_ms":42000,"subject":"fix: tls edge"}
`)
	entries := LedgerEntries(path, "")
	if len(entries) != 2 {
		t.Fatalf("LedgerEntries() = %d entries, want 2", len(entries))
	}
	first := entries[0]
	if first.Ts != "2026-10-08T10:05:00Z" || first.Host != "root@203.0.113.9" ||
		first.Tag != "aaa1111" || first.Result != "success" ||
		first.DurationMs != 182000 || first.Subject != "feat: ledger merge" {
		t.Errorf("entry[0] = %+v", first)
	}
	if entries[1].Result != "rollback" || entries[1].DurationMs != 42000 {
		t.Errorf("entry[1] = %+v", entries[1])
	}
}

func TestLedgerEntriesHostFilterUsesTheSeam(t *testing.T) {
	path := writeLedger2(t, `{"ts":"2026-10-08T10:05:00Z","host":"root@203.0.113.9","sha":"aaa1111000000000000000000000000000000000","tag":"aaa1111","result":"success","duration_ms":1,"subject":"mine"}
{"ts":"2026-10-06T08:00:00Z","host":"root@10.0.0.1","sha":"beef888000000000000000000000000000000000","tag":"beef888","result":"success","duration_ms":1,"subject":"other"}
`)
	entries := LedgerEntries(path, "root@203.0.113.9")
	if len(entries) != 1 || entries[0].Tag != "aaa1111" {
		t.Fatalf("host-filtered entries = %+v, want only the 203.0.113.9 row", entries)
	}
}

func TestLedgerEntriesMissingFileIsEmpty(t *testing.T) {
	if entries := LedgerEntries(filepath.Join(t.TempDir(), "nope.jsonl"), ""); len(entries) != 0 {
		t.Fatalf("missing ledger = %+v, want empty (rendering never fails on a fresh machine)", entries)
	}
}

func TestLedgerEntriesSkipsMalformedLines(t *testing.T) {
	path := writeLedger2(t, "garbage line\n"+`{"ts":"2026-10-08T10:05:00Z","host":"h","sha":"aaa1111000000000000000000000000000000000","tag":"aaa1111","result":"success","duration_ms":1,"subject":"ok"}`+"\n{broken\n")
	entries := LedgerEntries(path, "")
	if len(entries) != 1 || entries[0].Tag != "aaa1111" {
		t.Fatalf("entries = %+v, want the one well-formed row (rendering skips garbage; LedgerCount still counts it)", entries)
	}
}

func TestLedgerEntriesPreservesFileOrder(t *testing.T) {
	// render_deploy_list enriches rows with the NEWEST entry per tag = the
	// last match in append order; order preservation is the contract.
	path := writeLedger2(t, `{"ts":"2026-10-01T00:00:00Z","host":"h","sha":"aaa1111000000000000000000000000000000000","tag":"aaa1111","result":"success","duration_ms":1,"subject":"old"}
{"ts":"2026-10-08T00:00:00Z","host":"h","sha":"aaa1111000000000000000000000000000000000","tag":"aaa1111","result":"rollback","duration_ms":1,"subject":"new"}
`)
	entries := LedgerEntries(path, "")
	if len(entries) != 2 || entries[1].Subject != "new" {
		t.Fatalf("entries = %+v, want file order preserved (last = newest)", entries)
	}
}

func TestLedgerEntriesSubjectWithEscapedQuote(t *testing.T) {
	// The shell ledger contract JSON-escapes values; the Go reader must
	// unescape them back ( subjects are single-line by construction).
	path := writeLedger2(t, `{"ts":"2026-10-08T10:05:00Z","host":"h","sha":"aaa1111000000000000000000000000000000000","tag":"aaa1111","result":"success","duration_ms":1,"subject":"fix: \"tls\" edge"}`+"\n")
	entries := LedgerEntries(path, "")
	if len(entries) != 1 || !strings.Contains(entries[0].Subject, `"tls"`) {
		t.Fatalf("entries = %+v, want the unescaped subject", entries)
	}
}
