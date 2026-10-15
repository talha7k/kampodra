// The blue/green pair model: the two instances (colors) that share one
// reserved IP, resolved from live OCI state, plus the guest-OS checks and
// the target health probe.

package command

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// Flip-time pacing (tests stub immediate answers, so no sleep fires).
const (
	flipPollSleep = 5 * time.Second
	flipAddrTries = 12 // guest watcher pickup: 60s
	flipACMETries = 24 // LE HTTP-01: 120s
)

var (
	bgColorRe = regexp.MustCompile(`^(blue|green)$`)
	bgCIDRRe  = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$`)
	bgIPRe    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$`)
)

// checkGuestOS fails closed on an unknown --os value, naming the offender
// and the supported set (mirror of cloud.CheckProvider's style).
func checkGuestOS(guestOS string) error {
	for _, s := range cloud.SupportedImportOSes() {
		if guestOS == s {
			return nil
		}
	}
	return fmt.Errorf("unsupported --os %q (supported: %s)", guestOS, strings.Join(cloud.SupportedImportOSes(), ", "))
}

// goldenImagePrefix is the golden-OS-image display-name prefix for the
// guest os, derived from the project container so provision's lookup and
// image-import's naming can never drift apart: alpine ->
// <container>-alpine, ubuntu -> <container>-ubuntu-24.04.
func goldenImagePrefix(container, guestOS string) (string, error) {
	if err := checkGuestOS(guestOS); err != nil {
		return "", err
	}
	if guestOS == "ubuntu" {
		return container + "-ubuntu-24.04", nil
	}
	return container + "-alpine", nil
}

// bgPair is the resolved pair context: OCI compartment + auth, and the
// instance display-name prefix + container name the pair's names derive
// from.
type bgPair struct {
	auth           cloud.CloudAuth
	container      string
	instancePrefix string // pair instance display names: <instancePrefix>-<color>
	sshKey         string // the RESOLVED target identity — every guest ssh leg rides it
	compName       string
	compOCID       string
	anchorConf     string // guest anchor.conf path (EnvDir-derived — matches OUR watcher)
	healthPath     string
	port           string
	envFile        string
	deployedSha    string
}

// colorName renders the pair instance display name
// (<instancePrefix>-<color>; the prefix falls back to container —
// project.Config.PairInstancePrefixEffective).
func (p bgPair) colorName(color string) string {
	return p.instancePrefix + "-" + color
}

// Surfaces that resolve through resolveBGPair whose gcp path is
// OCI-only-by-design: image-import refuses on gcp with the ONE contract
// text (cloud.ImageImportGCPRefusal — the Cloud Build trust-shape; the
// neutral image ops behind the surface refuse too, so this entry guard is
// defense in depth) and fails closed BEFORE any exec. (bluegreen
// dispatched for real since slice 4 — the bg ops it fronts, cloud.BG*,
// switch on auth.Provider — so only image-import remains guarded here.)
const surfaceImageImport = "image-import"

// Surface tag for the bluegreen subcommands: their gcp legs landed in
// slice 4 (bgops_gcp.go), so the refusal never fires for this value — it
// stays as the resolveBGPair discriminator the call sites pass.
const surfaceBluegreen = "bluegreen"

// gcpOCIOnlyRefusal is the fail-closed gate for the still-OCI-only
// surfaces: nil for every provider whose CLI the legs actually exec, and
// nil for gcp on surfaces that dispatch — the surface-specific refusal
// (the ONE contract const, shared with the ops' gcp defaults) otherwise.
func gcpOCIOnlyRefusal(auth cloud.CloudAuth, surface string) error {
	if auth.Provider != "gcp" || surface != surfaceImageImport {
		return nil
	}
	return errors.New(cloud.ImageImportGCPRefusal)
}

// resolveBGPair resolves the OCI compartment (flag override > cloud block
// > OCI_COMPARTMENT env, name or ocid) and the pair naming from the target.
// A missing compartment is a usage error naming both configuration sites.
// surface (surfaceBluegreen | surfaceImageImport) drives the gcp
// refusal for the still-OCI-only surfaces — it fires before any exec.
func resolveBGPair(d Deps, target Target, compartmentOverride, surface string) (bgPair, error) {
	auth := cloudAuthFor(d, target)
	if err := cloud.CheckProvider(auth); err != nil {
		return bgPair{}, err
	}
	if err := gcpOCIOnlyRefusal(auth, surface); err != nil {
		return bgPair{}, err
	}
	name := compartmentOverride
	if name == "" {
		name = auth.Compartment
	}
	if name == "" {
		return bgPair{}, fmt.Errorf("no OCI compartment configured — set the instance profile's \"cloud\" block ({\"compartment\": \"<name or ocid>\"}) in ~/.kampodra/config.json, or export OCI_COMPARTMENT=<compartment name or ocid> — compartments are account-specific")
	}
	ocid, err := cloud.BGResolveScope(context.Background(), auth, name)
	if err != nil {
		return bgPair{}, err
	}
	p := target.Project
	return bgPair{
		auth:           auth,
		container:      p.Container,
		instancePrefix: p.PairInstancePrefixEffective(),
		sshKey:         target.HostSpec.SSHKey,
		compName:       name,
		compOCID:       ocid,
		anchorConf:     path.Dir(p.EnvFile) + "/anchor.conf",
		healthPath:     p.HealthPath,
		port:           p.Port,
		envFile:        p.EnvFile,
		deployedSha:    p.DeployedShaFile,
	}, nil
}

// bgInstance resolves one color: the newest RUNNING instance row + its
// ephemeral public IP ("" when the VNIC has none yet). Empty row = not
// provisioned.
func bgInstance(ctx context.Context, pair bgPair, color string) (cloud.BGInstance, error) {
	return cloud.BGColorInstance(ctx, pair.auth, pair.compOCID, pair.colorName(color))
}

// bgAnchorIP resolves the secondary-private-ip anchor on a VNIC ("" when
// none). LOOKUP-ONLY by contract — callers that may create use the flip
// target's own path.
func bgAnchorIP(ctx context.Context, pair bgPair, vnic string) (string, error) {
	return cloud.BGAnchorIP(ctx, pair.auth, vnic)
}

// instanceHealthy gates on the target's OWN IP: service up + loopback app
// check (wget with a curl fallback — golden images are minimal). The ssh
// leg rides the resolved target identity — a bare spec only worked when
// the agent happened to hold the ops key (2026-10-09 live fire).
func instanceHealthy(ctx context.Context, d Deps, ip, sshKey, container, port, healthPath string) bool {
	if ip == "" {
		return false
	}
	spec := transport.HostSpec{Host: "root@" + ip, SSHKey: sshKey}
	probe := fmt.Sprintf("rc-service %s status >/dev/null 2>&1 && (busybox wget -q -O /dev/null http://127.0.0.1:%s%s || curl -s -m 8 -o /dev/null http://127.0.0.1:%s%s)",
		container, port, healthPath, port, healthPath)
	_, err := d.Runner.Run(ctx, spec, probe)
	return err == nil
}
