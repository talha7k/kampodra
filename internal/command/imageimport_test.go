package command_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/talha7k/kampodra/internal/command"
)

type importHarness struct {
	deps   command.Deps
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	ociLog string
}

func (h *importHarness) run(t *testing.T, args ...string) int {
	t.Helper()
	return command.Execute("test", h.deps, append([]string{"image-import"}, args...))
}

func (h *importHarness) ociCalls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.ociLog)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func setupImport(t *testing.T, imageState, firmware string) (*importHarness, string) {
	t.Helper()
	home := t.TempDir()
	stub := t.TempDir()
	ociLog := filepath.Join(stub, "oci-calls.log")

	qcow2 := filepath.Join(t.TempDir(), "app-alpine-3.22.6-aarch64.qcow2")
	os.WriteFile(qcow2, []byte("fake-qcow2-bytes"), 0o600)

	ociShim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$*\" >> '" + ociLog + "'\n" +
		"case \"$1 $2\" in\n" +
		"  \"iam compartment\") printf '{\"data\":[{\"id\":\"ocid1.compartment.test\",\"name\":\"test-compartment\"}]}'; exit 0 ;;\n" +
		"  \"os ns\") printf '{\"data\":\"test-ns\"}'; exit 0 ;;\n" +
		"  \"os object\") exit 0 ;;\n" +
		"  \"compute image\") if [[ \"$3\" == import ]]; then printf '{\"data\":{\"id\":\"ocid1.image.imp\"}}'; " +
		"else printf '{\"data\":{\"lifecycle-state\":\"" + imageState + "\",\"launch-options\":{\"firmware\":\"" + firmware + "\"}}}'; fi; exit 0 ;;\n" +
		"esac\n" +
		"exit 0\n"
	os.WriteFile(filepath.Join(stub, "oci"), []byte(ociShim), 0o755)

	t.Setenv("PATH", stub+":/usr/bin:/bin")
	t.Setenv("OCI_COMPARTMENT", "test-compartment")
	for _, k := range []string{"KAMPODRA_PROFILE", "OCI_PROFILE"} {
		t.Setenv(k, "")
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &importHarness{
		deps: command.Deps{
			Home:   home,
			Env:    systemLookup,
			Stdout: stdout,
			Stderr: stderr,
			Stdin:  strings.NewReader(""),
		},
		stdout: stdout, stderr: stderr, ociLog: ociLog,
	}, qcow2
}

func TestImageImportHappyPath(t *testing.T) {
	h, qcow2 := setupImport(t, "AVAILABLE", "UEFI_64")
	if code := h.run(t, "--image", qcow2); code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	for _, want := range []string{"[import] uploading", "[import] IMPORTED:", "image OCID: ocid1.image.imp", "A1-ready"} {
		if !strings.Contains(out, want) {
			t.Errorf("import output missing %q:\n%s", want, out)
		}
	}
	joined := strings.Join(h.ociCalls(t), "\n")
	for _, want := range []string{"object put", "image import from-object", "--source-image-type QCOW2", "--launch-mode PARAVIRTUALIZED", "object delete"} {
		if !strings.Contains(joined, want) {
			t.Errorf("oci flow missing %q:\n%s", want, joined)
		}
	}
}

func TestImageImportRequiresImage(t *testing.T) {
	h, _ := setupImport(t, "AVAILABLE", "UEFI_64")
	if code := h.run(t); code == 0 {
		t.Error("missing --image must fail")
	}
	if code := h.run(t, "--image", "/nonexistent/disk.qcow2"); code == 0 {
		t.Error("missing file must fail")
	}
}

func TestImageImportBIOSWarnsAndKeepsObject(t *testing.T) {
	h, qcow2 := setupImport(t, "AVAILABLE", "BIOS")
	if code := h.run(t, "--image", qcow2, "--keep-object"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	out := h.stdout.String()
	if !strings.Contains(out, "WILL reject this image at launch") {
		t.Errorf("BIOS firmware must warn loudly:\n%s", out)
	}
	for _, call := range h.ociCalls(t) {
		if strings.Contains(call, "object delete") {
			t.Errorf("--keep-object must skip the delete: %s", call)
		}
	}
}

func TestImageImportTerminalStateFails(t *testing.T) {
	h, qcow2 := setupImport(t, "FAILED", "BIOS")
	if code := h.run(t, "--image", qcow2); code == 0 {
		t.Error("terminal import state must fail")
	}
	if !strings.Contains(h.stderr.String(), "terminal state") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
}

func TestImageImportRequiresCompartment(t *testing.T) {
	h, qcow2 := setupImport(t, "AVAILABLE", "UEFI_64")
	t.Setenv("OCI_COMPARTMENT", "")
	if code := h.run(t, "--image", qcow2); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
}
