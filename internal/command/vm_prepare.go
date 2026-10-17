// vm-prepare — the host bootstrap (vm-prepare.sh port), OS-dispatching:
// the guest's /etc/os-release picks the provisioner. The VM goes to
// production from its first boot: no blue->green flip, no cert carry; the
// first TLS certificate issues during the first deploy.
//
// Wiring, the provisioner core, and the shared wait/ansible legs live here;
// the guest-OS-agnostic steps in vm_prepare_shared.go; the per-OS
// sequences in vm_prepare_alpine.go / vm_prepare_ubuntu.go.

package command

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/project"
	"github.com/talha7k/kampodra/internal/adapter/state"
	"github.com/talha7k/kampodra/internal/adapter/transport"
	"github.com/talha7k/kampodra/internal/adapter/vmbootstrap"
)

// vmPrepareHelp — the host bootstrap (vm-prepare.sh port), OS-dispatching:
// the guest's /etc/os-release picks the provisioner. The VM goes to
// production from its first boot: no blue->green flip, no cert carry; the
// first TLS certificate issues during the first deploy.
const vmPrepareHelp = `Usage:
  kampodra vm-prepare --host root@<new-ip> [--ssh-key <path>] [--profile <name>] [--ansible <playbook>]

First-run bootstrap of a bare VM — Alpine or Ubuntu 24.04, auto-detected
from the guest's /etc/os-release (no flag; idempotent — safe to re-run):

  common:      connectivity, guest OS detect, UEFI gate (both golden
               images are UEFI-only)

  alpine:      1. sanity gates (no systemd anywhere, OpenRC tooling)
               2. sshd hardening ensured (drop-in + Include + restart, then gated)
               3. apk community repo + the podman stack (netavark backend)
               4. the project state dir + /etc/containers/registries.conf (insecure registry)
               5. sysctl net.ipv4.ip_unprivileged_port_start=80 (persisted + live)
               6. OpenRC services: the api container + kamal-proxy (supervise-daemon),
                  rc-update'd into the default runlevel — that IS boot survival
               7. kampodra-anchor: the blue/green flip's guest half (inert without the conf)
               8. kamal-proxy image pulled + service UP (first deploy issues fresh ACME TLS)

  ubuntu 24.04: 1. sanity gates (systemd IS pid1, apt operational)
                2. the same sshd hardening (systemctl restart ssh, then gated)
                3. universe + the podman stack via apt (netavark backend)
                4. the same state dir + managed files (sysctl --system)
                5. systemd units: api + kamal-proxy + kampodra-anchor —
                   daemon-reload, then enable (the api defers its start to
                   the first deploy) and enable --now (edge + anchor)
                6. is-enabled / is-active readiness gates

The api container is NOT started on a fresh VM: neither its image nor the
env file exists yet — the FIRST DEPLOY provides both (both OSes).

--ansible <playbook> runs ansible-playbook against the host AFTER bootstrap
(inventory derived from --host, --private-key from the resolved ssh key).

Host/key resolution: --host | --profile <name> | KAMPODRA_PROFILE |
config defaultProfile; --ssh-key | profile sshKey | KAMPODRA_SSH_KEY.
`

// waitPolicy is the retry shape of the two readiness loops (shrunk by the
// internal test; never by production code paths).
type waitPolicy struct {
	attempts int
	interval time.Duration
}

var (
	vmPrepareSSHWait   = waitPolicy{attempts: 60, interval: 5 * time.Second}
	vmPrepareProxyWait = waitPolicy{attempts: 20, interval: 3 * time.Second}
)

// vmProvisioner carries the shared bootstrap machinery (the ssh closures +
// the resolved target/naming) into one provisioner per guest OS. Dispatch
// happens ONCE, after connectivity, before any mutation: the provisioners
// speak through the same helpers and differ only in their sequences
// (siblings behind one Naming surface — never branches inside a sequence).
type vmProvisioner struct {
	ctx    context.Context
	d      Deps
	target Target
	n      vmbootstrap.Naming
}

