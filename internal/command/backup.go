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

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// restoreVerifyBin is the VM's restore-verify entrypoint (infra contract
// of the VM image, not project naming): FILE-based — `restore-verify -db
// <path>` (the RUNBOOK drill shape). Members are uploaded to VM scratch
// files, verified there, and removed — it NEVER touches the data dir.
const restoreVerifyBin = "/usr/local/bin/restore-verify"

// backupHelp is the shell backup.sh usage heredoc, kampodra-fied.
const backupHelp = `Usage:
  kampodra backup list [--prefix <prefix>] [--bucket <name>]
  kampodra backup download <object> [--out <file>] [--bucket <name>]
  kampodra backup verify <local.db|.tgz> [--host <user@ip>] [--ssh-key <path>] [--profile <name>]
  kampodra backup restore-plan <object> [--bucket <name>]

Auth is the oci CLI's own — kampodra passes no auth flags unless the
instance profile's "cloud" block overrides it (profile, compartment,
instancePrincipal); OCI_PROFILE / OCI_COMPARTMENT env are the escape
hatch. kampodra never accepts, stores, or logs credential material.
Bucket: --bucket (default from the project config; KAMPODRA_BUCKET
overrides). LIST-denied by policy is not fatal for download/verify — GET
works with just the object name; ` + "`list`" + ` prints the exact policy shape
when denied. verify uploads each .db member to a VM scratch file and runs
/usr/local/bin/restore-verify -db on it (read-only; scratch only, NEVER
writes into the data dir). --migrated-topology passes restore-verify's
drill flag through (bulk-migration chain shape downgraded to warnings).
restore-plan prints the documented stop/swap/start sequence and NEVER
executes anything.

Examples:
  kampodra backup list --prefix tenants/
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
	pf.String("bucket", "", "object-storage bucket (default: project config bucket; KAMPODRA_BUCKET overrides)")
	pf.String("prefix", "", "object name prefix for list")
	pf.String("out", "", "download destination (default: the object's basename)")
	pf.String("profile", "", "kampodra INSTANCE profile (~/.kampodra/config.json) — beats KAMPODRA_PROFILE / defaultProfile")
	pf.String("host", "", "VM target for verify (user@ip or ssh-config alias)")
	pf.String("ssh-key", "", "identity file for verify — beats KAMPODRA_SSH_KEY")
	pf.Bool("migrated-topology", false, "verify: pass restore-verify's --migrated-topology drill flag (migration-fallout chain shape → warnings)")

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

// backupBucket resolves the bucket: --bucket flag > the project config's
// bucket — the ladder-resolved value (KAMPODRA_BUCKET > profile project
// block > manifest > default already applied inside it; the ONE place the
// name lives).
func backupBucket(c *cobra.Command, projectBucket string) string {
	if v := flagString(c, "bucket"); v != "" {
		return v
	}
	return projectBucket
}

// cloudAuthFor resolves the provider-CLI auth overrides for one run:
// the instance profile's `cloud` block, with the provider's own env vars
// as the escape hatch. Everything empty = the oci CLI resolves natively
// (its config's default/DEFAULT/first-profile precedence) — kampodra
// passes no auth flags at all. Credentials NEVER appear here: the block
// names config entries, and the provider CLI reads them itself.
func cloudAuthFor(d Deps, target Target) cloud.CloudAuth {
	auth := cloud.ParseCloudAuth(target.Cloud)
	if auth.Profile == "" {
		if v, ok := d.Env("OCI_PROFILE"); ok {
			auth.Profile = v
		}
	}
	if auth.Compartment == "" {
		if v, ok := d.Env("OCI_COMPARTMENT"); ok {
			auth.Compartment = v
		}
	}
	return auth
}

// profileLabel renders the auth identity for human output: the configured
// profile name, or a marker for the empty (oci-CLI-native) case.
func profileLabel(auth cloud.CloudAuth) string {
	if auth.Profile != "" {
		return auth.Profile
	}
	if auth.InstancePrincipal {
		return "instance-principal"
	}
	return "oci-cli default"
}

func authDeniedGuidance(d Deps, auth cloud.CloudAuth, bucket string) {
	identity := profileLabel(auth)
	fmt.Fprintf(d.Stderr, "[backup] FAIL: LIST denied — this identity (%s) lacks INSPECT+READ on bucket %s.\n", identity, bucket)
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
	bucket := backupBucket(c, target.Project.Bucket)
	auth := cloudAuthFor(d, target)
	if err := cloud.CheckProvider(auth); err != nil {
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

	out, err := cloud.RunOCI(c.Context(), cloud.ObjectListArgs(bucket, prefix, auth.Profile, auth.InstancePrincipal))
	if err != nil {
		msg := err.Error()
		if cloud.IsAuthDenied(msg) {
			authDeniedGuidance(d, auth, bucket)
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
	bucket := backupBucket(c, target.Project.Bucket)
	auth := cloudAuthFor(d, target)
	if err := cloud.CheckProvider(auth); err != nil {
		return err
	}

	out := flagString(c, "out")
	if out == "" {
		out = filepath.Base(obj)
	}

	headJSON, _ := cloud.RunOCI(c.Context(), cloud.ObjectHeadArgs(bucket, obj, auth.Profile, auth.InstancePrincipal))
	expected := cloud.ExpectedDigest(headJSON)

	// 0600 FROM CREATION: pre-create the file 0600, oci get truncates into it.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("cannot write %s", out)
	}
	f.Close()
	if _, err := cloud.RunOCI(c.Context(), cloud.ObjectGetArgs(bucket, obj, out, auth.Profile, auth.InstancePrincipal)); err != nil {
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

func runBackupRestorePlan(d Deps, c *cobra.Command, obj string) error {
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
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

// resolveAnyTarget resolves the target through the standard ladder:
// --profile selects the INSTANCE profile (CLI-wide meaning; KAMPODRA_PROFILE
// and config defaultProfile fill in below it), --host/--ssh-key ride the
// usual host/key ladder; a missing profile/host is NOT an error — callers
// decide what a target is required for.
func resolveAnyTarget(d Deps, c *cobra.Command) (Target, error) {
	host := flagString(c, "host")
	key := flagString(c, "ssh-key")
	profile := flagString(c, "profile")
	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return Target{}, err
	}
	mf, err := d.manifestFor()
	if err != nil {
		return Target{}, err
	}
	return ResolveTarget(cfg, host, key, profile, mf, d.Env)
}
