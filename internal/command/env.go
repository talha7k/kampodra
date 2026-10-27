package command

import (
	"path/filepath"

	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/envfile"
	"github.com/talha7k/kampodra/internal/adapter/envschema"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// envHelp is the shell env.sh usage heredoc (byte-equal for the shell-era
// lines); the kampodra-native diff line documents the added surface.
const envHelp = `Usage:
  kampodra env list [--host <user@ip>] [--profile <name>] [--ssh-key <path>]             # KEY + fingerprint table (NEVER values)
  kampodra env push --file <local-env-file> [...] [--profile <name>]                     # upload: 0600 temp + atomic mv + restart hint
  kampodra env pull [--out <file>] [...] [--profile <name>]                              # raw payload to stdout/--out (0600); masked summary follows
  kampodra env fingerprint [--file <f>]                                                  # preview the masking for a LOCAL file / stdin (never values)
  kampodra env diff <local-file> [--host <user@ip>] [--profile <name>] [--ssh-key <path>] # fingerprint-level local vs remote (exit 1 = differs)

Fingerprints NEVER leak values: every line is KEY + value length + first 2 chars.
Raw values move only in push's upload stream and pull's stdout/--out payload.
Host/key resolution matches deploy.sh: --host | --profile <name> |
KAMPODRA_PROFILE | config defaultProfile | KAMPODRA_HOST; --ssh-key |
profile sshKey | KAMPODRA_SSH_KEY | ssh-agent / ~/.ssh/config.

Examples:
  kampodra env list --host root@203.0.113.10
  kampodra env push --file ./ops/env.production --host root@203.0.113.10
  kampodra env pull --out ./env.snapshot --host root@203.0.113.10   # written 0600
  kampodra env pull --host root@203.0.113.10 | wc -l                # raw payload on stdout, summary on stderr
  kampodra env fingerprint --file ./.env.local                      # preview masking, values never leave stdin
  kampodra env diff ./ops/env.production --host root@203.0.113.10    # what would this push change?
`

func newEnvCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "remote app env file (project envFile, 0600): list | push | pull | fingerprint | diff — values NEVER printed, fingerprints only",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Fprint(c.OutOrStdout(), envHelp)
			return nil
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), envHelp)
	})

	subList := newEnvListSub(d)
	subPush := newEnvPushSub(d)
	subSet := newEnvSetSub(d)
	subPull := newEnvPullSub(d)
	subFingerprint := newEnvFingerprintSub(d)
	subDiff := newEnvDiffSub(d)

	subFromSchema := newEnvFromSchemaCommand(d)
	remoteTargetFlags(subFromSchema)
	cmd.AddCommand(subList, subSet, subPush, subPull, subFingerprint, subDiff, subFromSchema)
	return cmd
}

// envTargetFlags registers the shared env-family target flags (the same
// three every remote env subcommand takes).
// remoteTargetFlags registers the shared target flag trio (host/profile/ssh-key)
// every remote command family takes — the SAME resolution ladder below them.
func remoteTargetFlags(c *cobra.Command) {
	c.Flags().String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODRA_HOST and any profile")
	c.Flags().String("profile", "", "per-instance profile (~/.kampodra/config.json) — beats KAMPODRA_PROFILE / defaultProfile")
	c.Flags().String("ssh-key", "", "identity file — beats KAMPODRA_SSH_KEY; empty = agent / ssh config")
}

// newEnvListSub is env's `list` subcommand: KEY + fingerprint table of the
// remote env file (NEVER values).
func newEnvListSub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "KEY + fingerprint table of the remote env file (NEVER values)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolveTargetFromFlags(d, c)
			if err != nil {
				return err
			}
			sayProvenance(d, "env", target)
			if err := requireHost(target); err != nil {
				return err
			}
			raw, err := fetchRemoteEnv(d, c.Context(), target, target.Project.EnvFile)
			if err != nil {
				return err
			}
			fmt.Fprint(d.Stdout, envfile.Table(raw))
			return nil
		},
	}
	remoteTargetFlags(cmd)
	return cmd
}

