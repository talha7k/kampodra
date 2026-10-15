// OCI legs of the neutral image-import ops: TODAY's exported builders +
// parsers, composed verbatim — the oci argv is byte-identical to the
// pre-gcp flow (the PATH-shim argv pins in command/imageimport_test.go
// prove it). The wait loop's lifecycle-state texts moved here with the
// loop (leg-owned rendering, the bgops_oci.go precedent); they are
// byte-identical to the old command bodies.
package cloud

import (
	"context"
	"fmt"
	"time"
)

// Import-poll cadence: 120 tries x 15s = the documented "up to 30m" the
// command surface prints (moved verbatim from the command body).
const (
	importPollTries = 120
	importPollSleep = 15 * time.Second
)

func imageStageOCI(ctx context.Context, auth CloudAuth, bucket, file, objectName string) error {
	_, err := RunOCI(ctx, OSObjectPutArgs(auth, bucket, file, objectName))
	return err
}

func imageImportOCI(ctx context.Context, auth CloudAuth, compOCID, bucket, namespace, object, displayName, osName, osVersion string) (string, error) {
	return RunOCI(ctx, ImageImportArgs(auth, compOCID, bucket, namespace, object, displayName, osName, osVersion))
}

func imageWaitReadyOCI(ctx context.Context, auth CloudAuth, imageID string) error {
	state := ""
	for i := 0; i < importPollTries; i++ {
		getOut, err := RunOCI(ctx, ImageGetArgs(auth, imageID))
		if err != nil {
			return err
		}
		state = ImageState(getOut)
		switch state {
		case "AVAILABLE":
			return nil
		case "PENDING_IMPORT", "IMPORTING", "UPLOADING":
			time.Sleep(importPollSleep)
		default:
			return fmt.Errorf("import reached terminal state: %s (console -> Compute -> Custom images for the error)", OrUnknownState(state))
		}
	}
	return fmt.Errorf("import did not become AVAILABLE in 30m (state: %s)", OrUnknownState(state))
}

func imageGetOCI(ctx context.Context, auth CloudAuth, imageID string) (string, error) {
	return RunOCI(ctx, ImageGetArgs(auth, imageID))
}

func imageDeleteStageOCI(ctx context.Context, auth CloudAuth, bucket, objectName string) error {
	_, err := RunOCI(ctx, OSObjectDeleteArgs(auth, bucket, objectName))
	return err
}
