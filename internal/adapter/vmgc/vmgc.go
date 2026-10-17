// Package vmgc is the `vm gc` adapter: the VM garbage sweep's classification
// and allowlist core. `kampodra vm gc` exists because agents kept hand-rolling
// find|rm sweeps over ssh — deleting other people's files by accident. The
// sweep inventories host artifacts into a typed table (dry run by default);
// --apply deletes ONLY the stale classes (scratch / stale-script / residue).
//
// The allowlists below are the safety contract, hard-coded as never-touch:
//   - OS paths (/bin,/sbin,/usr,/etc,/boot) and podman's own storage
//   - the live artifact/web/backup chains under /data
//   - kampodra's own version-stamped helpers (/tmp/kampodra-*)
//   - OS session dirs (/tmp/.X11-unix & friends)
//   - live build contexts (scratch-zone dirs holding a Dockerfile)
//   - owner-decision items by pattern (cutover db snapshots next to the
//     data dir, /root pre-op safety snapshots)
//
// The deployed project's own naming (data dir, containers, services) is
// routed in from ProjectConfig via Protect — never hard-coded here.
//
// podman containers/images are inventoried and cross-checked against the
// estate naming, but the adapter exposes NO mutation surface for them —
// report-only, by construction.
package vmgc

import (
	"fmt"
	"sort"
	"strings"
)

// Entry is one inventoried host artifact.
type Entry struct {
	Path  string
	Size  int64
	MTime int64  // unix epoch
	Kind  string // "file" | "dir" | "other"
}

// Class is the typed classification verdict for one entry.
type Class string

const (
	ClassScratch       Class = "scratch"         // /tmp,/var/tmp,/opt,/home junk — deletable
	ClassStaleScript   Class = "stale-script"    // .sh outside OS/infra paths — deletable
	ClassResidue       Class = "residue"         // legacy build residues — deletable
	ClassKampodra      Class = "kampodra-helper" // kampodra's own helpers — never
	ClassOSSession     Class = "os-session"      // /tmp/.X11-unix & friends — never
	ClassOS            Class = "os"              // OS trees — never
	ClassInfra         Class = "infra"           // vm-prepare-rendered infra scripts — never
	ClassTenantData    Class = "tenant-data"     // the project's data dir — never
	ClassLiveArtifact  Class = "live"            // artifact/web/backup chains — never
	ClassOwnerDecision Class = "owner-decision"  // owner decides — never automatic
	ClassMandatory     Class = "mandatory"       // mandatory estate (podman) — never
	ClassUnknown       Class = "unknown"         // unrecognized — report, never delete
)

// Deletable reports whether a class is in the --apply deletion set.
func Deletable(c Class) bool {
	switch c {
	case ClassScratch, ClassStaleScript, ClassResidue:
		return true
	}
	return false
}

// Protect carries the deployed project's estate naming into the
// classifier — routed from ProjectConfig, never hard-coded here (the
// genericity guard forbids app naming outside project.go). Zero fields
// fail closed: paths that would need them classify unknown (report-only).
type Protect struct {
	Container    string   // the app container (ProjectConfig.Container)
	ShadowSuffix string   // rolling-deploy shadow suffix
	Services     []string // the estate's service names (ProjectConfig.Services)
	DataDir      string   // the tenant/app data dir (ProjectConfig.DataDir)
}

