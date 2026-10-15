// Provider gate and the ONE exec seam shared by every cloud family: the
// provider dispatch (oci | gcp), the per-CLI identity flags, and RunCloud.
//
// AUTH RULE (hard, same as every family in this package): the provider
// CLI's own auth ONLY — oci's config file / instance principal, gcloud's
// active config. kampodra NEVER accepts, stores, or logs credential
// material: the cloud block names IDENTITY entries only (profile,
// compartment, project, account). Token flags, key files, and credential
// env vars are forbidden forever.
package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// CloudAuth is the instance profile's `cloud` block: kampodra's ONLY
// cloud-auth surface. It names overrides for the provider CLI's own config
// (provider, profile, compartment, auth mode, gcp project/account) — never
// credentials. Empty block = the provider CLI resolves everything natively
// (its default profile / active config, its env, its own precedence).
type CloudAuth struct {
	Provider          string `json:"provider"`
	Profile           string `json:"profile"`           // oci-leg only
	Compartment       string `json:"compartment"`       // oci-leg only
	Namespace         string `json:"namespace"`         // oci-leg only
	InstancePrincipal bool   `json:"instancePrincipal"` // oci-leg only
	Project           string `json:"project"`           // gcp-only; empty = gcloud's active config
	Account           string `json:"account"`           // gcp-only; empty = gcloud's active config
}

// SupportedProviders lists the cloud providers kampodra speaks. The
// `provider` field is the forward-compat slot: "" stays oci for every
// pre-gcp profile (byte-identical default).
func SupportedProviders() []string {
	return []string{"oci", "gcp"}
}

// CheckProvider fails closed on an unknown provider name (empty = the
// default). Call it on every path that actually calls a provider CLI —
// never on paths that don't need the cloud at all.
func CheckProvider(auth CloudAuth) error {
	if auth.Provider == "" || auth.Provider == "oci" || auth.Provider == "gcp" {
		return nil
	}
	return fmt.Errorf("unsupported cloud provider %q (supported: %s) — set the instance profile's \"cloud\" block provider, or omit it for the default", auth.Provider, strings.Join(SupportedProviders(), ", "))
}

// CLIName maps a provider to its CLI binary — the LookPath key for the one
// exec seam and every pre-flight guard.
func CLIName(provider string) string {
	if provider == "gcp" {
		return "gcloud"
	}
	return "oci"
}

// InstallHint is the fail-closed path's whole remedy: how to get the CLI,
// then how to init its OWN auth. Never a credential workaround.
func InstallHint(provider string) string {
	if provider == "gcp" {
		return "install Google Cloud SDK (https://cloud.google.com/sdk/docs/install), then: gcloud init"
	}
	return "install oci-cli (https://docs.oracle.com/en-us/iaas/Content/API/SDKDocs/cliinstall.htm), then: oci setup config"
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

// withGCPAuth mirrors withProfile: the cloud block's identity names ride as
// gcloud's own config overrides, ONLY when set — an empty project/account
// lets gcloud resolve its active config natively (its "project not set"
// error is the actionable message). Identity names, never credentials.
func withGCPAuth(args []string, auth CloudAuth) []string {
	if auth.Project != "" {
		args = append(args, "--project="+auth.Project)
	}
	if auth.Account != "" {
		args = append(args, "--account="+auth.Account)
	}
	return args
}

// AuthArgs ports the oci CLI's AUTH_ARGS: instance principal appends --auth
// instance_principal; config-file auth adds nothing. (oci-leg only.)
func AuthArgs(instancePrincipal bool) []string {
	if instancePrincipal {
		return []string{"--auth", "instance_principal"}
	}
	return nil
}

// withProfile appends the oci CLI's config-profile overrides: the cloud
// block's explicit --profile only when set (an empty profile lets the oci
// CLI resolve natively — its config's default/DEFAULT/first-profile
// precedence), then the instance-principal auth args. (oci-leg only.)
func withProfile(args []string, profile string, instancePrincipal bool) []string {
	if profile != "" {
		args = append(args, "--profile", profile)
	}
	return append(args, AuthArgs(instancePrincipal)...)
}

// RunOCI execs the oci CLI (LookPath'd like every other external seam) and
// returns stdout; stderr rides the error so callers can classify failures.
// It IS the oci leg of RunCloud below — every oci argv builder in the
// package feeds it; the gcp twin is runGCloud.
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

// RunCloud is THE exec seam for provider traffic: dispatch on
// auth.Provider. oci (and empty — the byte-identical default) delegates to
// RunOCI; gcp execs gcloud with the same LookPath + stderr-attachment
// error shape; anything else fails closed.
func RunCloud(ctx context.Context, auth CloudAuth, args []string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return runGCloud(ctx, auth, args)
	}
	return RunOCI(ctx, args)
}

// runGCloud execs the gcloud CLI: its own auth IS the auth — the ONLY
// flags kampodra adds are the cloud block's identity names, appended here
// at the single choke point (a leg that forgot them would silently fall
// back to gcloud's active config — a wrong-project write). stderr rides
// the error so callers can classify failures.
func runGCloud(ctx context.Context, auth CloudAuth, args []string) (string, error) {
	bin, err := exec.LookPath(CLIName(auth.Provider))
	if err != nil {
		return "", fmt.Errorf("%s CLI not found — %s", CLIName(auth.Provider), InstallHint(auth.Provider))
	}
	cmd := exec.CommandContext(ctx, bin, withGCPAuth(args, auth)...)
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			msg = strings.TrimSpace(string(exitErr.Stderr))
		}
		if msg != "" {
			return string(out), fmt.Errorf("gcloud %s: %s", args[0], msg)
		}
		return string(out), fmt.Errorf("gcloud %s: %w", args[0], err)
	}
	return string(out), nil
}
