// Package command is kampodine's cobra command tree — the Go port of the
// shell CLI (github.com/talha7k/kampodine). Commands orchestrate adapters
// and render; every business decision lives in internal/adapter/* or in
// pure helpers here, and the command-parity guard (parity_guard_test.go)
// keeps this tree's surface pinned to the shell golden.
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

// rootIndex is the vercel-style command index — byte-equal to the committed
// shell cli.js usage const (kampodine HEAD).
const rootIndex = `kampodine — kamal-alternative CLI for Alpine + Podman deploys, built on kamal-proxy

Usage: kampodine <command> [args...]

DEPLOY
  deploy         stream deploy (podman save | ssh podman load) with sha-verified health gate; --rollback [sha] = instant image-tag rollback
  bluegreen      reserved-IP blue/green pair: status | init | provision | flip | rollback (each sub-step has --help)
  migrate        tenant db migrations over SSH

DEPLOY LIFECYCLE
  deploy list    deployment history: VM sha-tagged images (running one marked) merged with the local ledger + total deployments count
  deploy prune   reclaim VM disk: remove old sha-tagged images (keeps running + ts-rollback + newest N); --dry-run prints exact commands
  deploy logs    tail the running api container's logs (--lines N)
  deploy restart restart the api service (init-aware: rc-service on Alpine, systemctl on systemd hosts)
  deploy shell   interactive sh in the api container (exec -- <cmd> for one-shot)

INFRA
  vm-prepare     first-run bootstrap of a bare Alpine host (OpenRC + podman stack)
  image-import   golden qcow2 -> OCI custom image
  status         live health + deployment count + VM disk/image/service state + metrics (--verbose) + the blue/green pair view

DNS
  dns            OCI DNS records (oci config-file / instance-principal auth ONLY): records | add | rm

ENV
  env            remote app env file (/etc/kampodine/env, 0600): list | push | pull — values NEVER printed, fingerprints only

CONFIG
  config         per-instance profiles (~/.kampodine/config.json, 0700/0600): init | list | show | set-default | remove
                 resolution everywhere: --profile flag > KAMPODINE_PROFILE env > defaultProfile > legacy env

METRICS
  metrics        one-shot VM snapshot over SSH: load, memory, disk (images/data/other), containers, top procs; --watch N; --warn-disk exits 1

BACKUP
  backup         OCI Object Storage backups (config-file / instance-principal auth ONLY): list | download | verify | restore-plan

Every command supports --help with usage + examples. All further args pass through to the underlying script.
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

// NewRoot builds the kampodine command tree.
func NewRoot(version string, deps Deps) *cobra.Command {
	d := deps.withDefaults()
	root := &cobra.Command{
		Use:           "kampodine",
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
			fmt.Fprintf(d.Stderr, "kampodine: unknown command: %s\n\n%s", args[0], rootIndex)
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
		fmt.Fprintf(d.Stderr, "kampodine: %v\n", err)
		return 1
	}
	return 0
}
