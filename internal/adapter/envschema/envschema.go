// Package envschema ports the deploy.sh varlock block: generate the app env
// file from a repo's COMMITTED .env.schema — the schema is extracted from
// `git archive HEAD` into a scratch dir INSIDE the repo (varlock's plugin
// resolution needs node_modules; local .env/.env.local never enter the
// pipeline), the repo's own varlock binary resolves pass() references, and
// the raw output is filtered into a podman --env-file: KEY=VALUE lines
// only, the rc-script-owned clear keys dropped (one source of truth per
// key), surrounding double quotes stripped (podman --env-file does NOT
// unquote). Values NEVER pass through kampodra's output — fingerprints do.
package envschema

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// envLineRe is the `grep -E '^[A-Za-z_][A-Za-z0-9_]*='` filter.
var envLineRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// quotedValueRe is the `sed -E 's/^(KEY)="(.*)"$/\1=\2/'` unquote.
var quotedValueRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)="(.*)"$`)

// BuildEnvFile filters varlock's env-format output into the env file body:
// KEY=VALUE lines only, clear keys dropped, double quotes stripped. Order
// is preserved. Empty output (no usable lines) means the caller reports the
// varlock failure.
func BuildEnvFile(varlockOutput string, clearKeys []string) string {
	clear := make(map[string]bool, len(clearKeys))
	for _, k := range clearKeys {
		clear[k] = true
	}
	var out []string
	for _, line := range strings.Split(varlockOutput, "\n") {
		m := envLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, value := m[1], m[2]
		if clear[key] {
			continue
		}
		if q := quotedValueRe.FindStringSubmatch(line); q != nil {
			value = q[2]
		}
		out = append(out, key+"="+value)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// ExtractTar unpacks `git archive` output into scratch, refusing members
// that would escape the scratch dir.
func ExtractTar(data []byte, dir string) error {
	tr := tar.NewReader(bytes.NewReader(data))
	scratchAbs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("schema archive read: %w", err)
		}
		dest := filepath.Join(dir, hdr.Name)
		abs, err := filepath.Abs(dest)
		if err != nil || abs != scratchAbs && !strings.HasPrefix(abs, scratchAbs+string(os.PathSeparator)) {
			return fmt.Errorf("refusing unsafe schema archive member: %s", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		outFile, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(outFile, tr); err != nil {
			outFile.Close()
			return err
		}
		outFile.Close()
	}
}

// GitArchive returns `git -C <repo> archive HEAD <schemaPath>` output (the
// COMMITTED schema — dirty working trees never leak into deploys).
func GitArchive(ctx context.Context, repo, schemaPath string) ([]byte, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git not found on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "-C", repo, "archive", "HEAD", schemaPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("git archive %s: %s", schemaPath, msg)
		}
		return nil, fmt.Errorf("git archive %s: %w (is %s committed?)", schemaPath, err, schemaPath)
	}
	return stdout.Bytes(), nil
}

// RunVarlock execs the REPO's varlock binary (<repo>/node_modules/.bin/
// varlock) with cwd=scratch — plugin resolution and the pass store belong
// to the repo toolchain, never to kampodra.
func RunVarlock(repo, scratch, projectDir string) (string, error) {
	bin := filepath.Join(repo, "node_modules", ".bin", "varlock")
	if _, err := os.Stat(bin); err != nil {
		return "", fmt.Errorf("%s not found — install the repo toolchain (npm install) first", bin)
	}
	cmd := exec.Command(bin, "load", "--format", "env", "--compact", "-p", projectDir)
	cmd.Dir = scratch
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("varlock: %s", msg)
		}
		return "", fmt.Errorf("varlock env generation failed: %w (pass store unlocked? schema committed?)", err)
	}
	return stdout.String(), nil
}

// Generate runs the whole flow for one repo+schema and returns the filtered
// env file body. The scratch dir lives INSIDE the repo and is removed on
// the way out.
func Generate(ctx context.Context, repoRoot, schemaPath string, clearKeys []string) (string, error) {
	scratch := filepath.Join(repoRoot, ".tmp", "kampodra-env-schema")
	if err := os.RemoveAll(scratch); err != nil {
		return "", fmt.Errorf("scratch reset: %w", err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return "", fmt.Errorf("scratch create: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	tarData, err := GitArchive(ctx, repoRoot, schemaPath)
	if err != nil {
		return "", err
	}
	if err := ExtractTar(tarData, scratch); err != nil {
		return "", err
	}
	projectDir := filepath.ToSlash(filepath.Dir(schemaPath))
	out, err := RunVarlock(repoRoot, scratch, projectDir)
	if err != nil {
		return "", err
	}
	body := BuildEnvFile(out, clearKeys)
	if body == "" {
		return "", fmt.Errorf("varlock env generation failed — no KEY=VALUE lines (pass store unlocked? schema committed?)")
	}
	return body, nil
}
