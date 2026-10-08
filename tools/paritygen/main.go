// Command paritygen derives kampodine's command-surface golden JSON from the
// shell repo's COMMITTED files (github.com/talha7k/kampodine, HEAD only —
// never the working tree) and writes it to internal/parity/golden.json.
//
// Usage:
//
//	go run ./tools/paritygen [-shell <path>] [-out <file>] [-check]
//
// -shell defaults to $KAMPODINE_SHELL_REPO, else the sibling directory
// `kampodine` next to this repo. -check regenerates and exits 1 when the
// committed golden differs (the CI ratchet: a new shell command without Go
// coverage must fail CI until the golden is consciously regenerated and the
// baseline updated).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/talha7k/kampodra/internal/parity"
)

const (
	generatorVersion = "paritygen/1"
	// The shell behavioral spec's canonical identity — recorded in the
	// golden instead of a machine-local path so CI comparisons are stable.
	shellRepoName = "github.com/talha7k/kampodine"
)

func main() {
	defaultShell := os.Getenv("KAMPODINE_SHELL_REPO")
	if defaultShell == "" {
		self, err := os.Executable()
		if err == nil {
			// go run puts the binary in a temp dir; resolve via workdir instead
			_ = self
		}
		if wd, err := os.Getwd(); err == nil {
			defaultShell = filepath.Join(filepath.Dir(wd), "kampodine")
		}
	}
	shell := flag.String("shell", defaultShell, "path to the shell kampodine repo (read-only; COMMITTED files only)")
	out := flag.String("out", "internal/parity/golden.json", "golden output path")
	check := flag.Bool("check", false, "regenerate and fail when the committed golden differs")
	flag.Parse()

	if err := run(*shell, *out, *check); err != nil {
		fmt.Fprintf(os.Stderr, "paritygen: %v\n", err)
		os.Exit(1)
	}
}

func run(shellRepo, outPath string, checkOnly bool) error {
	absShell, err := filepath.Abs(shellRepo)
	if err != nil {
		return err
	}
	head, err := git(absShell, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("resolving %s HEAD (is this the shell repo?): %w", absShell, err)
	}
	head = strings.TrimSpace(head)

	refs, err := loadRefs(absShell)
	if err != nil {
		return err
	}

	golden, err := parity.BuildGolden(generatorVersion, shellRepoName, head, refs, func(path string) (string, error) {
		data, err := gitShow(absShell, path)
		if err != nil {
			return "", fmt.Errorf("reading committed %s: %w", path, err)
		}
		return data, nil
	})
	if err != nil {
		return err
	}

	buf, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')

	if checkOnly {
		committed, err := os.ReadFile(outPath)
		if err != nil {
			return fmt.Errorf("-check: %w", err)
		}
		if !bytes.Equal(committed, buf) {
			return fmt.Errorf("-check: %s is stale — regenerate with `go run ./tools/paritygen -shell %s` and commit", outPath, absShell)
		}
		fmt.Printf("paritygen: %s matches kampodine HEAD %s (%d commands)\n", outPath, head[:10], len(golden.Commands))
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outPath, buf, 0o644); err != nil {
		return err
	}
	fmt.Printf("paritygen: wrote %s from kampodine HEAD %s (%d commands)\n", outPath, head[:10], len(golden.Commands))
	return nil
}

func loadRefs(shellRepo string) ([]parity.CommandRef, error) {
	cli, err := gitShow(shellRepo, "cli.js")
	if err != nil {
		return nil, fmt.Errorf("reading committed cli.js: %w", err)
	}
	return parity.ParseCommandsJS(cli)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s: %w", args[0], errOut.String(), err)
	}
	return out.String(), nil
}

func gitShow(dir, path string) (string, error) {
	return git(dir, "show", "HEAD:"+path)
}
