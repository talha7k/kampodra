package command

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// Blue/green reserved-IP pair management for OCI VMs: one instance can
// start alone; when a second is warranted (zero-downtime upgrades, risky
// migrations), the pair shares one RESERVED public IP — the internet-facing
// address never changes, the flip is one OCI API call, rollback is the
// same call reversed. The reserved IP is derived from live OCI state — no
// local state file. Colors are instance display names: <container>-blue /
// <container>-green.
//
// FLIP = ACME-FIRST (health gate on the target's own IP → anchor.conf on
// the target guest, watcher configures the address → OCI assigns the
// reserved IP to the anchor → ACME on the target → verify through the
// reserved IP). Any post-assign failure auto-rolls back to the other
// color's EXISTING anchor (lookup-only) or to dormant.
const bluegreenHelp = `Usage:
  kampodra bluegreen status                  # pair view: instances, IP holder, health
  kampodra bluegreen init                    # create the reserved public IP (dormant, unassigned)
  kampodra bluegreen provision <color>       # launch the second instance (golden image; see below)
  kampodra bluegreen flip --to <color>       # ACME-first health-gated flip (see below)
  kampodra bluegreen rollback                # unassign to DORMANT + holder guest cleanup

Each sub-step has its own --help: status | init | provision | flip | rollback.
Cloud auth rides the instance profile's cloud block (or OCI_PROFILE /
OCI_COMPARTMENT env); the oci CLI resolves natively otherwise.

Examples:
  kampodra bluegreen status
  kampodra bluegreen init
  kampodra bluegreen provision green
  kampodra bluegreen flip --to green
  kampodra bluegreen rollback
`

func newBluegreenCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bluegreen",
		Short: "OCI reserved-IP blue/green pair: status | init | provision | flip | rollback",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprint(c.OutOrStdout(), bluegreenHelp)
			return nil
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), bluegreenHelp)
	})
	tp := cmd.PersistentFlags()
	tp.String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODRA_HOST and any profile")
	tp.String("profile", "", "per-instance profile (~/.kampodra/config.json) — beats KAMPODRA_PROFILE / defaultProfile")
	tp.String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY; empty = agent / ssh config")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "status",
			Short: "pair view: reserved IP + holder, both instances, per-color app health (read-only)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runBluegreenStatus(d, c)
			},
		},
		&cobra.Command{
			Use:   "init",
			Short: "create the DORMANT reserved public IP (idempotent — exits 0 when it exists)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runBluegreenInit(d, c)
			},
		},
		&cobra.Command{
			Use:   "provision <blue|green>",
			Short: "launch the second instance (native UEFI golden image, else platform-image + golden-disk injection)",
			Args:  cobra.ExactArgs(1),
			RunE: func(c *cobra.Command, args []string) error {
				return runBluegreenProvision(d, c, args[0])
			},
		},
		&cobra.Command{
			Use:   "flip",
			Short: "ACME-first health-gated cutover (--to <blue|green>; --force overrides an unhealthy target)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runBluegreenFlip(d, c)
			},
		},
		&cobra.Command{
			Use:   "rollback",
			Short: "unassign the reserved IP to DORMANT + holder guest cleanup (to move traffic: flip --to <other>)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				return runBluegreenRollback(d, c)
			},
		},
	)
	// Shared surface, declared once: persistent flags are inherited by
	// every bluegreen subcommand (root-local flags would be invisible to
	// them — cobra only inherits persistent flags).
	pf := cmd.PersistentFlags()
	pf.String("image-id", "", "provision: launch this exact image (skips the native/inject route detection)")
	pf.String("qcow2", "", "provision inject route: golden qcow2 path (ALPINE_QCOW2 env, else error)")
	pf.String("platform-key", "", "provision inject route: ssh public key FILE for the platform-image first boot (OPS_SSH_PUBKEY env, else error)")
	pf.String("platform-user", "", "provision inject route: platform-image ssh user (PLATFORM_SSH_USER env, default ubuntu)")
	pf.String("to", "", "flip: target color (blue|green)")
	pf.Bool("force", false, "flip: cut over even when the target app is unhealthy")
	return cmd
}

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

// goldenImagePrefix is the golden-OS-image display-name prefix, derived
// from the project container so provision's lookup and image-import's
// naming can never drift apart.
func goldenImagePrefix(container string) string {
	return container + "-alpine"
}

// bgPair is the resolved pair context: OCI compartment + auth, and the
// container name the pair's display names derive from.
type bgPair struct {
	auth        cloud.CloudAuth
	container   string
	compName    string
	compOCID    string
	anchorConf  string // guest anchor.conf path (EnvDir-derived — matches OUR watcher)
	healthPath  string
	port        string
	envFile     string
	deployedSha string
}

// colorName renders the pair instance display name (<container>-<color>).
func (p bgPair) colorName(color string) string {
	return p.container + "-" + color
}

