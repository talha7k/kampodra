// bluegreen status: the pair at a glance — both colors' state, IP, app
// probe verdict, and which color holds the reserved IP.

package command

import (
	"fmt"

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
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
