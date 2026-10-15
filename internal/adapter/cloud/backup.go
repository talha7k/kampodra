// Backup family builders/parsers (neutral): the ops layer (backupops.go)
// dispatches to *_oci/_gcp legs; oci auth via the oci CLI's own config only
// (--profile / instance principal — AuthArgs/withProfile live with the exec
// seam in provider.go). kampodra NEVER accepts, stores, or logs credential
// material. The gcp leg (GCS) scopes by bucket+project: no namespace, no
// profile flags — gcloud's own config IS the auth.
package cloud

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// CloudAuth / CheckProvider / ParseCloudAuth and the exec/auth seam
// (RunOCI / withProfile / AuthArgs / RunCloud) live in provider.go — the
// provider gate and the ONE exec choke point are shared by every family
// here.

// withNamespace appends the resolved tenancy namespace. Empty stays
// flag-free: the caller explicitly chose CLI-native resolution.
func withNamespace(args []string, namespace string) []string {
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	return args
}

// ObjectListArgs ports the `oci os object list --all` shape (--profile stays
// on the composed vector; the scripts' static gates read it there).
// namespace is the RESOLVED tenancy namespace ("" = let the CLI resolve
// natively) — the laptop path must pass it: the CLI's internal resolution
// fails there ("Unable to retrieve namespace internally").
func ObjectListArgs(bucket, prefix, namespace, profile string, instancePrincipal bool) []string {
	args := []string{"os", "object", "list", "--all", "--bucket-name", bucket}
	if prefix != "" {
		args = append(args, "--prefix", prefix)
	}
	args = withNamespace(args, namespace)
	return withProfile(args, profile, instancePrincipal)
}

// ObjectHeadArgs ports `oci os object head` (digest metadata source).
func ObjectHeadArgs(bucket, object, namespace, profile string, instancePrincipal bool) []string {
	args := []string{"os", "object", "head", "--bucket-name", bucket, "--name", object}
	args = withNamespace(args, namespace)
	return withProfile(args, profile, instancePrincipal)
}

// ObjectGetArgs ports `oci os object get --file` (the download leg).
func ObjectGetArgs(bucket, object, file, namespace, profile string, instancePrincipal bool) []string {
	args := []string{"os", "object", "get", "--bucket-name", bucket, "--name", object, "--file", file}
	args = withNamespace(args, namespace)
	return withProfile(args, profile, instancePrincipal)
}

// ExpectedDigest ports backup_expected_digest: "sha256:<hex>" from
// opc-meta-sha256 / content-sha256, else "md5:<base64>" from
// opc-content-md5, else "" (verification skipped). Tolerates garbage — a
// failed HEAD must not break the download.
func ExpectedDigest(headJSON string) string {
	var parsed struct {
		Data struct {
			MetaSHA256    string `json:"opc-meta-sha256"`
			ContentSHA256 string `json:"content-sha256"`
			ContentMD5    string `json:"opc-content-md5"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(headJSON), &parsed) != nil {
		return ""
	}
	if v := parsed.Data.MetaSHA256; v != "" {
		return "sha256:" + v
	}
	if v := parsed.Data.ContentSHA256; v != "" {
		return "sha256:" + v
	}
	if v := parsed.Data.ContentMD5; v != "" {
		return "md5:" + v
	}
	return ""
}

// BackupObject is one parsed backup listing row — the neutral shape both
// providers map onto. Digest carries the ExpectedDigest form ("sha256:<hex>"
// / "md5:<base64>"; "" = verification skipped).
type BackupObject struct {
	Name        string
	Size        int64
	TimeCreated string // "-" when the API returned none
	Digest      string
}

// ParseObjectList ports the jq row extraction
// (.data.objects[]? | [.name, (.size|tostring), (.timeCreated // "-")]).
// The oci CLI's `os object list` returns rows DIRECTLY as an array under
// .data; the raw-API shape nests them under .data.objects — both parse.
func ParseObjectList(listJSON string) ([]BackupObject, error) {
	type objectListRow struct {
		Name        string      `json:"name"`
		Size        json.Number `json:"size"`
		TimeCreated *string     `json:"timeCreated"`
	}
	var parsed struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(listJSON), &parsed); err != nil {
		return nil, fmt.Errorf("object list: %w", err)
	}
	if len(parsed.Data) == 0 || string(parsed.Data) == "null" {
		return nil, fmt.Errorf("object list: response has no .data — unexpected oci CLI output (want an objects array or {\"data\":{\"objects\":[…]}}); check the bucket/namespace and oci CLI version")
	}
	var rows []objectListRow
	if payload := strings.TrimSpace(string(parsed.Data)); strings.HasPrefix(payload, "[") {
		if err := json.Unmarshal(parsed.Data, &rows); err != nil {
			return nil, fmt.Errorf("object list: %w", err)
		}
	} else {
		var objShape struct {
			Objects []objectListRow `json:"objects"`
		}
		if err := json.Unmarshal(parsed.Data, &objShape); err != nil {
			return nil, fmt.Errorf("object list: %w", err)
		}
		rows = objShape.Objects
	}
	objs := make([]BackupObject, 0, len(rows))
	for _, o := range rows {
		size, _ := o.Size.Int64()
		tc := "-"
		if o.TimeCreated != nil && *o.TimeCreated != "" {
			tc = *o.TimeCreated
		}
		objs = append(objs, BackupObject{Name: o.Name, Size: size, TimeCreated: tc})
	}
	return objs, nil
}

// IsAuthDenied ports the LIST-denied classifier: policy gaps are a distinct,
// actionable failure — never a generic error.
func IsAuthDenied(output string) bool {
	for _, marker := range []string{
		"NotAuthorizedOrNotFound", "NotAuthorized", "NotAuthenticated", "Authorization failed",
	} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// HashFile ports hash_file with native crypto: sha256 → hex, md5 → base64
// (the same encoding OCI reports). Any other algorithm is an error — never
// a silent pass.
func HashFile(path, alg string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	switch alg {
	case "sha256":
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	case "md5":
		h := md5.New()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
	default:
		return "", fmt.Errorf("unsupported hash algorithm: %s", alg)
	}
}
