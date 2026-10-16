// Resolution provenance: ONE terse line at the top of every host-aware
// command run — which profile/host/provider the invocation resolved to,
// printed BEFORE any network or exec. The 2026-10-16 incident: commands
// never said which profile they resolved to, so an automation sat on a
// localhost test default unnoticed until the deploy was blocked.

package command

import (
	"encoding/json"
	"fmt"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
)

// cloudProviderLabel derives the provenance line's provider: parse the
// cloud block's "provider" (encoding/json, fail-open like
// cloud.ParseCloudAuth — a corrupt block resolves native, never an error)
// and treat "" as the oci default, byte-identical to how
// cloud.CheckProvider admits the empty provider.
func cloudProviderLabel(raw json.RawMessage) string {
	provider := cloud.ParseCloudAuth(raw).Provider
	if provider == "" {
		return "oci"
	}
	return provider
}

// sayProvenance prints the resolution provenance line: which profile (or
// "-" when none was selected), which host, which cloud provider. Callers
// invoke it right after target resolution, before any network/exec — a
// mis-resolved target must be visible in the first line of output.
func sayProvenance(d Deps, cmdName string, t Target) {
	fmt.Fprintf(d.Stdout, "[%s] profile %s · host %s · provider %s\n",
		cmdName, dashEmpty(t.ProfileName), t.HostSpec.Host, cloudProviderLabel(t.Cloud))
}