// Classification rules, first match wins. Zones outside the sweep roots
// (/tmp /var/tmp /opt /home /data /root) can only appear in tests or
// hand-built entries; they classify defensively to a never-touch class.
func ClassifyWith(pr Protect, e Entry) Class {
	p := e.Path
	switch {
	case hasAnyPrefix(p, "/proc/", "/sys/", "/dev/", "/run/"):
		return ClassOS
	case hasAnyPrefix(p, "/var/lib/containers/"):
		return ClassOS // podman's own storage — never host-touchable
	case hasAnyPrefix(p, "/bin/"), hasAnyPrefix(p, "/sbin/"), p == "/bin", p == "/sbin":
		return ClassOS
	case hasAnyPrefix(p, "/usr/"):
		if hasAnyPrefix(p, "/usr/local/sbin/") {
			return ClassInfra // vm-prepare-rendered infra (anchor, datamount, growroot)
		}
		return ClassOS
	case hasAnyPrefix(p, "/etc/"), hasAnyPrefix(p, "/boot/"):
		return ClassOS
	case p == "/data" || hasAnyPrefix(p, "/data/"):
		return classifyData(pr, p, e)
	case p == "/root" || hasAnyPrefix(p, "/root/"):
		return classifyRoot(e)
	case hasAnyPrefix(p, "/tmp/"), hasAnyPrefix(p, "/var/tmp/"):
		return classifyScratchZone(p, e)
	case hasAnyPrefix(p, "/opt/"), hasAnyPrefix(p, "/home/"):
		// /opt holds optional software, /home holds user data — neither is
		// ephemeral like /tmp, so unrecognized content is report-only.
		return classifySemiZone(p, e)
	default:
		return ClassUnknown
	}
}

// Classify classifies with no estate knowledge (zero Protect). Tenant and
// estate-specific paths fail closed to report-only classes.
func Classify(e Entry) Class { return ClassifyWith(Protect{}, e) }

func classifyData(pr Protect, p string, e Entry) Class {
	switch {
	case pr.DataDir != "" && (p == pr.DataDir || hasAnyPrefix(p, pr.DataDir+"/")):
		return ClassTenantData
	case pr.DataDir != "" && hasAnyPrefix(p, pr.DataDir+"-"):
		// Cutover db snapshots (<dataDir>-preflip-snap-*, …) — part of the
		// flip plan until the owner retires them.
		return ClassOwnerDecision
	case p == "/data/app":
		return ClassLiveArtifact
	case hasAnyPrefix(p, "/data/app/releases"):
		// The legacy web-release staging dir (superseded by /data/web in
		// the binary-flow era). Guarded empty-dir delete at apply time.
		return ClassResidue
	case hasAnyPrefix(p, "/data/app/"):
		return ClassLiveArtifact // the binary chain + anything deploy-managed
	case p == "/data/web" || hasAnyPrefix(p, "/data/web/"):
		return ClassLiveArtifact // the web release chain (current + .prev)
	case p == "/data/.last-backup":
		return ClassLiveArtifact
	case hasAnyPrefix(p, "/data/") && strings.HasSuffix(p, ".log"):
		return ClassLiveArtifact // backup/ops telemetry
	default:
		return ClassUnknown
	}
}

// classifyRoot: /root is home — only the directive's headline class (stale
// .sh scripts) is deletable there; everything else is an owner decision
// (pre-op safety snapshots live here).
func classifyRoot(e Entry) Class {
	if e.Kind == "file" && strings.HasSuffix(e.Path, ".sh") {
		return ClassStaleScript
	}
	return ClassOwnerDecision
}

func classifyScratchZone(p string, e Entry) Class {
	base := baseName(p)
	switch {
	case base == ".X11-unix" || base == ".ICE-unix" || base == ".font-unix" || base == ".Test-unix":
		return ClassOSSession
	case strings.HasPrefix(p, "/tmp/kampodra-") || strings.HasPrefix(p, "/var/tmp/kampodra-"):
		return ClassKampodra // version-stamped helpers + snapshot scratch
	case e.Kind == "file" && strings.HasSuffix(p, ".sh"):
		return ClassStaleScript
	default:
		return ClassScratch
	}
}

// classifySemiZone: /opt and /home are sweep zones but not ephemeral ones —
// only the directive's headline class (.sh outside OS paths) is deletable;
// everything unrecognized is report-only.
func classifySemiZone(p string, e Entry) Class {
	if e.Kind == "file" && strings.HasSuffix(p, ".sh") {
		return ClassStaleScript
	}
	return ClassUnknown
}

// --- sweep plumbing ---------------------------------------------------------

// sweepZones are the sweep's start points; everything else is out of scope.
var sweepZones = []string{"/tmp", "/var/tmp", "/opt", "/home", "/data", "/root"}