func (p *vmProvisioner) say(format string, args ...any) {
	fmt.Fprintf(p.d.Stdout, "[vm-prepare] "+format+"\n", args...)
}

func (p *vmProvisioner) run(remote string) error {
	_, err := p.d.Runner.Run(p.ctx, p.target.HostSpec, remote)
	return err
}

func (p *vmProvisioner) runSh(snippet string) error {
	_, err := p.d.Runner.RunWithStdin(p.ctx, p.target.HostSpec, "sh -s", strings.NewReader(snippet))
	return err
}

func (p *vmProvisioner) upload(path, content string) error {
	if _, err := p.d.Runner.RunWithStdin(p.ctx, p.target.HostSpec, "umask 077; cat > "+path, strings.NewReader(content)); err != nil {
		return fmt.Errorf("upload %s: %w", path, err)
	}
	return nil
}

func (p *vmProvisioner) die(context string, err error) error {
	return fmt.Errorf("%s: %w", context, err)
}

func newVMPrepareCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm-prepare --host root@<new-ip> [--ansible <playbook>]",
		Short: "first-run bootstrap of a bare host (Alpine or Ubuntu 24.04, auto-detected): gates, sshd hardening, podman stack, init services, kamal-proxy edge, anchor watcher",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runVMPrepare(d, c)
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), vmPrepareHelp)
	})
	cmd.Flags().String("host", "", "target VM (user@ip or ssh-config alias)")
	cmd.Flags().String("profile", "", "per-instance profile (~/.kampodra/config.json)")
	cmd.Flags().String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY")
	cmd.Flags().String("ansible", "", "run this ansible-playbook file against the host after bootstrap")
	cmd.Flags().String("binary", "", "binary artifact this VM serves (kampodra.json \"binary\" block) — required when the repo declares several exec-kind blocks; the unit bind-mounts its dir and execs its entry (every static block's mount + env always renders)")
	return cmd
}

// namingFromProject derives the bootstrap naming from the resolved project
// config (the adapter never imports the project adapter sideways).
// bin is the deploy --binary artifact this VM's app unit serves (nil =
// none — the unit renders byte-identical to the pre-binary shape).
// statics are EVERY static-kind block's mounts/envs (a SPA dist shares the
// unit with the exec-kind backend; the pinned block's own dir is skipped —
// its mount already rode in via BinaryMount). serveWeb=false (the no-web
// estate — web elsewhere, the VM strictly api) drops every static-kind
// block from the render, pinned or swept: no -v dist mount, no read-through
// -e, no releases layout.
func namingFromProject(pj project.Config, bin *project.ManifestBinary, statics []project.ManifestBinary, unitEnvs map[string]string) vmbootstrap.Naming {
	envDir := pj.EnvFile
	if i := strings.LastIndex(pj.EnvFile, "/"); i > 0 {
		envDir = pj.EnvFile[:i]
	}
	serveWeb := pj.ServeWebEnabled()
	// A pinned STATIC block IS web serving — it contradicts a no-web
	// estate and drops with the rest (the unit then serves only the
	// image/binary entry).
	if !serveWeb && bin != nil && bin.Kind == project.BinaryKindStatic {
		bin = nil
	}
	n := vmbootstrap.Naming{
		Container:       pj.Container,
		EnvFile:         pj.EnvFile,
		EnvDir:          envDir,
		AnchorConf:      envDir + "/anchor.conf",
		DataDir:         pj.DataDir,
		ImagePrefix:     pj.ImagePrefix,
		Registry:        imageRegistryHost(pj.ImagePrefix),
		ImageRef:        pj.ImagePrefix + ":latest",
		Port:            pj.Port,
		Network:         pj.Network,
		DeployedShaFile: pj.DeployedShaFile,
	}
	if bin != nil {
		n.BinaryDir = bin.Dir
		n.BinaryMount = bin.MountArg()
		n.BinaryEntrypointFlag = bin.EntrypointFlag()
		n.BinaryCmdOverride = bin.CmdOverride()
		n.BinaryDepsPath = bin.DepsPath
		// A pinned static block's env still renders (its mount rode in
		// above as BinaryMount).
		if env := bin.StaticEnvArg(); env != "" {
			n.StaticEnvs = append(n.StaticEnvs, env)
		}
	}
	if serveWeb {
		for _, s := range statics {
			n.StaticReleases = true
			if bin != nil && s.Dir == bin.Dir {
				continue // the pinned block already rendered this dir's mount
			}
			n.StaticMounts = append(n.StaticMounts, s.MountArg())
			if env := s.StaticEnvArg(); env != "" {
				n.StaticEnvs = append(n.StaticEnvs, env)
			}
		}
	}
	// Repo-owned unit envs (manifest unit.env) render LAST: -e is
	// later-wins in podman, and a declared repo key outranks a renderer
	// clear-env default.
	keys := make([]string, 0, len(unitEnvs))
	for k := range unitEnvs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n.UnitEnvs = append(n.UnitEnvs, "-e "+k+"="+unitEnvs[k])
	}
	return n
}