// newEnvPushSub is env's `push` subcommand: upload a local env file —
// 0600 from creation remotely (umask 077) + atomic mv within the same
// directory, byte-equal to the shell flow.
func newEnvPushSub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "push",
		Short: "upload a local env file: 0600 temp + atomic mv (+ --restart applies it init-aware); values NEVER printed",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			file, _ := c.Flags().GetString("file")
			if file == "" {
				return fmt.Errorf("push requires --file <local-env-file> (--help)")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("no such file: %s", file)
			}
			if envfile.CountKeyLines(string(data)) == 0 {
				return fmt.Errorf("no KEY=VALUE lines in %s — nothing to push", file)
			}
			target, err := resolveTargetFromFlags(d, c)
			if err != nil {
				return err
			}
			sayProvenance(d, "env", target)
			if err := requireHost(target); err != nil {
				return err
			}
			remote := target.Project.EnvFile
			fmt.Fprintf(d.Stdout, "[env] pushing %s -> %s:%s (fingerprint summary below; values are NEVER printed)\n",
				file, target.HostSpec.Host, remote)
			fmt.Fprint(d.Stdout, envfile.Table(string(data)))

			if err := installRemoteEnv(d, c.Context(), target, string(data)); err != nil {
				return err
			}
			if restart, _ := c.Flags().GetBool("restart"); restart {
				return runDeployRestart(d, c.Context(), target)
			}
			fmt.Fprintf(d.Stdout, "[env] restart to apply: kampodra env push --restart, ssh %s 'rc-service %s restart', or kampodra deploy\n", target.HostSpec.Host, target.Project.Container)
			return nil
		},
	}
	remoteTargetFlags(cmd)
	cmd.Flags().String("file", "", "local env file to upload (verbatim — what you push is what lands)")
	cmd.Flags().Bool("restart", false, "restart the api service after the push (init-aware) — the env file is re-read on container start")
	return cmd
}

// installRemoteEnv lands env content on the VM atomically: 0600 temp in
// the env file's directory + same-dir mv. The ONLY write path for env
// content (push and set share it).
func installRemoteEnv(d Deps, ctx context.Context, target Target, content string) error {
	remote := target.Project.EnvFile
	tmp := envfile.RemoteTmpPath(remote, os.Getpid())
	// mkdir -p first: the env file's directory exists on vm-prepared hosts,
	// but env set must also work on bare hosts (the F18 hop finding — a
	// missing env dir surfaced as a cryptic "upload failed").
	if _, err := d.Runner.RunWithStdin(ctx, target.HostSpec, "umask 077; mkdir -p "+filepath.Dir(tmp)+"; cat > "+tmp, strings.NewReader(content)); err != nil {
		return fmt.Errorf("upload failed")
	}
	if _, err := d.Runner.Run(ctx, target.HostSpec, fmt.Sprintf("chmod 600 %s && mv -f %s %s", tmp, tmp, remote)); err != nil {
		return fmt.Errorf("atomic install failed (remote temp left at: %s)", tmp)
	}
	fmt.Fprintf(d.Stdout, "[env] installed %s (0600) on %s\n", remote, target.HostSpec.Host)
	return nil
}

// newEnvSetSub is env's `set` subcommand: upsert one or more KEY=VALUE
// pairs into the REMOTE env file without maintaining a local copy — the
// remote content is pulled, the pairs replace-in-place (or append), and
// the merged file installs atomically. Values never print.
func newEnvSetSub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set KEY=VALUE [KEY=VALUE...] [KEY+=VALUE...]",
		Short: "upsert KEY=VALUE pairs into the remote env file (values NEVER printed); KEY+=VALUE APPENDS to a comma-list (the trusted-origins pattern); pairs with spaces pass raw — podman --env-file takes the whole remainder",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			pairs, err := parseEnvSetPairs(args)
			if err != nil {
				return err
			}
			target, err := resolveTargetFromFlags(d, c)
			if err != nil {
				return err
			}
			sayProvenance(d, "env set", target)
			if err := requireHost(target); err != nil {
				return err
			}
			restart, _ := c.Flags().GetBool("restart")
			return runEnvSet(d, c.Context(), target, pairs, restart)
		},
	}
	remoteTargetFlags(cmd)
	cmd.Flags().Bool("restart", false, "restart the api service after the set (init-aware) — the env file is re-read on container start")
	return cmd
}

// envSetPair is one requested upsert: the comma-list APPEND flag rides
// separately from the key/value (a plain value may legitimately contain
// `+=` — separator precedence is first-=-before-+=).
type envSetPair struct {
	key, value string
	appendMode bool
}

