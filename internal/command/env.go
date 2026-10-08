package command

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/talha7k/kampodra/internal/adapter/envfile"
	"github.com/talha7k/kampodra/internal/adapter/state"
)

// envHelp is the shell env.sh usage heredoc (byte-equal for the shell-era
// lines); the kampodra-native diff line documents the added surface.
const envHelp = `Usage:
  kampodine env list [--host <user@ip>] [--profile <name>] [--ssh-key <path>]             # KEY + fingerprint table (NEVER values)
  kampodine env push --file <local-env-file> [...] [--profile <name>]                     # upload: 0600 temp + atomic mv + restart hint
  kampodine env pull [--out <file>] [...] [--profile <name>]                              # raw payload to stdout/--out (0600); masked summary follows
  kampodine env fingerprint [--file <f>]                                                  # preview the masking for a LOCAL file / stdin (never values)
  kampodra env diff <local-file> [--host <user@ip>] [--profile <name>] [--ssh-key <path>] # fingerprint-level local vs remote (exit 1 = differs)

Fingerprints NEVER leak values: every line is KEY + value length + first 2 chars.
Raw values move only in push's upload stream and pull's stdout/--out payload.
Host/key resolution matches deploy.sh: --host | --profile <name> |
KAMPODINE_PROFILE | config defaultProfile | KAMPODINE_HOST; --ssh-key |
profile sshKey | KAMPODINE_SSH_KEY | ssh-agent / ~/.ssh/config.

Examples:
  kampodine env list --host root@203.0.113.10
  kampodine env push --file ./ops/env.production --host root@203.0.113.10
  kampodine env pull --out ./env.snapshot --host root@203.0.113.10   # written 0600
  kampodine env pull --host root@203.0.113.10 | wc -l                # raw payload on stdout, summary on stderr
  kampodine env fingerprint --file ./.env.local                      # preview masking, values never leave stdin
  kampodra env diff ./ops/env.production --host root@203.0.113.10    # what would this push change?
`

// envDefaultRemotePath is ENV_FILE_REMOTE's default (/etc/kampodine/env,
// 0600 root — the shell's constant); KAMPODINE_ENV_REMOTE overrides.
const envDefaultRemotePath = "/etc/kampodine/env"

func envRemotePath(lookup func(string) (string, bool)) string {
	if v, ok := lookup("KAMPODINE_ENV_REMOTE"); ok && v != "" {
		return v
	}
	return envDefaultRemotePath
}

