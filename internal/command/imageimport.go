package command

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
)

// image-import: upload a packer-built qcow2 to Object Storage and import
// it as an OCI custom image (runs on the BUILD machine; the VM never
// touches OCI APIs). Alpine is not in OCI's supported import OS list, so
// the import is self-supported (generic OS metadata, PARAVIRTUALIZED).
// The import PINs firmware=BIOS (OCI platform behavior — not derivable);
// A1/Ampere is UEFI-only and rejects BIOS images at launch, so a non-UEFI
// result warns loudly instead of failing: x86 shapes launch it fine, and
// the A1 route is capture-from-instance. Cloud auth rides the instance
// profile's cloud block (or OCI_PROFILE/OCI_COMPARTMENT env); the oci CLI
// resolves natively otherwise. kampodra never accepts credentials —
// configure the oci CLI (`oci setup config`) with keys sourced from your
// own pass store.
const imageImportHelp = `Usage:
  kampodra image-import --image <qcow2> [--os alpine|ubuntu] [--bucket <name>]
                        [--name-prefix <p>] [--compartment <name>] [--keep-object]

Examples:
  kampodra image-import --image build/app-alpine-3.22.6-aarch64.qcow2
`

func newImageImportCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image-import",
		Short: "upload a qcow2 to Object Storage and import it as an OCI custom image",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runImageImport(d, c)
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), imageImportHelp)
	})
	pf := cmd.PersistentFlags()
	pf.String("profile", "", "kampodra instance profile (project naming + cloud block)")
	pf.String("image", "", "qcow2 file to import (required)")
	pf.String("bucket", "", "staging bucket (KAMPODRA_IMPORT_BUCKET env, else <container>-image-import)")
	pf.String("name-prefix", "", "image display-name prefix (default <container>-alpine, or <container>-ubuntu-24.04 with --os ubuntu)")
	pf.String("os", "alpine", "guest OS of the qcow2 (alpine|ubuntu)")
	pf.String("compartment", "", "compartment name or ocid (cloud block or OCI_COMPARTMENT env)")
	pf.Bool("keep-object", false, "keep the staged qcow2 object after a successful import")
	return cmd
}

// importStage is the validated local input set: the qcow2 path, staging
// bucket, display-name prefix, the guest OS + its OCI import metadata,
// and the keep-object choice.
type importStage struct {
	image      string
	bucket     string
	namePrefix string
	osName     string
	osVersion  string
	keepObject bool
}

// bgImportStage validates the local inputs (flags over env over derived
// defaults) before any OCI call happens. The guest OS resolves to its
// import metadata here (fail-closed); the prefix falls back to the
// per-OS golden-image default.
func bgImportStage(d Deps, c *cobra.Command, pair bgPair) (importStage, error) {
	var st importStage
	st.image = flagString(c, "image")
	if st.image == "" {
		return st, fmt.Errorf("--image <qcow2> is required (build it with packer first)")
	}
	if _, err := os.Stat(st.image); err != nil {
		return st, fmt.Errorf("image not found: %s", st.image)
	}
	guestOS := flagString(c, "os")
	osName, osVersion, err := cloud.ImportOSMetadata(guestOS)
	if err != nil {
		return st, err
	}
	st.osName, st.osVersion = osName, osVersion
	st.bucket = flagString(c, "bucket")
	if st.bucket == "" {
		if v, ok := d.Env("KAMPODRA_IMPORT_BUCKET"); ok && v != "" {
			st.bucket = v
		} else {
			st.bucket = pair.container + "-image-import"
		}
	}
	st.namePrefix = flagString(c, "name-prefix")
	if st.namePrefix == "" {
		if st.namePrefix, err = goldenImagePrefix(pair.container, guestOS); err != nil {
			return st, err
		}
	}
	st.keepObject = flagBool(c, "keep-object")
	return st, nil
}

const (
	importPollTries = 120
	importPollSleep = 15 * time.Second
)

