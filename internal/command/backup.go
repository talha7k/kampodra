package command

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// restoreVerifyRemote is the VM's restore-verify entrypoint (infra contract
// of the VM image, not project naming): backups are piped over ssh stdin,
// scratch only — it NEVER writes into the data dir.
const restoreVerifyRemote = "/usr/local/bin/restore-verify /dev/stdin"

// backupHelp is the shell backup.sh usage heredoc, kampodra-fied.
const backupHelp = `Usage:
  kampodra backup list [--prefix <prefix>] [--bucket <name>] [--profile <oci>] [--instance-principal]
  kampodra backup download <object> [--out <file>] [--bucket <name>] [--profile <oci>] [--instance-principal]
  kampodra backup verify <local.db|.tgz> [--host <user@ip>] [--ssh-key <path>] [--kampodine-profile <name>]
  kampodra backup restore-plan <object> [--bucket <name>]

Auth is the oci CLI's own — its config file (--profile) or instance
principal. kampodra never accepts, stores, or logs credential material.
Bucket: --bucket (default from the project config, KAMPODRA_BACKUP_BUCKET
overrides). LIST-denied by policy is not fatal for download/verify — GET
works with just the object name; ` + "`list`" + ` prints the exact policy shape
when denied. verify is read-only over ssh stdin into /usr/local/bin/
restore-verify; scratch dirs only, NEVER writes into the data dir.
restore-plan prints the documented stop/swap/start sequence and NEVER
executes anything.

Examples:
  kampodra backup list --prefix tenants/
  kampodra backup list --bucket my-backups --profile my-oci-profile
  kampodra backup download tenants/acme/20261008T050000Z.db --out /tmp/restore.db
  kampodra backup verify /tmp/restore.db --host root@203.0.113.10
  kampodra backup verify ./bundle.tgz          # every .db member, read-only
  kampodra backup restore-plan tenants/acme/20261008T050000Z.db
`

func newBackupCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "OCI Object Storage backups: list | download | verify | restore-plan (restore-plan never executes)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), backupHelp)
			return nil
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), backupHelp)
	})
	pf := cmd.PersistentFlags()
	pf.String("bucket", "", "object-storage bucket (default: project config bucket; KAMPODRA_BACKUP_BUCKET overrides)")
	pf.String("prefix", "", "object name prefix for list")
	pf.String("out", "", "download destination (default: the object's basename)")
	pf.String("profile", "", "OCI CONFIG profile (dns convention; default: OCI_PROFILE env > default)")
	pf.Bool("instance-principal", false, "authenticate as the instance principal (when run ON a VM)")
	pf.String("host", "", "VM target for verify (user@ip or ssh-config alias)")
	pf.String("ssh-key", "", "identity file for verify — beats KAMPODRA_SSH_KEY")
	pf.String("kampodine-profile", "", "kampodra INSTANCE profile for verify's ssh leg (frozen-spec flag name)")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "objects in the bucket (policy guidance when LIST is denied)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runBackupList(d, c)
			},
		},
		&cobra.Command{
			Use:   "download <object>",
			Short: "fetch one object (0600, sha256/md5-verified against the object's digest metadata)",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				obj, err := requireBackupObject(args, "download <object> [--out <file>] (--help)")
				if err != nil {
					return err
				}
				return runBackupDownload(d, c, obj)
			},
		},
		&cobra.Command{
			Use:   "verify <local.db|.tgz>",
			Short: "pipe a local db image (or every .db member of a .tgz) through the VM's restore-verify, read-only",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				obj, err := requireBackupObject(args, "verify <local.db|.tgz> (--help)")
				if err != nil {
					return err
				}
				return runBackupVerify(d, c, obj)
			},
		},
		&cobra.Command{
			Use:   "restore-plan <object>",
			Short: "PRINT the recovery sequence for one object — kampodra never executes it",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				obj, err := requireBackupObject(args, "restore-plan <object> [--bucket <name>]")
				if err != nil {
					return err
				}
				return runBackupRestorePlan(d, c, obj)
			},
		},
	)
	return cmd
}

func requireBackupObject(args []string, usage string) (string, error) {
	if len(args) == 0 || args[0] == "" {
		return "", fmt.Errorf("usage: kampodra backup %s", usage)
	}
	return args[0], nil
}

// backupBucket resolves the bucket: --bucket flag > KAMPODRA_BACKUP_BUCKET >
// the project config's bucket (the ONE place the name lives).
func backupBucket(d Deps, c *cobra.Command, projectBucket string) string {
	if v := flagString(c, "bucket"); v != "" {
		return v
	}
	if v, ok := d.Env("KAMPODRA_BACKUP_BUCKET"); ok && v != "" {
		return v
	}
	return projectBucket
}

// ociProfile resolves the OCI CONFIG profile: --profile > OCI_PROFILE >
// "default" (the dns convention).
func ociProfile(d Deps, c *cobra.Command) string {
	if v := flagString(c, "profile"); v != "" {
		return v
	}
	if v, ok := d.Env("OCI_PROFILE"); ok && v != "" {
		return v
	}
	return "default"
}

