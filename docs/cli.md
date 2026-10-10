# kampodra CLI reference

Every `kampodra` command, its flags, defaults, and behavior — matched
against the source of truth (the Cobra definitions in
`internal/command/`). For the zero-to-first-deploy walkthrough see
[GETTING_STARTED.md](GETTING_STARTED.md); for internal design see
[ARCHITECTURE.md](ARCHITECTURE.md). Every command supports `--help`; bare
`kampodra` prints the command index; an unknown command names the offender,
reprints the index on stderr, and **exits 2** (all other failures exit 1
unless noted). No command prompts — gates are flags (`--yes`, `--force`).

**Contents:** [deploy](#deploy) · [env](#env) · [ssh](#ssh) ·
[config](#config) · [backup](#backup) · [vm-prepare](#vm-prepare) ·
[vm-wipe](#vm-wipe) · [metrics](#metrics) · [dns](#dns) ·
[migrate](#migrate) · [status](#status) ·
[Target & config resolution](#target--config-resolution) ·
[kampodra.json](#kampodrajson) · [Files on disk](#files-on-disk) ·
[Not yet ported](#not-yet-ported)

## deploy

The full pipeline: clean-tree gate → `podman build` (`--build-arg
GIT_SHA=<sha>`, sha tag from `git rev-parse`) → `podman save | ssh podman
load` (registry-free stream) → env push → init-aware restart → VM-side
health gate (the served sha must match the deployed tag) → kamal-proxy
re-point → public smoke → ledger append → disk report → keep-set image
cleanup. `--rollback` flips the tag instantly; `--rolling` is the opt-in
zero-downtime shadow-container double re-point.

```
kampodra deploy [--host root@<ip>] [--profile <name>] [--sha <sha7>] [--rollback [<sha7>]] [--rolling]
                 [--drain-timeout <s>] [--dockerfile <path>] [--env-file <path>] [--ssh-key <path>]
                 [--skip-smoke] [--disk-threshold <pct>] [--binary [<name>]]
```

| flag | type | default | description |
|---|---|---|---|
| `--host` | string | `""` | target VM (`user@ip` or ssh-config alias) — beats `KAMPODRA_HOST` and any profile |
| `--profile` | string | `""` | instance profile (`~/.kampodra/config.json`) — beats `KAMPODRA_PROFILE` / `defaultProfile` |
| `--ssh-key` | string | `""` | identity file — beats `KAMPODRA_SSH_KEY`; empty = ssh agent / config |
| `--dockerfile` | string | `""` | Containerfile/Dockerfile to build (context = repo root); default: project config `dockerfile`, else `Dockerfile` |
| `--sha` | string | `""` | stream an EXISTING local build of this sha (sha7 required, never inferred; skips the clean-tree gate) |
| `--rollback` | string | `""` | instant image-tag rollback: explicit sha, else the VM's deployed-sha stamp, else die (never git HEAD); bare `--rollback` = stamp-resolved |
| `--rolling` | bool | `false` | zero-downtime shadow-container double re-point |
| `--drain-timeout` | int | `10` | seconds to wait for the proxy switch to confirm (`--rolling` only) |
| `--disk-threshold` | string | `""` | fail closed BEFORE the build when VM disk usage ≥ pct (0–100) |
| `--binary` | string | `""` | fast-push a declared artifact (kampodra.json `"binary"`) instead of the image: local cross-build → atomic remote swap → restart → the SAME identity gates (served-sha health gate, public smoke, stamp, ledger). Bare `--binary` picks the lone block; `--binary <name>` (space form) picks among several. Exclusive with `--sha`/`--rolling` |

### deploy --binary (the fast path)

`kampodra.json`'s `"binary"` block declares one artifact per stack — e.g.
the Go api AND the TS api during a cutover:

```json
{
  "binary": {
    "api-go": { "kind": "go",   "dir": "/data/app", "buildDir": "apps/api-go",
                "target": "./cmd/server", "entry": "esellar-api-go",
                "imagePath": "/usr/local/bin/esellar-api-go" },
    "api-ts": { "kind": "node", "dir": "/data/app", "buildDir": "apps/api",
                "entry": "src/serve-node.ts", "exec": "tsx",
                "imagePath": "/app/apps/api", "depsPath": "/app/node_modules" },
    "api-rs": { "kind": "rust", "dir": "/data/app", "buildDir": "crates/api",
                "target": "esellar-api", "entry": "esellar-api",
                "imagePath": "/usr/local/bin/esellar-api" }
  }
}
```

- `vm-prepare --binary <name>` renders the app unit with that artifact's
  bind mount (`-v <dir>:<dir>`) and its exec override (`<dir>/<entry>`,
  or `<exec> <dir>/<entry>` for node). The repo's LONE block needs no
  name; several blocks require the flag. `vm-prepare` also creates the
  dir and, for node, symlinks `<dir>/node_modules` at `depsPath` — the
  mount carries SOURCE only; dependencies stay baked in the image.
- **go/rust**: a sha-stamped cross-build (go: `CGO_ENABLED=0 GOOS=linux
  GOARCH=arm64`, same ldflags as the image; rust: `cargo build --release
  --target <triple>`, default `aarch64-unknown-linux-musl` — the golden
  images are musl-based), then a single-file scp.
- **node**: no compile — the source tree is tarred (`node_modules`
  excluded), extracted into a staging sibling, and the whole dir swaps.
- The swap is atomic (staging and run path share a filesystem), guarded
  by a remote mkdir lock (a concurrent push dies naming it), and keeps
  the previous artifact at `<run>.prev` plus a `<run>.prev.sha` marker.
- **Safety rails**: build failure touches nothing; the dependency-drift
  guard (node) refuses when the dep manifest hash differs from the
  image-stamped `.deps-sha` marker (the fast push is CODE-only —
  dependency changes redeploy the image); a failed health gate or public
  smoke AUTO-RESTORES the previous artifact, restarts, and re-verifies
  before reporting the failure; `--binary --rollback` restores the
  recorded previous artifact (marker-verified; only the last push is
  undoable — image rollback stays `kampodra deploy --rollback`).
- Image deploys keep the mount honest: every image deploy mirrors the
  image's artifact into the mount (`podman cp` from a throwaway create)
  so the unit's exec override never serves the last fast-pushed version.
  Multiple blocks: pin the VM's artifact with `vm-prepare --binary <name>`.

Deploy also builds+streams every sidecar declared in the repo manifest's
`images.sidecars` block (deploy mode only — `--rollback`/`--sha` never
touch sidecars): same platform + `GIT_SHA` identity as the primary,
landed on the VM as `<imagePrefix>-<name>:latest`, fail-closed verified
after the load.
| `--env-file` | string | `""` | push this env file (0600 + atomic mv, sha-stamped) before restart |
| `--skip-smoke` | bool | `false` | skip the public smoke check |

```bash
kampodra deploy --host root@203.0.113.10
kampodra deploy --host root@203.0.113.10 --rolling --env-file ./ops/env.production
kampodra deploy --rollback                      # stamp-resolved
kampodra deploy --rollback <sha7>               # explicit target
kampodra deploy --sha <sha7> --skip-smoke   # stream an existing build
kampodra deploy --disk-threshold 85
kampodra deploy --binary                     # fast-push the lone artifact
kampodra deploy --binary api-go              # pick among several
kampodra deploy --binary api-go --rollback   # restore the recorded .prev
```

- Modes are exclusive: `--rollback` with `--sha`, or with `--rolling`,
  is an error; `--drain-timeout` requires `--rolling`.
- Deploy mode requires a git repo (sha from `git rev-parse`) and a clean
  tree; not a repo → use `--sha <sha7>`.
- Rollback resolution: explicit sha > the VM's deployed-sha stamp file >
  **die** — it never falls back to git HEAD. When the target image still
  exists on the VM the build/stream is skipped entirely; the stamp is never
  rewritten.
- Rolling failure paths keep the safer state: a bad shadow never took
  traffic; a completed switch leaves the shadow serving — finish with
  `deploy converge`.

All subcommands are target-aware (`--host` / `--profile` / `--ssh-key`,
same resolution as `deploy`).

### deploy converge

Finish an interrupted `--rolling` deploy: retag → restart → health gate →
re-point to the main container → remove the shadow.

```
kampodra deploy converge [--drain-timeout <s>] [<sha7>]
```

`--drain-timeout` (int, default `10`) carries the drain budget from the
interrupted deploy. The sha comes from the argument (must be a git sha
fragment), else the shadow's own image — never HEAD. A leftover shadow
must be converged before the next `--rolling`.

### deploy list

Deployment history: the VM's sha-tagged images (running one marked) merged
with the local ledger, with a `Total deployments` footer.
`--all-profiles` (bool, default `false`) renders one sorted section per
configured profile; `--group <g>` renders one per profile in that group
(exclusive with `--all-profiles`). Same-tag rows merge to one row showing
the NEWEST deploy timestamp (image build time is the fallback when no
ledger entry exists). An unreachable VM degrades loudly to a
ledger-only view.

```
kampodra deploy list [--all-profiles] [--group <g>]
```

### deploy prune

Reclaim VM disk: remove old sha-tagged images. Always kept: the running
image, `ts-rollback`, and the newest `--keep N`. Refuses to prune if the
image listing over ssh fails; reports disk usage after pruning and warns
above 90%.

```
kampodra deploy prune [--keep N] [--dry-run]
```

`--keep` (string, default `2`) is the newest-N keep count (non-negative
integer); `--dry-run` (bool, default `false`) prints the exact `podman rmi`
commands and removes nothing.

### deploy logs

Tail the running app container's logs. `--lines` (string, default `100`,
positive integer) bounds the tail; `--follow` (bool, default `false`)
streams until ctrl-c (clean exit, remote exit code propagates).

```
kampodra deploy logs [--lines N] [--follow]
```

### deploy restart

Restart the app service — init-aware (`rc-service` on Alpine/OpenRC,
`systemctl` on systemd hosts), then a best-effort status check.

```
kampodra deploy restart
```

### deploy shell

Interactive `sh` inside the running app container; `exec -- <cmd>...`
runs a one-shot command instead (POSIX-sh-quoted argv; the command vector
must follow `--`, and the remote exit code propagates).

```
kampodra deploy shell [exec -- <cmd>...]      # e.g. exec -- df -h /var/lib/containers
```

## env

The remote app env file (project config `envFile`, 0600): inspect and
push/pull it. Values are **never printed** — every listing is a
fingerprint: KEY + value length + first 2 chars. Raw values move only in
`push`'s upload stream and `pull`'s stdout/`--out` payload.

```
kampodra env list            [--host root@<ip>] [--profile <name>] [--ssh-key <path>]
kampodra env push --file <local-env-file> [...]    # 0600 temp + atomic mv + restart hint
kampodra env pull [--out <file>] [...]             # raw payload to stdout/--out (0600)
kampodra env fingerprint [--file <f>]              # preview masking for a LOCAL file / stdin
kampodra env diff <local-file> [...]               # fingerprint-level diff (exit 1 = differs)
kampodra env from-schema [--repo <path>] [--schema <path>] [--out <file>]
```

```bash
kampodra env list --host root@203.0.113.10
kampodra env push --file ./ops/env.production --host root@203.0.113.10
kampodra env pull --out ./env.snapshot --host root@203.0.113.10   # written 0600
kampodra env diff ./ops/env.production --host root@203.0.113.10
```

| flag | type | default | description |
|---|---|---|---|
| `--file` (push) | string | `""` | **required**; local env file uploaded verbatim; must contain KEY=VALUE lines |
| `--out` (pull) | string | `""` | write the raw payload here (0600) instead of stdout |
| `--file` (fingerprint) | string | `""` | local file to preview; default: stdin |
| `--repo` (from-schema) | string | `.` | the app repo (needs `node_modules/.bin/varlock` + the committed schema; non-git dir needs an explicit `--repo`) |
| `--schema` (from-schema) | string | `apps/api/.env.schema` | schema path relative to the repo root |
| `--out` (from-schema) | string | `""` | write the generated file here (0600) instead of pushing |

`list`/`push`/`pull`/`diff`/`from-schema` are target-aware (`--host` /
`--profile` / `--ssh-key`). `from-schema` (kampodra-native) generates the
env file from the repo's committed `.env.schema` via varlock (secret refs
resolve from the pass store; values never echo), then writes it locally or
pushes it through the same flow.

- `push` installs 0600 remotely via `umask 077` + atomic `mv` and prints
  the restart hint; `pull --out` and `from-schema --out` create the file
  0600.
- `env diff` takes exactly one positional `<local-file>` and exits 1 when
  local and remote differ (GNU diff convention).

## ssh

Host-level ssh passthrough through the resolved profile host/key
(kampodra-native). No command = interactive login shell (forced tty); with
a command the argv passes through verbatim and ssh's exit code propagates.

```
kampodra ssh [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [<cmd>...]
```

```bash
kampodra ssh                                  # login shell on the default profile's host
kampodra ssh --host root@203.0.113.10 df -h   # one-shot, argv verbatim
```

- Flags are not reinterpreted after the first non-flag argument — the rest
  of the argv belongs to the remote command.

## config

Per-instance profiles in `~/.kampodra/config.json` (dir 0700, file 0600).
Flat — no inheritance: a profile owns its values; `group` labels a fleet
(`config list --group`, `status --group`, `deploy list --group`); `clone`
stamps out siblings with explicit copies. `sshKey` is stored as a path
only; secrets never live in the config. The first profile auto-becomes the
default.

```
kampodra config init --name <name> --host <user@ip> --ssh-key <path> [--proxy-host <host>] [--group <g>] [--set-default] [--force]
kampodra config list [--group <g>]
kampodra config show [--profile <name>]
kampodra config print [--profile <name>]
kampodra config set-default <name>
kampodra config remove <name> [--force]
kampodra config clone <src> <new> --host <user@ip> [--ssh-key <path>] [--proxy-host <host>] [--group <g>] [--set-default] [--force]
```

| flag | type | default | description |
|---|---|---|---|
| `--name` | string | `""` | profile name (`init`, required) — `[A-Za-z0-9][A-Za-z0-9_-]*`, max 64 |
| `--host` | string | `""` | target VM for `init`; **required for `clone`** (a clone targets a different instance) |
| `--ssh-key` | string | `""` | identity file PATH for `init` (tilde expanded; path only; `clone`: overrides the copied value) |
| `--proxy-host` | string | `""` | public TLS edge hostname for `init` (`clone`: overrides) |
| `--group` | string | `""` | fleet label — `init`/`clone` set it; `list`/`status`/`deploy list --group` filter by it |
| `--profile` | string | `""` | profile for `show` / `print` (default: `defaultProfile`) |
| `--set-default` | bool | `false` | make the profile the default at `init`/`clone` |
| `--force` | bool | `false` | `init`/`clone`: overwrite an existing profile; `remove`: remove the LAST profile |

```bash
kampodra config init --name prod --host root@203.0.113.10 \
  --ssh-key ~/.ssh/id_ed25519 --proxy-host app.example.com --set-default
kampodra config clone prod prod-2 --host root@203.0.113.11   # fleet sibling
kampodra config print                # effective project config + per-field provenance
```

- `config print` renders the fully resolved effective project config with
  the winning layer of every field (flag/env/profile/repo-file/default) —
  the debugging tool for the [resolution ladder](#target--config-resolution).
- Re-init (`--force`) refreshes the flat fields and keeps the tooling-owned
  cached init verdict unless host/ssh-key changed; the profile's `project`
  block always survives (`config init` never manages it).
- Removing the last profile requires `--force`; removing the default
  reassigns it sorted-first.

## backup

OCI Object Storage backups. Auth is the `oci` CLI's own — kampodra passes
no auth flags unless the instance profile's `cloud` block overrides it
(`{"profile": "…", "compartment": "…", "namespace": "…", "instancePrincipal": true}`);
`OCI_PROFILE` / `OCI_COMPARTMENT` / `OCI_NAMESPACE` env are the escape
hatch. kampodra never accepts, stores, or logs credential material. The
bucket resolves `--bucket` > `KAMPODRA_BUCKET` > project config. The
tenancy namespace resolves `--namespace` > cloud block / `OCI_NAMESPACE` >
`oci os ns get` and is passed explicitly to every `os object` call — the
oci CLI's internal namespace resolution fails on user-principal laptop
configs ("Unable to retrieve namespace internally"). LIST denied by policy
is not fatal for `download`/`verify` (GET works with just the object name);
`list` prints the exact policy shape when denied (exit 1).

```
kampodra backup list [--prefix <prefix>] [--bucket <name>] [--namespace <ns>]
kampodra backup download <object> [--out <file>] [--bucket <name>] [--namespace <ns>]
kampodra backup verify <local.db|.tgz> [--host <user@ip>] [--ssh-key <path>] [--migrated-topology]
kampodra backup restore-plan <object> [--bucket <name>]
```

| flag | type | default | description |
|---|---|---|---|
| `--bucket` | string | `""` | object-storage bucket (default: project config bucket; `KAMPODRA_BUCKET` overrides) |
| `--namespace` | string | `""` | tenancy object-storage namespace (default: resolved via `oci os ns get`; `OCI_NAMESPACE` / the profile cloud block `namespace` override) |
| `--prefix` | string | `""` | object name prefix (`list`) |
| `--out` | string | `""` | download destination (default: the object's basename) |
| `--host` | string | `""` | VM target for `verify` |
| `--ssh-key` | string | `""` | identity file for `verify` |
| `--migrated-topology` | bool | `false` | `verify`: pass `restore-verify`'s drill flag through (bulk-migration chain shape → warnings) |

```bash
kampodra backup download tenants/20261008T050000Z.db --out /tmp/restore.db
kampodra backup verify /tmp/restore.db --host root@203.0.113.10
kampodra backup verify ./bundle.tgz            # every .db member, read-only
kampodra backup restore-plan tenants/20261008T050000Z.db
```

- `verify` takes a `.db`/`.sqlite`/`.sqlite3` file or a `.tgz`/`.tar.gz`
  archive. Each db member is uploaded to a per-member VM scratch file
  (`/tmp/kampodra-verify-<n>-<random>.db`) and checked by the VM's
  `/usr/local/bin/restore-verify -db <path>` — read-only; the scratch file
  is always removed and the data dir is never touched.
  `--migrated-topology` is passed through to `restore-verify` untouched.
- In a `.tgz`, members are verified sorted; a member named `root.db` inside
  a `root/` directory is **skipped** (schema-only — not in restore-verify
  scope).
- `verify` needs a VM target: `--host` or the standard profile ladder
  (`--profile` > `KAMPODRA_PROFILE` / config `defaultProfile`). Cloud auth
  (`verify` has no cloud leg) follows the OCI rules above.
- `download` writes the file 0600 from creation and verifies its sha256/md5
  against the object's digest metadata (mismatch → exit 1; no metadata →
  verification is skipped with a note).
- `restore-plan` only PRINTS the documented stop/snapshot/swap/start
  sequence for one object — kampodra never executes it.

## vm-prepare

First-run bootstrap of a bare **Alpine** host (idempotent — safe to
re-run): sanity gates (UEFI boot, no systemd anywhere, OpenRC tooling),
sshd hardening (ensured + gated), the podman stack (netavark), the state
dir + insecure-registry config, unprivileged-port sysctl, OpenRC services
for the app container + kamal-proxy, and the `kampodra-anchor` watcher
(inert without its conf). The app container is NOT started — the first
deploy provides image and env. Accepts unknown host keys (first contact).

```
kampodra vm-prepare --host root@<new-ip> [--ssh-key <path>] [--profile <name>] [--ansible <playbook>]
```

| flag | type | default | description |
|---|---|---|---|
| `--host` | string | `""` | **required**; target VM |
| `--ansible` | string | `""` | run this ansible-playbook file against the host after bootstrap (inventory derived from `--host`, `--private-key` from the resolved key) |
| `--profile` / `--ssh-key` | string | `""` | target flags |

```bash
kampodra vm-prepare --host root@203.0.113.10
```

- Gates fail closed BEFORE mutating anything; unit rewrites never bounce a
  running service (drift is warned, not restarted). The first TLS
  certificate issues during the first deploy.

## vm-wipe

kampodra-native teardown of the project stack on the target VM: stop +
disable every project service, remove the project containers (app, its
rolling shadow, kamal-proxy), prune the image store, delete the env file,
deployed-sha stamp, and anchor config. The env dir itself is NEVER
`rm -rf`'d — it may hold other services' files; the wipe names what it
leaves behind. DESTRUCTIVE — `--yes` gates every mutation.

```
kampodra vm-wipe --yes [--keep-data] [--force] [--host root@<ip>] [--profile <name>] [--ssh-key <path>]
```

| flag | type | default | description |
|---|---|---|---|
| `--yes` | bool | `false` | **required** to confirm the teardown |
| `--keep-data` | bool | `false` | preserve the tenant data dir (everything else goes) |
| `--force` | bool | `false` | override the services-mismatch refusal (wrong host?) |
| `--host` / `--profile` / `--ssh-key` | string | `""` | target flags |

```bash
kampodra vm-wipe --profile prod --yes
kampodra vm-wipe --profile prod --yes --keep-data
```

- Refuses to wipe a host where no project container or service matches —
  pass `--force` to mean it.

## metrics

One-shot VM snapshot over SSH: load, memory, disk breakdown, containers,
top processes. One round-trip per snapshot, busybox-safe remote commands,
client-side rendering — no continuous monitoring.

```
kampodra metrics [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--disk-threshold <pct>] [--watch <sec>] [--count <n>]
```

| flag | type | default | description |
|---|---|---|---|
| `--disk-threshold` | string | `90` | exit 1 when root disk usage ≥ this percentage (1–100) |
| `--watch` | string | `""` | re-snapshot every N seconds (positive integer; ctrl-c ends) |
| `--count` | string | `""` | bound the number of snapshots (positive integer) |
| `--host` / `--profile` / `--ssh-key` | string | `""` | target flags |

```bash
kampodra metrics --host root@203.0.113.10
kampodra metrics --watch 10 --count 6
```

- Disk above 90% always prints a warning pointing at
  `deploy prune --dry-run`; `--disk-threshold` makes it exit 1 at the threshold.

## dns

OCI DNS record management through the `oci` CLI's own auth — kampodra passes
no auth flags unless the instance profile's `cloud` block overrides it
(`{"profile": "…", "compartment": "…", "instancePrincipal": true}`);
`OCI_PROFILE` / `OCI_COMPARTMENT` env are the escape hatch. Never
credential material. A compartment is still **required** (block or env —
no default). Types are pinned to
`A | AAAA | CNAME`; TTL range 60–172800. All argument validation happens
locally before any `oci` call.

```
kampodra dns records [--zone <id-or-name>]
kampodra dns add --name <label> --type A|AAAA|CNAME --value <target> [--ttl 300]
kampodra dns rm --name <label> --type <A|AAAA|CNAME> --value <target>
# all subcommands: optional --zone <id-or-name>; auth via the instance
# profile's `cloud` block or OCI_PROFILE/OCI_COMPARTMENT env.
```

| flag | type | default | description |
|---|---|---|---|
| `--zone` | string | `""` | zone id or name; default: the compartment's ONLY zone — several require this flag |
| `--name` | string | `""` | record name: a DNS label (`app`) or an fqdn inside the zone (`add`/`rm`, required) |
| `--type` | string | `""` | record type: A, AAAA, or CNAME (`add`/`rm`, required) |
| `--value` | string | `""` | record target, type-shaped: IPv4 / IPv6 / hostname (`add`/`rm`, required) |
| `--ttl` | string | `300` | TTL seconds, 60–172800 (`add`) |
| `--profile` | string | `""` | instance profile — carries the `cloud` block (OCI profile/compartment/instance-principal; `OCI_PROFILE`/`OCI_COMPARTMENT` env override) |

```bash
kampodra dns records
kampodra dns add --name app --type A --value 203.0.113.10 --zone app.example.com
kampodra dns add --name www --type CNAME --value app.example.com --ttl 3600
kampodra dns rm --name app --type A --value 203.0.113.10
```

- `add` merges into the existing RRSet (round-robin records survive) and is
  idempotence-aware; `rm` filters the value out — an emptied RRSet is
  removed entirely, and removing a non-existent record errors.
- A compartment name (not `ocid1.…`) resolves through `iam compartment
  list`; multiple zones in the compartment fail with the candidate list.

## migrate

Tenant db migrations over SSH: the app's own migration script runs per db
file VIA THE REPO ON THE VM — the script path comes from the `migrateScript`
config (default `scripts/migrate-db.ts`; set it in `kampodra.json`/profile/
`KAMPODRA_MIGRATE_SCRIPT` to your repo's real layout), resolved inside
`--repo-root`, executed with `pnpm exec tsx` on the VM. Order: `root.db`
first, then tenant files sorted, bounded-parallel. Per-file failures are
collected (one bad tenant does not block the others) and the command exits 1
if any failed — do not start the API on a half-migrated estate.

```
kampodra migrate [--allow-running] [--profile <name>] [--host <user@ip>] [--ssh-key <path>] [--repo-root <path>] [--jobs <n>]
```

| flag | type | default | description |
|---|---|---|---|
| `--repo-root` | string | `""` | the app repo checkout ON THE VM (required; beats `KAMPODRA_REPO_ROOT`) |
| `--jobs` | string | `""` | parallel tenant migrations, 1–64 (default: `MIGRATE_JOBS` env, else 4) |
| `--allow-running` | bool | `false` | migrate while the app service is running (SQLITE_BUSY risk) |
| `--host` / `--profile` / `--ssh-key` | string | `""` | target flags |

```bash
kampodra migrate --repo-root /srv/app
kampodra migrate --jobs 1
```

- Stop-first guard: refuses to run while the app service is up (init-aware
  detection) unless `--allow-running`; on a host with neither init system
  the state is unknown and it proceeds with a loud warning.
- Preconditions probed in one round trip: repo root (the migrate script),
  `pnpm`, and the tenant data dir must all exist — fail closed otherwise.

## status

Live health + deployment count + VM state: the ledger's deployment total,
the public edge health through kamal-proxy (`proxyHost` + `healthPath`),
VM disk usage, sha-tagged image count with a prune estimate, the service
roll call, and — with `--verbose` — the full metrics snapshot.

```
kampodra status [--host root@<ip>] [--profile <name>] [--ssh-key <path>] [--verbose] [--all-profiles] [--group <g>]
```

| flag | type | default | description |
|---|---|---|---|
| `--verbose` | bool | `false` | add the full metrics snapshot (needs a target) |
| `--all-profiles` | bool | `false` | one full status section per configured profile |
| `--group` | string | `""` | one section per profile in this group (exclusive with `--all-profiles`/`--host`/`--profile`) |
| `--host` / `--profile` / `--ssh-key` | string | `""` | target flags |

```bash
kampodra status --host root@203.0.113.10 --verbose
kampodra status --all-profiles
kampodra status --group web
```

- Without a target the ledger count still renders (across all hosts), but
  VM state and metrics need one. The blue/green pair view is skipped
  (see [Not yet ported](#not-yet-ported)).

## Target & config resolution

**Target ladder** (every host-aware command):

- profile: `--profile` flag > `KAMPODRA_PROFILE` env > config `defaultProfile`
- host: `--host` flag > profile `host` > `KAMPODRA_HOST`
- ssh key: `--ssh-key` flag > profile `sshKey` > `KAMPODRA_SSH_KEY` (empty =
  ssh agent / `~/.ssh/config`)
- proxy host: profile project `proxyHost` (or the legacy flat field) >
  `KAMPODRA_PROXY_HOST` > project default

An unknown profile name fails closed naming the known set; a missing
target errors with every resolution route (`target required: …`).

**Project-shape ladder** (every naming field):
`flags` > `KAMPODRA_*` env > profile `project` block > `kampodra.json` >
built-in defaults. Each layer wins only where it provides a value.

**Environment variables:**

| variable | used by | meaning |
|---|---|---|
| `KAMPODRA_HOST` | all host-aware commands | target VM when no `--host`/profile host |
| `KAMPODRA_PAIR_INSTANCE_PREFIX` | `bluegreen` | pair instance display-name prefix (empty = container-derived) |
| `KAMPODRA_SSH_KEY` | all host-aware commands | identity file when no `--ssh-key`/profile key |
| `KAMPODRA_PROFILE` | all host-aware commands | profile when no `--profile` (beats `defaultProfile`) |
| `KAMPODRA_BUCKET` | `backup` (all subcommands) | the project ladder's canonical bucket override — beats the project config's `bucket` |
| `KAMPODRA_REPO_ROOT` | `migrate` | VM repo checkout when no `--repo-root` |
| `KAMPODRA_MIGRATE_SCRIPT` | `migrate` | repo-relative script path when no `migrateScript` config |
| `OCI_PROFILE` | `backup`, `dns` | OCI **config** profile override when the profile's `cloud` block sets none (else the oci CLI resolves natively) |
| `OCI_COMPARTMENT` | `dns` | compartment name or ocid — required, no default |
| `MIGRATE_JOBS` | `migrate` | parallel tenant migrations when no `--jobs` |

Every project-shape field additionally has its own `KAMPODRA_*` override —
see the table in [kampodra.json](#kampodrajson).

## kampodra.json

A committed repo manifest (the `vercel.json` pattern) carrying project
naming only — hosts, ssh keys, and secret material never belong there.
Discovered by walking up from the working directory like `package.json`;
the NEAREST file wins. Parsed **strictly, fail-closed**: malformed JSON or
unknown keys are an error (the error lists the valid keys — a committed
config typo must fail, never silently resolve to defaults). All 17 fields
are optional camelCase keys; empty = not overridden. An optional `"$schema"`
key is accepted and ignored — point it at
[kampodra.schema.json](kampodra.schema.json) for editor autocomplete.
`config print` shows the resolved value and winning source of every field.

| field | type | built-in default | `KAMPODRA_*` env override |
|---|---|---|---|
| `container` | string | `app` | `KAMPODRA_CONTAINER` |
| `pairInstancePrefix` | string | `""` (falls back to `container`) | `KAMPODRA_PAIR_INSTANCE_PREFIX` |
| `shadowSuffix` | string | `-shadow` | `KAMPODRA_SHADOW_SUFFIX` |
| `envFile` | string | `/etc/kampodra/env` | `KAMPODRA_ENV_FILE` |
| `dataDir` | string | `/data` | `KAMPODRA_DATA_DIR` |
| `bucket` | string | `app-backups` | `KAMPODRA_BUCKET` |
| `objectPrefix` | string | `db` | `KAMPODRA_OBJECT_PREFIX` |
| `healthPath` | string | `/up` | `KAMPODRA_HEALTH_PATH` |
| `proxyHost` | string | `app.example.com` | `KAMPODRA_PROXY_HOST` |
| `services` | list | `app`, `kamal-proxy` | `KAMPODRA_SERVICES` |
| `imagePrefix` | string | `127.0.0.1:5000/app` | `KAMPODRA_IMAGE_PREFIX` |
| `port` | string | `8080` | `KAMPODRA_PORT` |
| `network` | string | `kamal` | `KAMPODRA_NETWORK` |
| `shadowProbePort` | string | `18080` | `KAMPODRA_SHADOW_PROBE_PORT` |
| `deployedShaFile` | string | `/etc/kampodra/deployed-sha` | `KAMPODRA_DEPLOYED_SHA_FILE` |
| `envClearKeys` | list | `NODE_ENV`, `PORT` | `KAMPODRA_ENV_CLEAR_KEYS` |
| `dockerfile` | string | `Dockerfile` | `KAMPODRA_DOCKERFILE` |
| `migrateScript` | string | `scripts/migrate-db.ts` | `KAMPODRA_MIGRATE_SCRIPT` |
| `images.sidecars[]` | list | `[]` | manifest-only (repo property — no env/profile override): secondary images `deploy` builds+streams+tags as `<imagePrefix>-<name>:latest`; each entry needs `name` (`^[a-z0-9][a-z0-9_-]*$`) + `dockerfile` |

```json
{
  "$schema": "https://raw.githubusercontent.com/talha7k/kampodra/main/docs/kampodra.schema.json",
  "container": "myapp-api",
  "envFile": "/etc/myapp/env",
  "proxyHost": "app.example.com"
}
```

(The profile's `project` block in `~/.kampodra/config.json` takes the same
field keys — `envFile` included; the block's legacy `envFilePath` spelling
is still accepted on load, with the canonical key winning when both are
present.)

## Files on disk

- **`~/.kampodra/config.json`** — per-instance profiles, dir 0700 / file
  0600, written atomically (same-dir tmp + mv). Shape: `defaultProfile` +
  `profiles`, each profile flat: `host`, `sshKey` (a path — never a
  secret), `proxyHost`, `group`, `init` (cached init verdict), the raw
  `project` block, and the optional raw `cloud` block
  (`{"provider": "oci", "profile": "…", "compartment": "…",
  "instancePrincipal": true}` — provider-CLI auth overrides; `provider`
  defaults to the only supported cloud (`oci`) and unknown values fail
  naming the supported set; absent block = native resolution). A missing
  file is an empty config; a corrupt one fails closed.
- **`~/.kampodra/deployments.jsonl`** — the append-only deployment ledger
  (0600, dir created on demand). One validated JSON line per deploy: `ts`,
  `host`, `sha`, `tag`, `result` (`success|failed|rollback`), `duration_ms`,
  `subject`. Read by `deploy list` and `status`; no repair, no deletion.

## Roadmap

No open roadmap items in the CLI surface — `bluegreen` and `image-import`
shipped this cycle (see below). Bigger follow-ups live in DEBTS.md
(rolling-by-default promotion, secondary-image streaming).

## bluegreen

OCI reserved-IP blue/green pair for zero-downtime cutovers across two
instances. One instance can start alone; the pair shares one RESERVED
public IP — the internet-facing address never changes, the flip is one
OCI API call, rollback is the same call reversed. The reserved IP is
derived from live OCI state (no local state file). Cloud auth rides the
instance profile's `cloud` block (or `OCI_PROFILE`/`OCI_COMPARTMENT` env).

```
kampodra bluegreen status                  # pair view: instances, IP holder, health
kampodra bluegreen init                    # create the reserved public IP (dormant, unassigned)
kampodra bluegreen provision <blue|green>  # launch the second instance (see below)
kampodra bluegreen flip --to <color>       # ACME-first health-gated flip (see below)
kampodra bluegreen rollback                # unassign to DORMANT + holder guest cleanup
```

| flag | type | default | description |
|---|---|---|---|
| `--host` / `--profile` / `--ssh-key` | string | `""` | target flags (ssh legs + project naming) |
| `--image-id` | string | `""` | `provision`: launch this exact image (skips route detection) |
| `--os` | string | `alpine` | `provision`: guest OS of the golden image (`alpine`\|`ubuntu` — ubuntu = 24.04 LTS); sets the golden-image lookup prefix (`<container>-alpine` / `<container>-ubuntu-24.04`) |
| `--qcow2` | string | `""` | `provision` inject route: golden qcow2 path (`ALPINE_QCOW2` env, else error) |
| `--platform-key` | string | `""` | `provision` inject route: ssh PUBLIC key FILE the platform image authorizes at first boot (`OPS_SSH_PUBKEY` env, else error). Authorization only — the platform ssh legs authenticate with the resolved `--ssh-key` identity. |
| `--platform-user` | string | `""` | `provision` inject route: platform-image ssh user (`PLATFORM_SSH_USER` env, default `ubuntu`) |
| `--to` | string | `""` | `flip`: target color (`blue`\|`green`) |
| `--force` | bool | `false` | `flip`: cut over even when the target app is unhealthy |

- `status` prints the pair view (reserved IP + holder, both instances with
  AD and per-color health); read-only.
- Pair instance display names are `<pairInstancePrefix>-blue|green` — the
  prefix falls back to `container`. Estates whose running instance predates
  container-derived naming (e.g. `esellar-green` with `container:
  esellar-api`) set `pairInstancePrefix` so the machinery resolves the
  RUNNING sibling as the provision template / rollback holder.
- `init` creates the DORMANT reserved IP (idempotent — exits 0 when it
  exists).
- `provision <color>` launches the sibling from the other color's AD +
  subnet: the newest `<container>-alpine*` (default `--os alpine`) or
  `<container>-ubuntu-24.04*` (`--os ubuntu`) custom image with UEFI_64
  firmware goes the native route; otherwise the template's LIVE image-id
  launches a platform instance and the golden qcow2 is injected onto its
  boot disk (qemu-img convert → gzip → ssh `gunzip | sudo dd`, reboot,
  per-OS boot verify: `/etc/alpine-release` 3.x for alpine, `ID=ubuntu`
  in `/etc/os-release` for ubuntu). Ends with `vm-prepare` next steps.
- `flip --to <color>` is ACME-first and health-gated: health check on the
  target's own IP → anchor conf on the target guest (the
  `kampodra-anchor` watcher configures the address) → OCI assigns the
  reserved IP to the anchor → ACME issues on the target for the reserved
  `<ip-dashes>.sslip.io` hostname → verify valid-cert HTTPS + served sha
  through the reserved IP. Any post-assign failure auto-rolls back to the
  other color's EXISTING anchor (lookup-only) or to dormant, then cleans
  the failed target's guest state.
- `rollback` unassigns to DORMANT: resolves the holder color by matching
  its anchor (lookup-only), cleans the holder guest (anchor.conf + address
  delete), then the OCI unassign. To move traffic to the other color of a
  real pair, use `flip --to <other>` instead.

## image-import

Upload a packer-built qcow2 to Object Storage and import it as an OCI
custom image (runs on the BUILD machine; the VM never touches OCI APIs).
Alpine is not in OCI's supported import list, so imports are
self-supported (generic OS metadata, PARAVIRTUALIZED). OCI pins imports
to firmware=BIOS — A1/Ampere is UEFI-only and rejects those at launch,
so a non-UEFI result warns loudly (x86 launches fine; the A1 route is
capture-from-instance). Cloud auth rides the profile `cloud` block / env.

```
kampodra image-import --image <qcow2> [--bucket <name>] [--name-prefix <p>] [--compartment <name>] [--keep-object]
```

| flag | type | default | description |
|---|---|---|---|
| `--image` | string | `""` | **required**; qcow2 file to import |
| `--os` | string | `alpine` | guest OS of the qcow2 (`alpine`\|`ubuntu` — ubuntu = 24.04 LTS); drives the import OS metadata (`Linux` / `Alpine (self-supported)` vs `Canonical Ubuntu` / `Ubuntu 24.04`) |
| `--bucket` | string | `""` | staging bucket (`KAMPODRA_IMPORT_BUCKET` env, else `<container>-image-import`) |
| `--name-prefix` | string | `""` | image display-name prefix (default `<container>-alpine`, or `<container>-ubuntu-24.04` with `--os ubuntu` — provision looks up this prefix) |
| `--compartment` | string | `""` | compartment name or ocid (cloud block or `OCI_COMPARTMENT` env) |
| `--keep-object` | bool | `false` | keep the staged qcow2 object after a successful import |

Flow: namespace resolve → staged upload → import → poll AVAILABLE (up to
30m; terminal states fail naming the console page) → firmware verdict →
delete the staged object unless `--keep-object` → print the image OCID.
Configure the oci CLI (`oci setup config`) with keys from your own pass
store — kampodra never touches credentials.
