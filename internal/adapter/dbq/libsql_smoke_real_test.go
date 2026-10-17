package dbq

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runCmd(cmd string, stdin *strings.Reader) string {
	sh := exec.Command("bash", "-c", cmd)
	sh.Stdin = stdin
	out, _ := sh.Output()
	return string(out)
}

// REAL-SMOKE (throwaway): runs the actual Hrana flow against a live sqld
// container on 127.0.0.1:18080. Skipped unless KAMPODRA_SMOKE_SQLD is set.
func TestSmokeLibsqlReal(t *testing.T) {
	url := os.Getenv("KAMPODRA_SMOKE_SQLD")
	if url == "" {
		t.Skip("KAMPODRA_SMOKE_SQLD not set")
	}
	stmt := "SELECT id, name FROM t ORDER BY id"
	body := HranaPipelineBody(stmt)
	cmd := "curl -sS -H 'Content-Type: application/json' --data-binary @- " + url + LibsqlHranaPath
	out := runCmd(cmd, strings.NewReader(body))
	flat, err := NormalizeHranaResponse(out)
	if err != nil {
		t.Fatalf("normalize on real sqld output: %v\nraw: %s", err, out)
	}
	t.Logf("flattened: %s", flat)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(flat), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, flat)
	}
	if len(rows) != 1 || rows[0]["id"] != float64(7) || rows[0]["name"] != "seven" {
		t.Fatalf("rows: %s", flat)
	}
}
