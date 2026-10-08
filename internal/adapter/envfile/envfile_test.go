package envfile

import (
	"strings"
	"testing"
)

// Ports of the shell env.sh masking contract (env_fp_lines): tests pin the
// exact table shapes so a rendering drift would break terminal parity, and
// the parser grammar (comments/blanks/invalid keys skipped, one pair of
// matching surrounding quotes stripped).

func TestParseLineGrammar(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []Var
	}{
		{
			name:    "plain pairs",
			content: "A=1\nB=two words\n",
			want:    []Var{{"A", "1"}, {"B", "two words"}},
		},
		{
			name:    "comments blanks and non-pairs skipped",
			content: "# comment\n\nA=1\nNOEQUALS\n=bare\n",
			want:    []Var{{"A", "1"}},
		},
		{
			name:    "invalid key names skipped",
			content: "1BAD=x\nHAS-DASH=x\nGOOD_x1=y\n",
			want:    []Var{{"GOOD_x1", "y"}},
		},
		{
			name:    "one pair of matching surrounding quotes stripped",
			content: "A=\"quoted\"\nB='single'\nC=\"mismatched'\nD=\"no closing\nE=plain\n",
			want:    []Var{{"A", "quoted"}, {"B", "single"}, {"C", "\"mismatched'"}, {"D", "\"no closing"}, {"E", "plain"}},
		},
		{
			name:    "first = splits, later = are value content",
			content: "URL=https://x/?a=b\n",
			want:    []Var{{"URL", "https://x/?a=b"}},
		},
		{
			name:    "empty value kept",
			content: "EMPTY=\n",
			want:    []Var{{"EMPTY", ""}},
		},
		{
			name:    "no trailing newline still parses the last line",
			content: "A=1\nB=2",
			want:    []Var{{"A", "1"}, {"B", "2"}},
		},
		{
			name:    "duplicate keys: last occurrence wins (podman env-file semantics)",
			content: "A=first\nA=second\n",
			want:    []Var{{"A", "first"}, {"A", "second"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.content)
			if len(got) != len(tt.want) {
				t.Fatalf("Parse() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Parse()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestFingerprintNeverLeaksValues(t *testing.T) {
	tests := []struct {
		name string
		v    Var
		want string
	}{
		{
			name: "empty value",
			v:    Var{Key: "EMPTY", Value: ""},
			want: "EMPTY                            len=0      (empty)",
		},
		{
			name: "key padded to 32, len left-justified in 7 + separator space, first 2 chars + ellipsis",
			v:    Var{Key: "DATABASE_URL", Value: "postgres://supersecret"},
			want: "DATABASE_URL                     len=22      po…",
		},
		{
			name: "one-char value has no ellipsis padding ambiguity",
			v:    Var{Key: "K", Value: "x"},
			want: "K                                len=1       x…",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Fingerprint(tt.v)
			if got != tt.want {
				t.Errorf("Fingerprint() = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, tt.v.Value) && len(tt.v.Value) > 2 {
				t.Errorf("Fingerprint() leaked the raw value: %q", got)
			}
		})
	}
}

func TestTableRendersOneLinePerVar(t *testing.T) {
	got := Table("A=1\n\n# c\nB=\"hello\"\n")
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("Table() lines = %d (%q), want 2", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "A ") || !strings.Contains(lines[0], "len=1") {
		t.Errorf("line 0 = %q", lines[0])
	}
	if !strings.Contains(lines[1], "B") || !strings.Contains(lines[1], "len=5") || !strings.Contains(lines[1], "he…") {
		t.Errorf("line 1 = %q", lines[1])
	}
}

func TestCountKeyLinesIsThePushGate(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{name: "none", content: "# nothing\nNOPE\n", want: 0},
		{name: "counts valid key lines only", content: "A=1\nBAD-KEY=2\n  INDENTED=3\nB=4\n", want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CountKeyLines(tt.content); got != tt.want {
				t.Errorf("CountKeyLines() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDiffClassification(t *testing.T) {
	local := Parse("ADDED=1\nCHANGED=local-value\nSAME=keep\n")
	remote := Parse("REMOVED=2\nCHANGED=remote-value\nSAME=keep\n")

	entries := Diff(local, remote)
	byKey := map[string]Entry{}
	for _, e := range entries {
		byKey[e.Key] = e
	}

	if e := byKey["ADDED"]; e.Kind != KindAdded || !e.HasLocal || e.HasRemote {
		t.Errorf("ADDED = %+v, want added with local only", e)
	}
	if e := byKey["REMOVED"]; e.Kind != KindRemoved || e.HasLocal || !e.HasRemote {
		t.Errorf("REMOVED = %+v, want removed with remote only", e)
	}
	if e := byKey["CHANGED"]; e.Kind != KindChanged || e.Local.Value != "local-value" || e.Remote.Value != "remote-value" {
		t.Errorf("CHANGED = %+v, want changed carrying both sides", e)
	}
	if e := byKey["SAME"]; e.Kind != KindUnchanged {
		t.Errorf("SAME = %+v, want unchanged", e)
	}
	if len(entries) != 4 {
		t.Errorf("entries = %d, want 4 (one per union key)", len(entries))
	}
}

func TestDiffSortedByKey(t *testing.T) {
	entries := Diff(Parse("B=1\nA=1\nC=1\n"), Parse("Z=2\nAA=2\n"))
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Key >= entries[i].Key {
			t.Fatalf("entries not sorted by key: %v", entries)
		}
	}
}

func TestDiffDetectsChangeOnFingerprintCollision(t *testing.T) {
	// Same first-2 chars and same length => identical fingerprints, but the
	// values differ. Changed detection compares VALUES (fingerprints are for
	// display only) so a collision can never hide a drift.
	local := Parse("K=abXXXX\n")
	remote := Parse("K=abYYYY\n")
	entries := Diff(local, remote)
	if len(entries) != 1 || entries[0].Kind != KindChanged {
		t.Fatalf("entries = %+v, want one changed entry", entries)
	}
	if Fingerprint(local[0]) != Fingerprint(remote[0]) {
		t.Fatalf("test premise broken: fingerprints should collide")
	}
}

func TestDiffLastOccurrenceWins(t *testing.T) {
	entries := Diff(Parse("K=first\nK=final\n"), Parse("K=stale\n"))
	if len(entries) != 1 || entries[0].Kind != KindChanged || entries[0].Local.Value != "final" {
		t.Fatalf("entries = %+v, want changed against the last local occurrence", entries)
	}
}

func TestRenderDiffLinesAndSummary(t *testing.T) {
	entries := Diff(
		Parse("ADDED=brand new\nCHANGED=localvalue\nSAME=x\n"),
		Parse("GONE=old one\nCHANGED=remotevalue\nSAME=x\n"),
	)
	got := RenderDiff(entries)
	want := []string{
		"+ ADDED                    added    (local: len=9       br…)",
		"- GONE                     removed  (remote: len=7       ol…)",
		"~ CHANGED                  changed  (local: len=10      lo…)",
		"    (remote: len=11      re…)",
		"== env diff summary: 1 added, 1 removed, 1 changed, 1 unchanged ==",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("RenderDiff() missing %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "brand new") || strings.Contains(got, "remotevalue") {
		t.Errorf("RenderDiff() leaked raw values:\n%s", got)
	}
}
