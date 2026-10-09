// Package migrate is the frozen migrate.sh port's remote composition: the
// tenant-db migration loop over libSQL db files (one .db file per tenant
// under the data dir, + root.db for the auth/org plane). The command layer
// drives three pieces over ssh:
//
//	ProbeCommand    — preconditions + the db-file listing in ONE round-trip
//	Plan            — the pure ordering decision (root first, tenants sorted)
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
	script := shellQuote(repoRoot + "/" + migrateScript)
	data := shellQuote(dataDir)
	return `if [ ! -f ` + script + ` ]; then echo "ERR repo-root"; fi; ` +
		`command -v pnpm >/dev/null 2>&1 || echo "ERR pnpm"; ` +
		`if [ ! -d ` + data + ` ]; then echo "ERR tenant-dir"; fi; ` +
		`for f in ` + data + `/*.db; do [ -f "$f" ] && printf 'db=%s\n' "$f"; done`
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

// Plan orders the migration work: root.db FIRST (the auth/org plane —
// root/root.db preferred, the legacy flat root.db honored), then the
// tenant_*.db files sorted. Files matching neither shape are ignored.
func Plan(dbFiles []string) (root *Job, tenants []Job) {
	legacyFlat := ""
	for _, f := range dbFiles {
		switch {
		case strings.HasSuffix(f, "/root/root.db"):
			// the nested root is authoritative; it replaces any earlier
			// legacy sighting (only one can exist on a real estate)
			root = &Job{Path: f, NS: "root"}
		case strings.HasSuffix(f, "/root.db"):
			legacyFlat = f // provisional; a nested root/root.db replaces it
		default:
			base := path.Base(f)
			if strings.HasPrefix(base, "tenant_") && strings.HasSuffix(base, ".db") {
				tenants = append(tenants, Job{Path: f, NS: strings.TrimSuffix(base, ".db")})
			}
		}
	}
	if root == nil && legacyFlat != "" {
		root = &Job{Path: legacyFlat, NS: "root"}
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].Path < tenants[j].Path })
	return root, tenants
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
