// Package migrate is the frozen migrate.sh port's remote composition: the
// tenant-db migration loop over libSQL db files (one .db file per tenant
// under the data dir, + root.db for the auth/org plane, + the support and
// billing class dbs — <data>/support/support.db, <data>/billing/billing.db).
// The command layer drives three pieces over ssh:
//
//	ProbeCommand    — preconditions + the db-file listing in ONE round-trip
//	Plan            — the pure ordering decision (root first, then the
//	                  support/billing class planes, tenants sorted)
//	MigrateCommand  — the per-file apply via the repo's configured migrate script
//
// Every snippet is busybox-safe (Alpine ships no bash): POSIX sh, no
// function export tricks, no GNU-only flags. Bounded parallelism lives in
// the command layer (a bounded worker pool over the per-file commands) —
// the shell's VM-side `xargs -P` with exported bash functions is not
// portable there; the ordering and the all-or-nothing failure gate are
// identical.
package migrate

import (
	"path"
	"sort"
	"strings"
)

// shellQuote single-quotes one path for POSIX sh (the '\” dance).
func shellQuote(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// ProbeCommand composes the one-round-trip probe: the three preconditions
// (repo checkout with the CONFIGURED migrate script, pnpm, data dir) as ERR
// lines, then every .db file under the data dir as db= lines. All checks run
// even when earlier ones fail, so one ssh hop reports the full picture; the
// command layer turns the ERR lines into the shell's fail messages in the
// shell's check order. migrateScript is the project config's repo-relative
// script path — config, never code.
func ProbeCommand(repoRoot, migrateScript, dataDir string) string {
	return probeCommand(repoRoot, migrateScript, dataDir, true)
}

// ProbeCommandBinary is the no-node variant of the probe: the same three
// shapes with the pnpm check OMITTED — migrateMode "binary" runs a static
// Go binary, and the target host is strictly no-node (no pnpm to find).
// The binary's own presence check stays: migrateScript names the binary's
// repo-relative path in binary mode.
func ProbeCommandBinary(repoRoot, binaryPath, dataDir string) string {
	return probeCommand(repoRoot, binaryPath, dataDir, false)
}

// probeCommand is the shared probe composer; checkPnpm is the only
// difference between the script and binary shapes (the script-mode output
// stays byte-identical).
func probeCommand(repoRoot, migrateScript, dataDir string, checkPnpm bool) string {
	script := shellQuote(repoRoot + "/" + migrateScript)
	data := shellQuote(dataDir)
	cmd := `if [ ! -f ` + script + ` ]; then echo "ERR repo-root"; fi; `
	if checkPnpm {
		cmd += `command -v pnpm >/dev/null 2>&1 || echo "ERR pnpm"; `
	}
	return cmd +
		`if [ ! -d ` + data + ` ]; then echo "ERR tenant-dir"; fi; ` +
		// The class dirs are their own globs — a nested db file is invisible
		// to a flat glob; the -f guard skips absent dirs (unexpanded glob).
		`for f in ` + data + `/*.db ` + data + `/root/*.db ` + data + `/support/*.db ` + data + `/billing/*.db; do [ -f "$f" ] && printf 'db=%s\n' "$f"; done`
}

// Probe is the parsed probe result.
type Probe struct {
	RepoOK      bool
	PnpmOK      bool
	TenantDirOK bool
	DbFiles     []string
}

// ParseProbe parses the probe output. Absent ERR lines mean OK.
func ParseProbe(out string) Probe {
	p := Probe{RepoOK: true, PnpmOK: true, TenantDirOK: true}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "ERR repo-root":
			p.RepoOK = false
		case line == "ERR pnpm":
			p.PnpmOK = false
		case line == "ERR tenant-dir":
			p.TenantDirOK = false
		case strings.HasPrefix(line, "db="):
			if f := strings.TrimPrefix(line, "db="); strings.HasSuffix(f, ".db") {
				p.DbFiles = append(p.DbFiles, f)
			}
		}
	}
	return p
}

// Job is one db file's migration work: the file path and its namespace
// (the file basename minus .db — the shell's `basename ... .db` parity).
type Job struct {
	Path string
	NS   string
}

