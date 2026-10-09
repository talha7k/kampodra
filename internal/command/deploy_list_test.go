package command

import (
	"testing"

	"github.com/talha7k/kampodra/internal/adapter/state"
)

// A redeployed tag re-appends its ledger line; ledger-only rows must
// merge to ONE row showing the NEWEST entry (not the earliest, not both).
func TestDeployListSameTagLedgerOnlyMergesToLatest(t *testing.T) {
	ledger := []state.LedgerEntry{
		{Ts: "2026-10-04T09:00:00Z", Tag: "eee5555", Result: "rollback", Subject: "vanished from VM"},
		{Ts: "2026-10-05T13:30:00Z", Tag: "eee5555", Result: "success", Subject: "re-registered"},
	}
	rows := buildDeployListRows(nil, "", ledger)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 merged row: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Tag != "eee5555" || r.Created != "2026-10-05T13:30:00Z" || r.Result != "success" || r.State != "not on VM" {
		t.Errorf("merged row = %+v, want tag eee5555, latest Ts, success", r)
	}
}

func TestLatestTagTimePrefersNewest(t *testing.T) {
	if got := latestTagTime("2026-10-08T11:00:00Z", "2026-10-06 08:00:00 +0000 UTC"); got != "2026-10-08 11:00" {
		t.Errorf("ledger-newer = %q", got)
	}
	if got := latestTagTime("2026-10-01T00:00:00Z", "2026-10-08 10:00:00 +0000 UTC"); got != "2026-10-08 10:00" {
		t.Errorf("image-newer = %q", got)
	}
	if got := latestTagTime("not-a-ts", "2026-10-08 10:00:00 +0000 UTC"); got != "2026-10-08 10:00" {
		t.Errorf("bad-ledger falls back to image time = %q", got)
	}
}
