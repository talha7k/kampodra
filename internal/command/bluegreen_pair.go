// The blue/green pair model: the two instances (colors) that share one
// reserved IP, resolved from live OCI state, plus the guest-OS checks and
// the target health probe.

package command

import (
	"context"
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

// Surfaces that resolve through resolveBGPair but whose gcp legs haven't
// landed (slices 3/5): CheckProvider passes a gcp profile, but their legs
// still call the oci CLI directly — gcp would exec the wrong CLI. The
// refusal below fails closed BEFORE any exec, naming the surface + remedy.
const (
	surfaceBluegreen   = "bluegreen"
	surfaceImageImport = "image-import"
)

// gcpOCIOnlyRefusal is the interim fail-closed gate for those surfaces:
// nil for every provider whose CLI the legs actually exec, the
// surface-specific refusal for gcp.
func gcpOCIOnlyRefusal(auth cloud.CloudAuth, surface string) error {
	if auth.Provider != "gcp" {
		return nil
	}
	if surface == surfaceImageImport {
		return fmt.Errorf("image-import is OCI-only today — gcloud compute images import requires the Cloud Build API; run it directly or use an oci profile")
	}
	return fmt.Errorf("bluegreen is OCI-only today — the gcp leg hasn't landed; run it with an oci profile")
}

// resolveBGPair resolves the OCI compartment (flag override > cloud block
// > OCI_COMPARTMENT env, name or ocid) and the pair naming from the target.
// A missing compartment is a usage error naming both configuration sites.
// surface (surfaceBluegreen | surfaceImageImport) drives the interim gcp
// refusal — it fires before ResolveCompartment, the first exec.
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
	ocid, err := cloud.ResolveCompartment(context.Background(), auth, name)
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
	out, err := cloud.RunOCI(ctx, cloud.InstanceListArgs(pair.auth, pair.compOCID, pair.colorName(color)))
	if err != nil {
		return cloud.BGInstance{}, err
	}
	inst, ok := cloud.ParseNewestInstance(out)
	if !ok {
		return cloud.BGInstance{}, nil
	}
	att, err := cloud.RunOCI(ctx, cloud.VnicAttachmentsArgs(pair.auth, pair.compOCID, inst.OCID))
	if err != nil {
		return cloud.BGInstance{}, err
	}
	vnic, _, ok := cloud.FirstVnic(att)
	if !ok {
		return inst, nil
	}
	vnicJSON, err := cloud.RunOCI(ctx, cloud.VnicGetArgs(pair.auth, vnic))
	if err != nil {
		return cloud.BGInstance{}, err
	}
	inst.PublicIP = cloud.VnicPublicIP(vnicJSON)
	return inst, nil
}

// bgAnchorIP resolves the secondary-private-ip anchor on a VNIC ("" when
// none). LOOKUP-ONLY by contract — callers that may create use the flip
// target's own path.
func bgAnchorIP(ctx context.Context, pair bgPair, vnic string) (string, error) {
	out, err := cloud.RunOCI(ctx, cloud.PrivateIPListArgs(pair.auth, vnic))
	if err != nil {
		return "", err
	}
	return cloud.SecondaryPrivateIP(out), nil
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