// resolveBGPair resolves the OCI compartment (flag override > cloud block
// > OCI_COMPARTMENT env, name or ocid) and the pair naming from the target.
// A missing compartment is a usage error naming both configuration sites.
func resolveBGPair(d Deps, target Target, compartmentOverride string) (bgPair, error) {
	auth := cloudAuthFor(d, target)
	if err := cloud.CheckProvider(auth); err != nil {
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
		auth:        auth,
		container:   p.Container,
		compName:    name,
		compOCID:    ocid,
		anchorConf:  path.Dir(p.EnvFile) + "/anchor.conf",
		healthPath:  p.HealthPath,
		port:        p.Port,
		envFile:     p.EnvFile,
		deployedSha: p.DeployedShaFile,
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
// check (wget with a curl fallback — golden images are minimal).
func instanceHealthy(ctx context.Context, d Deps, ip, container, port, healthPath string) bool {
	if ip == "" {
		return false
	}
	spec := transport.HostSpec{Host: "root@" + ip}
	probe := fmt.Sprintf("rc-service %s status >/dev/null 2>&1 && (busybox wget -q -O /dev/null http://127.0.0.1:%s%s || curl -s -m 8 -o /dev/null http://127.0.0.1:%s%s)",
		container, port, healthPath, port, healthPath)
	_, err := d.Runner.Run(ctx, spec, probe)
	return err == nil
}

func runBluegreenStatus(d Deps, c *cobra.Command) error {
	ctx := c.Context()
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "== %s blue/green pair (compartment %s) ==\n", pair.container, pair.compName)
	rpOut, err := cloud.RunOCI(ctx, cloud.ReservedIPListArgs(pair.auth, pair.compOCID))
	if err != nil {
		return err
	}
	rip, ok := cloud.ParseReservedIPRow(rpOut)
	if !ok {
		fmt.Fprintln(d.Stdout, "reserved IP : NONE (run 'kampodra bluegreen init')")
	} else {
		fmt.Fprintf(d.Stdout, "reserved IP : %s (%s)\n", rip.Address, rip.OCID)
		if !rip.Assigned {
			fmt.Fprintln(d.Stdout, "assigned to : UNASSIGNED (dormant — flip will attach)")
		} else {
			fmt.Fprintf(d.Stdout, "assigned to : %s\n", rip.PrivateIP)
		}
	}
	for _, color := range []string{"blue", "green"} {
		inst, err := bgInstance(ctx, pair, color)
		if err != nil {
			return err
		}
		if inst.OCID == "" {
			fmt.Fprintf(d.Stdout, "%s : not provisioned\n", pair.colorName(color))
			continue
		}
		verdict := "UNHEALTHY/unreachable"
		if instanceHealthy(ctx, d, inst.PublicIP, pair.container, pair.port, pair.healthPath) {
			verdict = "HEALTHY"
		}
		fmt.Fprintf(d.Stdout, "%s : %s  ip=%s  app=%s\n", pair.colorName(color), inst.OCID, orNone(inst.PublicIP), verdict)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func runBluegreenInit(d Deps, c *cobra.Command) error {
	ctx := c.Context()
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "")
	if err != nil {
		return err
	}
	rpOut, err := cloud.RunOCI(ctx, cloud.ReservedIPListArgs(pair.auth, pair.compOCID))
	if err != nil {
		return err
	}
	if rip, ok := cloud.ParseReservedIPRow(rpOut); ok {
		fmt.Fprintf(d.Stdout, "reserved IP already exists: %s — nothing to do\n", rip.Address)
		return nil
	}
	out, err := cloud.RunOCI(ctx, cloud.ReservedIPCreateArgs(pair.auth, pair.compOCID, "kampodra-active"))
	if err != nil {
		return err
	}
	var created struct {
		Data struct {
			IPAddress string `json:"ip-address"`
		} `json:"data"`
	}
	addr := strings.TrimSpace(out)
	if err := json.Unmarshal([]byte(out), &created); err == nil && created.Data.IPAddress != "" {
		addr = created.Data.IPAddress
	}
	fmt.Fprintf(d.Stdout, "created DORMANT reserved IP: %s (unassigned — no instance attached)\n", addr)
	fmt.Fprintln(d.Stdout, "Point DNS at this address when the pair goes active.")
	return nil
}

// runBluegreenProvision launches the second instance: native UEFI golden
// image when one exists, else platform-image launch + golden-disk
// injection (the only sanctioned A1 route — imports pin BIOS).
func runBluegreenProvision(d Deps, c *cobra.Command, color string) error {
	ctx := c.Context()
	if !bgColorRe.MatchString(color) {
		return fmt.Errorf("usage: kampodra bluegreen provision <blue|green>")
	}
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "")
	if err != nil {
		return err
	}
	if cur, err := bgInstance(ctx, pair, color); err != nil {
		return err
	} else if cur.OCID != "" {
		return fmt.Errorf("%s already RUNNING", pair.colorName(color))
	}
	tmpl, subnet, err := bgProvisionTemplate(ctx, pair, color)
	if err != nil {
		return err
	}
	image, mode, sshKeyFile, err := bgProvisionImage(ctx, d, c, pair, tmpl)
	if err != nil {
		return err
	}

	fmt.Fprintf(d.Stdout, "launching %s: ad=%s mode=%s\n", pair.colorName(color), tmpl.AD, mode)
	launchOut, err := cloud.RunOCI(ctx, cloud.InstanceLaunchArgs(pair.auth, pair.compOCID, tmpl.AD, subnet, image, pair.colorName(color), sshKeyFile))
	if err != nil {
		return fmt.Errorf("instance launch failed: %w", err)
	}
	iid := strings.TrimSpace(launchOut)
	if !cloud.IsOCID(iid, "instance") {
		if parsed, ok := cloud.ParseLaunchID(launchOut); ok {
			iid = parsed
		} else {
			return fmt.Errorf("unexpected launch response: %s", launchOut)
		}
	}
	fmt.Fprintf(d.Stdout, "LAUNCHED %s: %s\n", pair.colorName(color), iid)
	if mode == "native" {
		fmt.Fprintln(d.Stdout, "next: wait RUNNING -> kampodra vm-prepare -> kampodra deploy,")
		fmt.Fprintf(d.Stdout, "then 'kampodra bluegreen flip --to %s' (health-gated) once its app checks green.\n", color)
		return nil
	}
	fmt.Fprintln(d.Stdout, "waiting for RUNNING (inject mode)…")
	if err := bgWaitRunning(d, ctx, pair, iid); err != nil {
		return err
	}
	newRow, err := bgInstance(ctx, pair, color)
	if err != nil {
		return err
	}
	if newRow.PublicIP == "" {
		return fmt.Errorf("no ephemeral public ip on %s yet — re-check with 'kampodra bluegreen status' and run the injection manually", iid)
	}
	qcow2, platformUser, err := bgInjectParams(d, c)
	if err != nil {
		return err
	}
	if err := bgInjectAlpine(d, ctx, pair.container, color, newRow.PublicIP, qcow2, sshKeyFile, platformUser); err != nil {
		return err
	}
	return nil
}

// bgProvisionTemplate resolves the OTHER color as the fault-domain template
// (its AD + subnet) — the new instance launches beside its sibling.
func bgProvisionTemplate(ctx context.Context, pair bgPair, color string) (cloud.BGInstance, string, error) {
	other := "green"
	if color == "green" {
		other = "blue"
	}
	tmpl, err := bgInstance(ctx, pair, other)
	if err != nil {
		return cloud.BGInstance{}, "", err
	}
	if tmpl.OCID == "" {
		return cloud.BGInstance{}, "", fmt.Errorf("%s not RUNNING — need its AD/subnet as the pair template", pair.colorName(other))
	}
	tmplAtt, err := cloud.RunOCI(ctx, cloud.VnicAttachmentsArgs(pair.auth, pair.compOCID, tmpl.OCID))
	if err != nil {
		return cloud.BGInstance{}, "", err
	}
	_, subnet, ok := cloud.FirstVnic(tmplAtt)
	if !ok || !cloud.IsOCID(subnet, "subnet") {
		return cloud.BGInstance{}, "", fmt.Errorf("could not resolve %s's subnet id", pair.colorName(other))
	}
	return tmpl, subnet, nil
}

// bgProvisionImage selects the launch image + mode. Explicit --image-id
// wins; else the newest <container>-alpine* custom image with UEFI_64
// firmware (native); else the template's LIVE image-id + injection (the
// only sanctioned A1 route — imports pin BIOS, which A1 rejects at
// launch).
func bgProvisionImage(ctx context.Context, d Deps, c *cobra.Command, pair bgPair, tmpl cloud.BGInstance) (image, mode, sshKeyFile string, err error) {
	mode = "native"
	if image = flagString(c, "image-id"); image != "" {
		return image, mode, "", nil
	}
	imgOut, err := cloud.RunOCI(ctx, cloud.ImageListArgs(pair.auth, pair.compOCID))
	if err != nil {
		return "", "", "", err
	}
	if newest, ok := cloud.NewestPrefixedImage(imgOut, goldenImagePrefix(pair.container)); ok {
		fwOut, err := cloud.RunOCI(ctx, cloud.ImageGetArgs(pair.auth, newest))
		if err != nil {
			return "", "", "", err
		}
		if fw := cloud.ImageFirmware(fwOut); fw == "UEFI_64" {
			return newest, mode, "", nil
		} else {
			fmt.Fprintf(d.Stdout, "note: newest %s* custom image is firmware=%s — A1 rejects BIOS-pinned imports, using the platform-image + injection route\n",
				goldenImagePrefix(pair.container), orUnknown(fw))
		}
	}
	trec, err := cloud.RunOCI(ctx, cloud.InstanceGetArgs(pair.auth, tmpl.OCID))
	if err != nil {
		return "", "", "", err
	}
	rec, err := cloud.ParseInstanceRecord(trec)
	if err != nil || !cloud.IsOCID(rec.ImageID, "image") {
		return "", "", "", fmt.Errorf("template instance has no resolvable image-id — cannot launch or inject")
	}
	sshKeyFile = flagString(c, "platform-key")
	if sshKeyFile == "" {
		if v, ok := d.Env("OPS_SSH_PUBKEY"); ok && v != "" {
			sshKeyFile = v
		}
	}
	if sshKeyFile == "" {
		return "", "", "", fmt.Errorf("inject route needs the ops ssh public key: --platform-key <file> (or OPS_SSH_PUBKEY) — the platform-image first boot authorizes it")
	}
	if _, err := os.Stat(sshKeyFile); err != nil {
		return "", "", "", fmt.Errorf("ops ssh public key not found: %s", sshKeyFile)
	}
	return rec.ImageID, "inject", sshKeyFile, nil
}

// bgInjectParams resolves the inject-only inputs (golden qcow2 path +
// platform ssh user, flags over env over default).
func bgInjectParams(d Deps, c *cobra.Command) (qcow2, platformUser string, err error) {
	qcow2 = flagString(c, "qcow2")
	if qcow2 == "" {
		if v, ok := d.Env("ALPINE_QCOW2"); ok {
			qcow2 = v
		}
	}
	if qcow2 == "" || !fileExists(qcow2) {
		return "", "", fmt.Errorf("golden qcow2 not found: %s (--qcow2 <path>, or ALPINE_QCOW2)", orNone(qcow2))
	}
	platformUser = flagString(c, "platform-user")
	if platformUser == "" {
		if v, ok := d.Env("PLATFORM_SSH_USER"); ok && v != "" {
			platformUser = v
		} else {
			platformUser = "ubuntu"
		}
	}
	return qcow2, platformUser, nil
}

// bgWaitRunning polls an instance to RUNNING (the oci CLI's instance get
// has no --wait-for-state). Terminal-bad states fail closed; everything
// else keeps the loop going.
func bgWaitRunning(d Deps, ctx context.Context, pair bgPair, iid string) error {
	_ = d
	for i := 0; i < 120; i++ {
		out, err := cloud.RunOCI(ctx, cloud.InstanceGetArgs(pair.auth, iid))
		if err != nil {
			return err
		}
		rec, err := cloud.ParseInstanceRecord(out)
		if err != nil {
			return err
		}
		switch rec.State {
		case "RUNNING":
			return nil
		case "FAILED", "TERMINATED", "TERMINATING":
			return fmt.Errorf("instance reached %s — nothing to inject, check the console", rec.State)
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("instance never reached RUNNING — check the console")
}

// bgInjectAlpine streams the golden disk onto a RUNNING platform-image
// instance's boot volume (qcow2 -> raw locally, gzip | ssh gunzip | dd
// over the ssh-detected boot disk, conv=fsync), reboots, and verifies a
// real Alpine boot via /etc/alpine-release. The two boots share one IP
// with two different host keys: a throwaway known-hosts file keeps the
// operator's known_hosts untouched; the instance record keeps the
// platform image metadata.
func bgInjectAlpine(d Deps, ctx context.Context, container, color, ip, qcow2, sshKeyFile, platformUser string) error {
	kh, err := os.CreateTemp("", "kampodra-inject-kh.*")
	if err != nil {
		return err
	}
	khPath := kh.Name()
	kh.Close()
	defer os.Remove(khPath)
	plat := func() transport.HostSpec {
		return transport.HostSpec{Host: platformUser + "@" + ip, SSHKey: sshKeyFile, AcceptNewHostKey: true, KnownHostsFile: khPath}
	}
	fmt.Fprintf(d.Stdout, "inject: waiting for ssh (%s@%s, platform-image first boot)…\n", platformUser, ip)
	probed := false
	for i := 0; i < 60; i++ {
		if _, err := d.Runner.Run(ctx, plat(), "true"); err == nil {
			probed = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !probed {
		return fmt.Errorf("ssh never came up on %s (%s) — check the instance console connection", ip, platformUser)
	}
	raw, err := os.CreateTemp("", "kampodra-inject-raw.*")
	if err != nil {
		return err
	}
	rawPath := raw.Name()
	raw.Close()
	defer os.Remove(rawPath)
	if _, err := os.Stat(qcow2); err != nil {
		return fmt.Errorf("golden qcow2 not found: %s", qcow2)
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return fmt.Errorf("qemu-img not found in PATH — required for the qcow2 -> raw conversion")
	}
	fmt.Fprintf(d.Stdout, "inject: converting %s -> raw…\n", qcow2)
	conv := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "raw", qcow2, rawPath)
	if out, err := conv.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	gzPath := rawPath + ".gz"
	gzFile, err := os.Create(gzPath)
	if err != nil {
		return err
	}
	rawIn, err := os.Open(rawPath)
	if err != nil {
		gzFile.Close()
		return err
	}
	gzw := gzip.NewWriter(gzFile)
	if _, err := io.Copy(gzw, rawIn); err != nil {
		_ = gzw.Close()
		rawIn.Close()
		gzFile.Close()
		return fmt.Errorf("gzip staging failed: %w", err)
	}
	// The writer Close flushes — its error is real (a failed flush would
	// stream a corrupt archive), unlike the plain file closes below.
	if err := gzw.Close(); err != nil {
		rawIn.Close()
		gzFile.Close()
		return fmt.Errorf("gzip flush failed: %w", err)
	}
	rawIn.Close()
	gzFile.Close()
	defer os.Remove(gzPath)
	fmt.Fprintf(d.Stdout, "inject: streaming golden disk -> %s boot volume (gunzip | dd, conv=fsync)…\n", ip)
	gzIn, err := os.Open(gzPath)
	if err != nil {
		return err
	}
	defer gzIn.Close()
	dd := `set -eu; DISK="$(lsblk -no PKNAME "$(findmnt -n -o SOURCE /)")"; [ -n "$DISK" ] || exit 3; echo "[inject] writing /dev/$DISK"; gunzip -c | sudo dd of="/dev/$DISK" bs=4M conv=fsync status=progress`
	if _, err := d.Runner.RunWithStdin(ctx, plat(), dd, gzIn); err != nil {
		return fmt.Errorf("disk stream to %s failed — instance left UNBOOTABLE-ish (platform image partially overwritten): terminate it, do NOT flip to %s: %w", ip, color, err)
	}
	os.Remove(rawPath)
	// reboot -f kills the platform sshd WITHOUT closing TCP: bound the dead
	// session instead of hanging on it, and REQUIRE success — a wedged
	// (un-rebooted) guest keeps answering ssh and would false-verify below.
	fmt.Fprintf(d.Stdout, "inject: rebooting into the injected disk (reboot -f — the old fs is gone)…\n")
	rbCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := d.Runner.Run(rbCtx, plat(), "sudo reboot -f"); err != nil {
		fmt.Fprintf(d.Stdout, "inject: reboot call ended (%v) — continuing to the Alpine verify\n", err)
	}
	// The injected Alpine boots a NEW host key under the SAME ip — scrub
	// the phase file so the probes below accept-new fresh.
	os.WriteFile(khPath, nil, 0o600)
	fmt.Fprintf(d.Stdout, "inject: waiting for Alpine ssh (root@%s)…\n", ip)
	alpine := transport.HostSpec{Host: "root@" + ip, AcceptNewHostKey: true, KnownHostsFile: khPath}
	rel := ""
	for i := 0; i < 60; i++ {
		out, err := d.Runner.Run(ctx, alpine, "cat /etc/alpine-release")
		// Verify the RELEASE STRING, not non-empty output: an un-rebooted
		// platform guest's banner would satisfy a non-empty check.
		if err == nil && bgReleaseRe.MatchString(strings.TrimSpace(out)) {
			rel = strings.TrimSpace(out)
			break
		}
		time.Sleep(5 * time.Second)
	}
	if rel == "" {
		return fmt.Errorf("injection streamed but no ALPINE 3.x ssh on %s after reboot — check the serial console; terminate, do NOT flip to %s", ip, color)
	}
	fmt.Fprintf(d.Stdout, "INJECTED %s-%s: Alpine %s boots on %s (instance image metadata stays the platform image)\n", container, color, rel, ip)
	fmt.Fprintf(d.Stdout, "next: kampodra vm-prepare --host root@%s -> kampodra deploy --host root@%s\n", ip, ip)
	fmt.Fprintf(d.Stdout, "then 'kampodra bluegreen flip --to %s' (health-gated) once its app checks green.\n", color)
	return nil
}

var bgReleaseRe = regexp.MustCompile(`^3\.[0-9]+\.[0-9]+`)

// runBluegreenFlip performs the ACME-first health-gated cutover.
func runBluegreenFlip(d Deps, c *cobra.Command) error {
	ctx := c.Context()
	to := flagString(c, "to")
	force := flagBool(c, "force")
	if !bgColorRe.MatchString(to) {
		fmt.Fprintln(d.Stdout, "usage: kampodra bluegreen flip --to <blue|green> [--force]")
		fmt.Fprintln(d.Stdout, "(rollback = unassign to dormant: 'kampodra bluegreen rollback')")
		return &exitError{code: 2}
	}
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "")
	if err != nil {
		return err
	}
	rpOut, err := cloud.RunOCI(ctx, cloud.ReservedIPListArgs(pair.auth, pair.compOCID))
	if err != nil {
		return err
	}
	rip, ok := cloud.ParseReservedIPRow(rpOut)
	if !ok {
		return fmt.Errorf("no reserved IP (run 'kampodra bluegreen init' first)")
	}
	ft, err := bgFlipResolveTarget(ctx, d, pair, to, force)
	if err != nil {
		return err
	}
	trow, tpip := ft.trow, ft.tpip

	ob := "green"
	if to == "green" {
		ob = "blue"
	}
	rhost := strings.ReplaceAll(rip.Address, ".", "-") + ".sslip.io"
	taddr, err := bgFlipAnchor(ctx, d, pair, to, trow, tpip)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "flip[2/4]: guest answers on %s — reserved %s -> %s (anchor %s)\n", taddr, rip.Address, pair.colorName(to), tpip)
	if _, err := cloud.RunOCI(ctx, cloud.PublicIPUpdateArgs(pair.auth, rip.OCID, tpip, "ASSIGNED")); err != nil {
		bgCleanupTargetAnchor(ctx, d, trow.PublicIP, pair.anchorConf, taddr)
		return fmt.Errorf("OCI flip call failed — guest conf removed; run 'kampodra bluegreen status': %w", err)
	}
	fmt.Fprintf(d.Stdout, "flip[3/4]: reserved IP ASSIGNED — registering %s on %s's kamal-proxy (ACME HTTP-01 through the reserved ip)…\n", rhost, pair.colorName(to))
	acme := fmt.Sprintf("podman exec kamal-proxy kamal-proxy deploy %s --host=%s --target=%s:%s --tls --health-check-path=%s",
		pair.container, rhost, pair.container, pair.port, pair.healthPath)
	if _, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + trow.PublicIP}, acme); err != nil {
		bgFlipFailureRollback(ctx, d, pair, to, ob, rip.OCID, trow.PublicIP, taddr)
		return &exitError{code: 1}
	}
	if !bgPollHTTPS(ctx, d, rip.Address, rhost, pair.healthPath, flipACMETries) {
		fmt.Fprintf(d.Stderr, "https://%s/ never answered with a VALID cert through %s\n", rhost, rip.Address)
		bgFlipFailureRollback(ctx, d, pair, to, ob, rip.OCID, trow.PublicIP, taddr)
		return &exitError{code: 1}
	}
	served := ""
	if stamp := bgReadDeployedSha(ctx, d, trow.PublicIP, pair.deployedSha); stamp != "" {
		if body, ok := d.Prober.ResolveStatus(ctx, rip.Address, rhost, pair.healthPath); ok {
			served = strings.TrimSpace(body)
			if !probe.BodyServesSha(body, stamp) {
				fmt.Fprintf(d.Stderr, "served body through %s does not carry the deployed sha %s\n", rip.Address, stamp)
				bgFlipFailureRollback(ctx, d, pair, to, ob, rip.OCID, trow.PublicIP, taddr)
				return &exitError{code: 1}
			}
		} else {
			fmt.Fprintf(d.Stderr, "could not re-fetch %s through %s after the cert verified\n", pair.healthPath, rip.Address)
			bgFlipFailureRollback(ctx, d, pair, to, ob, rip.OCID, trow.PublicIP, taddr)
			return &exitError{code: 1}
		}
	}
	fmt.Fprintf(d.Stdout, "flip[4/4]: cert for %s VALID + serving through %s\n", rhost, rip.Address)
	fmt.Fprintf(d.Stdout, "FLIPPED: https://%s/ (https://%s/) now serves from %s\n", rip.Address, rhost, pair.colorName(to))
	fmt.Fprintf(d.Stdout, "served %s: %s\n", pair.healthPath, orAngle(served))
	return nil
}