// parseEnvSetPairs validates the CLI grammar: KEY=VALUE replaces, KEY+=V
// appends to a comma-list. The plain `=` wins when it occurs at or before
// any `+=` (a value containing `+=` stays a plain replace).
func parseEnvSetPairs(args []string) ([]envSetPair, error) {
	pairs := []envSetPair{}
	for _, arg := range args {
		eq := strings.Index(arg, "=")
		plusEq := strings.Index(arg, "+=")
		appendMode := plusEq >= 0 && (eq < 0 || plusEq < eq)
		sep := "="
		if appendMode {
			sep = "+="
		}
		key, value, found := strings.Cut(arg, sep)
		if !found {
			return nil, fmt.Errorf("%q is not KEY=VALUE or KEY+=VALUE — the first = (or +=) splits key from value", arg)
		}
		vars := envfile.Parse(key + "=" + value)
		if len(vars) != 1 || vars[0].Key != key {
			return nil, fmt.Errorf("invalid env key %q — use [A-Za-z_][A-Za-z0-9_]*", key)
		}
		pairs = append(pairs, envSetPair{key, value, appendMode})
	}
	return pairs, nil
}

// runEnvSet pulls the remote content, applies the upsert (plain pairs
// replace in place — later duplicates dropped; append pairs add to the
// comma-list idempotently; new keys append), installs atomically, and
// optionally restarts. Values never print — the fingerprint table masks.
func runEnvSet(d Deps, ctx context.Context, target Target, pairs []envSetPair, restart bool) error {
	remote := target.Project.EnvFile
	// Missing file = a fresh estate (empty start). UNREADABLE file must
	// FAIL CLOSED — an empty read here would clobber the real env with
	// only the new pairs — so the sentinel distinguishes missing from
	// error.
	const noFileSentinel = "__kampodra_no_env_file__"
	current, err := d.Runner.Run(ctx, target.HostSpec, fmt.Sprintf("if [ -f %s ]; then cat %s; else printf '%s\n'; fi", remote, remote, noFileSentinel))
	if err != nil {
		return fmt.Errorf("cannot read the remote env file: %w", err)
	}
	if strings.TrimSpace(current) == noFileSentinel {
		current = ""
	}
	apply := func(existing string, sp envSetPair) string {
		if !sp.appendMode {
			return sp.value
		}
		vals := []string{}
		for _, v := range strings.Split(existing, ",") {
			if v != "" && v != sp.value {
				vals = append(vals, v)
			}
		}
		return strings.Join(append(vals, sp.value), ",")
	}
	var lines []string
	for _, v := range envfile.Parse(current) {
		line := v.Key + "=" + v.Value
		for _, sp := range pairs {
			if sp.key == v.Key {
				line = v.Key + "=" + apply(v.Value, sp)
			}
		}
		lines = append(lines, line)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		seen[strings.SplitN(line, "=", 2)[0]] = true
	}
	for _, sp := range pairs {
		if !seen[sp.key] {
			lines = append(lines, sp.key+"="+apply("", sp))
		}
	}
	merged := strings.Join(lines, "\n") + "\n"
	fmt.Fprint(d.Stdout, envfile.Table(merged))
	if err := installRemoteEnv(d, ctx, target, merged); err != nil {
		return err
	}
	if restart {
		return runDeployRestart(d, ctx, target)
	}
	fmt.Fprintf(d.Stdout, "[env] restart to apply: kampodra env set --restart, ssh %s 'rc-service %s restart', or kampodra deploy\n", target.HostSpec.Host, target.Project.Container)
	return nil
}

// newEnvPullSub is env's `pull` subcommand: raw payload to stdout/--out
// (0600), masked fingerprint summary follows.
func newEnvPullSub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "raw payload to stdout/--out (0600); masked fingerprint summary follows",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolveTargetFromFlags(d, c)
			if err != nil {
				return err
			}
			// stdout IS the payload here (`env pull | wc -l`), so the
			// provenance line rides stderr with the masked summary.
			sayProvenance(Deps{Stdout: d.Stderr}, "env", target)
			if err := requireHost(target); err != nil {
				return err
			}
			remote := target.Project.EnvFile
			raw, err := fetchRemoteEnv(d, c.Context(), target, remote)
			if err != nil {
				return err
			}
			payload := strings.TrimRight(raw, "\n") + "\n"
			if out, _ := c.Flags().GetString("out"); out != "" {
				return writeSecretFile0600(d, out, payload,
					"fingerprint summary below (payload went to the file, stdout is free):")
			}
			// stdout IS the payload (pipe into whatever needs the values);
			// the human-readable summary goes to stderr, masked.
			fmt.Fprint(d.Stdout, payload)
			fmt.Fprintf(d.Stderr, "[env] fingerprint summary for %s on %s (payload above on stdout):\n", remote, target.HostSpec.Host)
			fmt.Fprint(d.Stderr, envfile.Table(payload))
			return nil
		},
	}
	remoteTargetFlags(cmd)
	cmd.Flags().String("out", "", "write the raw payload to this file (0600) instead of stdout")
	return cmd
}

