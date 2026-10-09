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
		// Retired app-specific names — no legacy: if any of these return,
		// an app contract has leaked back into the tool.
		"kampodine-api",          // RETIRED container/service/image name → Container / ImagePrefix
		"walshipper",             // RETIRED service name → Services
		"/etc/kampodine",         // RETIRED remote state dir → EnvFile / DeployedShaFile
		"/data/tenants",          // RETIRED tenant data dir → DataDir
		"esellar-libsql-backups", // RETIRED object-storage bucket → Bucket
		"/api/auth/ok",           // RETIRED app health endpoint → HealthPath
		// Today's DISTINCTIVE defaults — project.go is their only home
		// (generic values like "app", "/data", "/up" are unguardable by design).
		"/etc/kampodra", // remote state dir → EnvFile / DeployedShaFile
		"app-backups",   // object-storage bucket → Bucket
		"migrate-db.ts", // the configured migrate script → MigrateScript
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