// flipTarget is the validated cutover target: instance row, primary VNIC,
// and the anchor secondary private-ip OCID.
type flipTarget struct {
	trow  cloud.BGInstance
	tvnic string
	tpip  string
}

// bgFlipResolveTarget validates the cutover target: RUNNING instance,
// primary VNIC, anchor secondary (reused or minted — the target is the only
// legitimate creation site), and the health gate (or --force).
func bgFlipResolveTarget(ctx context.Context, d Deps, pair bgPair, to string, force bool) (flipTarget, error) {
	var ft flipTarget
	trow, err := bgInstance(ctx, pair, to)
	if err != nil {
		return ft, err
	}
	if trow.OCID == "" {
		return ft, fmt.Errorf("%s is not RUNNING — nothing to flip to", pair.colorName(to))
	}
	tatt, err := cloud.RunOCI(ctx, cloud.VnicAttachmentsArgs(pair.auth, pair.compOCID, trow.OCID))
	if err != nil {
		return ft, err
	}
	tvnic, _, ok := cloud.FirstVnic(tatt)
	if !ok || tvnic == "" {
		return ft, fmt.Errorf("no primary VNIC on %s", pair.colorName(to))
	}
	var tpip string
	if aout, err := cloud.RunOCI(ctx, cloud.PrivateIPListArgs(pair.auth, tvnic)); err != nil {
		return ft, err
	} else if tpip = cloud.SecondaryPrivateIP(aout); tpip == "" {
		cout, err := cloud.RunOCI(ctx, cloud.PrivateIPCreateArgs(pair.auth, tvnic, "kampodra-reserved-anchor"))
		if err != nil {
			return ft, fmt.Errorf("could not create the reserved-anchor secondary private ip on %s: %w", pair.colorName(to), err)
		}
		tpip = cloud.PrivateIPID(cout)
	}
	if !cloud.IsOCID(tpip, "privateip") {
		return ft, fmt.Errorf("could not resolve/create the reserved-anchor secondary private ip on %s", pair.colorName(to))
	}
	if instanceHealthy(ctx, d, trow.PublicIP, pair.container, pair.port, pair.healthPath) {
		fmt.Fprintf(d.Stdout, "target health: %s app HEALTHY on its own IP\n", pair.colorName(to))
	} else if !force {
		return ft, fmt.Errorf("%s app UNHEALTHY — refusing flip (override: --force)", pair.colorName(to))
	} else {
		fmt.Fprintf(d.Stdout, "target health: UNHEALTHY — flipping anyway (--force)\n")
	}
	return flipTarget{trow: trow, tvnic: tvnic, tpip: tpip}, nil
}

