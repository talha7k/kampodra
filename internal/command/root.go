// Package command is kampodra's cobra command tree. Commands orchestrate
// adapters and render; every business decision lives in
// internal/adapter/* or in pure helpers here. The public command set is
// pinned by surface_test.go: exactly the implemented commands, each with
// a Use and a Short.
package command

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/probe"
	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/transport"
)

// rootIndex is the vercel-style bare-invocation index: one screen listing
// every implemented command, grouped by job.
const rootIndex = `kampodra — kamal-alternative CLI for Alpine + Podman deploys, built on kamal-proxy

Usage: kampodra <command> [args...]

DEPLOY
  deploy         the full pipeline: build → save|load stream → env → restart → health gate → proxy re-point → smoke; --rollback [sha]; --rolling; converge
  migrate        tenant db migrations over SSH: root.db first, then tenants bounded-parallel; stop-first guard; --allow-running

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
  vm-wipe        kampodra-native teardown: stop+disable services, remove project containers, prune images, delete env/stamps/state (--yes gated; --keep-data)
  vm-prepare     first-run bootstrap of a bare Alpine host: gates, sshd hardening, podman stack, OpenRC services, kamal-proxy edge, anchor watcher; --pull-images; --ansible <playbook>
  status         live health + deployment count + VM disk/image/service state + metrics (--verbose); --all-profiles renders every profile

DNS
  dns            OCI DNS records, oci auth only (never credential material): records | add | rm

ENV
  env            remote app env file (0600): list | push | pull | fingerprint | diff | from-schema — values NEVER printed, fingerprints only

CONFIG
  config         per-instance profiles (~/.kampodra/config.json, 0700/0600): init | list | show | set-default | remove
                 resolution everywhere: --profile flag > KAMPODRA_PROFILE env > defaultProfile

HOST
  ssh            host-level ssh passthrough through the profile's host/key (no command = interactive login) — kampodra-native

METRICS
  metrics        one-shot VM snapshot over SSH: load, memory, disk, containers, top procs; --disk-threshold gates

BACKUP
  backup         OCI Object Storage backups: list | download | verify | restore-plan (restore-plan never executes)

Every command supports --help with usage + examples.
`

// Deps are the injectable boundaries of the CLI (tests never touch the real
// network, filesystem, or environment).
type Deps struct {
	Home   string
	Dir    string // the working directory (repo-manifest upward discovery)
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
	if d.Dir == "" {
		if wd, err := os.Getwd(); err == nil {
			d.Dir = wd
		}
	}
	return d
}

// manifestFor discovers the repo-level kampodra.json project manifest from
// the run directory (upward walk, nearest wins). No manifest in the tree is
// (nil, nil); an existing but malformed one FAILS CLOSED — a committed
// config typo must error, never silently resolve to defaults.
func (d Deps) manifestFor() (*project.Manifest, error) {
	mf, err := project.DiscoverManifest(d.Dir)
	if err != nil || mf.Path == "" {
		return nil, err
	}
	return &mf, nil
}

// exitError carries a specific process exit code: unknown commands exit 2
// (distinct from "ran and failed", which exits 1).
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
			// Unknown-command contract: name the offender, reprint the
			// index on stderr, exit 2.
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
	root.AddCommand(newBackupCommand(d))
	root.AddCommand(newVMWipeCommand(d))
	root.AddCommand(newVMPrepareCommand(d))
	root.AddCommand(newMetricsCommand(d))
	root.AddCommand(newDNSCommand(d))
	root.AddCommand(newMigrateCommand(d))
	root.AddCommand(newBluegreenCommand(d))
	root.AddCommand(newImageImportCommand(d))
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
