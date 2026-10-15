// OCI legs of the neutral backup ops: TODAY's exported builders + parsers,
// composed verbatim — the oci argv is byte-identical to the pre-gcp flow
// (the PATH-shim argv pins in command/backup_test.go prove it). The tenancy
// namespace ladder lives here too, so the `os ns get` laptop fix is
// OCI-internal: gcp has no namespace concept and never runs it.
package cloud

import (
	"context"
	"fmt"
)

func backupListOCI(ctx context.Context, auth CloudAuth, bucket, prefix, namespace string) ([]BackupObject, error) {
	out, err := RunOCI(ctx, ObjectListArgs(bucket, prefix, namespace, auth.Profile, auth.InstancePrincipal))
	if err != nil {
		return nil, err
	}
	return ParseObjectList(out)
}

func backupHeadOCI(ctx context.Context, auth CloudAuth, bucket, obj, namespace string) BackupObject {
	// The swallowed error is today's semantics: a failed HEAD digest-skips,
	// it must not break the download.
	out, _ := RunOCI(ctx, ObjectHeadArgs(bucket, obj, namespace, auth.Profile, auth.InstancePrincipal))
	return BackupObject{Digest: ExpectedDigest(out)}
}

func backupDownloadOCI(ctx context.Context, auth CloudAuth, bucket, obj, outPath, namespace string) error {
	_, err := RunOCI(ctx, ObjectGetArgs(bucket, obj, outPath, namespace, auth.Profile, auth.InstancePrincipal))
	return err
}

// ResolveBackupNamespace resolves the tenancy object-storage namespace for
// the `os object` calls, in preference order: an explicit --namespace flag,
// the cloud block / OCI_NAMESPACE (carried on auth), else `oci os ns get` —
// the same call the oci CLI itself wraps for its internal resolution. The
// ns get leg is load-bearing on the operator laptop: the CLI's internal
// namespace resolution fails under user-principal configs ("Unable to
// retrieve namespace internally", 2026-10-10 live fire) while
// instance-principal VMs resolved fine — `os ns get` works in both worlds.
// gcp has NO namespace concept: GCS scopes by bucket+project, so a gcp auth
// resolves to "" without any CLI call at all.
func ResolveBackupNamespace(ctx context.Context, auth CloudAuth, explicit string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", nil
	}
	if explicit != "" {
		return explicit, nil
	}
	if auth.Namespace != "" {
		return auth.Namespace, nil
	}
	nsOut, err := RunOCI(ctx, OSNamespaceArgs(auth))
	if err != nil {
		return "", fmt.Errorf("could not resolve the tenancy object-storage namespace (pass it explicitly: --namespace <ns>, the profile cloud block \"namespace\", or OCI_NAMESPACE): %w", err)
	}
	ns := OSNamespace(nsOut)
	if ns == "" {
		return "", fmt.Errorf("could not resolve the tenancy object-storage namespace — pass it explicitly: --namespace <ns>, the profile cloud block \"namespace\", or OCI_NAMESPACE")
	}
	return ns, nil
}