// PlanResult is the parsed ordering decision: the root plane (root.db —
// the auth/org plane), the class planes (support + billing — one file
// each), the bounded-parallel tenant pool, and every file that matched NO
// known shape (surfaced so the command layer can warn — a silent ignore
// is how a new db class drifted with zero errors).
type PlanResult struct {
	Root    *Job
	Classes []Job // support + billing, fixed apply order (support, billing)
	Tenants []Job // tenant_*.db, sorted
	Unknown []string
}

// classApplyOrder pins the class-plane apply order (support, billing —
// the esellar class enumeration; each class is one file).
var classApplyOrder = map[string]int{"support": 0, "billing": 1}

// setClass keeps ONE job per class plane: a later sighting replaces an
// earlier one — the nested-root rule's parity (only one class dir exists
// on a real estate).
func setClass(classes []Job, j Job) []Job {
	for i := range classes {
		if classes[i].NS == j.NS {
			classes[i] = j
			return classes
		}
	}
	return append(classes, j)
}

// Plan orders the migration work: root.db FIRST (the auth/org plane —
// root/root.db preferred, the legacy flat root.db honored), then the
// support + billing class planes (<data>/support/support.db,
// <data>/billing/billing.db — handled exactly like the root pairing),
// then the tenant_*.db files sorted. Files matching no known shape land
// in Unknown — never applied, never silently dropped.
func Plan(dbFiles []string) PlanResult {
	var p PlanResult
	legacyFlat := ""
	for _, f := range dbFiles {
		switch {
		case strings.HasSuffix(f, "/root/root.db"):
			// the nested root is authoritative; it replaces any earlier
			// legacy sighting (only one can exist on a real estate)
			p.Root = &Job{Path: f, NS: "root"}
		case strings.HasSuffix(f, "/root.db"):
			legacyFlat = f // provisional; a nested root/root.db replaces it
		case strings.HasSuffix(f, "/support/support.db"):
			p.Classes = setClass(p.Classes, Job{Path: f, NS: "support"})
		case strings.HasSuffix(f, "/billing/billing.db"):
			p.Classes = setClass(p.Classes, Job{Path: f, NS: "billing"})
		default:
			base := path.Base(f)
			if strings.HasPrefix(base, "tenant_") && strings.HasSuffix(base, ".db") {
				p.Tenants = append(p.Tenants, Job{Path: f, NS: strings.TrimSuffix(base, ".db")})
			} else {
				p.Unknown = append(p.Unknown, f)
			}
		}
	}
	if p.Root == nil && legacyFlat != "" {
		p.Root = &Job{Path: legacyFlat, NS: "root"}
	}
	sort.Slice(p.Classes, func(i, j int) bool { return classApplyOrder[p.Classes[i].NS] < classApplyOrder[p.Classes[j].NS] })
	sort.Slice(p.Tenants, func(i, j int) bool { return p.Tenants[i].Path < p.Tenants[j].Path })
	return p
}

// MigrateCommand composes one file's migrate apply, run from the repo
// checkout on the VM (the shell's `(cd "$REPO_ROOT" && pnpm exec tsx
// <migrateScript> --db "file:…" --ns …)`). migrateScript is the project
// config's repo-relative script path — the app's own applier, never a
// parallel implementation, never a hardcoded path.
func MigrateCommand(repoRoot, migrateScript string, j Job) string {
	return "cd " + shellQuote(repoRoot) +
		" && pnpm exec tsx " + shellQuote(migrateScript) +
		" --db 'file:" + j.Path + "'" +
		" --ns " + shellQuote(j.NS)
}

// MigrateCommandBinary composes one file's migrate apply via the
// CONFIGURED GO BINARY (migrateMode "binary": migrateScript names the
// binary's repo-relative path — the static runner streamed into the repo
// checkout; migrateDir is the repo-relative migrations root). The
// migrater's own pairing drives -dir: the root job reads
// <root>/root, the support + billing class jobs their own dirs, and
// every tenant job <root>/tenant — one composition cannot mispair the
// dirs. -db takes the PLAIN sqlite path (the binary opens it
// directly; the file: URI is the tsx script's contract).
func MigrateCommandBinary(repoRoot, binaryPath, migrationsRoot string, j Job) string {
	sub := "tenant"
	switch j.NS {
	case "root":
		sub = "root"
	case "support", "billing":
		sub = j.NS
	}
	return shellQuote(repoRoot+"/"+binaryPath) +
		" -db " + shellQuote(j.Path) +
		" -dir " + shellQuote(repoRoot+"/"+path.Join(migrationsRoot, sub))
}
