package command

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The golden-image OS parameterization helpers must fail closed on an
// unknown --os, name the supported set, and keep the alpine default
// byte-identical to the pre-ubuntu behavior.

func TestGoldenImagePrefixPerOS(t *testing.T) {
	alpine, err := goldenImagePrefix("app", "alpine")
	if err != nil || alpine != "app-alpine" {
		t.Errorf("goldenImagePrefix(app, alpine) = %q %v, want app-alpine nil", alpine, err)
	}
	ubuntu, err := goldenImagePrefix("app", "ubuntu")
	if err != nil || ubuntu != "app-ubuntu-24.04" {
		t.Errorf("goldenImagePrefix(app, ubuntu) = %q %v, want app-ubuntu-24.04 nil", ubuntu, err)
	}
	if _, err := goldenImagePrefix("app", "debian"); err == nil ||
		!strings.Contains(err.Error(), `"debian"`) ||
		!strings.Contains(err.Error(), "alpine") || !strings.Contains(err.Error(), "ubuntu") {
		t.Errorf("unknown os must fail closed naming itself and the supported set: %v", err)
	}
}

func TestOSReleaseCheckPerOS(t *testing.T) {
	cmd, pattern, err := osReleaseCheck("alpine")
	if err != nil || cmd != "cat /etc/alpine-release" {
		t.Errorf("osReleaseCheck(alpine) = %q %v, want cat /etc/alpine-release nil", cmd, err)
	}
	alpineRe := regexp.MustCompile(pattern)
	if !alpineRe.MatchString("3.22.6") || alpineRe.MatchString("24.04") {
		t.Errorf("alpine pattern %q must accept 3.x releases only", pattern)
	}

	cmd, pattern, err = osReleaseCheck("ubuntu")
	if err != nil || cmd != "cat /etc/os-release" {
		t.Errorf("osReleaseCheck(ubuntu) = %q %v, want cat /etc/os-release nil", cmd, err)
	}
	ubuntuRe := regexp.MustCompile(pattern)
	if !ubuntuRe.MatchString("NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=24.04\n") {
		t.Errorf("ubuntu pattern %q must match ID=ubuntu in os-release", pattern)
	}
	if ubuntuRe.MatchString("ID=ubuntu-core\n") || ubuntuRe.MatchString("ID=jammy\n") {
		t.Errorf("ubuntu pattern %q must anchor the exact ID line", pattern)
	}

	if _, _, err := osReleaseCheck("debian"); err == nil ||
		!strings.Contains(err.Error(), "alpine") || !strings.Contains(err.Error(), "ubuntu") {
		t.Errorf("unknown os must fail closed naming the supported set: %v", err)
	}
}

// stageGoldenDisk converts the golden qcow2 into a gzipped raw archive:
// missing input and missing qemu-img fail closed; success yields a
// readable gzip archive of the converted raw disk.
func TestStageGoldenDiskConvertsAndFailsClosed(t *testing.T) {
	newDeps := func() Deps {
		return Deps{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	}
	qcow2 := filepath.Join(t.TempDir(), "golden.qcow2")
	if err := os.WriteFile(qcow2, []byte("fake-disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := stageGoldenDisk(newDeps(), context.Background(), filepath.Join(t.TempDir(), "nope.qcow2")); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("missing qcow2 must fail closed: %v", err)
	}

	// Hide qemu-img: the conversion must refuse instead of half-staging.
	emptyStub := t.TempDir()
	t.Setenv("PATH", emptyStub+":/usr/bin:/bin")
	if _, err := stageGoldenDisk(newDeps(), context.Background(), qcow2); err == nil ||
		!strings.Contains(err.Error(), "qemu-img") {
		t.Errorf("missing qemu-img must fail closed: %v", err)
	}

	// Fake qemu-img: convert -O raw SRC DST -> plain copy.
	qemuShim := "#!/bin/bash\ncp \"$4\" \"$5\"\n"
	if err := os.WriteFile(filepath.Join(emptyStub, "qemu-img"), []byte(qemuShim), 0o755); err != nil {
		t.Fatal(err)
	}
	gzPath, err := stageGoldenDisk(newDeps(), context.Background(), qcow2)
	if err != nil {
		t.Fatalf("stageGoldenDisk err = %v", err)
	}
	defer os.Remove(gzPath)
	if !strings.HasSuffix(gzPath, ".gz") {
		t.Errorf("staged archive must be .gz: %s", gzPath)
	}
	gzFile, err := os.Open(gzPath)
	if err != nil {
		t.Fatal(err)
	}
	defer gzFile.Close()
	gz, err := gzip.NewReader(gzFile)
	if err != nil {
		t.Fatalf("staged file is not gzip: %v", err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil || string(raw) != "fake-disk" {
		t.Errorf("gunzipped disk = %q %v, want the qemu-img copy of the qcow2", raw, err)
	}
}
