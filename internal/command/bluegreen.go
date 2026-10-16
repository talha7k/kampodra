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
//
// Wiring + tiny formatters live here; pair/instance resolution in
// bluegreen_pair.go, status in bluegreen_status.go, provision/inject in
// bluegreen_provision.go, flip/rollback in bluegreen_flip.go.

package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const bluegreenHelp = `Usage:
  kampodra bluegreen status                  # pair view: instances, IP holder, health
  kampodra bluegreen init                    # create the reserved public IP (dormant, unassigned)
  kampodra bluegreen provision <color>       # launch the second instance (golden image; see below)
  kampodra bluegreen deploy --to <color>     # update the STANDBY (build+push+gate on its own ip), then the health-gated flip
  kampodra bluegreen flip --to <color>       # ACME-first health-gated flip (see below)
  kampodra bluegreen rollback                # unassign to DORMANT + holder guest cleanup

Each sub-step has its own --help: status | init | provision | deploy | flip | rollback.
Cloud auth rides the instance profile's cloud block (or KAMPODRA_CLOUD_PROFILE /
KAMPODRA_CLOUD_COMPARTMENT env — deprecated aliases: OCI_PROFILE /
OCI_COMPARTMENT); the oci CLI resolves natively otherwise.

bluegreen deploy (the standby-update composite — "blue-green always up"):
  deploys onto the NON-serving color on its own public ip (the deploy
  pipeline: build → push → restart → served-sha health gate) and — only
  after that gate passes — runs the same ACME-first flip. Any pipeline
  failure returns before the flip: the holder keeps serving.
  v1 boundaries: image deploys + binary fast-push (the pipeline's native
  paths); rolling is NOT composed (the flip IS the cutover); no --env-file
  (the standby keeps its installed env); --force-flip forwards to the
  flip's --force.

Examples:
  kampodra bluegreen status
  kampodra bluegreen init
  kampodra bluegreen provision green
  kampodra bluegreen deploy --to green
  kampodra bluegreen flip --to green
  kampodra bluegreen rollback
`

func newBluegreenCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bluegreen",
		Short: "OCI reserved-IP blue/green pair: status | init | provision | deploy | flip | rollback",
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
		newBluegreenDeploySub(d),
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
	pf.String("os", "alpine", "provision: guest OS of the golden image (alpine|ubuntu)")
	pf.String("qcow2", "", "provision inject route: golden qcow2 path (ALPINE_QCOW2 env, else error)")
	pf.String("platform-key", "", "provision inject route: ssh public key FILE for the platform-image first boot (OPS_SSH_PUBKEY env, else error)")
	pf.String("platform-user", "", "provision inject route: platform-image ssh user (PLATFORM_SSH_USER env, default ubuntu)")
	pf.String("to", "", "flip/deploy: target color (blue|green)")
	pf.Bool("force", false, "flip: cut over even when the target app is unhealthy")
	return cmd
}

// newBluegreenDeploySub wires the standby-update composite: the RunE body
// (runBluegreenDeploy) lives in bluegreen_deploy.go. --to rides the shared
// persistent flag; --force-flip is the composite's own (it forwards to the
// flip's --force).
func newBluegreenDeploySub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "update the STANDBY <color> in place (build+push+gate on its own ip), then the health-gated flip — one command, zero-dark",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runBluegreenDeploy(d, c)
		},
	}
	cmd.Flags().Bool("force-flip", false, "cut over even when the standby is unhealthy (forwards to flip --force)")
	return cmd
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