// newEnvFingerprintSub is env's `fingerprint` subcommand: preview the
// masking for a LOCAL file / stdin (never values).
func newEnvFingerprintSub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fingerprint",
		Short: "preview the masking for a LOCAL file / stdin (never values)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var content string
			if file, _ := c.Flags().GetString("file"); file != "" {
				data, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("no such file: %s", file)
				}
				content = string(data)
			} else {
				data, err := io.ReadAll(d.Stdin)
				if err != nil {
					return fmt.Errorf("cannot read stdin: %w", err)
				}
				content = string(data)
			}
			fmt.Fprint(d.Stdout, envfile.Table(content))
			return nil
		},
	}
	cmd.Flags().String("file", "", "local env file to preview (default: stdin)")
	return cmd
}

// newEnvDiffSub is env's `diff` subcommand: fingerprint-level diff of a
// local env file vs the remote one (exit 1 = differs — GNU convention).
func newEnvDiffSub(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <local-file>",
		Short: "fingerprint-level diff of a local env file vs the remote one (exit 1 = differs)",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("diff requires exactly one <local-file> argument (--help)")
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			localPath := args[0]
			localData, err := os.ReadFile(localPath)
			if err != nil {
				return fmt.Errorf("no such file: %s", localPath)
			}
			target, err := resolveTargetFromFlags(d, c)
			if err != nil {
				return err
			}
			sayProvenance(d, "env", target)
			if err := requireHost(target); err != nil {
				return err
			}
			remote := target.Project.EnvFile
			raw, err := fetchRemoteEnv(d, c.Context(), target, remote)
			if err != nil {
				return err
			}
			entries := envfile.Diff(envfile.Parse(string(localData)), envfile.Parse(raw))
			fmt.Fprintf(d.Stdout, "== env diff: %s vs %s:%s ==\n", localPath, target.HostSpec.Host, remote)
			fmt.Fprint(d.Stdout, envfile.RenderDiff(entries))
			for _, e := range entries {
				if e.Kind != envfile.KindUnchanged {
					// GNU diff convention: differences found.
					return &exitError{code: 1}
				}
			}
			return nil
		},
	}
	remoteTargetFlags(cmd)
	return cmd
}

// newEnvFromSchemaCommand is kampodra-NATIVE (beyond the frozen spec): the
// deploy.sh varlock block, surfaced as its own subcommand — generate the
// env file from the repo's COMMITTED .env.schema (pass() refs resolve via
// the repo's varlock + the pass store; values NEVER echo), then write it
// locally (--out, 0600) or push it through the env family's flow.
func newEnvFromSchemaCommand(d Deps) *cobra.Command {
	sub := &cobra.Command{
		Use:   "from-schema [--repo <path>] [--schema apps/api/.env.schema] [--out <file>]",
		Short: "kampodra-native: generate the env file from the repo's committed .env.schema via varlock (fingerprints only), write locally or push",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runEnvFromSchema(d, c)
		},
	}
	sub.Flags().String("repo", ".", "the app repo (must contain node_modules/.bin/varlock and the committed schema)")
	sub.Flags().String("schema", "apps/api/.env.schema", "schema path relative to the repo root")
	sub.Flags().String("out", "", "write the generated env file here (0600) instead of pushing")
	return sub
}

// writeFile0600 writes content to path with 0600-from-creation semantics
// (pre-created 0600, chmod after close so the umask can never widen it) —
// the single place secret/db payloads touch local disk.
func writeFile0600(path string, content []byte) error {
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("cannot write %s", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("cannot write %s", path)
	}
	return nil
}

