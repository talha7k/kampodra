package command

import (
	"context"
	"fmt"

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
  kampodra backup list [--prefix <prefix>] [--bucket <name>] [--namespace <ns>]
  kampodra backup download <object> [--out <file>] [--bucket <name>] [--namespace <ns>]
  kampodra backup verify <local.db|.tgz> [--host <user@ip>] [--ssh-key <path>] [--profile <name>]
  kampodra backup restore-plan <object> [--bucket <name>]

Auth is the oci CLI's own — kampodra passes no auth flags unless the
instance profile's "cloud" block overrides it (profile, compartment,
namespace, instancePrincipal); OCI_PROFILE / OCI_COMPARTMENT / OCI_NAMESPACE
env are the escape hatch. kampodra never accepts, stores, or logs credential
material. The tenancy namespace resolves --namespace > cloud block /
OCI_NAMESPACE > oci os ns get and is passed explicitly (the CLI's internal
namespace resolution fails on laptop configs). Bucket: --bucket (default
from the project config; KAMPODRA_BUCKET overrides). LIST-denied by policy
is not fatal for download/verify — GET works with just the object name;
` + "`list`" + ` prints the exact policy shape when denied. verify uploads each .db
member to a VM scratch file and runs /usr/local/bin/restore-verify -db on it
(read-only; scratch only, NEVER writes into the data dir).
--migrated-topology passes restore-verify's drill flag through
(bulk-migration chain shape downgraded to warnings). restore-plan prints
the documented stop/swap/start sequence and NEVER executes anything.

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
	pf.String("namespace", "", "tenancy object-storage namespace (default: resolved via oci os ns get; OCI_NAMESPACE / the profile cloud block \"namespace\" override)")
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
	if auth.Namespace == "" {
		if v, ok := d.Env("OCI_NAMESPACE"); ok {
			auth.Namespace = v
		}
	}
	return auth
}

// resolveOSNamespace resolves the tenancy object-storage namespace for the
// `os object` calls, in preference order: an explicit --namespace flag, the
// cloud block / OCI_NAMESPACE (carried on auth), else `oci os ns get` — the
// same call the oci CLI itself wraps for its internal resolution. The ns get
// leg is load-bearing on the operator laptop: the CLI's internal namespace
// resolution fails under user-principal configs ("Unable to retrieve
// namespace internally", 2026-10-10 live fire) while instance-principal VMs
// resolved fine — `os ns get` works in both worlds. Shared by the backup
// family and image-import.
func resolveOSNamespace(ctx context.Context, auth cloud.CloudAuth, explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if auth.Namespace != "" {
		return auth.Namespace, nil
	}
	nsOut, err := cloud.RunOCI(ctx, cloud.OSNamespaceArgs(auth))
	if err != nil {
		return "", fmt.Errorf("could not resolve the tenancy object-storage namespace (pass it explicitly: --namespace <ns>, the profile cloud block \"namespace\", or OCI_NAMESPACE): %w", err)
	}
	ns := cloud.OSNamespace(nsOut)
	if ns == "" {
		return "", fmt.Errorf("could not resolve the tenancy object-storage namespace — pass it explicitly: --namespace <ns>, the profile cloud block \"namespace\", or OCI_NAMESPACE")
	}
	return ns, nil
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