// bgFlipAnchor runs the flip's guest half: resolve the anchor address
// (subnet CIDR gives the prefix), write the conf, and wait for the
// watcher to configure it. NOTHING is mutated before the conf write, and
// a failed wait cleans the conf up — callers only assign after this
// returns.
func bgFlipAnchor(ctx context.Context, d Deps, pair bgPair, to string, trow cloud.BGInstance, tpip string) (string, error) {
	fmt.Fprintf(d.Stdout, "flip[1/4]: anchor conf -> %s guest (%s), watcher configures the address\n", pair.colorName(to), trow.PublicIP)
	tsubOut, err := cloud.RunOCI(ctx, cloud.VnicAttachmentsArgs(pair.auth, pair.compOCID, trow.OCID))
	if err != nil {
		return "", err
	}
	_, tsubnet, ok := cloud.FirstVnic(tsubOut)
	if !ok || !cloud.IsOCID(tsubnet, "subnet") {
		return "", fmt.Errorf("could not resolve %s's subnet id", pair.colorName(to))
	}
	cidrOut, err := cloud.RunOCI(ctx, cloud.SubnetGetArgs(pair.auth, tsubnet))
	if err != nil {
		return "", err
	}
	tcidr := cloud.SubnetCIDR(cidrOut)
	if !bgCIDRRe.MatchString(tcidr) {
		return "", fmt.Errorf("could not resolve %s's subnet cidr (got: %s)", pair.colorName(to), orNone(tcidr))
	}
	pipOut, err := cloud.RunOCI(ctx, cloud.PrivateIPGetArgs(pair.auth, tpip))
	if err != nil {
		return "", err
	}
	tanchor := cloud.PrivateIPAddress(pipOut)
	if !bgIPRe.MatchString(tanchor) {
		return "", fmt.Errorf("could not resolve the anchor private address on %s", pair.colorName(to))
	}
	taddr := tanchor + "/" + strings.SplitN(tcidr, "/", 2)[1]
	if err := bgWriteAnchorConf(ctx, d, trow.PublicIP, pair.anchorConf, taddr); err != nil {
		return "", fmt.Errorf("anchor.conf write failed on %s (%s) — nothing mutated, flip aborted: %w", pair.colorName(to), trow.PublicIP, err)
	}
	if !bgPollAddr(ctx, d, trow.PublicIP, taddr, flipAddrTries) {
		bgCleanupTargetAnchor(ctx, d, trow.PublicIP, pair.anchorConf, taddr)
		return "", fmt.Errorf("guest watcher never configured %s on %s — is the kampodra-anchor service running? (conf removed, NOTHING mutated)", taddr, pair.colorName(to))
	}
	return taddr, nil
}
func bgWriteAnchorConf(ctx context.Context, d Deps, ip, confPath, addr string) error {
	remote := fmt.Sprintf("umask 077; mkdir -p %s && printf 'ANCHOR_ADDR=%%s\\nANCHOR_IFACE=\\n' '%s' > %s && echo ANCHOR_CONF_WRITTEN",
		bgQuote(path.Dir(confPath)), addr, confPath)
	_, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip}, remote)
	return err
}

