// The backup verify VM flow: dbVerifier carries one verify run's context
// (the extracted-from-runBackupVerify per-member flow) plus the local .tgz
// inspection helpers. The VM contract is restore-verify -db <scratch>, run
// read-only on generated scratch paths — never the data dir.

package command

import (
	"archive/tar"
	"compress/gzip"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// dbVerifier carries one verify run's context: the Deps, request context,
// target, and the --migrated-topology passthrough. Extracted from
// runBackupVerify so the per-member flow reads linearly instead of
// nesting three closures deep.
type dbVerifier struct {
	d        Deps
	ctx      context.Context
	target   Target
	migrated bool
	seq      int
}

// runCleanup executes the scratch rm against context.Background() —
// cleanup must survive ctrl-C/pipeline cancellation (tenant db bytes must
// never linger on the VM), and its failure must never mask the verify
// verdict.
func (v *dbVerifier) runCleanup(remote string) {
	if _, err := v.d.Runner.Run(context.Background(), v.target.HostSpec, remote); err != nil {
		fmt.Fprintf(v.d.Stderr, "[backup] WARNING: scratch cleanup failed (%s): %v\n", remote, err)
	}
}

// verifyOneDb uploads one local db image to a VM scratch file, runs
// restore-verify -db on it, and always removes the scratch — the data dir
// is never touched.
func (v *dbVerifier) verifyOneDb(label, localPath string) error {
	v.seq++
	scratch := verifyScratchPath(v.seq)
	defer v.runCleanup("rm -f " + scratch) // tolerant: cleanup never masks the verdict
	if _, err := v.d.Runner.RunWithStdin(v.ctx, v.target.HostSpec, "umask 077; cat > "+scratch, mustOpen(localPath)); err != nil {
		return fmt.Errorf("scratch upload for %s: %w", label, err)
	}
	out, err := v.d.Runner.Run(v.ctx, v.target.HostSpec, restoreVerifyCmd(scratch, v.migrated))
	if out != "" {
		fmt.Fprint(v.d.Stdout, out)
	}
	if err != nil {
		return fmt.Errorf("restore-verify FAILED for %s: %w", label, err)
	}
	return nil
}

// verifyScratchPath mints one per-member VM scratch path under /tmp —
// generated names only (never member-supplied), so the remote command needs
// no quoting.
func verifyScratchPath(seq int) string {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return fmt.Sprintf("/tmp/kampodra-verify-%d-%s.db", seq, hex.EncodeToString(b[:]))
}

// isRootDbMember reports whether a tar member is the schema-only ROOT db —
// restore-verify is a TENANT verifier (its gates scan the order/PIH chain
// tables); feeding it root.db fails spuriously (2026-10-09 live fire).
func isRootDbMember(rel string) bool {
	return filepath.Base(rel) == "root.db" && filepath.Base(filepath.Dir(rel)) == "root"
}

// restoreVerifyCmd composes the file-based verify call — the RUNBOOK drill
// shape (restore-verify -db <path>), with the --migrated-topology drill
// flag passed through untouched.
func restoreVerifyCmd(dbPath string, migratedTopology bool) string {
	if migratedTopology {
		return restoreVerifyBin + " -db " + dbPath + " --migrated-topology"
	}
	return restoreVerifyBin + " -db " + dbPath
}

func mustOpen(path string) io.Reader {
	f, err := os.Open(path)
	if err != nil {
		return strings.NewReader("")
	}
	return f
}

// extractTgz unpacks a .tgz into scratch, refusing members that would
// escape the scratch dir (path traversal).
func extractTgz(archivePath, scratch string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("cannot open %s", archivePath)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a gzip archive", archivePath)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	scratchAbs, err := filepath.Abs(scratch)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive read: %w", err)
		}
		dest := filepath.Join(scratch, hdr.Name)
		abs, err := filepath.Abs(dest)
		if err != nil || !strings.HasPrefix(abs, scratchAbs+string(os.PathSeparator)) {
			return fmt.Errorf("refusing unsafe archive member: %s", hdr.Name)
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

// dbMembers lists the extracted .db/.sqlite/.sqlite3 files (sorted — the
// shell's LC_ALL=C find|sort contract).
func dbMembers(scratch string) ([]string, error) {
	var members []string
	err := filepath.WalkDir(scratch, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip dotfile-prefixed junk dirs too (.hidden, .DS_Store
			// siblings) — WalkDir would otherwise descend into them.
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".db", ".sqlite", ".sqlite3":
			// Skip dotfile-prefixed junk (macOS AppleDouble `._*.db` sidecars
			// ride mac bundles into tarballs — never feed them to the verifier).
			if strings.HasPrefix(d.Name(), ".") {
				return nil
			}
			members = append(members, path)
		}
		return nil
	})
	sort.Strings(members)
	return members, err
}
