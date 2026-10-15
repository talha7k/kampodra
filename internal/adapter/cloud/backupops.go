// Provider-neutral backup ops: the backup command surface calls THESE,
// never a provider CLI directly. Same shape as the dns family: a
// CheckProvider gate, a provider switch, and a fail-closed default; the
// per-provider legs live in backupops_oci.go (byte-identical to the
// pre-gcp flow) and backupops_gcp.go. The namespace param is OCI-only —
// gcp legs ignore it (GCS scopes by bucket+project, no namespace concept).
package cloud

import "context"

// BackupList lists a bucket's objects under a prefix ("" = the whole
// bucket, listed flat).
func BackupList(ctx context.Context, auth CloudAuth, bucket, prefix, namespace string) ([]BackupObject, error) {
	if err := CheckProvider(auth); err != nil {
		return nil, err
	}
	if auth.Provider == "gcp" {
		return backupListGCP(ctx, auth, bucket, prefix)
	}
	return backupListOCI(ctx, auth, bucket, prefix, namespace)
}

// BackupHead reads one object's digest metadata. Like today's flow, a
// failed HEAD reads as zero (Digest "") — verification skips, the download
// never breaks on it.
func BackupHead(ctx context.Context, auth CloudAuth, bucket, obj, namespace string) (BackupObject, error) {
	if err := CheckProvider(auth); err != nil {
		return BackupObject{}, err
	}
	if auth.Provider == "gcp" {
		return backupHeadGCP(ctx, auth, bucket, obj)
	}
	return backupHeadOCI(ctx, auth, bucket, obj, namespace), nil
}

// BackupDownload streams one object to outPath. The caller owns the 0600
// file handling and the digest verify.
func BackupDownload(ctx context.Context, auth CloudAuth, bucket, obj, outPath, namespace string) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return backupDownloadGCP(ctx, auth, bucket, obj, outPath)
	}
	return backupDownloadOCI(ctx, auth, bucket, obj, outPath, namespace)
}