// bgPollAddr waits for the guest watcher to configure the anchor address.
func bgPollAddr(ctx context.Context, d Deps, ip, addr string, tries int) bool {
	for i := 0; i < tries; i++ {
		if _, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip},
			fmt.Sprintf("ip -4 addr show | grep -qF -- '%s'", addr)); err == nil {
			return true
		}
		time.Sleep(flipPollSleep)
	}
	return false
}

// bgPollHTTPS waits for a cert-valid 2xx through the reserved IP.
func bgPollHTTPS(ctx context.Context, d Deps, raddr, rhost, healthPath string, tries int) bool {
	for i := 0; i < tries; i++ {
		if _, ok := d.Prober.ResolveStatus(ctx, raddr, rhost, healthPath); ok {
			return true
		}
		time.Sleep(flipPollSleep)
	}
	return false
}

// bgReadDeployedSha reads the VM's deployed-sha stamp (empty = skip the
// served-sha comparison, like a stamp-less first deploy).
func bgReadDeployedSha(ctx context.Context, d Deps, ip, stampPath string) string {
	out, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip}, "cat "+stampPath+" 2>/dev/null || true")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// bgCleanupTargetAnchor removes the conf then deletes the address (retry:
// the add-only watcher may have sourced the conf just before removal and
// re-added it once).
func bgCleanupTargetAnchor(ctx context.Context, d Deps, ip, confPath, addr string) {
	// Best-effort by design (cleanup after a failed flip): errors are
	// swallowed here — the caller already reports the flip failure.
	tolerant := func(remote string) {
		_, _ = d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip}, remote)
	}
	tolerant("rm -f " + confPath + " >/dev/null 2>&1 || true")
	for i := 0; i < 3; i++ {
		tolerant(
			fmt.Sprintf(`iface=$(ip -4 route show default 2>/dev/null | awk '{print $5; exit}'); [ -n "$iface" ] && ip addr del '%s' dev "$iface" 2>/dev/null; true`, addr))
		if _, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip},
			fmt.Sprintf("ip -4 addr show | grep -qF -- '%s'", addr)); err != nil {
			return
		}
		time.Sleep(flipPollSleep * 2)
	}
	fmt.Fprintf(d.Stderr, "warning: guest anchor address %s still present on %s after cleanup (watcher resurrection?) — inert once the reserved ip moves, verify before reuse\n", addr, ip)
}

