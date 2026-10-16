// backup subcommand bodies: list | download | verify | restore-plan.
// Wiring and the cloud-auth/namespace resolvers live in backup.go; the
// restore-verify VM flow lives in backup_verify.go.

package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
)

func runBackupList(d Deps, c *cobra.Command) error {
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	sayProvenance(d, "backup", target)
	bucket := backupBucket(c, target.Project.Bucket)
	auth := cloudAuthFor(d, target)
	if err := cloud.CheckProvider(auth); err != nil {
		return err
	}
	namespace, err := cloud.ResolveBackupNamespace(c.Context(), auth, flagString(c, "namespace"))
	if err != nil {
		return err
	}
	prefix := flagString(c, "prefix")

	suffix := fmt.Sprintf(" (profile: %s", profileLabel(auth))
	if auth.InstancePrincipal {
		suffix += ", instance-principal"
	}
	suffix += "):"
	if prefix != "" {
		fmt.Fprintf(d.Stdout, "[backup] objects in bucket %s (prefix: %s)%s\n", bucket, prefix, suffix)
	} else {
		fmt.Fprintf(d.Stdout, "[backup] objects in bucket %s%s\n", bucket, suffix)
	}

	objs, err := cloud.BackupList(c.Context(), auth, bucket, prefix, namespace)
	if err != nil {
		msg := err.Error()
		if cloud.IsAuthDenied(msg) {
			authDeniedGuidance(d, auth, bucket)
			return &exitError{code: 1}
		}
		return fmt.Errorf("object list failed: %s", msg)
	}
	if len(objs) == 0 {
		if prefix != "" {
			fmt.Fprintf(d.Stdout, "[backup] (no objects with prefix %s)\n", prefix)
		} else {
			fmt.Fprintln(d.Stdout, "[backup] (no objects)")
		}
		return nil
	}
	fmt.Fprintf(d.Stdout, "%-56s %-12s %s\n", "NAME", "SIZE", "UPDATED")
	for _, o := range objs {
		fmt.Fprintf(d.Stdout, "%-56s %-12d %s\n", o.Name, o.Size, o.TimeCreated)
	}
	return nil
}

func runBackupDownload(d Deps, c *cobra.Command, obj string) error {
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	sayProvenance(d, "backup", target)
	bucket := backupBucket(c, target.Project.Bucket)
	auth := cloudAuthFor(d, target)
	if err := cloud.CheckProvider(auth); err != nil {
		return err
	}
	namespace, err := cloud.ResolveBackupNamespace(c.Context(), auth, flagString(c, "namespace"))
	if err != nil {
		return err
	}

	out := flagString(c, "out")
	if out == "" {
		out = filepath.Base(obj)
	}

	head, _ := cloud.BackupHead(c.Context(), auth, bucket, obj, namespace)
	expected := head.Digest

	// 0600 FROM CREATION: pre-create the file 0600, the download truncates
	// into it.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write %s", out)
	}
	f.Close()
	if err := cloud.BackupDownload(c.Context(), auth, bucket, obj, out, namespace); err != nil {
		return fmt.Errorf("object get failed for %s (bucket %s): %w", obj, bucket, err)
	}
	if err := os.Chmod(out, 0o600); err != nil {
		return fmt.Errorf("cannot chmod %s", out)
	}
	fi, err := os.Stat(out)
	if err != nil {
		return fmt.Errorf("cannot stat %s", out)
	}
	fp, _ := cloud.HashFile(out, "sha256")
	fmt.Fprintf(d.Stdout, "[backup] downloaded %s -> %s (0600, %d bytes)\n", obj, out, fi.Size())
	if fp != "" {
		fmt.Fprintf(d.Stdout, "[backup] sha256: %s…\n", fp[:min(len(fp), 16)])
	}
	if expected == "" {
		fmt.Fprintln(d.Stdout, "[backup] integrity: skipped (no digest metadata on the object)")
		return nil
	}
	alg, want := expected, ""
	if i := strings.Index(expected, ":"); i >= 0 {
		alg, want = expected[:i], expected[i+1:]
	}
	got, err := cloud.HashFile(out, alg)
	if err != nil || got != want {
		fmt.Fprintf(d.Stderr, "[backup] FAIL: digest MISMATCH (%s)\n", alg)
		fmt.Fprintf(d.Stderr, "  expected: %s\n", want)
		fmt.Fprintf(d.Stderr, "  got     : %s\n", got)
		return &exitError{code: 1}
	}
	fmt.Fprintf(d.Stdout, "[backup] integrity: verified (%s)\n", alg)
	return nil
}

