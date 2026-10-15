// Provider-neutral image-import ops: the imageimport.go command surface
// calls THESE, never a provider CLI directly. Same shape as the dns/
// backup/bg families: a CheckProvider gate, a provider switch, and a
// fail-closed default. The per-provider legs live in imageops_oci.go
// (byte-identical to the pre-gcp flow — the PATH-shim argv pins in
// command/imageimport_test.go prove it). There is no imageops_gcp.go:
// image-import is OCI-only BY DESIGN (owner-approved, D2) — see
// ImageImportGCPRefusal — so every gcp case is the same refusal.
package cloud

import (
	"context"
	"errors"
)

// ImageImportGCPRefusal is the fail-closed contract for image-import on a
// gcp profile, ONE source of truth rendered by BOTH the command surface's
// entry guard (gcpOCIOnlyRefusal) and every op's gcp default below:
// `gcloud compute images import` requires enabling the Cloud Build API — a
// billing-bearing, project-level mutation kampodra must never trigger
// implicitly — and the import runs a 10-25min async pipeline outside
// kampodra's reach. The sanctioned escape is manual, operator-run:
// gcloud compute images import <name> --source-file=<qcow2> --os=<os>
const ImageImportGCPRefusal = "image-import is OCI-only today — gcloud compute images import requires the Cloud Build API; run it directly or use an oci profile"

// imageImportGCPOnly is the family's gcp case: the refusal, never an
// improvised argv (defense in depth behind the entry guard — an op called
// straight from future code can only refuse, never exec the wrong cloud).
func imageImportGCPOnly() error {
	return errors.New(ImageImportGCPRefusal)
}

// OrUnknownState renders an empty provider state as UNKNOWN — the CLI can
// return an empty lifecycle-state when its JSON is unreadable, and both
// the op error texts and the command's firmware display must name that
// instead of printing a silent blank.
func OrUnknownState(s string) string {
	if s == "" {
		return "UNKNOWN"
	}
	return s
}

// ImageStage uploads the qcow2 to the staging bucket (os object put).
func ImageStage(ctx context.Context, auth CloudAuth, bucket, file, objectName string) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return imageImportGCPOnly()
	}
	return imageStageOCI(ctx, auth, bucket, file, objectName)
}

// ImageImport imports the staged object as a custom image and returns the
// RAW import response — the caller owns the OCID parse (ImportImageOCID)
// and its unexpected-response rendering.
func ImageImport(ctx context.Context, auth CloudAuth, compOCID, bucket, namespace, object, displayName, osName, osVersion string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", imageImportGCPOnly()
	}
	return imageImportOCI(ctx, auth, compOCID, bucket, namespace, object, displayName, osName, osVersion)
}

// ImageWaitReady polls the import to AVAILABLE (up to 30m) and returns nil
// then; a terminal state or the timeout is the caller's error verbatim.
// The lifecycle-state machine (PENDING_IMPORT/IMPORTING/UPLOADING) is
// provider knowledge — that is why the loop lives here, not in the command.
func ImageWaitReady(ctx context.Context, auth CloudAuth, imageID string) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return imageImportGCPOnly()
	}
	return imageWaitReadyOCI(ctx, auth, imageID)
}

// ImageGet fetches one image record (raw output; the caller parses
// firmware/state). Serves the post-poll firmware verdict read.
func ImageGet(ctx context.Context, auth CloudAuth, imageID string) (string, error) {
	if err := CheckProvider(auth); err != nil {
		return "", err
	}
	if auth.Provider == "gcp" {
		return "", imageImportGCPOnly()
	}
	return imageGetOCI(ctx, auth, imageID)
}

// ImageDeleteStage removes the staged qcow2 object after a successful
// import (skipped by the caller on --keep-object; a failure is a WARN
// there, never a failed import).
func ImageDeleteStage(ctx context.Context, auth CloudAuth, bucket, objectName string) error {
	if err := CheckProvider(auth); err != nil {
		return err
	}
	if auth.Provider == "gcp" {
		return imageImportGCPOnly()
	}
	return imageDeleteStageOCI(ctx, auth, bucket, objectName)
}