// bgFlipFailureRollback is the post-assign failure path: reassign to the
// other color's EXISTING anchor (lookup-ONLY — never mint artifacts on the
// holder we fail away from), else unassign to dormant; then clean the
// failed target's guest state. A failed rollback is FATAL with the OCID
// for console recovery.
func bgFlipFailureRollback(ctx context.Context, d Deps, pair bgPair, to, ob, rocid, tpub, taddr string) {
	fmt.Fprintln(d.Stderr, "post-assign failure — auto-rollback")
	recovered := false
	if brow, err := bgInstance(ctx, pair, ob); err == nil && brow.OCID != "" {
		if batt, err := cloud.RunOCI(ctx, cloud.VnicAttachmentsArgs(pair.auth, pair.compOCID, brow.OCID)); err == nil {
			if bnic, _, ok := cloud.FirstVnic(batt); ok && bnic != "" {
				if bpip, err := bgAnchorIP(ctx, pair, bnic); err == nil && cloud.IsOCID(bpip, "privateip") {
					if _, err := cloud.RunOCI(ctx, cloud.PublicIPUpdateArgs(pair.auth, rocid, bpip, "")); err == nil {
						fmt.Fprintf(d.Stderr, "rolled back to %s (existing anchor %s)\n", pair.colorName(ob), bpip)
						recovered = true
					}
				}
			}
		}
	}
	if !recovered {
		if _, err := cloud.RunOCI(ctx, cloud.PublicIPUpdateArgs(pair.auth, rocid, "", "AVAILABLE")); err == nil {
			fmt.Fprintf(d.Stderr, "reserved ip UNASSIGNED (dormant) — no existing anchor on %s\n", pair.colorName(ob))
		} else {
			bgCleanupTargetAnchor(ctx, d, tpub, pair.anchorConf, taddr)
			fmt.Fprintf(d.Stderr, "AUTO-ROLLBACK FAILED — reserved ip state unknown; flip manually via console: %s\n", rocid)
			return
		}
	}
	bgCleanupTargetAnchor(ctx, d, tpub, pair.anchorConf, taddr)
	fmt.Fprintf(d.Stderr, "%s cleaned (anchor.conf removed, anchor address deleted) — investigate before retrying\n", pair.colorName(to))
}

