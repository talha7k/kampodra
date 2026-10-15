// bluegreen init + provision: create the reserved IP when absent, launch
// the second instance (native image or golden-qcow2 inject route), and
// inject the guest stack (anchor conf + kamal-proxy) over the resolved
// ssh key.

package command

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

func runBluegreenInit(d Deps, c *cobra.Command) error {
	ctx := c.Context()
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "", surfaceBluegreen)
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
	guestOS := flagString(c, "os")
	if err := checkGuestOS(guestOS); err != nil {
		return err
	}
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "", surfaceBluegreen)
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
	image, mode, sshKeyFile, err := bgProvisionImage(ctx, d, c, pair, tmpl, guestOS)
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
	if err := bgInjectGuest(d, ctx, pair.container, color, newRow.PublicIP, qcow2, platformUser, pair.sshKey, guestOS); err != nil {
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
// wins; else the newest <os-prefix>* custom image with UEFI_64 firmware
// (native; <os-prefix> = goldenImagePrefix(container, os) — alpine or
// ubuntu-24.04); else the template's LIVE image-id + injection (the
// only sanctioned A1 route — imports pin BIOS, which A1 rejects at
// launch).
func bgProvisionImage(ctx context.Context, d Deps, c *cobra.Command, pair bgPair, tmpl cloud.BGInstance, guestOS string) (image, mode, sshKeyFile string, err error) {
	mode = "native"
	if image = flagString(c, "image-id"); image != "" {
		return image, mode, "", nil
	}
	prefix, err := goldenImagePrefix(pair.container, guestOS)
	if err != nil {
		return "", "", "", err
	}
	imgOut, err := cloud.RunOCI(ctx, cloud.ImageListArgs(pair.auth, pair.compOCID))
	if err != nil {
		return "", "", "", err
	}
	if newest, ok := cloud.NewestPrefixedImage(imgOut, prefix); ok {
		fwOut, err := cloud.RunOCI(ctx, cloud.ImageGetArgs(pair.auth, newest))
		if err != nil {
			return "", "", "", err
		}
		if fw := cloud.ImageFirmware(fwOut); fw == "UEFI_64" {
			return newest, mode, "", nil
		} else {
			fmt.Fprintf(d.Stdout, "note: newest %s* custom image is firmware=%s — A1 rejects BIOS-pinned imports, using the platform-image + injection route\n",
				prefix, orUnknown(fw))
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

// stageGoldenDisk stages the golden qcow2 as a gzipped raw image
// (qemu-img convert -> gzip), returning the archive path. It owns the
// raw scratch file (removed on return); the CALLER owns the .gz — it is
// consumed by the disk stream and removed after the verify. Missing
// input and a missing qemu-img fail closed.
func stageGoldenDisk(d Deps, ctx context.Context, qcow2 string) (string, error) {
	if _, err := os.Stat(qcow2); err != nil {
		return "", fmt.Errorf("golden qcow2 not found: %s", qcow2)
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return "", fmt.Errorf("qemu-img not found in PATH — required for the qcow2 -> raw conversion")
	}
	raw, err := os.CreateTemp("", "kampodra-inject-raw.*")
	if err != nil {
		return "", err
	}
	rawPath := raw.Name()
	raw.Close()
	defer os.Remove(rawPath)
	fmt.Fprintf(d.Stdout, "inject: converting %s -> raw…\n", qcow2)
	conv := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "raw", qcow2, rawPath)
	if out, err := conv.CombinedOutput(); err != nil {
		return "", fmt.Errorf("qemu-img convert failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	gzPath := rawPath + ".gz"
	gzFile, err := os.Create(gzPath)
	if err != nil {
		return "", err
	}
	rawIn, err := os.Open(rawPath)
	if err != nil {
		gzFile.Close()
		return "", err
	}
	gzw := gzip.NewWriter(gzFile)
	if _, err := io.Copy(gzw, rawIn); err != nil {
		_ = gzw.Close()
		rawIn.Close()
		gzFile.Close()
		return "", fmt.Errorf("gzip staging failed: %w", err)
	}
	// The writer Close flushes — its error is real (a failed flush would
	// stream a corrupt archive), unlike the plain file closes below.
	if err := gzw.Close(); err != nil {
		rawIn.Close()
		gzFile.Close()
		return "", fmt.Errorf("gzip flush failed: %w", err)
	}
	rawIn.Close()
	gzFile.Close()
	return gzPath, nil
}

// bgInjectAlpine streams the golden disk onto a RUNNING platform-image
// instance's boot volume (qcow2 -> raw locally, gzip | ssh gunzip | dd
// over the ssh-detected boot disk, conv=fsync), reboots, and verifies the
// REAL golden guest booted via the per-OS release check (osReleaseCheck:
// /etc/alpine-release 3.x for alpine, /etc/os-release ID=ubuntu for
// ubuntu). The two boots share one IP with two different host keys: a
// throwaway known-hosts file keeps the operator's known_hosts untouched;
// the instance record keeps the platform image metadata.
func bgInjectGuest(d Deps, ctx context.Context, container, color, ip, qcow2, platformUser, targetKey, guestOS string) error {
	osName := osDisplayName(guestOS)
	verifyCmd, verifyPattern, err := osReleaseCheck(guestOS)
	if err != nil {
		return err
	}
	verifyRe := regexp.MustCompile(verifyPattern)
	kh, err := os.CreateTemp("", "kampodra-inject-kh.*")
	if err != nil {
		return err
	}
	khPath := kh.Name()
	kh.Close()
	defer os.Remove(khPath)
	// The platform ssh identity is the RESOLVED private key (the pubkey file
	// --platform-key authorizes it via --ssh-authorized-keys-file — it is
	// NOT itself a usable identity; `ssh -i ops.pub` only limps through on
	// agent fallback, 2026-10-09 live fire).
	plat := func() transport.HostSpec {
		return transport.HostSpec{Host: platformUser + "@" + ip, SSHKey: targetKey, AcceptNewHostKey: true, KnownHostsFile: khPath}
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
	gzPath, err := stageGoldenDisk(d, ctx, qcow2)
	if err != nil {
		return err
	}
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
	// reboot -f kills the platform sshd WITHOUT closing TCP: bound the dead
	// session instead of hanging on it, and REQUIRE success — a wedged
	// (un-rebooted) guest keeps answering ssh and would false-verify below.
	fmt.Fprintf(d.Stdout, "inject: rebooting into the injected disk (reboot -f — the old fs is gone)…\n")
	rbCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := d.Runner.Run(rbCtx, plat(), "sudo reboot -f"); err != nil {
		fmt.Fprintf(d.Stdout, "inject: reboot call ended (%v) — continuing to the %s verify\n", err, osName)
	}
	// The injected golden guest boots a NEW host key under the SAME ip —
	// scrub the phase file so the probes below accept-new fresh. The ssh
	// identity is the RESOLVED target key (the golden image bakes the ops
	// pubkey) — never the platform --platform-key file (a .pub path is not
	// a usable identity; relying on agent fallback breaks headless runs).
	os.WriteFile(khPath, nil, 0o600)
	fmt.Fprintf(d.Stdout, "inject: waiting for %s ssh (root@%s)…\n", osName, ip)
	guest := transport.HostSpec{Host: "root@" + ip, SSHKey: targetKey, AcceptNewHostKey: true, KnownHostsFile: khPath}
	rel := ""
	for i := 0; i < 60; i++ {
		out, err := d.Runner.Run(ctx, guest, verifyCmd)
		// Verify the RELEASE STRING, not non-empty output: an un-rebooted
		// platform guest's banner would satisfy a non-empty check.
		if err == nil && verifyRe.MatchString(strings.TrimSpace(out)) {
			rel = strings.TrimSpace(out)
			break
		}
		time.Sleep(5 * time.Second)
	}
	if rel == "" {
		return fmt.Errorf("injection streamed but no verified %s ssh on %s after reboot — check the serial console; terminate, do NOT flip to %s", osName, ip, color)
	}
	fmt.Fprintf(d.Stdout, "INJECTED %s-%s: %s %s boots on %s (instance image metadata stays the platform image)\n", container, color, osName, rel, ip)
	fmt.Fprintf(d.Stdout, "next: kampodra vm-prepare --host root@%s -> kampodra deploy --host root@%s\n", ip, ip)
	fmt.Fprintf(d.Stdout, "then 'kampodra bluegreen flip --to %s' (health-gated) once its app checks green.\n", color)
	return nil
}

// bgReleaseRe is the alpine injected-boot proof: a real 3.x release
// string (cat /etc/alpine-release).
var bgReleaseRe = regexp.MustCompile(`^3\.[0-9]+\.[0-9]+`)

// osDisplayName is the human name for a validated guest os (progress and
// error strings): alpine -> Alpine, ubuntu -> Ubuntu.
func osDisplayName(guestOS string) string {
	if guestOS == "ubuntu" {
		return "Ubuntu"
	}
	return "Alpine"
}

// osReleaseCheck returns the per-OS injected-boot verify: the remote
// command reading the guest's OS identity, plus the pattern proving the
// REAL golden guest booted (not the un-rebooted platform image). The
// verify code path stays single — only cmd+pattern vary.
func osReleaseCheck(guestOS string) (remoteCmd, pattern string, err error) {
	if err := checkGuestOS(guestOS); err != nil {
		return "", "", err
	}
	if guestOS == "ubuntu" {
		return "cat /etc/os-release", `(?m)^ID=ubuntu$`, nil
	}
	return "cat /etc/alpine-release", bgReleaseRe.String(), nil
}
