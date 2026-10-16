// bluegreen flip + rollback: the ACME-first cutover — health gate the
// target, anchor.conf + watcher pickup, assign the reserved IP, ACME,
// verify through the reserved IP — with auto-rollback to the other
// color's existing anchor (or dormant) on any post-assign failure.

package command

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

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
	pair, err := resolveBGPair(d, target, "", surfaceBluegreen)
	if err != nil {
		return err
	}
	rip, ok, err := cloud.BGReservedIPList(ctx, pair.auth, pair.compOCID)
	if err != nil {
		return err
	}
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
	// The holder (current traffic holder) is the flip's rollback target:
	// when it is ALREADY dead, the cutover leaves NO healthy fallback —
	// a rollback would restore a dead instance. Warn-only (the operator
	// may be rescuing a dead pair on purpose); the probe mirrors the
	// target's own health gate below.
	if brow, err := bgInstance(ctx, pair, ob); err == nil && brow.OCID != "" &&
		!instanceHealthy(ctx, d, brow.PublicIP, pair.sshKey, pair.container, pair.port, pair.healthPath) {
		fmt.Fprintf(d.Stderr, "WARN: %s is UNHEALTHY — after this flip there is NO healthy fallback (rollback would restore a dead instance)\n", pair.colorName(ob))
	}
	drain, _ := c.Flags().GetInt("drain-timeout")
	rhost := strings.ReplaceAll(rip.Address, ".", "-") + ".sslip.io"
	// The anchor-conf stage is oci-leg only: on oci the guest watcher
	// configures the secondary private IP from anchor.conf; on gcp the
	// reserved address attaches at the NIC (access-config) and the guest
	// sees it via the metadata server — the watcher's metadata poll is a
	// vmbootstrap follow-up, nothing to write here (live-fire 2026-10-16:
	// the stage's subnet/cidr ocid gates refused every gcp flip).
	taddr := ""
	if pair.auth.Provider != "gcp" {
		taddr, err = bgFlipAnchor(ctx, d, pair, to, trow, tpip)
		if err != nil {
			return err
		}
	} else {
		fmt.Fprintln(d.Stdout, "flip[1/4]: gcp — reserved address attaches at the NIC (no guest conf stage)")
	}
	fmt.Fprintf(d.Stdout, "flip[2/4]: guest answers on %s — reserved %s -> %s (anchor %s)\n", taddr, rip.Address, pair.colorName(to), tpip)
	// The bounded drain grace (opt-in --drain-timeout): the cutover is an
	// instantaneous NAT move — established connections through the
	// reserved IP die at the assign without the holder's app ever being
	// signaled. Operators who want a grace window get one, observably,
	// before the cut.
	if drain > 0 {
		fmt.Fprintf(d.Stdout, "flip[2/4]: drain grace %ds before the cutover — in-flight requests through the reserved IP will be cut\n", drain)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(drain) * time.Second):
		}
	}
	if err := cloud.BGReservedIPAssign(ctx, pair.auth, rip.OCID, trow.OCID, tpip, "ASSIGNED"); err != nil {
		bgCleanupTargetAnchor(ctx, d, trow.PublicIP, pair.sshKey, pair.anchorConf, taddr)
		return fmt.Errorf("flip call failed — guest conf removed; run 'kampodra bluegreen status': %w", err)
	}
	fmt.Fprintf(d.Stdout, "flip[3/4]: reserved IP ASSIGNED — registering %s on %s's kamal-proxy (ACME HTTP-01 through the reserved ip)…\n", rhost, pair.colorName(to))
	acme := fmt.Sprintf("podman exec kamal-proxy kamal-proxy deploy %s --host=%s --target=%s:%s --tls --health-check-path=%s",
		pair.container, rhost, pair.container, pair.port, pair.healthPath)
	if _, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + trow.PublicIP, SSHKey: pair.sshKey}, acme); err != nil {
		bgFlipFailureRollback(ctx, d, pair, to, ob, rip.OCID, trow.PublicIP, taddr)
		return &exitError{code: 1}
	}
	if !bgPollHTTPS(ctx, d, rip.Address, rhost, pair.healthPath, flipACMETries) {
		fmt.Fprintf(d.Stderr, "https://%s/ never answered with a VALID cert through %s\n", rhost, rip.Address)
		bgFlipFailureRollback(ctx, d, pair, to, ob, rip.OCID, trow.PublicIP, taddr)
		return &exitError{code: 1}
	}
	served := ""
	if stamp := bgReadDeployedSha(ctx, d, trow.PublicIP, pair.sshKey, pair.deployedSha); stamp != "" {
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
	// The pre-flip host is DE-REGISTERED by this flip — every proxyHost
	// referencing it (the profile's project block SHADOWS kampodra.json in
	// the ladder; update both) now probes a dead edge and fails deploys
	// with a served-sha mismatch (live-fire 2026-10-16 incident).
	fmt.Fprintf(d.Stdout, "post-flip: update proxyHost to %s (or point DNS at %s) in the profile's project block AND kampodra.json — the pre-flip host no longer serves\n", rhost, rip.Address)
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
	tvnic, _, ok, err := cloud.BGPrimaryVnic(ctx, pair.auth, pair.compOCID, trow.OCID)
	if err != nil {
		return ft, err
	}
	if !ok || tvnic == "" {
		return ft, fmt.Errorf("no primary VNIC on %s", pair.colorName(to))
	}
	tpip, err := cloud.BGAnchorIP(ctx, pair.auth, tvnic)
	if err != nil {
		return ft, err
	}
	if tpip == "" {
		tpip, err = cloud.BGPrivateIPCreate(ctx, pair.auth, tvnic, "kampodra-reserved-anchor")
		if err != nil {
			return ft, fmt.Errorf("could not create the reserved-anchor secondary private ip on %s: %w", pair.colorName(to), err)
		}
	}
	// The anchor handle is ocid-shaped ONLY on oci (a private-ip ocid);
	// gcp's anchor is the reserved external IP itself — its name while
	// dormant, its address while IN_USE (live-fire 2026-10-16: this gate
	// refused every gcp flip).
	if !cloud.IsOCID(tpip, "privateip") && pair.auth.Provider != "gcp" {
		return ft, fmt.Errorf("could not resolve/create the reserved-anchor secondary private ip on %s", pair.colorName(to))
	}
	if instanceHealthy(ctx, d, trow.PublicIP, pair.sshKey, pair.container, pair.port, pair.healthPath) {
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
	_, tsubnet, ok, err := cloud.BGPrimaryVnic(ctx, pair.auth, pair.compOCID, trow.OCID)
	if err != nil {
		return "", err
	}
	if !ok || !cloud.IsOCID(tsubnet, "subnet") {
		return "", fmt.Errorf("could not resolve %s's subnet id", pair.colorName(to))
	}
	tcidr, err := cloud.BGSubnetCIDR(ctx, pair.auth, tsubnet)
	if err != nil {
		return "", err
	}
	if !bgCIDRRe.MatchString(tcidr) {
		return "", fmt.Errorf("could not resolve %s's subnet cidr (got: %s)", pair.colorName(to), orNone(tcidr))
	}
	tanchor, err := cloud.BGPrivateIPAddress(ctx, pair.auth, tpip)
	if err != nil {
		return "", err
	}
	if !bgIPRe.MatchString(tanchor) {
		return "", fmt.Errorf("could not resolve the anchor private address on %s", pair.colorName(to))
	}
	taddr := tanchor + "/" + strings.SplitN(tcidr, "/", 2)[1]
	if err := bgWriteAnchorConf(ctx, d, trow.PublicIP, pair.sshKey, pair.anchorConf, taddr); err != nil {
		return "", fmt.Errorf("anchor.conf write failed on %s (%s) — nothing mutated, flip aborted: %w", pair.colorName(to), trow.PublicIP, err)
	}
	if !bgPollAddr(ctx, d, trow.PublicIP, pair.sshKey, taddr, flipAddrTries) {
		bgCleanupTargetAnchor(ctx, d, trow.PublicIP, pair.sshKey, pair.anchorConf, taddr)
		return "", fmt.Errorf("guest watcher never configured %s on %s — is the kampodra-anchor service running? (conf removed, NOTHING mutated)", taddr, pair.colorName(to))
	}
	return taddr, nil
}
func bgWriteAnchorConf(ctx context.Context, d Deps, ip, sshKey, confPath, addr string) error {
	remote := fmt.Sprintf("umask 077; mkdir -p %s && printf 'ANCHOR_ADDR=%%s\\nANCHOR_IFACE=\\n' '%s' > %s && echo ANCHOR_CONF_WRITTEN",
		bgQuote(path.Dir(confPath)), addr, confPath)
	_, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip, SSHKey: sshKey}, remote)
	return err
}