// SweepCommand builds the read-only inventory command: busybox find over the
// sweep zones, pruning the live tenant estate (huge, protected), stat-
// printing every entry. Nothing here mutates. With an empty DataDir the
// prune clause is dropped — those paths then classify unknown (report-only).
func SweepCommand(pr Protect) string {
	prune := ""
	if pr.DataDir != "" {
		prune = ` \( -path ` + pr.DataDir + ` -o -path "` + pr.DataDir + `/*" \) -prune -o`
	}
	return `find ` + strings.Join(sweepZones, " ") + ` -xdev` + prune +
		` -mindepth 1 -exec stat -c "%s|%Y|%F|%n" {} + 2>/dev/null`
}

// BuildContextProbeCommand finds Dockerfiles in the ephemeral zones: the
// dir containing one is a live build context — someone's work in flight,
// never auto-deletable garbage.
func BuildContextProbeCommand() string {
	return `find /tmp /var/tmp /opt /home -maxdepth 3 -name Dockerfile 2>/dev/null`
}

// DirOfBuildContext maps Dockerfile hits to their build-context dirs
// (one level up), deduplicated.
func DirOfBuildContext(hits []string) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, h := range hits {
		dir := pathDirOf(h)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	return dirs
}

// InUseProbeCommand builds the activity probe: every /proc cmdline (NULs
// flattened) plus every running container's bind-mount sources, one token
// per line. Candidates that appear in this stream are IN USE — the plan
// blocks them instead of deleting.
func InUseProbeCommand() string {
	return `( cat /proc/[0-9]*/cmdline 2>/dev/null | tr "\0" " ";` +
		` podman ps -aq 2>/dev/null | xargs -r podman inspect --format "{{range .Mounts}}{{.Source}} {{end}}" 2>/dev/null )` +
		` | tr " " "\n" | grep -v "^$"`
}

// PodmanPSCommand inventories containers (report-only).
func PodmanPSCommand() string {
	return `podman ps -a --format "{{.Names}}|{{.Image}}|{{.Status}}"`
}

// PodmanImagesCommand inventories images (report-only).
func PodmanImagesCommand() string {
	return `podman images --format "{{.Repository}}:{{.Tag}}|{{.ID}}|{{.Size}}"`
}

// ParseSweepListing parses SweepCommand's stat output into entries; garbage
// lines (find warnings, blanks) are skipped.
func ParseSweepListing(out string) []Entry {
	var entries []Entry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 || !strings.HasPrefix(parts[3], "/") {
			continue
		}
		var size, mtime int64
		if _, err := fmt.Sscanf(parts[0], "%d", &size); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(parts[1], "%d", &mtime); err != nil {
			continue
		}
		kind := "other"
		switch parts[2] {
		case "regular file":
			kind = "file"
		case "directory":
			kind = "dir"
		}
		entries = append(entries, Entry{Path: parts[3], Size: size, MTime: mtime, Kind: kind})
	}
	return entries
}

// Container is one inventoried container (report-only — never mutated).
type Container struct{ Name, Image, Status string }

// Image is one inventoried image (report-only — never mutated).
type Image struct{ Ref, ID, Size string }

// ParsePodmanPS parses PodmanPSCommand's output.
func ParsePodmanPS(out string) []Container {
	var cs []Container
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		cs = append(cs, Container{Name: parts[0], Image: parts[1], Status: parts[2]})
	}
	return cs
}

// ParsePodmanImages parses PodmanImagesCommand's output.
func ParsePodmanImages(out string) []Image {
	var ims []Image
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 3 || parts[0] == "" {
			continue
		}
		ims = append(ims, Image{Ref: parts[0], ID: parts[1], Size: parts[2]})
	}
	return ims
}

// proxyEdgeContainer is kampodra's own TLS edge (infra constant — the same
// name deploy_pipeline re-points; not project naming).
const proxyEdgeContainer = "kamal-proxy"

// ClassifiedContainer / ClassifiedImage pair a podman entry with its
// estate verdict (report-only — vm gc never mutates podman).
type ClassifiedContainer struct {
	Container
	Class Class
}

