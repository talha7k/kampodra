// bluegreen status: the pair at a glance — both colors' state, IP, app
// probe verdict, and which color holds the reserved IP.

package command

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/cloud"
)

func runBluegreenStatus(d Deps, c *cobra.Command) error {
	ctx := c.Context()
	target, err := resolveAnyTarget(d, c)
	if err != nil {
		return err
	}
	pair, err := resolveBGPair(d, target, "", surfaceBluegreen)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "== %s blue/green pair (compartment %s) ==\n", pair.container, pair.compName)
	rip, ok, err := cloud.BGReservedIPList(ctx, pair.auth, pair.compOCID)
	if err != nil {
		return err
	}
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
		if instanceHealthy(ctx, d, inst.PublicIP, pair.sshKey, pair.container, pair.port, pair.healthPath) {
			verdict = "HEALTHY"
		}
		fmt.Fprintf(d.Stdout, "%s : %s  ip=%s  app=%s\n", pair.colorName(color), inst.OCID, orNone(inst.PublicIP), verdict)
	}
	// The anchor watcher section (added after the 2026-10-16 incident: the
	// watcher was invisible to every check while operators suspected it of
	// restarting containers — it is ADD-ONLY and never touches podman, but
	// nothing surfaced its state). ONE ssh round-trip reads everything:
	// unit liveness (init-aware), conf presence + address, address-on-iface,
	// and the last transitions from the watcher log. SINGLE LINE: the ssh
	// invocations log is line-per-call and the guest-legs identity test
	// walks it line by line.
	remote := `if command -v rc-service >/dev/null 2>&1; then echo "unit: $(rc-service kampodra-anchor status >/dev/null 2>&1 && echo active || echo inactive)"; else echo "unit: $(systemctl is-active kampodra-anchor.service 2>/dev/null || echo inactive)"; fi; if [ -f ` + pair.anchorConf + ` ]; then addr=$(grep -h '^ANCHOR_ADDR=' ` + pair.anchorConf + ` | cut -d= -f2); echo "conf: $addr"; ip -4 addr show | grep -qF -- "$addr" && echo "address: on iface" || echo "address: NOT on iface"; else echo "conf: none (watcher idling)"; fi; echo "--- last transitions ---"; tail -n 5 /var/log/kampodra-anchor.log 2>/dev/null || true`
	fmt.Fprintln(d.Stdout, "watcher     :")
	if out, err := d.Runner.Run(ctx, target.HostSpec, remote); err != nil {
		fmt.Fprintf(d.Stdout, "  unreadable: %v\n", err)
	} else {
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if line != "" {
				fmt.Fprintf(d.Stdout, "  %s\n", line)
			}
		}
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
