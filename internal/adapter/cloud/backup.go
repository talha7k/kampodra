// Backup family of the oci CLI surface (the backup.sh port): object LIST /
// HEAD / GET argument builders, digest-extraction and integrity hashing,
// list parsing, and auth-denied detection.
//
// AUTH RULE (hard, same as the dns family): the oci CLI's own auth ONLY —
// its config file (--profile) or instance principal. kampodra NEVER
// accepts, stores, or logs credential material.
package cloud

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// AuthArgs ports AUTH_ARGS: instance principal appends --auth
// instance_principal; config-file auth adds nothing.
func AuthArgs(instancePrincipal bool) []string {
	if instancePrincipal {
		return []string{"--auth", "instance_principal"}
	}
	return nil
}

// CloudAuth is the instance profile's `cloud` block: kampodra's ONLY
// cloud-auth surface. It names overrides for the provider CLI's own config
// (provider, profile, compartment, auth mode) — never credentials. Empty
// block = the provider CLI resolves everything natively (its default
// profile, its env, its own precedence).
type CloudAuth struct {
	Provider          string `json:"provider"`
	Profile           string `json:"profile"`
	Compartment       string `json:"compartment"`
	Namespace         string `json:"namespace"`
	InstancePrincipal bool   `json:"instancePrincipal"`
}

// SupportedProviders lists the cloud providers kampodra speaks. OCI is
// the only one today; the `provider` field exists so a second provider
// slots in without renaming anything.
func SupportedProviders() []string {
	return []string{"oci"}
}

// CheckProvider fails closed on an unknown provider name (empty = the
// default). Call it on every path that actually calls a provider CLI —
// never on paths that don't need the cloud at all.
func CheckProvider(auth CloudAuth) error {
	if auth.Provider == "" || auth.Provider == "oci" {
		return nil
	}
	return fmt.Errorf("unsupported cloud provider %q (supported: %s) — set the instance profile's \"cloud\" block provider, or omit it for the default", auth.Provider, strings.Join(SupportedProviders(), ", "))
}

// ParseCloudAuth parses the profile's raw `cloud` block. Fail-open like the
// profile project block: nil or corrupt input resolves to zero (native
// provider-CLI auth) — a live config typo must not brick cloud access.
// (An unknown *provider name* still fails closed via CheckProvider —
// typos there must not silently resolve to OCI.)
func ParseCloudAuth(raw json.RawMessage) CloudAuth {
	var auth CloudAuth
	if len(raw) == 0 {
		return auth
	}
	_ = json.Unmarshal(raw, &auth) // corrupt block = native auth, never an error
	return auth
}

func withProfile(args []string, profile string, instancePrincipal bool) []string {
	if profile != "" {
		// Explicit override only: an empty profile lets the oci CLI resolve
		// natively (its config's default/DEFAULT/first-profile precedence).
		args = append(args, "--profile", profile)
	}
	return append(args, AuthArgs(instancePrincipal)...)
}

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

// BackupObject is one parsed `os object list` row.
type BackupObject struct {
	Name        string
	Size        int64
	TimeCreated string // "-" when the API returned none
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

// RunOCI execs the oci CLI (LookPath'd like every other external seam) and
// returns stdout; stderr rides the error so callers can classify failures.
func RunOCI(ctx context.Context, args []string) (string, error) {
	bin, err := exec.LookPath("oci")
	if err != nil {
		return "", fmt.Errorf("oci CLI not found — install oci-cli (https://docs.oracle.com/en-us/iaas/Content/API/SDKDocs/cliinstall.htm), then: oci setup config")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			msg = strings.TrimSpace(string(exitErr.Stderr))
		}
		if msg != "" {
			return string(out), fmt.Errorf("oci %s: %s", args[0], msg)
		}
		return string(out), fmt.Errorf("oci %s: %w", args[0], err)
	}
	return string(out), nil
}