// ClassifiedImage pairs an image with its estate verdict.
type ClassifiedImage struct {
	Image
	Class Class
}

// ClassifyEstate cross-checks the podman inventory against the project's
// estate naming (routed from ProjectConfig):
//   - the project stack (app container + its shadow + the TLS edge) is
//     mandatory — the deploy flow owns it;
//   - any other configured service's container (and its image) is an
//     estate service — owner-decision;
//   - random-named strays (orphaned `podman run` probes) are unknown —
//     report-only, the owner removes them deliberately.
func ClassifyEstate(pr Protect, cs []Container, ims []Image) ([]ClassifiedContainer, []ClassifiedImage) {
	mandatory := map[string]bool{proxyEdgeContainer: true}
	if pr.Container != "" {
		mandatory[pr.Container] = true
		if pr.ShadowSuffix != "" {
			mandatory[pr.Container+pr.ShadowSuffix] = true
		}
	}
	services := map[string]bool{}
	for _, s := range pr.Services {
		services[s] = true
	}
	// Image verdicts ride the containers that use them: an image in use by
	// a mandatory container is mandatory; one used by an estate service is
	// an owner decision; unclaimed refs are unknown. When several
	// containers share an image the strongest verdict wins.
	imageClass := map[string]Class{}
	var ccs []ClassifiedContainer
	for _, c := range cs {
		var cl Class
		switch {
		case mandatory[c.Name]:
			cl = ClassMandatory
		case services[c.Name]:
			cl = ClassOwnerDecision
		default:
			cl = ClassUnknown
		}
		imageClass[c.Image] = strongerClass(imageClass[c.Image], cl)
		ccs = append(ccs, ClassifiedContainer{Container: c, Class: cl})
	}
	var cims []ClassifiedImage
	for _, im := range ims {
		cl := imageClass[im.Ref]
		if cl == "" {
			cl = ClassUnknown
		}
		cims = append(cims, ClassifiedImage{Image: im, Class: cl})
	}
	return ccs, cims
}

// strongerClass merges two verdicts about the same artifact: the stricter
// (more protective) one wins — mandatory > owner-decision > unknown.
func strongerClass(a, b Class) Class {
	rank := func(c Class) int {
		switch c {
		case ClassMandatory:
			return 3
		case ClassOwnerDecision:
			return 2
		case ClassUnknown:
			return 1
		}
		return 0
	}
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// --- planning ----------------------------------------------------------------

// Classified pairs an entry with its verdict.
type Classified struct {
	Entry Entry
	Class Class
}

// Removal is one planned deletion (or one blocked candidate).
type Removal struct {
	Entry Entry
	Cmd   string
}

// Plan is the classified inventory plus the guarded deletion set.
type Plan struct {
	Rows    []Classified // every entry, sorted by path
	Removes []Removal    // deletable, not in use, not folded into a parent
	Blocked []Removal    // deletable but IN USE — reported, never deleted
	Freed   int64        // bytes reclaimed (dir subtrees counted once)
}

// BuildPlan classifies entries, overrides live build contexts to
// owner-decision, folds deletable children into their deletable parent
// dirs, blocks in-use candidates, and builds exact removal commands.
// The allowlist is enforced twice: the classifier decides, and
// RemoveCommand re-validates each path before emitting an rm.
func BuildPlan(entries []Entry, inUse, ownerDirs []string, pr Protect) Plan {
	ownerDirSet := map[string]bool{}
	for _, d := range ownerDirs {
		ownerDirSet[d] = true
	}
	rows := make([]Classified, 0, len(entries))
	for _, e := range entries {
		cl := ClassifyWith(pr, e)
		if Deletable(cl) {
			for d := range ownerDirSet {
				if e.Path == d || hasAnyPrefix(e.Path, d+"/") {
					cl = ClassOwnerDecision // live build context — never auto
					break
				}
			}
		}
		rows = append(rows, Classified{Entry: e, Class: cl})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Entry.Path < rows[j].Entry.Path })

	inUseSet := map[string]bool{}
	for _, p := range inUse {
		inUseSet[p] = true
	}

	// deletableDirs lets children fold into a deletable parent's rm -rf.
	deletableDirs := map[string]bool{}
	for _, r := range rows {
		if Deletable(r.Class) && r.Entry.Kind == "dir" {
			deletableDirs[r.Entry.Path] = true
		}
	}

	plan := Plan{Rows: rows}
	for _, r := range rows {
		if !Deletable(r.Class) {
			continue
		}
		if foldedUnder(r.Entry.Path, deletableDirs) {
			plan.Freed += r.Entry.Size // bytes reclaimed by the parent's rm -rf
			continue
		}
		cmd, err := RemoveCommand(r.Entry)
		if err != nil {
			// Unreachable when Classify and RemoveCommand agree — belt
			// suspender: skip and surface in the summary.
			continue
		}
		plan.Freed += r.Entry.Size
		if inUseSet[r.Entry.Path] {
			plan.Blocked = append(plan.Blocked, Removal{Entry: r.Entry, Cmd: cmd})
			continue
		}
		plan.Removes = append(plan.Removes, Removal{Entry: r.Entry, Cmd: cmd})
	}
	return plan
}