func runImageImport(d Deps, c *cobra.Command) error {
	ctx := c.Context()
	// Fail closed on --os BEFORE any network/ssh work (the compartment
	// resolve below is the first OCI call — it must not run for a usage
	// error).
	guestOS := flagString(c, "os")
	if err := checkGuestOS(guestOS); err != nil {
		return err
	}
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, flagString(c, "compartment"))
	if err != nil {
		return err
	}
	auth := pair.auth
	stage, err := bgImportStage(d, c, pair)
	if err != nil {
		return err
	}
	image, bucket, namePrefix, keepObject := stage.image, stage.bucket, stage.namePrefix, stage.keepObject
	namespace, err := resolveOSNamespace(ctx, auth, "")
	if err != nil {
		return err
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	objectName := namePrefix + "-" + stamp + ".qcow2"
	imageName := namePrefix + "-" + strings.TrimSuffix(filepath.Base(image), ".qcow2") + "-" + stamp
	// Strip a repeated prefix (image app-alpine-3.22 + prefix app-alpine ->
	// app-alpine-3.22, not app-alpine-app-alpine-3.22).
	imageName = strings.Replace(imageName, namePrefix+"-"+namePrefix+"-", namePrefix+"-", 1)

	fmt.Fprintf(d.Stdout, "[import] uploading %s -> os://%s/%s\n", filepath.Base(image), bucket, objectName)
	if _, err := cloud.RunOCI(ctx, cloud.OSObjectPutArgs(auth, bucket, image, objectName)); err != nil {
		return fmt.Errorf("object upload failed (bucket exists? oci os bucket create -bn %s): %w", bucket, err)
	}

	fmt.Fprintf(d.Stdout, "[import] importing as custom image '%s' (self-supported: PARAVIRTUALIZED)…\n", imageName)
	importOut, err := cloud.RunOCI(ctx, cloud.ImageImportArgs(auth, pair.compOCID, bucket, namespace, objectName, imageName, stage.osName, stage.osVersion))
	if err != nil {
		return fmt.Errorf("import call failed: %w", err)
	}
	imageOCID, ok := cloud.ImportImageOCID(importOut)
	if !ok {
		return fmt.Errorf("unexpected import response: %s", importOut)
	}

	fmt.Fprintf(d.Stdout, "[import] polling import -> AVAILABLE (up to 30m)…\n")
	state := ""
	for i := 0; i < importPollTries; i++ {
		getOut, err := cloud.RunOCI(ctx, cloud.ImageGetArgs(auth, imageOCID))
		if err != nil {
			return err
		}
		state = cloud.ImageState(getOut)
		switch state {
		case "AVAILABLE":
			goto polled
		case "PENDING_IMPORT", "IMPORTING", "UPLOADING":
			time.Sleep(importPollSleep)
		default:
			return fmt.Errorf("import reached terminal state: %s (console -> Compute -> Custom images for the error)", orUnknownState(state))
		}
	}
	return fmt.Errorf("import did not become AVAILABLE in 30m (state: %s)", orUnknownState(state))
polled:

	firmware := ""
	fwOut, err := cloud.RunOCI(ctx, cloud.ImageGetArgs(auth, imageOCID))
	if err == nil {
		firmware = cloud.ImageFirmware(fwOut)
	}
	if firmware != "" && firmware != "UEFI_64" {
		fmt.Fprintf(d.Stdout, "[import] WARN: import landed firmware=%s — VM.Standard.A1.Flex (Ampere, UEFI-only) WILL reject this image at launch.\n", firmware)
		fmt.Fprintln(d.Stdout, "      OCI has no import-time firmware control: the sanctioned A1 route is")
		fmt.Fprintln(d.Stdout, "      capture-from-instance: boot the qcow2 elsewhere, then 'oci compute image create --instance-id <running-a1>'.")
		fmt.Fprintln(d.Stdout, "      x86 shapes CAN launch this image directly.")
	}

	if !keepObject {
		fmt.Fprintln(d.Stdout, "[import] deleting the staged object (image data now lives in the custom image)…")
		if _, err := cloud.RunOCI(ctx, cloud.OSObjectDeleteArgs(auth, bucket, objectName)); err != nil {
			fmt.Fprintln(d.Stdout, "[import] WARN: staged object delete failed (cleanup manually)")
		}
	}

	fmt.Fprintln(d.Stdout, "[import] IMPORTED:")
	fmt.Fprintf(d.Stdout, "  image OCID: %s\n", imageOCID)
	fmt.Fprintf(d.Stdout, "  display   : %s\n", imageName)
	fmt.Fprintf(d.Stdout, "  firmware  : %s\n", orUnknownState(firmware))
	if firmware == "UEFI_64" {
		fmt.Fprintf(d.Stdout, "  A1-ready. 'kampodra bluegreen provision <blue|green>' picks this image (newest %s*).\n", namePrefix)
	} else {
		fmt.Fprintf(d.Stdout, "  A1 launch will REJECT this image (firmware %s) — see the WARN above.\n", orUnknownState(firmware))
	}
	return nil
}

func orUnknownState(s string) string {
	if s == "" {
		return "UNKNOWN"
	}
	return s
}