func newEnvCommand(d Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "remote app env file (/etc/kampodine/env, 0600): list | push | pull | fingerprint | diff — values NEVER printed, fingerprints only",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Fprint(c.OutOrStdout(), envHelp)
			return nil
		},
	}
	cmd.SetHelpFunc(func(c *cobra.Command, _ []string) {
		fmt.Fprint(c.OutOrStdout(), envHelp)
	})

	envFlags := func(c *cobra.Command) {
		c.Flags().String("host", "", "target VM (user@ip or ssh-config alias) — beats KAMPODINE_HOST and any profile")
		c.Flags().String("profile", "", "per-instance profile (~/.kampodine/config.json) — beats KAMPODINE_PROFILE / defaultProfile")
		c.Flags().String("ssh-key", "", "identity file — beats KAMPODINE_SSH_KEY; empty = agent / ssh config")
	}
	resolve := func(c *cobra.Command) (Target, error) {
		host, _ := c.Flags().GetString("host")
		key, _ := c.Flags().GetString("ssh-key")
		profile, _ := c.Flags().GetString("profile")
		cfg, err := state.LoadConfig(d.Home)
		if err != nil {
			return Target{}, err
		}
		return ResolveTarget(cfg, host, key, profile, d.Env)
	}

	subList := &cobra.Command{
		Use:   "list",
		Short: "KEY + fingerprint table of the remote env file (NEVER values)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolve(c)
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			raw, err := fetchRemoteEnv(d, c.Context(), target, envRemotePath(d.Env))
			if err != nil {
				return err
			}
			fmt.Fprint(d.Stdout, envfile.Table(raw))
			return nil
		},
	}
	envFlags(subList)

	subPush := &cobra.Command{
		Use:   "push",
		Short: "upload a local env file: 0600 temp + atomic mv + restart hint (values NEVER printed)",
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
			target, err := resolve(c)
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			remote := envRemotePath(d.Env)
			fmt.Fprintf(d.Stdout, "[env] pushing %s -> %s:%s (fingerprint summary below; values are NEVER printed)\n",
				file, target.HostSpec.Host, remote)
			fmt.Fprint(d.Stdout, envfile.Table(string(data)))

			// 0600 FROM CREATION remotely (umask 077) + atomic mv within the
			// same directory — busybox-safe, byte-equal to the shell flow.
			tmp := filepath.Join(filepath.Dir(remote), fmt.Sprintf("env.tmp.%d", os.Getpid()))
			if _, err := d.Runner.RunWithStdin(c.Context(), target.HostSpec, "umask 077; cat > "+tmp, strings.NewReader(string(data))); err != nil {
				return fmt.Errorf("upload failed")
			}
			if _, err := d.Runner.Run(c.Context(), target.HostSpec, fmt.Sprintf("chmod 600 %s && mv -f %s %s", tmp, tmp, remote)); err != nil {
				return fmt.Errorf("atomic install failed (remote temp left at: %s)", tmp)
			}
			fmt.Fprintf(d.Stdout, "[env] installed %s (0600) on %s\n", remote, target.HostSpec.Host)
			fmt.Fprintf(d.Stdout, "[env] restart to apply: ssh %s 'rc-service kampodine-api restart'   # or: kampodra deploy\n", target.HostSpec.Host)
			return nil
		},
	}
	envFlags(subPush)
	subPush.Flags().String("file", "", "local env file to upload (verbatim — what you push is what lands)")

	subPull := &cobra.Command{
		Use:   "pull",
		Short: "raw payload to stdout/--out (0600); masked fingerprint summary follows",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			target, err := resolve(c)
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			remote := envRemotePath(d.Env)
			raw, err := fetchRemoteEnv(d, c.Context(), target, remote)
			if err != nil {
				return err
			}
			payload := strings.TrimRight(raw, "\n") + "\n"
			if out, _ := c.Flags().GetString("out"); out != "" {
				f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
				if err != nil {
					return fmt.Errorf("cannot write %s", out)
				}
				if _, err := f.WriteString(payload); err != nil {
					f.Close()
					return fmt.Errorf("cannot write %s", out)
				}
				if err := f.Close(); err != nil {
					return fmt.Errorf("cannot write %s", out)
				}
				if err := os.Chmod(out, 0o600); err != nil {
					return fmt.Errorf("cannot write %s", out)
				}
				fmt.Fprintf(d.Stdout, "[env] wrote %s (0600) — fingerprint summary below (payload went to the file, stdout is free):\n", out)
				fmt.Fprint(d.Stdout, envfile.Table(payload))
				return nil
			}
			// stdout IS the payload (pipe into whatever needs the values);
			// the human-readable summary goes to stderr, masked.
			fmt.Fprint(d.Stdout, payload)
			fmt.Fprintf(d.Stderr, "[env] fingerprint summary for %s on %s (payload above on stdout):\n", remote, target.HostSpec.Host)
			fmt.Fprint(d.Stderr, envfile.Table(payload))
			return nil
		},
	}
	envFlags(subPull)
	subPull.Flags().String("out", "", "write the raw payload to this file (0600) instead of stdout")

	subFingerprint := &cobra.Command{
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
	subFingerprint.Flags().String("file", "", "local env file to preview (default: stdin)")

	subDiff := &cobra.Command{
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
			target, err := resolve(c)
			if err != nil {
				return err
			}
			if err := requireHost(target); err != nil {
				return err
			}
			remote := envRemotePath(d.Env)
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
	envFlags(subDiff)

	cmd.AddCommand(subList, subPush, subPull, subFingerprint, subDiff)
	return cmd
}

// requireHost is the shell scripts' require_host: a missing target is an
// error naming every resolution route.
func requireHost(target Target) error {
	if target.HostSpec.Host == "" {
		return fmt.Errorf("target required: --host root@<ip>, --profile <name>, KAMPODINE_PROFILE, config defaultProfile, or KAMPODINE_HOST=root@<ip> (resolution matches deploy.sh)")
	}
	return nil
}

// fetchRemoteEnv is the shell's fetch_remote_env: `cat <remote>` with a
// failure message naming the remedy (no env file yet?).
func fetchRemoteEnv(d Deps, ctx context.Context, target Target, remote string) (string, error) {
	raw, err := d.Runner.Run(ctx, target.HostSpec, "cat "+remote)
	if err != nil {
		return "", fmt.Errorf("cannot read %s on %s (no env file yet? run: kampodine env push --file <env>, or kampodra deploy)", remote, target.HostSpec.Host)
	}
	return raw, nil
}
