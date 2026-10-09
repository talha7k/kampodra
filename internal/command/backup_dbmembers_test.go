package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// macOS AppleDouble sidecars (`._*.db`) ride mac bundles into tarballs:
// the member collector must never feed dotfile-prefixed junk to the
// verifier.
func TestBackupAppleDoubleMembersSkipped(t *testing.T) {
	members, err := dbMembers(t.TempDir())
	if err != nil || len(members) != 0 {
		t.Fatalf("empty scratch: %v %v", err, members)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tenant.db"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "._tenant.db"), []byte("appledouble"), 0o600)
	os.MkdirAll(filepath.Join(dir, ".hidden"), 0o755)
	os.WriteFile(filepath.Join(dir, ".hidden", "h.db"), []byte("x"), 0o600)
	members, err = dbMembers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || !strings.HasSuffix(members[0], "tenant.db") {
		t.Errorf("dotfile-prefixed junk must be skipped, got: %v", members)
	}
}
