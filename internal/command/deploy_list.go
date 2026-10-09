package command

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

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

	// Last ledger entry per tag is the newest (append-only ledger).
	lastByTag := map[string]state.LedgerEntry{}
	for _, e := range ledger {
		if e.Tag != "" {
			lastByTag[e.Tag] = e
		}
	}

	rows, vmTags := vmListRows(images, running, lastByTag)
	return append(rows, ledgerOnlyRows(ledger, vmTags, lastByTag)...)
}

// vmListRows renders the VM sha-tagged images, newest-first by CreatedAt
// (GNU sort's -k2,2r shape: equal timestamps fall back to ascending tag —
// deterministic), enriched with each tag's newest ledger entry. It also
// returns the rendered tag set so ledger-only rows can skip them.
func vmListRows(images []runtime.Image, running map[string]bool, lastByTag map[string]state.LedgerEntry) ([]deployListRow, map[string]bool) {
	vmImages := runtime.ShaTagged(images)
	sort.SliceStable(vmImages, func(i, j int) bool {
		if vmImages[i].CreatedAt != vmImages[j].CreatedAt {
			return vmImages[i].CreatedAt > vmImages[j].CreatedAt
		}
		return vmImages[i].Tag < vmImages[j].Tag
	})

	var rows []deployListRow
	vmTags := map[string]bool{}
	for _, img := range vmImages {
		st, result, subject := "on VM", "-", "-"
		if running[img.Tag] {
			st = "RUNNING"
		}
		created := img.CreatedAt
		if len(created) > 16 {
			created = created[:16]
		}
		if e, ok := lastByTag[img.Tag]; ok {
			if e.Result != "" {
				result = e.Result
			}
			if e.Subject != "" {
				subject = e.Subject
			}
			// Same tag redeployed: the row merges VM truth with the newest
			// ledger event — display the LATEST of the two timestamps (the
			// image CreatedAt never moves on retag; the ledger records the
			// actual deploy). Formatted to the same column shape either way.
			if later := latestTagTime(e.Ts, img.CreatedAt); later != "" {
				created = later
			}
		}
		rows = append(rows, deployListRow{Tag: img.Tag, Created: created, State: st, Result: result, Subject: subject})
		vmTags[img.Tag] = true
	}
	return rows, vmTags
}

// ledgerOnlyRows renders tags the VM no longer has (pruned images,
// deploys from elsewhere, failed-before-load), in file order — one row
// per tag (redeploys re-append the same tag; the ledger is append-only,
// so the LAST entry is the newest and wins the merge).
func ledgerOnlyRows(ledger []state.LedgerEntry, vmTags map[string]bool, lastByTag map[string]state.LedgerEntry) []deployListRow {
	var rows []deployListRow
	seenLedgerOnly := map[string]bool{}
	for _, e := range ledger {
		if !shaTagRe.MatchString(e.Tag) || vmTags[e.Tag] || seenLedgerOnly[e.Tag] {
			continue
		}
		seenLedgerOnly[e.Tag] = true
		last := lastByTag[e.Tag]
		result, subject := last.Result, last.Subject
		if result == "" {
			result = "-"
		}
		if subject == "" {
			subject = "-"
		}
		rows = append(rows, deployListRow{Tag: e.Tag, Created: last.Ts, State: "not on VM", Result: result, Subject: subject})
	}
	return rows
}

// latestTagTime returns the later of a ledger timestamp (RFC3339) and a
// podman image timestamp, formatted for the deploy-list CREATED column
// ("2006-01-02 15:04"). Unparseable ledger time falls back to the
// trimmed image time; "" when neither yields anything.
func latestTagTime(ledgerTs, imageCreatedAt string) string {
	ledgerT, ledgerErr := time.Parse(time.RFC3339, ledgerTs)
	imgT, imgErr := parseImageTime(imageCreatedAt)
	switch {
	case ledgerErr == nil && imgErr == nil:
		if ledgerT.After(imgT) {
			return ledgerT.Format("2006-01-02 15:04")
		}
		return imgT.Format("2006-01-02 15:04")
	case ledgerErr == nil:
		return ledgerT.Format("2006-01-02 15:04")
	default:
		if len(imageCreatedAt) > 16 {
			return imageCreatedAt[:16]
		}
		return imageCreatedAt
	}
}

// parseImageTime parses podman --format CreatedAt output ("2006-01-02
// 15:04:05 -0700 MST", tolerant of the offset/timezone part).
func parseImageTime(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05 -0700",
		time.RFC3339,
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable image time %q", s)
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
