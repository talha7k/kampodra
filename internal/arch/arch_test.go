package arch

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ports-and-adapters layering rule, enforced statically:
//
//	cmd/kampodra         imports stdlib + internal/command ONLY
//	internal/command     may import internal/adapter/* (and stdlib/libs)
//	internal/adapter/*   may import stdlib + external libs ONLY — never
//	                     internal/command, never the parent internal/adapter,
//	                     never a sibling adapter sideways
//
// Adapter communication goes UP through the command layer (or through
// explicitly injected interfaces), never sideways.
func TestAdapterLayering(t *testing.T) {
	repoRoot := "../.."
	adapterRoot := filepath.Join(repoRoot, "internal", "adapter")

	entries, err := os.ReadDir(adapterRoot)
	if err != nil {
		t.Fatalf("read adapter root: %v", err)
	}
	var violations []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		self := "github.com/talha7k/kampodra/internal/adapter/" + entry.Name()
		dir := filepath.Join(adapterRoot, entry.Name())
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		if len(files) == 0 {
			t.Errorf("adapter package %s has no Go files", entry.Name())
			continue
		}
		for _, file := range files {
			for _, imp := range imports(t, file) {
				switch {
				case strings.HasPrefix(imp, "github.com/talha7k/kampodra/internal/command"):
					violations = append(violations, file+": adapter imports internal/command ("+imp+")")
				case imp == "github.com/talha7k/kampodra/internal/adapter":
					violations = append(violations, file+": adapter imports its own parent package")
				case strings.HasPrefix(imp, "github.com/talha7k/kampodra/internal/adapter/"):
					if imp != self {
						violations = append(violations, file+": sideways adapter import ("+imp+") — go up through the command layer or inject an interface")
					}
				}
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("adapter layering violations:\n  %s", strings.Join(violations, "\n  "))
	}
}

func TestEntrypointIsThin(t *testing.T) {
	file := filepath.Join("../..", "cmd", "kampodra", "main.go")
	for _, imp := range imports(t, file) {
		if strings.HasPrefix(imp, "github.com/talha7k/kampodra/internal/") &&
			imp != "github.com/talha7k/kampodra/internal/command" {
			t.Errorf("%s: cmd/kampodra must import internal/command only, found %s", file, imp)
		}
	}
}

func imports(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		out = append(out, path)
	}
	return out
}