func authDeniedGuidance(d Deps, profile, bucket string) {
	fmt.Fprintf(d.Stderr, "[backup] FAIL: LIST denied — this identity (profile %s / instance principal) lacks INSPECT+READ on bucket %s.\n", profile, bucket)
	fmt.Fprintln(d.Stderr, "The bucket compartment needs a policy like:")
	fmt.Fprintln(d.Stderr, "  Allow dynamic-group <your-dg> to read buckets in compartment <compartment>")
	fmt.Fprintln(d.Stderr, "  Allow dynamic-group <your-dg> to read objects in compartment <compartment>")
	fmt.Fprintln(d.Stderr, "GET works without LIST once the object name is known:")
	fmt.Fprintf(d.Stderr, "  kampodra backup download <object> --bucket %s\n", bucket)
}

func runBackupList(d Deps, c *cobra.Command) error {
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	bucket := backupBucket(d, c, target.Project.Bucket)
	profile := ociProfile(d, c)
	instancePrincipal := flagBool(c, "instance-principal")
	prefix := flagString(c, "prefix")

	suffix := fmt.Sprintf(" (profile: %s", profile)
	if instancePrincipal {
		suffix += ", instance-principal"
	}
	suffix += "):"
	if prefix != "" {
		fmt.Fprintf(d.Stdout, "[backup] objects in bucket %s (prefix: %s)%s\n", bucket, prefix, suffix)
	} else {
		fmt.Fprintf(d.Stdout, "[backup] objects in bucket %s%s\n", bucket, suffix)
	}

	out, err := cloud.RunOCI(c.Context(), cloud.ObjectListArgs(bucket, prefix, profile, instancePrincipal))
	if err != nil {
		msg := err.Error()
		if cloud.IsAuthDenied(msg) {
			authDeniedGuidance(d, profile, bucket)
			return &exitError{code: 1}
		}
		return fmt.Errorf("object list failed: %s", msg)
	}
	objs, err := cloud.ParseObjectList(out)
	if err != nil {
		return err
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
	bucket := backupBucket(d, c, target.Project.Bucket)
	profile := ociProfile(d, c)
	instancePrincipal := flagBool(c, "instance-principal")

	out := flagString(c, "out")
	if out == "" {
		out = filepath.Base(obj)
	}

	headJSON, _ := cloud.RunOCI(c.Context(), cloud.ObjectHeadArgs(bucket, obj, profile, instancePrincipal))
	expected := cloud.ExpectedDigest(headJSON)

	// 0600 FROM CREATION: pre-create the file 0600, oci get truncates into it.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write %s", out)
	}
	f.Close()
	if _, err := cloud.RunOCI(c.Context(), cloud.ObjectGetArgs(bucket, obj, out, profile, instancePrincipal)); err != nil {
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
	// The ssh leg resolves through the standard ladder; --kampodine-profile
	// is the frozen-spec flag name for the instance profile here.
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	if err := requireHost(target); err != nil {
		return fmt.Errorf("verify needs a VM target: --host root@<ip>, --kampodine-profile <name>, KAMPODRA_PROFILE, config defaultProfile, or KAMPODINE_HOST")
	}
	ctx := c.Context()
	switch mode {
	case "db":
		fmt.Fprintf(d.Stdout, "[backup] piping %s over ssh stdin to /usr/local/bin/restore-verify on %s (read-only; scratch only — NEVER writes into %s)\n",
			obj, target.HostSpec.Host, target.Project.DataDir)
		if _, err := d.Runner.RunWithStdin(ctx, target.HostSpec, restoreVerifyRemote, mustOpen(obj)); err != nil {
			return fmt.Errorf("restore-verify FAILED for %s: %w", filepath.Base(obj), err)
		}
		fmt.Fprintln(d.Stdout, "[backup] OK — restore-verify accepted the db image")
	case "tgz":
		scratch, err := os.MkdirTemp("", "kampodra-verify.")
		if err != nil {
			return fmt.Errorf("scratch dir: %w", err)
		}
		defer os.RemoveAll(scratch)
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
		fmt.Fprintf(d.Stdout, "[backup] piping every .db member of %s over ssh stdin (read-only; scratch dir %s)\n", obj, scratch)
		for _, m := range members {
			rel, _ := filepath.Rel(scratch, m)
			fmt.Fprintf(d.Stdout, "[backup] verify member: %s\n", rel)
			if _, err := d.Runner.RunWithStdin(ctx, target.HostSpec, restoreVerifyRemote, mustOpen(m)); err != nil {
				return fmt.Errorf("restore-verify FAILED for %s: %w", rel, err)
			}
		}
		fmt.Fprintln(d.Stdout, "[backup] OK — every db member passed restore-verify")
	}
	return nil
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
	defer gz.Close()
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
			return nil
		}
		switch filepath.Ext(path) {
		case ".db", ".sqlite", ".sqlite3":
			members = append(members, path)
		}
		return nil
	})
	sort.Strings(members)
	return members, err
}

func runBackupRestorePlan(d Deps, c *cobra.Command, obj string) error {
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	bucket := backupBucket(d, c, target.Project.Bucket)
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

// resolveAnyTarget resolves the verify leg's target. ONLY
// --kampodine-profile selects the INSTANCE profile here (--profile is the
// OCI CONFIG profile, the dns convention — the frozen spec keeps the two
// namespaces apart); a missing profile/host is NOT an error — callers
// decide what a target is required for.
func resolveAnyTarget(d Deps, c *cobra.Command) (Target, error) {
	host := flagString(c, "host")
	key := flagString(c, "ssh-key")
	profile := flagString(c, "kampodine-profile")
	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return Target{}, err
	}
	return ResolveTarget(cfg, host, key, profile, d.Env)
}
