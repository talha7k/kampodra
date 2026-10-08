package command

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/talha7k/kampodra/internal/adapter/runtime"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// deployListRow is one render_deploy_list output row (the shell's
// `%-9s %-17s %-9s %-9s %s` table).
type deployListRow struct {
	Tag     string
	Created string
	State   string
	Result  string
	Subject string
}

// shaTagRe is the shared sha-like tag rule (runtime owns the same regex for
// prune candidates; rendering re-checks ledger tags independently).
var shaTagRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// buildDeployListRows ports render_deploy_list: VM sha-tagged rows
// newest-first (the running one marked), then ledger-only rows (tags pruned
// on the VM, or deployed from elsewhere / failed before load). Ledger
// entries enrich each row with the last recorded result + subject.
func buildDeployListRows(images []runtime.Image, psOutput string, ledger []state.LedgerEntry) []deployListRow {
	running := map[string]bool{}
	for _, tag := range runtime.RunningTags(psOutput) {
		running[tag] = true
	}

	// VM rows, newest-first by CreatedAt (GNU sort's -k2,2r shape: equal
	// timestamps fall back to ascending tag — deterministic).
	vmImages := runtime.ShaTagged(images)
	sort.SliceStable(vmImages, func(i, j int) bool {
		if vmImages[i].CreatedAt != vmImages[j].CreatedAt {
			return vmImages[i].CreatedAt > vmImages[j].CreatedAt
		}
		return vmImages[i].Tag < vmImages[j].Tag
	})

	// Last ledger entry per tag is the newest (append-only ledger).
	lastByTag := map[string]state.LedgerEntry{}
	for _, e := range ledger {
		if e.Tag != "" {
			lastByTag[e.Tag] = e
		}
	}

	var rows []deployListRow
	vmTags := map[string]bool{}
	for _, img := range vmImages {
		st, result, subject := "on VM", "-", "-"
		if running[img.Tag] {
			st = "RUNNING"
		}
		if e, ok := lastByTag[img.Tag]; ok {
			if e.Result != "" {
				result = e.Result
			}
			if e.Subject != "" {
				subject = e.Subject
			}
		}
		created := img.CreatedAt
		if len(created) > 16 {
			created = created[:16]
		}
		rows = append(rows, deployListRow{Tag: img.Tag, Created: created, State: st, Result: result, Subject: subject})
		vmTags[img.Tag] = true
	}

	// Ledger-only rows, in file order.
	for _, e := range ledger {
		if !shaTagRe.MatchString(e.Tag) || vmTags[e.Tag] {
			continue
		}
		result, subject := e.Result, e.Subject
		if result == "" {
			result = "-"
		}
		if subject == "" {
			subject = "-"
		}
		rows = append(rows, deployListRow{Tag: e.Tag, Created: e.Ts, State: "not on VM", Result: result, Subject: subject})
	}
	return rows
}

// renderDeployListRows formats the rows (no header/footer — the caller owns
// those), byte-shaped like the shell's printf.
func renderDeployListRows(rows []deployListRow) string {
	var sb strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&sb, "%-9s %-17s %-9s %-9s %s\n", r.Tag, r.Created, r.State, r.Result, r.Subject)
	}
	return sb.String()
}
