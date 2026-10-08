package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AppendEntry is the ledger_append port: ONE validated JSONL line, 0600,
// refusing to write on any violation (a malformed line would poison every
// reader — the ledger is append-only, there is no repair).

func appendArgs() LedgerEntry {
	return LedgerEntry{
		Ts:         "2026-10-08T12:00:00Z",
		Host:       "root@203.0.113.9",
		Sha:        "abc1234",
		Tag:        "abc1234",
		Result:     "success",
		DurationMs: 182000,
		Subject:    "feat: deploy pipeline",
	}
}

func TestAppendEntryWritesOneJSONLLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kampodra", "deployments.jsonl")
	if err := AppendEntry(path, appendArgs()); err != nil {
		t.Fatalf("AppendEntry() = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSuffix(string(data), "\n")
	want := `{"ts":"2026-10-08T12:00:00Z","host":"root@203.0.113.9","sha":"abc1234","tag":"abc1234","result":"success","duration_ms":182000,"subject":"feat: deploy pipeline"}`
	if got != want {
		t.Errorf("AppendEntry() line =\n  %s\nwant\n  %s", got, want)
	}
	// The writer must keep the seam contract the readers match on.
	if !strings.Contains(got, `"host":"root@203.0.113.9","sha":"`) {
		t.Errorf("AppendEntry() broke the host seam contract: %s", got)
	}
}

func TestAppendEntryCreatesTheDirAndKeeps0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kampodra", "deployments.jsonl")
	if err := AppendEntry(path, appendArgs()); err != nil {
		t.Fatalf("AppendEntry() = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("AppendEntry() file perm = %o, want 600 (0600 from creation)", perm)
	}
}

func TestAppendEntryAppendsWithoutClobbering(t *testing.T) {
	home := writeLedger(t, `{"ts":"2026-10-01T00:00:00Z","host":"h","sha":"aaa1111","tag":"aaa1111","result":"success","duration_ms":1,"subject":"old"}`+"\n")
	path := LedgerPath(home)
	if err := AppendEntry(path, appendArgs()); err != nil {
		t.Fatalf("AppendEntry() = %v", err)
	}
	entries := LedgerEntries(path, "")
	if len(entries) != 2 || entries[0].Subject != "old" || entries[1].Tag != "abc1234" {
		t.Fatalf("entries after append = %+v, want old line preserved + new line appended", entries)
	}
}

func TestAppendEntryFillsTheTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".kampodra", "deployments.jsonl")
	e := appendArgs()
	e.Ts = ""
	if err := AppendEntry(path, e); err != nil {
		t.Fatalf("AppendEntry() = %v", err)
	}
	entries := LedgerEntries(path, "")
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	ts := entries[0].Ts
	if len(ts) != len("2006-01-02T15:04:05Z") || !strings.HasSuffix(ts, "Z") {
		t.Errorf("AppendEntry() filled ts = %q, want UTC YYYY-MM-DDTHH:MM:SSZ", ts)
	}
}

func TestAppendEntryFlattensAndEscapesTheSubject(t *testing.T) {
	// json_escape_str port: backslash + double quote escaped, tab/newline/CR
	// flattened to spaces (subjects are single-line by construction).
	path := filepath.Join(t.TempDir(), ".kampodra", "deployments.jsonl")
	e := appendArgs()
	e.Subject = "fix: \"tls\" edge\\case\twrapped\nline"
	if err := AppendEntry(path, e); err != nil {
		t.Fatalf("AppendEntry() = %v", err)
	}
	// The STORED line carries the shell-escaped wire shape (the seam
	// contract); the READER unescapes it back to the original.
	line := readFileLine(t, path)
	if !strings.Contains(line, `subject":"fix: \"tls\" edge\\case wrapped line"}`) {
		t.Errorf("stored subject = %s, want the shell-escaped shape with control chars flattened", line)
	}
	entries := LedgerEntries(path, "")
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if got := entries[0].Subject; got != `fix: "tls" edge\case wrapped line` {
		t.Errorf("subject after round-trip = %q, want the original value unescaped", got)
	}
}

func readFileLine(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(data), "\n")
}

func TestAppendEntryRefusesInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LedgerEntry)
	}{
		{"empty host", func(e *LedgerEntry) { e.Host = "" }},
		{"whitespace host", func(e *LedgerEntry) { e.Host = "root@203.0.113.9 evil" }},
		{"empty sha", func(e *LedgerEntry) { e.Sha = "" }},
		{"non-hex sha", func(e *LedgerEntry) { e.Sha = "release-42" }},
		{"short sha", func(e *LedgerEntry) { e.Tag = "abc" }},
		{"long sha", func(e *LedgerEntry) { e.Sha = strings.Repeat("a", 41) }},
		{"unknown result", func(e *LedgerEntry) { e.Result = "redeployed" }},
		{"negative duration", func(e *LedgerEntry) { e.DurationMs = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".kampodra", "deployments.jsonl")
			e := appendArgs()
			tc.mutate(&e)
			if err := AppendEntry(path, e); err == nil {
				t.Error("AppendEntry() = nil, want a validation refusal (never write a malformed line)")
			}
			if _, err := os.Stat(path); err == nil {
				t.Error("AppendEntry() wrote a file despite refusing the entry")
			}
		})
	}
}

// LedgerSubjectForTag is the ledger_subject_for_tag port: the NEWEST
// recorded subject for host+tag (a rollback's subject is the ORIGINAL
// deploy's subject — git HEAD would name the wrong commit).

func TestLedgerSubjectForTagReturnsTheNewestMatch(t *testing.T) {
	path := writeLedger2(t, `{"ts":"2026-10-01T00:00:00Z","host":"root@203.0.113.9","sha":"aaa1111","tag":"aaa1111","result":"success","duration_ms":1,"subject":"older deploy"}
{"ts":"2026-10-08T00:00:00Z","host":"root@203.0.113.9","sha":"aaa1111","tag":"aaa1111","result":"success","duration_ms":2,"subject":"newest deploy"}
{"ts":"2026-10-08T01:00:00Z","host":"root@10.0.0.1","sha":"aaa1111","tag":"aaa1111","result":"success","duration_ms":3,"subject":"other host"}
`)
	if got := LedgerSubjectForTag(path, "root@203.0.113.9", "aaa1111"); got != "newest deploy" {
		t.Errorf("LedgerSubjectForTag() = %q, want %q", got, "newest deploy")
	}
}

func TestLedgerSubjectForTagEmptyWhenAbsent(t *testing.T) {
	path := writeLedger2(t, `{"ts":"2026-10-01T00:00:00Z","host":"h","sha":"aaa1111","tag":"aaa1111","result":"success","duration_ms":1,"subject":"x"}`+"\n")
	if got := LedgerSubjectForTag(path, "h", "zzz9999"); got != "" {
		t.Errorf("LedgerSubjectForTag(unknown tag) = %q, want empty", got)
	}
	if got := LedgerSubjectForTag(filepath.Join(t.TempDir(), "nope.jsonl"), "h", "aaa1111"); got != "" {
		t.Errorf("LedgerSubjectForTag(missing ledger) = %q, want empty", got)
	}
}
