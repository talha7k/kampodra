// Package osfacts reads and renders remote OS facts over the transport
// seam: df -P disk usage, the disk-guard verdicts, and the one-shot metrics
// snapshot (load, memory, uptime, disk breakdown, containers, top procs).
// Every renderer is a byte-exact port of the retired shell predecessor's
// scripts/common.sh awk implementations — output parity is the contract.
package osfacts

import (
	"fmt"
	"strconv"
	"strings"
)

// Verdict is the disk-guard outcome (port of disk_verdict).
type Verdict string

const (
	VerdictOK      Verdict = "ok"
	VerdictWarn    Verdict = "warn"
	VerdictFail    Verdict = "fail"
	VerdictUnknown Verdict = "unknown"
)

// DiskUsedPct ports disk_pct_from_df: the Use% column of the LAST df line
// with >= 5 fields; ok=false when the input is not df output.
func DiskUsedPct(dfOutput string) (int, bool) {
	pct := ""
	for _, line := range strings.Split(dfOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 {
			pct = fields[4]
		}
	}
	if !strings.HasSuffix(pct, "%") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(pct, "%"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// DiskVerdict ports disk_verdict: fail when usage >= require (the
// --disk-threshold fail-closed gate), warn when usage > 90, ok otherwise,
// unknown when the percentage is missing or unparseable.
func DiskVerdict(pct, require string) Verdict {
	p, err := strconv.Atoi(pct)
	if err != nil || pct == "" {
		return VerdictUnknown
	}
	if require != "" {
		if r, err := strconv.Atoi(require); err == nil && p >= r {
			return VerdictFail
		}
	}
	if p > 90 {
		return VerdictWarn
	}
	return VerdictOK
}

// MetricsRemoteCmd is the one-shot remote snapshot command — byte-equal to
// the committed common.sh METRICS_REMOTE_CMD (section markers + probes).
// Section buffers are consumed by RenderMetrics.
const MetricsRemoteCmd = "echo '%%KAMPODRA:LOAD%%'; cat /proc/loadavg 2>/dev/null; echo '%%KAMPODRA:CPU%%'; nproc 2>/dev/null; echo '%%KAMPODRA:MEM%%'; cat /proc/meminfo 2>/dev/null; echo '%%KAMPODRA:UPTIME%%'; cat /proc/uptime 2>/dev/null; echo '%%KAMPODRA:DISK%%'; df -P / 2>/dev/null; echo '%%KAMPODRA:DU%%'; du -sm /var/lib/containers /data 2>/dev/null; echo '%%KAMPODRA:PODMAN%%'; podman stats --no-stream --format '{{.Name}}|{{.CPUPerc}}|{{.MemUsage}}' 2>/dev/null; echo '%%KAMPODRA:TOP%%'; ps aux 2>/dev/null | grep -v '^USER' | sort -k6 -rn | head -n 5; echo '%%KAMPODRA:END%%'"

// MetricsUptimeHuman ports metrics_uptime_human: "<N>d <N>h <N>m" from a
// /proc/uptime first field; "" when absent/garbage.
func MetricsUptimeHuman(uptimeLine string) string {
	fields := strings.Fields(uptimeLine)
	if len(fields) == 0 {
		return ""
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return ""
	}
	s := int(secs)
	d := s / 86400
	s -= d * 86400
	h := s / 3600
	s -= h * 3600
	m := s / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// MetricsLoadLine ports metrics_load_line: "<1m> <5m> <15m> (<nproc> CPUs)";
// "load unreadable" when the loadavg line carries no averages.
func MetricsLoadLine(loadavgLine, nprocLine string) string {
	fields := strings.Fields(loadavgLine)
	if len(fields) < 3 {
		return "load unreadable"
	}
	avg := strings.Join(fields[:3], " ")
	cpus := digitsOnly(nprocLine)
	if cpus == "" {
		cpus = "?"
	}
	return fmt.Sprintf("%s (%s CPUs)", avg, cpus)
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// MetricsMemUsage ports metrics_mem_usage: "used N MiB · avail N MiB ·
// total N MiB"; available prefers MemAvailable and falls back to
// MemFree+Buffers+Cached; used floors at 0.
func MetricsMemUsage(meminfo string) string {
	var total, avail, free, buffers, cached int
	haveAvail := false
	for _, line := range strings.Split(meminfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = n
		case "MemAvailable:":
			avail, haveAvail = n, true
		case "MemFree:":
			free = n
		case "Buffers:":
			buffers = n
		case "Cached:":
			cached = n
		}
	}
	if !haveAvail {
		avail = free + buffers + cached
	}
	totalMiB := total / 1024
	availMiB := avail / 1024
	usedMiB := totalMiB - availMiB
	if usedMiB < 0 {
		usedMiB = 0
	}
	return fmt.Sprintf("used %d MiB · avail %d MiB · total %d MiB", usedMiB, availMiB, totalMiB)
}

// MetricsDiskBreakdown ports metrics_disk_breakdown: the root usage line
// plus images/data/other. "other" is the df used MiB minus the two du
// readings, floored at 0 (du and df disagree by design; triage view, not
// accounting).
func MetricsDiskBreakdown(dfRoot, duOutput string) string {
	var b strings.Builder
	pct, ok := DiskUsedPct(dfRoot)
	if ok {
		fmt.Fprintf(&b, "disk /    : %d%% used\n", pct)
	} else {
		b.WriteString("disk /    : unknown (df unreadable)\n")
	}
	images := duFieldFor(duOutput, "/var/lib/containers")
	data := duFieldFor(duOutput, "/data")
	if images == "" {
		images = "?"
	}
	if data == "" {
		data = "0"
	}
	usedMB := dfUsedMB(dfRoot) // -1 when unreadable
	fmt.Fprintf(&b, "  images  : %s MB (/var/lib/containers)\n", images)
	fmt.Fprintf(&b, "  data    : %s MB (/data)\n", data)
	if usedMB >= 0 {
		other := usedMB - atoiOr(images, 0) - atoiOr(data, 0)
		if other < 0 {
			other = 0
		}
		fmt.Fprintf(&b, "  other   : %d MB (est.)\n", other)
	} else {
		b.WriteString("  other   : unknown (df unreadable)\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// dfUsedMB ports the `NF >= 6 { u = $3 } END { print int(u / 1024) }` awk:
// the Used column (KiB) of the last df line with >= 6 fields, as MiB.
func dfUsedMB(dfOutput string) int {
	used := ""
	for _, line := range strings.Split(dfOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 {
			used = fields[2]
		}
	}
	if used == "" {
		return -1
	}
	n, err := strconv.Atoi(used)
	if err != nil {
		return -1
	}
	return n / 1024
}

func atoiOr(s string, fallback int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func duFieldFor(duOutput, mount string) string {
	for _, line := range strings.Split(duOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == mount {
			return fields[0]
		}
	}
	return ""
}

// MetricsContainersLines ports metrics_containers_lines: table rows from
// `name|cpu|mem` lines, or the honest "(no containers…)" line.
func MetricsContainersLines(statsOutput string) string {
	if strings.TrimSpace(statsOutput) == "" {
		return "  (no containers or podman unreachable)"
	}
	var rows []string
	for _, line := range strings.Split(statsOutput, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		name := parts[0]
		cpu, mem := "", ""
		if len(parts) > 1 {
			cpu = parts[1]
		}
		if len(parts) > 2 {
			mem = parts[2]
		}
		rows = append(rows, fmt.Sprintf("  %-22s %-8s %s", name, cpu, mem))
	}
	return strings.Join(rows, "\n")
}

// MetricsTopProcs ports metrics_top_procs: at most 5 rows, header dropped,
// `  %8d KB  <user>  <command>`.
func MetricsTopProcs(psOutput string) string {
	var rows []string
	for _, line := range strings.Split(psOutput, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "USER ") || strings.HasPrefix(line, "% CPU") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		rss, err := strconv.Atoi(fields[5])
		if err != nil {
			continue
		}
		user := fields[0]
		cmdFrom := 10 // field 11, 0-indexed 10
		if len(fields) < 11 {
			cmdFrom = 8
		}
		cmd := line
		if len(fields) > cmdFrom {
			cmd = strings.Join(fields[cmdFrom:], " ")
		}
		rows = append(rows, fmt.Sprintf("  %8d KB  %-8s %s", rss, user, cmd))
		if len(rows) >= 5 {
			break
		}
	}
	return strings.Join(rows, "\n")
}

// MetricsSnapshot is the parsed section buffer of one raw snapshot.
type MetricsSnapshot struct {
	sections map[string]string
}

// Section returns one section's raw text ("" when the marker was absent).
func (s MetricsSnapshot) Section(name string) string { return s.sections[name] }

// ParseMetrics slices the raw snapshot into section buffers on the
// %%KAMPODRA:*%% markers (the metrics_render loop, made reusable). Marker
// names are UPPERCASE in the wire format and lowercase in the section map.
func ParseMetrics(raw string) MetricsSnapshot {
	sections := map[string][]string{}
	section := ""
	for _, line := range strings.Split(raw, "\n") {
		if marker, ok := strings.CutPrefix(line, "%%KAMPODRA:"); ok && strings.HasSuffix(marker, "%%") {
			name := strings.ToLower(strings.TrimSuffix(marker, "%%"))
			if _, known := knownSections[name]; !known {
				section = ""
				continue
			}
			section = name
			continue
		}
		if section != "" {
			sections[section] = append(sections[section], line)
		}
	}
	flat := map[string]string{}
	for name, lines := range sections {
		flat[name] = strings.Join(lines, "\n")
	}
	return MetricsSnapshot{sections: flat}
}

var knownSections = map[string]bool{
	"load": true, "cpu": true, "mem": true, "uptime": true,
	"disk": true, "du": true, "podman": true, "top": true,
}

// RootDiskPct is the disk percentage of the snapshot's root df (M_DISK),
// for the verbose >90% warning.
func RootDiskPct(s MetricsSnapshot) (int, bool) {
	return DiskUsedPct(s.Section("disk"))
}

// RenderMetricsSnapshot renders the parsed snapshot block (no heading —
// the caller owns it).
func RenderMetricsSnapshot(s MetricsSnapshot) string {
	var out []string
	out = append(out, fmt.Sprintf("uptime    : %s", MetricsUptimeHuman(s.Section("uptime"))))
	out = append(out, fmt.Sprintf("load      : %s", MetricsLoadLine(s.Section("load"), s.Section("cpu"))))
	out = append(out, fmt.Sprintf("memory    : %s", MetricsMemUsage(s.Section("mem"))))
	out = append(out, MetricsDiskBreakdown(s.Section("disk"), s.Section("du")))
	out = append(out, "containers:")
	out = append(out, MetricsContainersLines(s.Section("podman")))
	out = append(out, "top rss   :")
	out = append(out, MetricsTopProcs(s.Section("top")))
	return strings.Join(out, "\n")
}

// RenderMetrics parses and renders a raw snapshot in one step.
func RenderMetrics(raw string) string {
	return RenderMetricsSnapshot(ParseMetrics(raw))
}