// verifyMode classifies the local artifact: a db image, or a .tgz archive
// whose .db members are each verified.
func verifyMode(path string) (string, error) {
	switch filepath.Ext(path) {
	case ".db", ".sqlite", ".sqlite3":
		return "db", nil
	case ".tgz", ".gz":
		if strings.HasSuffix(path, ".tar.gz") || strings.HasSuffix(path, ".tgz") {
			return "tgz", nil
		}
	}
	return "", fmt.Errorf("backup verify takes a .db (or .sqlite/.sqlite3) or .tgz file — got: %s", path)
}

func runBackupVerify(d Deps, c *cobra.Command, obj string) error {
	mode, err := verifyMode(obj)
	if err != nil {
		return err
	}
	if _, err := os.Stat(obj); err != nil {
		return fmt.Errorf("no such file: %s", obj)
	}
	// The ssh leg resolves through the standard ladder (--host > profile
	// host > KAMPODRA_HOST; --profile > KAMPODRA_PROFILE > defaultProfile).
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	sayProvenance(d, "backup", target)
	if err := requireHost(target); err != nil {
		return fmt.Errorf("verify needs a VM target: --host root@<ip>, --profile <name>, KAMPODRA_PROFILE, config defaultProfile, or KAMPODRA_HOST")
	}
	ctx := c.Context()
	migrated := flagBool(c, "migrated-topology")
	verifier := &dbVerifier{d: d, ctx: ctx, target: target, migrated: migrated}
	switch mode {
	case "db":
		fmt.Fprintf(d.Stdout, "[backup] uploading %s to VM scratch + restore-verify -db on %s (read-only; scratch only — NEVER writes into %s)\n",
			obj, target.HostSpec.Host, target.Project.DataDir)
		if err := verifier.verifyOneDb(filepath.Base(obj), obj); err != nil {
			return err
		}
		fmt.Fprintln(d.Stdout, "[backup] OK — restore-verify accepted the db image")
	case "tgz":
		scratch, err := os.MkdirTemp("", "kampodra-verify.")
		if err != nil {
			return fmt.Errorf("scratch dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(scratch) }()
		if err := extractTgz(obj, scratch); err != nil {
			return err
		}
		members, err := dbMembers(scratch)
		if err != nil {
			return err
		}
		if len(members) == 0 {
			return fmt.Errorf("no .db/.sqlite members in %s — nothing to verify", obj)
		}
		fmt.Fprintf(d.Stdout, "[backup] verifying every .db member of %s on %s via VM scratch files (read-only; local scratch %s)\n", obj, target.HostSpec.Host, scratch)
		verified := 0
		for _, m := range members {
			rel, _ := filepath.Rel(scratch, m)
			if isRootDbMember(rel) {
				fmt.Fprintf(d.Stdout, "[backup] skipping %s (root db is schema-only — not in restore-verify scope)\n", rel)
				continue
			}
			fmt.Fprintf(d.Stdout, "[backup] verify member: %s\n", rel)
			if err := verifier.verifyOneDb(rel, m); err != nil {
				return err
			}
			verified++
		}
		if verified == 0 {
			return fmt.Errorf("nothing verified — bundle contains only the schema-only root db")
		}
		fmt.Fprintln(d.Stdout, "[backup] OK — every db member passed restore-verify")
	}
	return nil
}

func runBackupRestorePlan(d Deps, c *cobra.Command, obj string) error {
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	sayProvenance(d, "backup", target)
	bucket := backupBucket(c, target.Project.Bucket)
	// PRINT-ONLY by design: no oci, no ssh, no mutation — the plan is
	// documentation bound to this object, never a runner.
	container := target.Project.Container
	dataDir := target.Project.DataDir
	fmt.Fprintf(d.Stdout, `== restore plan (print-only — kampodra NEVER executes these steps) ==
object : %s
bucket : %s

 1. stage the backup locally (download works even when LIST is denied):
      kampodra backup download '%s' --out /tmp/restore.db
 2. copy to the VM scratch dir (never directly into %s):
      scp /tmp/restore.db root@<vm>:/tmp/restore.db
 3. verify BEFORE touching anything (read-only, over ssh stdin):
      kampodra backup verify /tmp/restore.db --host root@<vm>
 4. STOP the api so writes drain:
      ssh root@<vm> 'rc-service %s stop'
 5. snapshot the current state (your rollback point):
      ssh root@<vm> 'cp -a %s/<tenant-dir> %s/<tenant-dir>.pre-restore'
 6. SWAP the restored file in (permissions matter):
      ssh root@<vm> 'install -m 640 -o root -g root /tmp/restore.db %s/<tenant-dir>/db.sqlite'
 7. START and smoke:
      ssh root@<vm> 'rc-service %s start'
      kampodra status

Notes:
  - step 3 is this family's restore-verify contract: the db image travels
    over ssh stdin; /usr/local/bin/restore-verify checks it read-only.
  - kampodra NEVER executes any step above — this plan documents the
    infra runbook sequence for this object; a human runs each step.
`, obj, bucket, obj, dataDir, container, dataDir, dataDir, dataDir, container)
	return nil
}
