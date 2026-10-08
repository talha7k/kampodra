package parity

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Command is one golden command surface.
type Command struct {
	Name        string              `json:"name"`
	Script      string              `json:"script"`
	DelegatedTo []string            `json:"delegated_scripts,omitempty"`
	TailExecs   []string            `json:"tail_execs,omitempty"`
	Subcommands []string            `json:"subcommands,omitempty"`
	Flags       []string            `json:"flags,omitempty"`
	Args        map[string][]string `json:"args,omitempty"`
}

// Golden is the committed, FROZEN description of the shell CLI's command
// surface — the spec kampodra ports against. Nothing regenerates it from
// the shell repo anymore: the shell line is retired, and this file is
// final (frozen_note records the provenance).
type Golden struct {
	Generator   string    `json:"generator"`
	SourceShell string    `json:"source_shell_repo"`
	SourceHead  string    `json:"source_shell_head"`
	Frozen      bool      `json:"frozen"`
	FrozenNote  string    `json:"frozen_note"`
	SourceFiles []string  `json:"source_files"`
	Commands    []Command `json:"commands"`
}

// Baseline is the explicit NOT_YET_PORTED list: commands present in the
// shell golden but deliberately not ported to the Go tree yet. A baseline
// entry whose command IS ported is stale and fails the guard.
type Baseline struct {
	NotYetPorted []string `json:"not_yet_ported"`
}

// TreeCommand is the observed surface of one Go cobra command: the union of
// its own flags with every descendant's, its subcommand names, and its
// positional arg specs by subcommand ("" = command-level).
type TreeCommand struct {
	Flags       map[string]bool
	Subcommands map[string]bool
	Args        map[string][]string
}

// TreeSnapshot is the Go CLI surface under test.
type TreeSnapshot struct {
	Commands map[string]TreeCommand
}

// BuildGolden assembles the golden from the parsed cli.js dispatch and the
// committed script sources. sourceFile fetches a committed file by repo path
// ("cli.js", "scripts/deploy.sh", …) — COMMITTED content only. Scripts
// delegated to via `exec bash "$HERE/x.sh" "$@"` merge their surface into the
// delegating command (one level is enough today, but the merge is transitive
// and cycle-safe).
func BuildGolden(generator, shellRepo, head string, refs []CommandRef, sourceFile func(path string) (string, error)) (*Golden, error) {
	g := &Golden{
		Generator:   generator,
		SourceShell: shellRepo,
		SourceHead:  head,
		Commands:    make([]Command, 0, len(refs)),
	}
	surfaces := map[string]*ScriptSurface{}
	var load func(script string) error
	load = func(script string) error {
		if _, seen := surfaces[script]; seen {
			return nil
		}
		src, err := sourceFile("scripts/" + script)
		if err != nil {
			return err
		}
		surface, err := ParseScript(script, src)
		if err != nil {
			return fmt.Errorf("%s: %w", script, err)
		}
		surfaces[script] = &surface
		g.SourceFiles = append(g.SourceFiles, "scripts/"+script)
		for _, dep := range surface.Delegates {
			if err := load(dep); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := sourceFile("cli.js"); err != nil {
		return nil, err
	}
	g.SourceFiles = append(g.SourceFiles, "cli.js")
	for _, ref := range refs {
		if err := load(ref.Script); err != nil {
			return nil, err
		}
		surface := surfaces[ref.Script]
		cmd := Command{
			Name:        ref.Name,
			Script:      ref.Script,
			DelegatedTo: surface.Delegates,
			TailExecs:   surface.TailExecs,
			Subcommands: surface.Subcommands,
			Flags:       surface.Flags,
			Args:        surface.Args,
		}
		// Merge delegated scripts' surfaces (transitive, cycle-safe).
		seen := map[string]bool{ref.Script: true}
		queue := append([]string{}, surface.Delegates...)
		for len(queue) > 0 {
			dep := queue[0]
			queue = queue[1:]
			if seen[dep] {
				continue
			}
			seen[dep] = true
			d := surfaces[dep]
			cmd.Flags = mergeSorted(cmd.Flags, d.Flags)
			cmd.Subcommands = mergeSorted(cmd.Subcommands, d.Subcommands)
			for sub, args := range d.Args {
				if cmd.Args == nil {
					cmd.Args = map[string][]string{}
				}
				cmd.Args[sub] = mergeSorted(cmd.Args[sub], args)
			}
			queue = append(queue, d.Delegates...)
		}
		g.Commands = append(g.Commands, cmd)
	}
	sort.SliceStable(g.SourceFiles, func(i, j int) bool { return g.SourceFiles[i] < g.SourceFiles[j] })
	return g, nil
}

func mergeSorted(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		set[s] = true
	}
	return sortedKeys(set)
}

// LoadGolden reads and validates a committed golden JSON file.
func LoadGolden(path string) (*Golden, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g Golden
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(g.Commands) == 0 {
		return nil, fmt.Errorf("%s: golden has zero commands", path)
	}
	return &g, nil
}

// LoadBaseline reads the NOT_YET_PORTED baseline.
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &b, nil
}