// runBluegreenRollback unassigns to DORMANT: holder guest cleanup first
// (anchor.conf removal + address delete — the flip tool's explicit job;
// the watcher is add-only), then the OCI unassign. To move traffic to the
// other color of a real pair, flip --to <other> instead.
func runBluegreenRollback(d Deps, c *cobra.Command) error {
	ctx := c.Context()
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "")
	if err != nil {
		return err
	}
	rpOut, err := cloud.RunOCI(ctx, cloud.ReservedIPListArgs(pair.auth, pair.compOCID))
	if err != nil {
		return err
	}
	rip, ok := cloud.ParseReservedIPRow(rpOut)
	if !ok {
		return fmt.Errorf("no reserved IP")
	}
	if !rip.Assigned {
		fmt.Fprintf(d.Stdout, "reserved %s already UNASSIGNED (dormant) — nothing to roll back\n", rip.Address)
		return nil
	}
	// Resolve the holder color by matching its anchor (LOOKUP-ONLY).
	holderColor, holderIP := "", ""
	for _, color := range []string{"blue", "green"} {
		crow, err := bgInstance(ctx, pair, color)
		if err != nil || crow.OCID == "" {
			continue
		}
		catt, err := cloud.RunOCI(ctx, cloud.VnicAttachmentsArgs(pair.auth, pair.compOCID, crow.OCID))
		if err != nil {
			continue
		}
		cnic, _, ok := cloud.FirstVnic(catt)
		if !ok || cnic == "" {
			continue
		}
		capip, err := bgAnchorIP(ctx, pair, cnic)
		if err != nil {
			continue
		}
		if capip == rip.PrivateIP {
			holderColor, holderIP = color, crow.PublicIP
			break
		}
	}
	if holderColor == "" {
		fmt.Fprintf(d.Stdout, "warning: reserved %s held by an anchor of neither RUNNING color (terminated instance?) — unassigning without guest cleanup\n", rip.Address)
	} else {
		fmt.Fprintf(d.Stdout, "current holder: %s (%s) — cleaning the guest anchor, then unassigning\n", pair.colorName(holderColor), holderIP)
		confAddr := bgReadAnchorAddr(ctx, d, holderIP, pair.anchorConf)
		bgCleanupTargetAnchor(ctx, d, holderIP, pair.anchorConf, confAddr)
		fmt.Fprintf(d.Stdout, "%s guest cleaned (anchor.conf removed%s)\n", pair.colorName(holderColor), orAddrDeleted(confAddr))
	}
	fmt.Fprintf(d.Stdout, "unassigning reserved %s (-> dormant)…\n", rip.Address)
	if _, err := cloud.RunOCI(ctx, cloud.PublicIPUpdateArgs(pair.auth, rip.OCID, "", "AVAILABLE")); err != nil {
		return fmt.Errorf("unassign failed — check 'kampodra bluegreen status' and the console: %w", err)
	}
	fmt.Fprintf(d.Stdout, "ROLLED BACK: reserved %s UNASSIGNED (dormant)\n", rip.Address)
	return nil
}

// bgReadAnchorAddr reads the guest's current ANCHOR_ADDR (with prefix) or
// "". Quotes stripped — the conf is shell-sourceable by the watcher.
func bgReadAnchorAddr(ctx context.Context, d Deps, ip, confPath string) string {
	out, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip},
		fmt.Sprintf(`grep -h "^ANCHOR_ADDR=" %s 2>/dev/null | head -n 1`, confPath))
	if err != nil {
		return ""
	}
	addr := strings.TrimSpace(out)
	addr = strings.TrimPrefix(addr, "ANCHOR_ADDR=")
	addr = strings.ReplaceAll(addr, `"`, "")
	addr = strings.ReplaceAll(addr, `'`, "")
	if !strings.Contains(addr, "/") {
		return ""
	}
	return addr
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func orAddrDeleted(addr string) string {
	if addr == "" {
		return ""
	}
	return ", address " + addr + " deleted"
}

func orAngle(s string) string {
	if s == "" {
		return "<no body>"
	}
	return s
}

// bgQuote single-quotes one remote path fragment (values reaching the
// guest are OCI-API derived and regex-gated, but the conf path comes from
// resolved config — quote it rather than trusting it).
func bgQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