// cloudProviderOf reads the profile cloud block's "provider" ("" = the oci
// default, the cloud adapter's own convention) — vm-prepare needs it only
// to pick the anchor watcher variant the guest image carries.
func CloudProviderOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var block struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return "" // a malformed block fails later at CheckProvider, named there
	}
	return block.Provider
}

// DroppedUnitEnvKeys diffs the -e keys of the unit ON the VM against the
// keys the fresh render carries. The live-fire 2026-10-16 class: a
// re-render silently dropped -e LIBSQL_TENANT_DIR (owned by the old
// ansible template, adopted by nobody) — the store never opened, routes
// vanished, and shallow health checks kept the smoke green. A dropped key is a
// fail-closed event naming the migration paths.
func DroppedUnitEnvKeys(oldUnit, newUnit string) []string {
	re := regexp.MustCompile(`-e ([A-Za-z_][A-Za-z0-9_]*)=`)
	old := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(oldUnit, -1) {
		old[m[1]] = true
	}
	fresh := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(newUnit, -1) {
		fresh[m[1]] = true
	}
	var dropped []string
	for k := range old {
		if !fresh[k] {
			dropped = append(dropped, k)
		}
	}
	sort.Strings(dropped)
	return dropped
}

// guardUnitEnvDrop refuses a unit upload whose render loses -e keys the
// CURRENT unit on the VM carries (diff of both renders' -e keys). Unreadable
// old unit = fresh VM, nothing to drop. Call it immediately before the app
// unit upload, with the SAME path + render.
func (p *vmProvisioner) guardUnitEnvDrop(unitPath, newUnit string) error {
	old, err := p.d.Runner.Run(p.ctx, p.target.HostSpec, "cat "+unitPath+" 2>/dev/null || true")
	if err != nil {
		return nil // fresh VM — nothing on disk to drop
	}
	dropped := DroppedUnitEnvKeys(old, newUnit)
	if len(dropped) == 0 {
		return nil
	}
	return fmt.Errorf("re-rendering %s would DROP these -e keys the current unit carries: %s — migrate them into kampodra.json's \"unit\": {\"env\": {...}} or the env file, then re-run (a dropped store/route env silently disables app surfaces while /api health checks stay green)", unitPath, strings.Join(dropped, ", "))
}

// staticBlocks collects the repo's static-kind blocks in a stable order
// (name-sorted — the unit render is deterministic) for the naming sweep.
func staticBlocks(blocks map[string]project.ManifestBinary) []project.ManifestBinary {
	names := sortedBinaryNames(blocks)
	out := make([]project.ManifestBinary, 0, len(names))
	for _, name := range names {
		if b := blocks[name]; b.Kind == project.BinaryKindStatic {
			out = append(out, b)
		}
	}
	return out
}

