package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The genericity guard: the deployed project's naming lives in project.go
// ALONE. Any other non-test Go file in cmd/ or internal/ naming these
// strings is an unreachable hardcode — route it through ProjectConfig.
// Test files are exempt (fixtures may name today's values); project.go is
// the single source; everything else fails here.
func TestNoProjectMagicStringsOutsideProjectDotGo(t *testing.T) {
	magic := []string{
		"kampodine-api",          // container/service/image name → Container / ImagePrefix
		"walshipper",             // service name → Services
		"/etc/kampodine",         // remote env file path → EnvFilePath
		"/data/tenants",          // tenant data dir → DataDir
		"esellar-libsql-backups", // object-storage bucket → Bucket
		"/api/auth/ok",           // app health endpoint → HealthPath
	}

	roots := []string{
		filepath.Join("..", "..", "..", "cmd"),
		filepath.Join("..", "..", "..", "internal"),
	}
	var violations []string
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil // fixtures may name today's values
			}
			if strings.HasSuffix(filepath.ToSlash(path), "internal/adapter/project/project.go") {
				return nil // the ONE source of the naming
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range magic {
				if strings.Contains(string(data), m) {
					violations = append(violations, path+": names "+m+" — route it through ProjectConfig")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(violations) > 0 {
		t.Errorf("project magic strings leaked outside project.go:\n  %s",
			strings.Join(violations, "\n  "))
	}
}
