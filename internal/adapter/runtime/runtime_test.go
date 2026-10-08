package runtime

import (
	"reflect"
	"strings"
	"testing"
)

// Fixtures mirror the shell repo's status-ops.test.ts fixtures (v0.5 shape).

var imagesFixture = strings.Join([]string{
	"aaa1111|2026-10-08 10:00:00 +0000 UTC|936 MB",
	"bbb2222|2026-10-07 09:00:00 +0000 UTC|936 MB",
	"ccc3333|2026-10-06 08:00:00 +0000 UTC|936 MB",
	"ddd4444|2026-10-05 07:00:00 +0000 UTC|936 MB",
	"latest|2026-10-08 10:00:00 +0000 UTC|936 MB",
}, "\n")

const psFixture = "127.0.0.1:5000/kampodine-api:ccc3333\nlocalhost/kamal-proxy:latest"

func TestParseImages(t *testing.T) {
	got := ParseImages(imagesFixture)
	want := []Image{
		{Tag: "aaa1111", CreatedAt: "2026-10-08 10:00:00 +0000 UTC", Size: "936 MB"},
		{Tag: "bbb2222", CreatedAt: "2026-10-07 09:00:00 +0000 UTC", Size: "936 MB"},
		{Tag: "ccc3333", CreatedAt: "2026-10-06 08:00:00 +0000 UTC", Size: "936 MB"},
		{Tag: "ddd4444", CreatedAt: "2026-10-05 07:00:00 +0000 UTC", Size: "936 MB"},
		{Tag: "latest", CreatedAt: "2026-10-08 10:00:00 +0000 UTC", Size: "936 MB"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseImages() = %+v, want %+v", got, want)
	}
	if got := ParseImages(""); len(got) != 0 {
		t.Errorf("ParseImages(empty) = %+v", got)
	}
}

func TestShaTagged(t *testing.T) {
	// The single rule that keeps latest / ts-rollback / <none> out of every
	// removal candidate: ^[0-9a-f]{7,40}$.
	got := ShaTagged(ParseImages(imagesFixture))
	var tags []string
	for _, img := range got {
		tags = append(tags, img.Tag)
	}
	want := []string{"aaa1111", "bbb2222", "ccc3333", "ddd4444"}
	if !reflect.DeepEqual(tags, want) {
		t.Errorf("ShaTagged() = %v, want %v", tags, want)
	}
	long := Image{Tag: strings.Repeat("a", 40), CreatedAt: "x", Size: "1 MB"}
	if got := ShaTagged([]Image{long}); len(got) != 1 {
		t.Errorf("40-char sha tag must pass, got %+v", got)
	}
	short := Image{Tag: "abc123", CreatedAt: "x", Size: "1 MB"} // 6 chars — too short
	if got := ShaTagged([]Image{short}); len(got) != 0 {
		t.Errorf("6-char tag must be rejected, got %+v", got)
	}
}

func TestRunningTags(t *testing.T) {
	got := RunningTags(psFixture)
	want := []string{"ccc3333", "latest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunningTags() = %v, want %v", got, want)
	}
	// Digest refs carry no tag and are skipped.
	if got := RunningTags("repo@sha256:abcdef0123\nplain"); got != nil {
		t.Errorf("digest refs must be skipped, got %v", got)
	}
}

func TestPruneSelect(t *testing.T) {
	images := ShaTagged(ParseImages(imagesFixture))
	got, err := PruneSelect(images, psFixture, 2)
	if err != nil {
		t.Fatalf("PruneSelect() error = %v", err)
	}
	want := []Removal{{Tag: "ddd4444", Size: "936 MB"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PruneSelect() = %+v, want %+v", got, want)
	}

	// keep 0 keeps everything (only running exclusions apply).
	got0, err := PruneSelect(images, psFixture, 0)
	if err != nil {
		t.Fatalf("PruneSelect(0) error = %v", err)
	}
	if len(got0) != 3 {
		t.Errorf("PruneSelect(keep=0) = %+v, want the 3 non-running tags", got0)
	}

	// Nothing to remove when every candidate is running or in the keep set.
	busy, err := PruneSelect(images, "img:aaa1111\nimg:bbb2222\nimg:ccc3333\nimg:ddd4444", 2)
	if err != nil {
		t.Fatalf("PruneSelect(all running) error = %v", err)
	}
	if len(busy) != 0 {
		t.Errorf("PruneSelect(all running) = %+v, want none", busy)
	}

	// Invalid keepN fails closed.
	if _, err := PruneSelect(images, psFixture, -1); err == nil {
		t.Error("PruneSelect(-1) must fail closed")
	}
	if _, err := PruneSelect(images, psFixture, 1); err != nil {
		t.Errorf("PruneSelect(1) error = %v", err)
	}
}

func TestPruneSelectNewestWinsOnTie(t *testing.T) {
	// Identical CreatedAt: deterministic tie-break (stable input order).
	tied := []Image{
		{Tag: "aaaa1111", CreatedAt: "2026-10-08 10:00:00 +0000 UTC", Size: "10 MB"},
		{Tag: "bbbb2222", CreatedAt: "2026-10-08 10:00:00 +0000 UTC", Size: "20 MB"},
		{Tag: "cccc3333", CreatedAt: "2026-10-08 10:00:00 +0000 UTC", Size: "30 MB"},
	}
	got, err := PruneSelect(tied, "", 1)
	if err != nil {
		t.Fatalf("PruneSelect() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("PruneSelect(tie, keep=1) removed %d, want 2", len(got))
	}
	for _, r := range got {
		if r.Tag == "aaaa1111" {
			t.Errorf("first candidate must be the kept-newest on tie, removals = %+v", got)
		}
	}
}

func TestSumSizesHuman(t *testing.T) {
	tests := []struct {
		name  string
		sizes []string
		want  string
	}{
		{name: "under a GB", sizes: []string{"936 MB"}, want: "~936 MB"},
		{name: "crosses a GB", sizes: []string{"936 MB", "936 MB"}, want: "~1.8 GB"},
		{name: "KB accumulates (awk %d floors)", sizes: []string{"512 KB", "1024 KB"}, want: "~1 MB"},
		{name: "nothing is ~0 MB", sizes: nil, want: "~0 MB"},
		{name: "tiny fragments floor to ~0 MB", sizes: []string{"100 KB"}, want: "~0 MB"},
		{name: "garbage sizes count as 0", sizes: []string{"abc MB", "5 MB"}, want: "~5 MB"},
		{name: "GB units re-render through MB totals", sizes: []string{"2 GB"}, want: "~2.0 GB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SumSizesHuman(tt.sizes); got != tt.want {
				t.Errorf("SumSizesHuman(%v) = %q, want %q", tt.sizes, got, tt.want)
			}
		})
	}
}