// imageRegistryHost strips the path off a repo reference (the ImagePrefix's
// host:port — e.g. "127.0.0.1:5000/repo-name" -> "127.0.0.1:5000").
func imageRegistryHost(imagePrefix string) string {
	if i := strings.Index(imagePrefix, "/"); i > 0 {
		return imagePrefix[:i]
	}
	return imagePrefix
}

func runVMPrepare(d Deps, c *cobra.Command) error {
	host, _ := c.Flags().GetString("host")
	key, _ := c.Flags().GetString("ssh-key")
	profile, _ := c.Flags().GetString("profile")
	ansiblePath, _ := c.Flags().GetString("ansible")
	binaryName, _ := c.Flags().GetString("binary")

	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return err
	}
	mf, mfErr := d.manifestFor()
	if mfErr != nil {
		return mfErr
	}
	target, err := ResolveTarget(cfg, host, key, profile, mf, d.Env)
	if err != nil {
		return err
	}
	if err := requireHost(target); err != nil {
		return fmt.Errorf("--host root@<ip> required (--profile <name> or KAMPODRA_HOST also work; --help for usage)")
	}
	// First contact: the bootstrap leg accepts unknown host keys.
	target.HostSpec.AcceptNewHostKey = true

	ctx := c.Context()
	// The binary artifact this VM's app unit serves (deploy --binary's
	// mount): the --binary flag names it; a lone repo block needs no name;
	// a repo with NO blocks pre-pdates the fast path and renders the
	// legacy image-entrypoint unit unchanged.
	var bin *project.ManifestBinary
	if len(target.Binary) > 0 {
		bin, err = ChooseBinaryBlock(target.Binary, binaryName)
		if err != nil {
			return err
		}
	}
	n := namingFromProject(target.Project, bin, staticBlocks(target.Binary), target.UnitEnvs)
	n.Provider = CloudProviderOf(target.Cloud)
	p := &vmProvisioner{ctx: ctx, d: d, target: target, n: n}

	// --- 0. connectivity ------------------------------------------------------
	p.say("waiting for ssh on %s…", target.HostSpec.Host)
	if err := waitForSSH(ctx, d.Runner, target.HostSpec, vmPrepareSSHWait); err != nil {
		return err
	}

	// --- 1. guest OS detect — the dispatch happens ONCE, here, BEFORE any -----
	// mutation (os-release(5) read remotely; the ID= value picks the sibling).
	p.say("detecting the guest OS (/etc/os-release)…")
	osRelease, err := d.Runner.Run(ctx, target.HostSpec, vmbootstrap.OSReleaseRead)
	if err != nil {
		return p.die("could not read /etc/os-release (not a kampodra golden image?)", err)
	}
	guestOS := vmbootstrap.GuestOSID(osRelease)

	// --- 2. common pre gate: UEFI (both golden images are UEFI-only) ----------
	p.say("gate: UEFI boot")
	if err := p.run(vmbootstrap.GateUEFI); err != nil {
		return p.die("not booted via UEFI (golden image is UEFI-only — wrong machine/image?)", err)
	}

	// --- 3. dispatch — one provisioner per guest OS -----------------------------
	switch guestOS {
	case "alpine":
		p.say("guest OS: alpine → the Alpine provisioner (OpenRC)")
		return p.provisionAlpine(ansiblePath)
	case "ubuntu":
		p.say("guest OS: ubuntu → the Ubuntu 24.04 provisioner (systemd)")
		return p.provisionUbuntu(ansiblePath)
	default:
		return fmt.Errorf("unsupported guest OS %q — vm-prepare provisions Alpine or Ubuntu 24.04 only (a third guest OS arrives as its own provisioner, not as branches here)", guestOS)
	}
}