// bgPollAddr waits for the guest watcher to configure the anchor address.
func bgPollAddr(ctx context.Context, d Deps, ip, sshKey, addr string, tries int) bool {
	for i := 0; i < tries; i++ {
		if _, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip, SSHKey: sshKey},
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
func bgReadDeployedSha(ctx context.Context, d Deps, ip, sshKey, stampPath string) string {
	out, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip, SSHKey: sshKey}, "cat "+stampPath+" 2>/dev/null || true")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// bgCleanupTargetAnchor removes the conf then deletes the address (retry:
// the add-only watcher may have sourced the conf just before removal and
// re-added it once).
func bgCleanupTargetAnchor(ctx context.Context, d Deps, ip, sshKey, confPath, addr string) {
	// Best-effort by design (cleanup after a failed flip): errors are
	// swallowed here — the caller already reports the flip failure.
	tolerant := func(remote string) {
		_, _ = d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip, SSHKey: sshKey}, remote)
	}
	tolerant("rm -f " + confPath + " >/dev/null 2>&1 || true")
	for i := 0; i < 3; i++ {
		tolerant(
			fmt.Sprintf(`iface=$(ip -4 route show default 2>/dev/null | awk '{print $5; exit}'); [ -n "$iface" ] && ip addr del '%s' dev "$iface" 2>/dev/null; true`, addr))
		if _, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip, SSHKey: sshKey},
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
		if bnic, _, ok, err := cloud.BGPrimaryVnic(ctx, pair.auth, pair.compOCID, brow.OCID); err == nil && ok && bnic != "" {
			if bpip, err := bgAnchorIP(ctx, pair, bnic); err == nil && bpip != "" && (pair.auth.Provider == "gcp" || cloud.IsOCID(bpip, "privateip")) {
				if err := cloud.BGReservedIPAssign(ctx, pair.auth, rocid, brow.OCID, bpip, ""); err == nil {
					fmt.Fprintf(d.Stderr, "rolled back to %s (existing anchor %s)\n", pair.colorName(ob), bpip)
					recovered = true
				}
			}
		}
	}
	if !recovered {
		if err := cloud.BGReservedIPRelease(ctx, pair.auth, rocid); err == nil {
			fmt.Fprintf(d.Stderr, "reserved ip UNASSIGNED (dormant) — no existing anchor on %s\n", pair.colorName(ob))
		} else {
			bgCleanupTargetAnchor(ctx, d, tpub, pair.sshKey, pair.anchorConf, taddr)
			fmt.Fprintf(d.Stderr, "AUTO-ROLLBACK FAILED — reserved ip state unknown; flip manually via console: %s\n", rocid)
			return
		}
	}
	bgCleanupTargetAnchor(ctx, d, tpub, pair.sshKey, pair.anchorConf, taddr)
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
	pair, err := resolveBGPair(d, target, "", surfaceBluegreen)
	if err != nil {
		return err
	}
	rip, ok, err := cloud.BGReservedIPList(ctx, pair.auth, pair.compOCID)
	if err != nil {
		return err
	}
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
		cnic, _, ok, err := cloud.BGPrimaryVnic(ctx, pair.auth, pair.compOCID, crow.OCID)
		if err != nil || !ok || cnic == "" {
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
		fmt.Fprintf(d.Stdout, "current holder: %s (%s) — unassigning FIRST, then cleaning the guest anchor\n", pair.colorName(holderColor), holderIP)
	}
	confAddr := ""
	if holderColor != "" {
		confAddr = bgReadAnchorAddr(ctx, d, holderIP, pair.sshKey, pair.anchorConf)
	}
	// ORDER IS THE FIX (live-fire 2026-10-16 incident): the cloud unassign
	// must land BEFORE the guest address is deleted, and must run on EVERY
	// path (a first cut of this reorder nested it in the holder-known
	// branch — the no-holder rollback skipped the unassign and printed
	// ROLLED BACK with the IP still attached). The old guest-cleanup-first
	// order opened a blackhole window where the reserved IP still mapped
	// to a PRIVATE address the guest no longer had — established
	// connections through the reserved IP died without the app ever being
	// signaled.
	if err := cloud.BGReservedIPRelease(ctx, pair.auth, rip.OCID); err != nil {
		return fmt.Errorf("unassign failed — check 'kampodra bluegreen status' and the console: %w", err)
	}
	if holderColor != "" {
		bgCleanupTargetAnchor(ctx, d, holderIP, pair.sshKey, pair.anchorConf, confAddr)
		fmt.Fprintf(d.Stdout, "%s guest cleaned (anchor.conf removed%s)\n", pair.colorName(holderColor), orAddrDeleted(confAddr))
	}
	fmt.Fprintf(d.Stdout, "ROLLED BACK: reserved %s UNASSIGNED (dormant)\n", rip.Address)
	return nil
}

// bgReadAnchorAddr reads the guest's current ANCHOR_ADDR (with prefix) or
// "". Quotes stripped — the conf is shell-sourceable by the watcher.
func bgReadAnchorAddr(ctx context.Context, d Deps, ip, sshKey, confPath string) string {
	out, err := d.Runner.Run(ctx, transport.HostSpec{Host: "root@" + ip, SSHKey: sshKey},
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
