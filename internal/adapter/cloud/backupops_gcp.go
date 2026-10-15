// gcloud legs of the neutral backup ops: everything rides the gcloud CLI's
// own auth and active config. kampodra appends ONLY the cloud block's
// identity names (--project/--account, via withGCPAuth) — never credential
// material. The namespace param is ignored on this leg: GCS scopes by
// bucket+project, there is no namespace concept. gcloud emits bare [] for
// empty listings; the parser reads bare arrays natively.
package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// backupListGCP lists flat with ONE recursive call: the trailing `**`
// wildcard (an empty prefix = the whole bucket).
func backupListGCP(ctx context.Context, auth CloudAuth, bucket, prefix string) ([]BackupObject, error) {
	out, err := RunCloud(ctx, auth, gcpStorageLsArgs(bucket, prefix+"**"))
	if err != nil {
		return nil, err
	}
	return parseGCPStorageObjects(out, bucket)
}

// backupHeadGCP reads one object's metadata with a single-object ls. The
// error is swallowed (today's HEAD semantics): a failed head digest-skips,
// it never breaks the download. An empty md5Hash (composite objects carry
// none) digest-skips too.
func backupHeadGCP(ctx context.Context, auth CloudAuth, bucket, obj string) (BackupObject, error) {
	out, _ := RunCloud(ctx, auth, gcpStorageLsArgs(bucket, obj))
	objs, err := parseGCPStorageObjects(out, bucket)
	if err != nil || len(objs) == 0 {
		return BackupObject{}, nil
	}
	return objs[0], nil
}

// backupDownloadGCP copies the object to outPath; 0600 handling and the
// digest verify stay with the caller.
func backupDownloadGCP(ctx context.Context, auth CloudAuth, bucket, obj, outPath string) error {
	_, err := RunCloud(ctx, auth, []string{"storage", "cp", "gs://" + bucket + "/" + obj, outPath})
	return err
}

// gcpStorageLsArgs pins `storage ls --json gs://<bucket>/<pattern>` — the
// caller decides flat-recursive (** suffix) vs single-object.
func gcpStorageLsArgs(bucket, pattern string) []string {
	return []string{"storage", "ls", "--json", "gs://" + bucket + "/" + pattern}
}

// gcpStorageObject is one `gcloud storage ls --json` row (the GCS JSON API
// object resource: size is a string, md5Hash is base64 MD5).
type gcpStorageObject struct {
	Name        string      `json:"name"`
	Size        json.Number `json:"size"`
	MD5Hash     string      `json:"md5Hash"`
	TimeCreated string      `json:"timeCreated"`
	Updated     string      `json:"updated"`
}

// parseGCPStorageObjects maps ls rows onto the neutral BackupObject: names
// tolerate a gs://bucket/ prefix (GCS object names are bucket-relative), an
// updated stamp falls into timeCreated's slot ("-" when neither exists —
// the same placeholder ParseObjectList emits), and md5Hash rides the
// existing digest semantics: "md5:<base64>", the exact shape ExpectedDigest
// emits for opc-content-md5 and HashFile("md5") reproduces.
func parseGCPStorageObjects(lsJSON, bucket string) ([]BackupObject, error) {
	var rows []gcpStorageObject
	if err := json.Unmarshal([]byte(lsJSON), &rows); err != nil {
		return nil, fmt.Errorf("object list: %w", err)
	}
	urlPrefix := "gs://" + bucket + "/"
	objs := make([]BackupObject, 0, len(rows))
	for _, r := range rows {
		size, _ := r.Size.Int64()
		updated := r.TimeCreated
		if updated == "" {
			updated = r.Updated
		}
		if updated == "" {
			updated = "-"
		}
		objs = append(objs, BackupObject{
			Name:        strings.TrimPrefix(r.Name, urlPrefix),
			Size:        size,
			TimeCreated: updated,
			Digest:      gcpMD5Digest(r.MD5Hash),
		})
	}
	return objs, nil
}

// gcpMD5Digest maps GCS's base64 md5Hash onto the digest string form; an
// empty hash (composite objects) leaves Digest empty — verification skips.
func gcpMD5Digest(md5Hash string) string {
	if md5Hash == "" {
		return ""
	}
	return "md5:" + md5Hash
}
