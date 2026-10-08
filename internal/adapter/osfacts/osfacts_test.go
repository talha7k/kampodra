package osfacts

import (
	"strings"
	"testing"
)

// All expectations below are ports of the committed kampodine
// scripts/common.sh awk implementations — byte-identical output is the goal.

const dfOK = "Filesystem     1K-blocks      Used Available Use% Mounted on\n" +
	"/dev/vda3       82078644  50778644  2973092  62% /"
const dfFull = "Filesystem     1K-blocks      Used Available Use% Mounted on\n" +
	"/dev/vda3       82078644  78783432   1173092  96% /"

func TestDiskUsedPct(t *testing.T) {
	tests := []struct {
		name  string
		dfOut string
		want  int
		ok    bool
	}{
		{name: "healthy disk", dfOut: dfOK, want: 62, ok: true},
		{name: "nearly full disk", dfOut: dfFull, want: 96, ok: true},
		{name: "last data line wins (df -P shape)", dfOut: "hdr 1 2 3 4 5\n/dev/vda3 82078644 50778644 2973092 62% /\n/dev/sda1 1000 500 500 50% /mnt", want: 50, ok: true},
		{name: "empty output is unknown", dfOut: "", ok: false},
		{name: "garbage is unknown", dfOut: "not df output", ok: false},
		{name: "5-field line without trailing % is unknown", dfOut: "a b c d e", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := DiskUsedPct(tt.dfOut)
			if ok != tt.ok {
				t.Fatalf("DiskUsedPct() ok = %v, want %v (got %d)", ok, tt.ok, got)
			}
			if ok && got != tt.want {
				t.Errorf("DiskUsedPct() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDiskVerdict(t *testing.T) {
	tests := []struct {
		name    string
		pct     string
		require string
		want    Verdict
	}{
		{name: "ok below 90", pct: "62", require: "", want: VerdictOK},
		{name: "warn above 90", pct: "96", require: "", want: VerdictWarn},
		{name: "fail at require gate", pct: "96", require: "90", want: VerdictFail},
		{name: "fail at exact require", pct: "90", require: "90", want: VerdictFail},
		{name: "ok under require", pct: "62", require: "90", want: VerdictOK},
		{name: "empty is unknown", pct: "", require: "", want: VerdictUnknown},
		{name: "non-numeric is unknown", pct: "abc", require: "", want: VerdictUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DiskVerdict(tt.pct, tt.require); got != tt.want {
				t.Errorf("DiskVerdict(%q, %q) = %q, want %q", tt.pct, tt.require, got, tt.want)
			}
		})
	}
}

func TestMetricsUptimeHuman(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "days hours minutes", line: "1046160.5 2.36", want: "12d 2h 36m"},
		{name: "hours only", line: "7200.0 0.0", want: "2h 0m"},
		{name: "minutes only", line: "2700.0 1.1", want: "45m"},
		{name: "under a minute floors to 0m (awk int semantics)", line: "45.2 1.1", want: "0m"},
		{name: "absent is empty", line: "", want: ""},
		{name: "garbage is empty", line: "up a while", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MetricsUptimeHuman(tt.line); got != tt.want {
				t.Errorf("MetricsUptimeHuman(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestMetricsLoadLine(t *testing.T) {
	tests := []struct {
		name   string
		loadav string
		nproc  string
		want   string
	}{
		{name: "normal", loadav: "0.52 0.58 0.59 2/123 4567", nproc: "4", want: "0.52 0.58 0.59 (4 CPUs)"},
		{name: "unreadable loadavg", loadav: "", nproc: "4", want: "load unreadable"},
		{name: "missing nproc renders ?", loadav: "0.52 0.58 0.59", nproc: "", want: "0.52 0.58 0.59 (? CPUs)"},
		{name: "garbage nproc renders ?", loadav: "0.52 0.58 0.59", nproc: "n/a", want: "0.52 0.58 0.59 (? CPUs)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MetricsLoadLine(tt.loadav, tt.nproc); got != tt.want {
				t.Errorf("MetricsLoadLine(%q, %q) = %q, want %q", tt.loadav, tt.nproc, got, tt.want)
			}
		})
	}
}

const meminfo = `MemTotal:        1573248 kB
MemFree:          123456 kB
MemAvailable:    1048576 kB
Buffers:           12345 kB
Cached:           654321 kB`

func TestMetricsMemUsage(t *testing.T) {
	// total 1573248/1024 = 1536; avail 1048576/1024 = 1024; used 512.
	want := "used 512 MiB · avail 1024 MiB · total 1536 MiB"
	if got := MetricsMemUsage(meminfo); got != want {
		t.Errorf("MetricsMemUsage() = %q, want %q", got, want)
	}
	// Fallback: no MemAvailable -> MemFree+Buffers+Cached (summed in kB,
	// THEN divided: (123456+12345+654321)/1024 = 771; used 1536-771 = 765).
	fallback := "MemTotal: 1573248 kB\nMemFree: 123456 kB\nBuffers: 12345 kB\nCached: 654321 kB"
	wantFallback := "used 765 MiB · avail 771 MiB · total 1536 MiB"
	if got := MetricsMemUsage(fallback); got != wantFallback {
		t.Errorf("MetricsMemUsage(fallback) = %q, want %q", got, wantFallback)
	}
	// Used floors at 0.
	overcommit := "MemTotal: 100 kB\nMemAvailable: 10240 kB"
	if got := MetricsMemUsage(overcommit); got != "used 0 MiB · avail 10 MiB · total 0 MiB" {
		t.Errorf("MetricsMemUsage(overcommit) = %q", got)
	}
}

func TestMetricsDiskBreakdown(t *testing.T) {
	duOut := "4096\t/var/lib/containers\n512\t/data\n"
	want := strings.Join([]string{
		"disk /    : 62% used",
		"  images  : 4096 MB (/var/lib/containers)",
		"  data    : 512 MB (/data)",
		"  other   : 49186 MB (est.)",
	}, "\n")
	// df used = 50778644 KiB -> int(50778644/1024) = 49588; other = 49588-4096-512 = 44980
	want = strings.Replace(want, "49186", "44980", 1)
	got := MetricsDiskBreakdown(dfOK, duOut)
	if got != want {
		t.Errorf("MetricsDiskBreakdown() =\n%s\nwant\n%s", got, want)
	}

	gotUnknown := MetricsDiskBreakdown("", "")
	wantUnknown := strings.Join([]string{
		"disk /    : unknown (df unreadable)",
		"  images  : ? MB (/var/lib/containers)",
		"  data    : 0 MB (/data)",
		"  other   : unknown (df unreadable)",
	}, "\n")
	if gotUnknown != wantUnknown {
		t.Errorf("MetricsDiskBreakdown(unknown) =\n%s\nwant\n%s", gotUnknown, wantUnknown)
	}
}

func TestMetricsContainersLines(t *testing.T) {
	if got := MetricsContainersLines("   \n"); got != "  (no containers or podman unreachable)" {
		t.Errorf("MetricsContainersLines(empty) = %q", got)
	}
	raw := "kampodine-api|2.10%|512MiB / 2GiB\nkamal-proxy|0.31%|64MiB / 2GiB"
	want := strings.Join([]string{
		"  kampodine-api          2.10%    512MiB / 2GiB",
		"  kamal-proxy            0.31%    64MiB / 2GiB",
	}, "\n")
	if got := MetricsContainersLines(raw); got != want {
		t.Errorf("MetricsContainersLines() = %q, want %q", got, want)
	}
}

func TestMetricsTopProcs(t *testing.T) {
	raw := strings.Join([]string{
		"USER       PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND",
		"root       123  0.5  1.2 123456 65432 ?        Sl   10:00   0:01 podman serve",
		"root       456  0.1  0.3  23456 12345 ?        Ss   10:00   0:00 supervise-daemon kampodine-api",
	}, "\n")
	want := strings.Join([]string{
		"     65432 KB  root     podman serve",
		"     12345 KB  root     supervise-daemon kampodine-api",
	}, "\n")
	if got := MetricsTopProcs(raw); got != want {
		t.Errorf("MetricsTopProcs() = %q, want %q", got, want)
	}
	// Caps at 5 rows.
	six := "a b c d e 111 cmd1\nb b c d e 222 cmd2\nc b c d e 333 cmd3\nd b c d e 444 cmd4\ne b c d e 555 cmd5\nf b c d e 666 cmd6"
	if n := strings.Count(MetricsTopProcs(six), "\n") + 1; n != 5 {
		t.Errorf("MetricsTopProcs() returned %d rows, want cap 5", n)
	}
}

func TestRenderMetrics(t *testing.T) {
	raw := strings.Join([]string{
		"%%KAMPODRA:LOAD%%",
		"0.52 0.58 0.59 2/123 4567",
		"%%KAMPODRA:CPU%%",
		"4",
		"%%KAMPODRA:MEM%%",
		meminfo,
		"%%KAMPODRA:UPTIME%%",
		"1046160.5 2.36",
		"%%KAMPODRA:DISK%%",
		dfOK,
		"%%KAMPODRA:DU%%",
		"4096\t/var/lib/containers",
		"512\t/data",
		"%%KAMPODRA:PODMAN%%",
		"kampodine-api|2.10%|512MiB / 2GiB",
		"%%KAMPODRA:TOP%%",
		"root       123  0.5  1.2 123456 65432 ?        Sl   10:00   0:01 podman serve",
		"%%KAMPODRA:END%%",
	}, "\n")

	want := strings.Join([]string{
		"uptime    : 12d 2h 36m",
		"load      : 0.52 0.58 0.59 (4 CPUs)",
		"memory    : used 512 MiB · avail 1024 MiB · total 1536 MiB",
		"disk /    : 62% used",
		"  images  : 4096 MB (/var/lib/containers)",
		"  data    : 512 MB (/data)",
		"  other   : 44980 MB (est.)",
		"containers:",
		"  kampodine-api          2.10%    512MiB / 2GiB",
		"top rss   :",
		"     65432 KB  root     podman serve",
	}, "\n")
	if got := RenderMetrics(raw); got != want {
		t.Errorf("RenderMetrics() =\n%s\nwant\n%s", got, want)
	}
}

func TestMetricsRemoteCommandShape(t *testing.T) {
	// The remote one-shot must carry every section marker the renderer
	// consumes — a drift here silently blanks the snapshot.
	for _, marker := range []string{
		"%%KAMPODRA:LOAD%%", "%%KAMPODRA:CPU%%", "%%KAMPODRA:MEM%%",
		"%%KAMPODRA:UPTIME%%", "%%KAMPODRA:DISK%%", "%%KAMPODRA:DU%%",
		"%%KAMPODRA:PODMAN%%", "%%KAMPODRA:TOP%%", "%%KAMPODRA:END%%",
	} {
		if !strings.Contains(MetricsRemoteCmd, marker) {
			t.Errorf("MetricsRemoteCmd missing marker %s", marker)
		}
	}
	if !strings.Contains(MetricsRemoteCmd, "df -P /") || !strings.Contains(MetricsRemoteCmd, "du -sm /var/lib/containers /data") {
		t.Error("MetricsRemoteCmd lost the df/du probes")
	}
}
