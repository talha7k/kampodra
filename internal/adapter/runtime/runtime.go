// Package runtime is the podman adapter: the VM's container/image surface
// (sha-tagged deploy images, running containers) and the prune keep-set
// math — byte-exact ports of the committed kampodine scripts/common.sh
// (sha_tagged_lines, running_tags_from_ps, prune_select, sum_sizes_human).
package runtime

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Image is one line of `podman images --format '{{.Tag}}|{{.CreatedAt}}|{{.Size}}'`.
type Image struct {
	Tag       string
	CreatedAt string
	Size      string
}

// Removal is one prune candidate: `tag|size`.
type Removal struct {
	Tag  string
	Size string
}

// shaTagRe is THE single rule keeping latest / ts-rollback / <none> out of
// every removal candidate: only sha-like tags are deploy images.
var shaTagRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// DeployImageRepo is the VM-local registry path the deploy pipeline retags
// to (the shell scripts' IMAGE constant).
const DeployImageRepo = "127.0.0.1:5000/kampodine-api"

// ParseImages parses the images listing (tolerant of empty input).
func ParseImages(imagesOutput string) []Image {
	var out []Image
	for _, line := range strings.Split(imagesOutput, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		img := Image{Tag: parts[0]}
		if len(parts) > 1 {
			img.CreatedAt = parts[1]
		}
		if len(parts) > 2 {
			img.Size = parts[2]
		}
		out = append(out, img)
	}
	return out
}

// ShaTagged filters image lines to sha-like tags.
func ShaTagged(images []Image) []Image {
	var out []Image
	for _, img := range images {
		if shaTagRe.MatchString(img.Tag) {
			out = append(out, img)
		}
	}
	return out
}

// RunningTags ports running_tags_from_ps: bare tags of running container
// images; digest refs (repo@sha256:…) carry no tag and are skipped.
func RunningTags(psOutput string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(psOutput, "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || strings.Contains(ref, "@") || !strings.Contains(ref, ":") {
			continue
		}
		tag := ref[strings.LastIndex(ref, ":")+1:]
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	return out
}

// PruneKeepTags are named tags the keep-set never touches (belt+braces —
// the sha filter already excludes them).
var PruneKeepTags = map[string]bool{"latest": true, "ts-rollback": true}

// PruneSelect ports prune_select: removal candidates = sha-tagged images
// minus the running ones, minus named keeps, minus the newest keepN by
// creation timestamp. The keep-newest selection sorts descending by the
// CreatedAt string (ISO-ish podman timestamps sort lexically = chronologically);
// equal timestamps fall back to ascending tag order (GNU sort's last-resort
// full-line comparison) so the result is deterministic.
func PruneSelect(images []Image, psOutput string, keepN int) ([]Removal, error) {
	if keepN < 0 {
		return nil, fmt.Errorf("keep must be >= 0 (got %d)", keepN)
	}
	running := map[string]bool{}
	for _, tag := range RunningTags(psOutput) {
		running[tag] = true
	}
	if len(images) == 0 {
		return nil, nil
	}
	keepNewest := map[string]bool{}
	if keepN > 0 {
		sorted := make([]Image, len(images))
		copy(sorted, images)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].CreatedAt != sorted[j].CreatedAt {
				return sorted[i].CreatedAt > sorted[j].CreatedAt
			}
			return sorted[i].Tag < sorted[j].Tag
		})
		for _, img := range sorted[:keepN] {
			keepNewest[img.Tag] = true
		}
	}
	var out []Removal
	for _, img := range images {
		if PruneKeepTags[img.Tag] || running[img.Tag] || keepNewest[img.Tag] {
			continue
		}
		out = append(out, Removal{Tag: img.Tag, Size: img.Size})
	}
	return out, nil
}

// SumSizesHuman ports sum_sizes_human: an approximate "~X GB"/"~X MB" total
// for the prune reward — it labels itself with ~ by design.
func SumSizesHuman(sizes []string) string {
	total := 0.0
	for _, size := range sizes {
		fields := strings.Fields(size)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		switch strings.ToUpper(fields[1]) {
		case "GB":
			total += v * 1024
		case "MB":
			total += v
		case "KB":
			total += v / 1024
		}
	}
	switch {
	case total >= 1024:
		return fmt.Sprintf("~%.1f GB", total/1024)
	case total >= 1:
		return fmt.Sprintf("~%d MB", int(total))
	default:
		return "~0 MB"
	}
}
