// The cloud-auth env rename: the vendor-neutral KAMPODRA_CLOUD_* names are
// primary on provider-neutral surfaces; the vendor-prefixed OCI_* names
// survive one release as read-but-deprecated aliases. THIS file owns the
// whole ladder — resolution order, empty-means-unset, and the warn-once-
// per-process stderr line — so no read site duplicates any of it.

package command

import (
	"fmt"
	"sync"
)

// The ladder pairs: kampodra-namespaced name first, deprecated vendor-
// prefixed alias second. The aliases stop resolving in a future release —
// removal is a delete here plus a row in the docs env table.
const (
	envCloudProfile           = "KAMPODRA_CLOUD_PROFILE"
	envCloudProfileLegacy     = "OCI_PROFILE"
	envCloudCompartment       = "KAMPODRA_CLOUD_COMPARTMENT"
	envCloudCompartmentLegacy = "OCI_COMPARTMENT"
	envCloudNamespace         = "KAMPODRA_CLOUD_NAMESPACE"
	envCloudNamespaceLegacy   = "OCI_NAMESPACE"
)

// cloudEnvDeprecatedWarned remembers which deprecated names already printed
// their one stderr line: a single run may read the same pair at a usage
// gate AND inside the auth resolver, and the operator still sees ONE line.
var cloudEnvDeprecatedWarned sync.Map

// resetCloudEnvDeprecationWarnings clears the warn-once guard. Test seam
// only — the guard is process-global by design (one notice per process).
func resetCloudEnvDeprecationWarnings() {
	cloudEnvDeprecatedWarned.Range(func(key, _ any) bool {
		cloudEnvDeprecatedWarned.Delete(key)
		return true
	})
}

// cloudEnvWithFallback resolves one cloud-auth setting: the kampodra-
// namespaced name wins; when only the deprecated OCI_* name supplies the
// value, it is honored (read-but-deprecated alias) and ONE stderr line per
// process names the rename. An empty value counts as unset for BOTH names
// (`OCI_PROFILE= kampodra …` is no signal, never a warning). usedDeprecated
// reports which name supplied the value.
func cloudEnvWithFallback(d Deps, newKey, oldKey string) (value string, usedDeprecated bool) {
	if v, ok := d.Env(newKey); ok && v != "" {
		return v, false
	}
	v, ok := d.Env(oldKey)
	if !ok || v == "" {
		return "", false
	}
	if _, warned := cloudEnvDeprecatedWarned.LoadOrStore(oldKey, struct{}{}); !warned {
		fmt.Fprintf(d.Stderr, "[config] %s is deprecated — use %s (the OCI_* names stop working in a future release)\n", oldKey, newKey)
	}
	return v, true
}