// foldedUnder reports whether some proper ancestor of p is itself a
// deletable dir (the parent's rm -rf covers p).
func foldedUnder(p string, deletableDirs map[string]bool) bool {
	for dir := range deletableDirs {
		if dir != p && strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

// --- deletion guard -----------------------------------------------------------

// ValidateDeletablePath is the delete-command gate: absolute, no shell
// metacharacters or traversal, inside a sweep zone, and classified
// deletable. Anything else is refused.
func ValidateDeletablePath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("refusing non-absolute path %q", p)
	}
	if strings.ContainsAny(p, " \t\n\"'`;&|$()<>()\\*?[]{}!#~") {
		return fmt.Errorf("refusing path with shell metacharacters: %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("refusing path with traversal: %q", p)
		}
	}
	zone := false
	for _, z := range sweepZones {
		if p == z || strings.HasPrefix(p, z+"/") {
			zone = true
			break
		}
	}
	if !zone {
		return fmt.Errorf("refusing path outside the sweep zones %v: %q", sweepZones, p)
	}
	if c := Classify(Entry{Path: p, Kind: kindForValidation(p)}); !Deletable(c) {
		return fmt.Errorf("refusing path classified %s (not deletable): %q", c, p)
	}
	return nil
}

// kindForValidation: the .sh rule keys on Kind=file; dirs delete by a
// different command shape. A path being validated for deletion is a file
// unless it classifies residue-dir by name (releases).
func kindForValidation(p string) string {
	if strings.HasSuffix(p, ".sh") {
		return "file"
	}
	if p == "/data/app/releases" || strings.HasPrefix(p, "/data/app/releases/") {
		return "dir"
	}
	return "file"
}

// RemoveCommand builds the exact deletion command for one entry. Dirs use
// rm -rf (scratch zones only — validated), the legacy empty release dir
// uses a guarded empty-only delete, files use rm -f. Non-deletable classes
// are refused — this is the second allowlist enforcement point.
func RemoveCommand(e Entry) (string, error) {
	if err := ValidateDeletablePath(e.Path); err != nil {
		return "", err
	}
	if e.Kind == "dir" {
		if e.Path == "/data/app/releases" || strings.HasPrefix(e.Path, "/data/app/releases/") {
			return fmt.Sprintf("find %s -maxdepth 0 -type d -empty -delete", e.Path), nil
		}
		if c := Classify(e); c != ClassScratch {
			return "", fmt.Errorf("rm -rf restricted to scratch-zone dirs — %q is %s", e.Path, c)
		}
		return "rm -rf " + e.Path, nil
	}
	return "rm -f " + e.Path, nil
}

// HumanSize renders a byte count for the inventory table.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func hasAnyPrefix(p string, prefixes ...string) bool {
	for _, pre := range prefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// pathDirOf is the parent dir of a path ("" when there is none).
func pathDirOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}