// writeSecretFile0600 writes secret content to out with 0600-from-creation
// semantics and prints the fingerprint table — the single place secret
// payloads touch disk, so the masking invariant lives here, not in each
// caller.
func writeSecretFile0600(d Deps, out, content, wroteNote string) error {
	if err := writeFile0600(out, []byte(content)); err != nil {
		return err
	}
	fmt.Fprintf(d.Stdout, "[env] wrote %s (0600) — %s\n", out, wroteNote)
	fmt.Fprint(d.Stdout, envfile.Table(content))
	return nil
}

func runEnvFromSchema(d Deps, c *cobra.Command) error {
	repoFlag, _ := c.Flags().GetString("repo")
	schema, _ := c.Flags().GetString("schema")
	out, _ := c.Flags().GetString("out")

	repoRoot := repoFlag
	if resolved, err := gitRepoRoot(repoRoot); err == nil {
		repoRoot = resolved
	} else if repoFlag == "." {
		return fmt.Errorf("no git repo at . — pass --repo <path>")
	}

	target, err := resolveTargetFromFlags(d, c)
	if out == "" {
		if err != nil {
			return err
		}
		if err := requireHost(target); err != nil {
			return err
		}
		sayProvenance(d, "env", target)
	}

	fmt.Fprintf(d.Stdout, "[env] generating from %s in %s (committed schema; secrets resolve from the pass store — never echoed)\n", schema, repoRoot)
	body, err := envschema.Generate(c.Context(), repoRoot, schema, target.Project.EnvClearKeys)
	if err != nil {
		return err
	}

	if out != "" {
		return writeSecretFile0600(d, out, body,
			"fingerprint summary (values NEVER printed):")
	}

	remote := target.Project.EnvFile
	fmt.Fprintf(d.Stdout, "[env] pushing generated env -> %s:%s (fingerprint summary below; values are NEVER printed)\n",
		target.HostSpec.Host, remote)
	fmt.Fprint(d.Stdout, envfile.Table(body))
	tmp := envfile.RemoteTmpPath(remote, os.Getpid())
	if _, err := d.Runner.RunWithStdin(c.Context(), target.HostSpec, "umask 077; cat > "+tmp, strings.NewReader(body)); err != nil {
		return fmt.Errorf("upload failed")
	}
	if _, err := d.Runner.Run(c.Context(), target.HostSpec, fmt.Sprintf("chmod 600 %s && mv -f %s %s", tmp, tmp, remote)); err != nil {
		return fmt.Errorf("atomic install failed (remote temp left at: %s)", tmp)
	}
	fmt.Fprintf(d.Stdout, "[env] installed %s (0600) on %s\n", remote, target.HostSpec.Host)
	fmt.Fprintf(d.Stdout, "[env] restart to apply: ssh %s 'rc-service %s restart'   # or: kampodra deploy\n", target.HostSpec.Host, target.Project.Container)
	return nil
}

// gitRepoRoot resolves the repo root via git (the shell's
// `git rev-parse --show-toplevel`).
func gitRepoRoot(dir string) (string, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveEnvTarget is the env family's target resolution: the standard
// flag > profile > env ladder over the three shared flags.
func resolveTargetFromFlags(d Deps, c *cobra.Command) (Target, error) {
	host, _ := c.Flags().GetString("host")
	key, _ := c.Flags().GetString("ssh-key")
	profile, _ := c.Flags().GetString("profile")
	cfg, err := state.LoadConfig(d.Home)
	if err != nil {
		return Target{}, err
	}
	mf, err := d.manifestFor()
	if err != nil {
		return Target{}, err
	}
	return ResolveTarget(cfg, host, key, profile, mf, d.Env)
}

// requireHost is the shell scripts' require_host: a missing target is an
// error naming every resolution route.
func requireHost(target Target) error {
	if target.HostSpec.Host == "" {
		return fmt.Errorf("target required: --host root@<ip>, --profile <name>, KAMPODRA_PROFILE, config defaultProfile, or KAMPODRA_HOST=root@<ip> (resolution matches deploy.sh)")
	}
	return nil
}

// fetchRemoteEnv is the shell's fetch_remote_env: `cat <remote>` with a
// failure message naming the remedy (no env file yet?).
func fetchRemoteEnv(d Deps, ctx context.Context, target Target, remote string) (string, error) {
	raw, err := d.Runner.Run(ctx, target.HostSpec, "cat "+remote)
	if err != nil {
		return "", fmt.Errorf("cannot read %s on %s (no env file yet? run: kampodra env push --file <env>, or kampodra deploy)", remote, target.HostSpec.Host)
	}
	return raw, nil
}
