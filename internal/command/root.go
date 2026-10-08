// Package command is kampodra's cobra command tree — ported from the
// frozen shell 0.6.0 spec (see internal/parity/golden.json). Commands
// orchestrate adapters and render; every business decision lives in
// internal/adapter/* or in pure helpers here, and the command-parity guard
// (parity_guard_test.go) keeps the frozen spec covered while allowing
// review-flagged kampodra-native additions.
package command

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// rootIndex is the vercel-style command index: the frozen-spec surface,
// marked "(not yet ported)" where only the spec entry exists, plus the
// kampodra-native additions at the end.
const rootIndex = `kampodra — kamal-alternative CLI for Alpine + Podman deploys, built on kamal-proxy

Usage: kampodra <command> [args...]

DEPLOY
  deploy         the full pipeline: build → save|load stream → env → restart → health gate → proxy re-point → smoke; --rollback [sha]; --rolling; converge
  bluegreen      reserved-IP blue/green pair (NOT_YET_PORTED): status | init | provision | flip | rollback
  migrate        tenant db migrations over SSH (NOT_YET_PORTED)

DEPLOY LIFECYCLE
  deploy          full build/stream pipeline with sha-verified health gate + kamal-proxy re-point + public smoke
  deploy --rollback [<sha>]  instant image-tag rollback (explicit sha > the VM's deployed-sha stamp > die — never HEAD)
  deploy --rolling        zero-downtime shadow-container double re-point (opt-in; failure paths keep the safer state)
  deploy converge finish an interrupted --rolling deploy (repair: retag → restart → gate → re-point → rm shadow)
  deploy list    deployment history: VM sha-tagged images (running one marked) merged with the local ledger + total deployments count; --all-profiles renders every profile
  deploy prune   reclaim VM disk: remove old sha-tagged images (keeps running + ts-rollback + newest N); --dry-run prints exact commands
  deploy logs    tail the running api container's logs (--lines N); --follow streams until ctrl-c
  deploy restart restart the api service (init-aware: rc-service on Alpine, systemctl on systemd hosts)
  deploy shell   interactive sh in the api container (exec -- <cmd> for one-shot)

INFRA
  vm-prepare     first-run bootstrap of a bare Alpine host (NOT_YET_PORTED)
  image-import   golden qcow2 -> OCI custom image (NOT_YET_PORTED)
  status         live health + deployment count + VM disk/image/service state + metrics (--verbose) + the blue/green pair view; --all-profiles renders every profile

DNS
  dns            OCI DNS records, oci auth (NOT_YET_PORTED): records | add | rm

ENV
  env            remote app env file (0600): list | push | pull | fingerprint | diff — values NEVER printed, fingerprints only

CONFIG
  config         per-instance profiles (~/.kampodra/config.json, 0700/0600) (NOT_YET_PORTED; profiles already drive every command)
                 resolution everywhere: --profile flag > KAMPODRA_PROFILE env > defaultProfile

HOST
  ssh            host-level ssh passthrough through the profile's host/key (no command = interactive login) — kampodra-native

METRICS
  metrics        one-shot VM snapshot over SSH (NOT_YET_PORTED): load, memory, disk, containers, top procs

BACKUP
  backup         OCI Object Storage backups (NOT_YET_PORTED): list | download | verify | restore-plan

Every command supports --help with usage + examples.
`

// Deps are the injectable boundaries of the CLI (tests never touch the real
// network, filesystem, or environment).
type Deps struct {
	Home   string
	Env    func(string) (string, bool)
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Runner transport.Runner
	Prober *probe.Prober
}

func (d Deps) withDefaults() Deps {
	if d.Env == nil {
		d.Env = os.LookupEnv
	}
	if d.Stdout == nil {
		d.Stdout = os.Stdout
	}
	if d.Stderr == nil {
		d.Stderr = os.Stderr
	}
	if d.Stdin == nil {
		d.Stdin = os.Stdin
	}
	if d.Runner == nil {
		d.Runner = &transport.SSHRunner{}
	}
	if d.Prober == nil {
		d.Prober = &probe.Prober{}
	}
	if d.Home == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			d.Home = home
		}
	}
	return d
}

// exitError carries a specific process exit code (cli.js parity: unknown
// commands exit 2, everything else 1).
type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// NewRoot builds the kampodra command tree.
func NewRoot(version string, deps Deps) *cobra.Command {
	d := deps.withDefaults()
	root := &cobra.Command{
		Use:           "kampodra",
		Short:         "kamal-alternative CLI for Alpine + Podman deploys, built on kamal-proxy",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       version,
		Args:          cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprint(d.Stdout, rootIndex)
				return nil
			}
			// cli.js parity: unknown command names the offender, reprints
			// the index on stderr, exits 2.
			fmt.Fprintf(d.Stderr, "kampodra: unknown command: %s\n\n%s", args[0], rootIndex)
			return &exitError{code: 2}
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetHelpCommand(&cobra.Command{Use: "no-help", Hidden: true})
	root.SetOut(d.Stdout)
	root.SetErr(d.Stderr)
	root.SetHelpFunc(func(c *cobra.Command, _ []string) {
		if c == root {
			fmt.Fprint(d.Stdout, rootIndex)
		}
	})

	root.AddCommand(newStatusCommand(d))
	root.AddCommand(newDeployCommand(d))
	root.AddCommand(newEnvCommand(d))
	root.AddCommand(newSSHCommand(d))
	root.AddCommand(newConfigCommand(d))
	return root
}

// Execute runs the CLI with args and returns the process exit code.
func Execute(version string, deps Deps, args []string) int {
	d := deps.withDefaults()
	root := NewRoot(version, deps)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		fmt.Fprintf(d.Stderr, "kampodra: %v\n", err)
		return 1
	}
	return 0
}