// --- shared provisioner stages (identical between the OS siblings) ---------
//
// The siblings differ ONLY in init system + package manager; these stages
// are the OS-agnostic skeleton both sequences call in the same order.
// waitForSSH polls the host until `true` succeeds (vm-prepare's readiness
// loop: 60 × 5s against a booting VM).
func waitForSSH(ctx context.Context, runner transport.Runner, spec transport.HostSpec, p waitPolicy) error {
	return waitUntil(ctx, runner, spec, p, "true", fmt.Sprintf("no ssh after %d attempts (firewall 22 rule for your IP? baked ops key in the agent?)", p.attempts))
}

func waitUntil(ctx context.Context, runner transport.Runner, spec transport.HostSpec, p waitPolicy, remote, failMsg string) error {
	for i := 0; i < p.attempts; i++ {
		if _, err := runner.Run(ctx, spec, remote); err == nil {
			return nil
		}
		if i < p.attempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(p.interval):
			}
		}
	}
	return fmt.Errorf("%s", failMsg)
}

// runAnsiblePlaybook runs the operator's playbook against the freshly
// bootstrapped host (inline inventory derived from --host + --private-key
// from the resolved ssh key). Output streams; failure is fatal (explicit
// flag = intent). ctx is the command's context: ctrl-C kills ansible
// (it used to run on a background context and outlive the interrupt).
func runAnsiblePlaybook(d Deps, ctx context.Context, target Target, playbookPath string) error {
	// Absolutize BEFORE the chdir: the exec below runs ansible from the
	// playbook's directory, so a relative arg would re-resolve against it
	// and die with "the playbook … could not be found" (2026-10-09 live fire).
	absPath, err := filepath.Abs(playbookPath)
	if err != nil {
		return fmt.Errorf("ansible playbook path: %w", err)
	}
	if _, err := os.Stat(absPath); err != nil {
		return fmt.Errorf("ansible playbook not found: %s", playbookPath)
	}
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		return fmt.Errorf("ansible-playbook not found on PATH — install ansible to use --ansible")
	}
	user, host := target.HostSpec.Host, target.HostSpec.Host
	if i := strings.LastIndex(target.HostSpec.Host, "@"); i >= 0 {
		user = target.HostSpec.Host[:i]
		host = target.HostSpec.Host[i+1:]
	}
	// The -i STRING form is a bare host list — ansible-core 2.21 parses
	// k=v pairs there as separate HOSTNAMES ("hostname contains invalid
	// characters"; ssh to "ansible_user=root", 2026-10-09 live fire).
	// Inline host vars are only reliably parsed in an inventory FILE:
	// write a temp INI file (alias + k=v under an [alias] group).
	invFile, err := os.CreateTemp("", "kampodra-inventory-*.ini")
	if err != nil {
		return fmt.Errorf("ansible inventory temp file: %w", err)
	}
	invPath := invFile.Name()
	defer os.Remove(invPath)
	if _, err := fmt.Fprintf(invFile, "[kampodra-vm]\nkampodra-vm ansible_host=%s ansible_user=%s\n", host, user); err != nil {
		invFile.Close()
		return fmt.Errorf("ansible inventory write: %w", err)
	}
	if err := invFile.Close(); err != nil {
		return fmt.Errorf("ansible inventory close: %w", err)
	}
	args := []string{"-i", invPath, absPath}
	if target.HostSpec.SSHKey != "" {
		args = append(args, "--private-key", target.HostSpec.SSHKey)
	}
	fmt.Fprintf(d.Stdout, "[vm-prepare] ansible-playbook (bootstrap converge)…\n")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = filepath.Dir(absPath)
	cmd.Stdout = d.Stdout
	cmd.Stderr = d.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ansible playbook failed: %w", err)
	}
	fmt.Fprintf(d.Stdout, "[vm-prepare] ansible playbook done\n")
	return nil
}
