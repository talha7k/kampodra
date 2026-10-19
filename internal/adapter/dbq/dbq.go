// Package dbq is the kampodra db surface's adapter: the read-only
// statement gate, the sqlq helper's contract (paths, commands, DSN), and
// the on-demand helper build. kampodra `db query` streams the helper to
// the VM's /tmp so agents never hand-roll read-only sqlite binaries over
// ssh again.
//
// The gate itself lives in gate.go — stdlib-only, because the materialized
// helper module compiles that file verbatim (one source of truth on both
// sides of the wire).
package dbq

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Version is the sqlq helper's content stamp. It rides BOTH cache paths
// (the VM's /tmp artifact and the local build cache), so any helper change
// invalidates every cached copy by name — bump it together with the
// sqlqVersion stamp in sqlq/main.go (pinned by a test).
// v2: the write mode (-write, db exec).
const Version = "2"

// sqlqTarget is the helper's cross-build triple: linux/arm64, CGO off —
// the same estate shape deploy --binary builds for (the OCI VM is aarch64).
// SQLQGOARCH is exported because the command layer composes the on-VM
// cache path with it — the name must agree with what was built.
const (
	sqlqGOOS   = "linux"
	SQLQGOARCH = "arm64"
)

// helperModulePath is the materialized helper module's own path — the
// rewrite target for sqlq/main.go's dbq import (the live source imports
// this module's internal package; the standalone module can't).
const helperModulePath = "kampodrasqlq"

// liveDBQImport is the dbq import path as written in sqlq/main.go.
const liveDBQImport = "github.com/talha7k/kampodra/internal/adapter/dbq"

// HelperRemotePath is the on-VM artifact path: /tmp, the kampodra scratch
// naming scheme, version- and arch-stamped. Generated name only (never
// operator-supplied), so the remote command needs no quoting.
func HelperRemotePath(goarch string) string {
	return fmt.Sprintf("/tmp/kampodra-sqlq-v%s-%s", Version, goarch)
}

// HelperLocalPath is the local build cache path under the caller's cache
// root (injectable — tests never touch the real user cache).
func HelperLocalPath(cacheRoot, goos, goarch string) string {
	return filepath.Join(cacheRoot, "dbq", fmt.Sprintf("kampodra-sqlq-%s-%s-v%s", goos, goarch, Version))
}

// SnapshotScratchPath mints one VM scratch path for the snapshot copy —
// the verify-scratch discipline: fixed /tmp prefix, crypto/rand suffix,
// never reused.
func SnapshotScratchPath() string {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return fmt.Sprintf("/tmp/kampodra-snapshot-%s.db", hex.EncodeToString(b[:]))
}

// --- remote command composition (all shapes the db flow runs) ------------

func HelperCheckCommand(remote string) string {
	return "test -x " + remote
}

func HelperInstallCommand(remote string) string {
	return "umask 077; cat > " + remote + " && chmod 755 " + remote
}

// QueryCommand runs the helper with the statement arriving on stdin — the
// SQL never rides the argv (no quoting hazards).
func QueryCommand(remoteHelper, dbPath string) string {
	return remoteHelper + " -db " + dbPath
}

// ExecCommand runs the helper in write mode (-write): ONE gated
// UPDATE/INSERT/DELETE/REPLACE from stdin, {"changes":N} JSON on stdout.
// Same argv discipline as the query leg — the statement never rides argv.
func ExecCommand(remoteHelper, dbPath string) string {
	return remoteHelper + " -db " + dbPath + " -write"
}

// Sqlite3ProbeCommand answers yes/no in ONE round-trip: sqlite3 present?
// (A bare `command -v` would fail the whole ssh run when absent.)
const Sqlite3ProbeCommand = `if command -v sqlite3 >/dev/null 2>&1; then echo yes; else echo no; fi`

func SnapshotBackupCommand(dbPath, scratch string) string {
	return fmt.Sprintf(`sqlite3 %s ".backup %s"`, dbPath, scratch)
}

func SnapshotCpCommand(dbPath, scratch string) string {
	return "cp " + dbPath + " " + scratch
}

func SnapshotFetchCommand(scratch string) string {
	return "cat " + scratch
}

func SnapshotCleanupCommand(scratch string) string {
	return "rm -f " + scratch
}

// --- the helper's local build --------------------------------------------

// MaterializeHelperModule writes the helper's standalone build unit into
// dir: the rewritten main.go, the verbatim stdlib-only gate (dbq/gate.go),
// and the module manifests. go.mod/go.sum are embedded verbatim (as .tmpl
// — a real go.mod in the tree would split the directory into a separate
// module); a pin test keeps their modernc.org/sqlite requirement aligned
// with the repo's.
func MaterializeHelperModule(dir string) error {
	mainSrc := strings.ReplaceAll(SQLQSource, liveDBQImport, helperModulePath+"/dbq")
	files := map[string][]byte{
		"main.go":     []byte(mainSrc),
		"dbq/gate.go": gateSource,
		"go.mod":      helperGoMod,
		"go.sum":      helperGoSum,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("cannot materialize %s: %w", name, err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return fmt.Errorf("cannot materialize %s: %w", name, err)
		}
	}
	return nil
}

// BuildLinuxHelper returns the local linux/arm64 helper binary, building
// it on demand: cache hit → return; else materialize the module into a
// temp dir and cross-build with the go toolchain from PATH (CGO off).
// Build progress and compiler errors stream to stderr — stdout is data.
func BuildLinuxHelper(ctx context.Context, cacheRoot string, stderr io.Writer) (string, error) {
	out := HelperLocalPath(cacheRoot, sqlqGOOS, SQLQGOARCH)
	if fi, err := os.Stat(out); err == nil && fi.Mode().IsRegular() {
		return out, nil
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("the sqlq helper needs the go toolchain on PATH (one-time local build): %w", err)
	}
	dir, err := os.MkdirTemp("", "kampodra-sqlq-")
	if err != nil {
		return "", fmt.Errorf("cannot create the helper build dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := MaterializeHelperModule(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", fmt.Errorf("cannot create the helper cache dir: %w", err)
	}
	fmt.Fprintf(stderr, "[db] building the sqlq helper (%s/%s, CGO off) — one-time; the first build may fetch the sqlite driver\n", sqlqGOOS, SQLQGOARCH)
	cmd := exec.CommandContext(ctx, goBin, "build", "-trimpath", "-ldflags", "-s -w", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(scrubGoEnv(os.Environ()),
		"CGO_ENABLED=0", "GOOS="+sqlqGOOS, "GOARCH="+SQLQGOARCH, "GOWORK=off")
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("sqlq helper build failed: %w", err)
	}
	return out, nil
}

// scrubGoEnv drops GOFLAGS/GOWORK from the inherited environment — an
// operator's workspace or vendoring flag must never leak into the helper's
// standalone module build.
func scrubGoEnv(environ []string) []string {
	env := environ[:0:0]
	for _, e := range environ {
		if strings.HasPrefix(e, "GOFLAGS=") || strings.HasPrefix(e, "GOWORK=") {
			continue
		}
		env = append(env, e)
	}
	return env
}
