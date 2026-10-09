# kampodra command reference

Per-command deep reference for AI agents operating the `kampodra` CLI. Every
flag name, type, and default below is taken from the Cobra definitions in the
CLI source (`internal/command/*.go`). When this file and `kampodra <command>
--help` disagree, re-run `--help` and trust the binary's live output.

**Contents**

- [Global resolution rules](#global-resolution-rules)
- [Environment variables](#environment-variables)
- [kampodra.json (project manifest)](#kampodrajson-project-manifest)
- [deploy](#deploy) (+ [converge](#deploy-converge-sha7) / [list](#deploy-list) / [prune](#deploy-prune) / [logs](#deploy-logs) / [restart](#deploy-restart) / [shell](#deploy-shell))
- [env](#env) (list / push / pull / fingerprint / diff / from-schema)
- [ssh](#ssh)
- [config](#config) (init / list / show / print / set-default / remove)
- [backup](#backup) (list / download / verify / restore-plan)
- [vm-prepare](#vm-prepare)
- [vm-wipe](#vm-wipe)
- [metrics](#metrics)
- [dns](#dns) (records / add / rm)
- [migrate](#migrate)
- [status](#status)
- [Exit codes](#exit-codes)
- [Agent gotchas](#agent-gotchas)

## Global resolution rules

Every host-aware command resolves its target the same way (first match wins):

- profile: `--profile` flag > `KAMPODRA_PROFILE` env > the config's `defaultProfile`
- host: `--host` flag > profile `host` > `KAMPODRA_HOST`
- ssh key: `--ssh-key` flag > profile `sshKey` > `KAMPODRA_SSH_KEY` (empty = ssh agent / `~/.ssh/config`)
- proxy host: profile `project.proxyHost` block > `KAMPODRA_PROXY_HOST` > project default; the profile's legacy flat `proxyHost` field still wins when set

The project's shape (container name, env-file path, ports, bucket, ...) resolves
through the ladder:

```
flags > KAMPODRA_* env > profile "project" block > kampodra.json > built-in defaults
```

An unknown profile name fails closed naming the known set. Profiles live in
`~/.kampodra/config.json` (dir 0700 / file 0600); ssh keys are stored as paths
only — the config never contains secret material. `kampodra config print`
renders the fully resolved effective config with per-field provenance; make it
the first debugging stop. Every command supports `--help` (usage + examples).
## Environment variables

| Variable | Effect |
|---|---|
| `KAMPODRA_HOST` | Fallback host (`user@ip` or ssh-config alias) when no flag/profile provides one |
| `KAMPODRA_SSH_KEY` | Fallback identity file path |
| `KAMPODRA_PROFILE` | Fallback profile name (beaten by `--profile`, beats `defaultProfile`) |
| `KAMPODRA_BUCKET` | The project ladder's canonical bucket override — beats the project config's `bucket` (only `--bucket` beats it) |
| `KAMPODRA_MIGRATE_SCRIPT` | Repo-relative migrate script path override for `migrate` |
| `KAMPODRA_REPO_ROOT` | Fallback VM repo checkout for `migrate` (`--repo-root` beats it) |
| `OCI_PROFILE` | OCI config profile override when the profile's `cloud` block sets none (else native resolution) |
| `OCI_COMPARTMENT` | REQUIRED by `dns` — no default; name or `ocid1.` ocid |
| `MIGRATE_JOBS` | Fallback parallelism for `migrate` (else 4; accepted range 1–64) |

Each `kampodra.json` field also has a `KAMPODRA_*` env override in the ladder:
`KAMPODRA_CONTAINER`, `KAMPODRA_SHADOW_SUFFIX`, `KAMPODRA_ENV_FILE`,
`KAMPODRA_DATA_DIR`, `KAMPODRA_BUCKET`, `KAMPODRA_OBJECT_PREFIX`,
`KAMPODRA_HEALTH_PATH`, `KAMPODRA_PROXY_HOST`, `KAMPODRA_SERVICES`,
`KAMPODRA_IMAGE_PREFIX`, `KAMPODRA_PORT`, `KAMPODRA_NETWORK`,
`KAMPODRA_SHADOW_PROBE_PORT`, `KAMPODRA_DEPLOYED_SHA_FILE`,
`KAMPODRA_ENV_CLEAR_KEYS`, `KAMPODRA_DOCKERFILE`.

## kampodra.json (project manifest)

Repo-level manifest, committed per-project; discovered upward from the working
directory (nearest file wins). Carries project NAMING only — never hosts, ssh
keys, or secrets. All 17 fields optional, camelCase; an optional `"$schema"`
key pointing at docs/kampodra.schema.json is accepted (ignored) for editor
autocomplete:

| Field | Type | Purpose |
|---|---|---|
| `container` | string | api container / service name |
| `shadowSuffix` | string | rolling-deploy shadow container suffix |
| `envFile` | string | remote env file path (profile blocks also accept the legacy `envFilePath` spelling; the manifest takes `envFile` only) |
| `dataDir` | string | tenant/app data dir on the VM |
| `bucket` | string | object-storage backup bucket |
| `objectPrefix` | string | backup object prefix |
| `healthPath` | string | app health endpoint behind the proxy |
| `proxyHost` | string | public TLS edge hostname |
| `services` | string[] | service roll call |
| `imagePrefix` | string | VM-local image reference prefix for deploy images |
| `port` | string | container's published/listening port |
| `network` | string | podman network shared with kamal-proxy |
| `shadowProbePort` | string | shadow container's loopback-only probe port |
| `deployedShaFile` | string | VM deployed-sha stamp (rollback's fallback) |
| `envClearKeys` | string[] | env keys owned by init — `env from-schema` drops them from varlock output |
| `dockerfile` | string | Containerfile/Dockerfile deploy builds |
| `migrateScript` | string | repo-relative db migrate script (`migrate` runs it on the VM) |

The parse is **strict fail-closed**: malformed JSON or an unknown key errors
instead of silently resolving to defaults. An empty object is valid. Built-in
defaults describe one sample app's shape, not neutral conventions — check
`config print` before the first run against a new tree.

## deploy

Full pipeline: clean-tree gate → `podman build --build-arg GIT_SHA=<sha>` (sha
tag from `git rev-parse --short HEAD`) → `podman save | ssh podman load`
(registry-free stream) → env push (`--env-file`; 0600 + atomic mv, `API_GIT_SHA`
stamped) → init restart → VM-side served-sha health gate → kamal-proxy re-point
→ public smoke → ledger append → disk report → keep-set image cleanup (running
+ `ts-rollback` + newest 3 kept).

| Flag | Type | Default |
|---|---|---|
| `--host` | string | `""` (resolution ladder) |
| `--profile` | string | `""` (resolution ladder) |
| `--ssh-key` | string | `""` (agent / ssh config) |
| `--dockerfile` | string | `""` → project config's `dockerfile` (else `Dockerfile`) |
| `--sha <sha7>` | string | `""`; sha fragment required, never inferred |
| `--rollback [<sha7>]` | string | bare `--rollback` = stamp-resolved |
| `--rolling` | bool | `false` |
| `--drain-timeout` | int | `10` (seconds; requires `--rolling`) |
| `--disk-threshold` | string | `""` (percentage 0–100) |
| `--env-file` | string | `""` |
| `--skip-smoke` | bool | `false` |
| `--refresh-config` | bool | `false` — compatibility no-op (a warning prints) |

- `--sha` streams an EXISTING local build: no rebuild, clean-tree gate
  skipped. `--rollback` and `--sha` are exclusive; `--rolling` and
  `--rollback` are exclusive.
- `--disk-threshold` fails closed BEFORE the build when VM disk usage >= pct; an
  unreadable df with the flag set is also an error. Disk > 90% always warns
  loudly, flag or not.
- Deploy mode requires a git repo (the sha is stamped). Rollback target
  resolution: explicit sha > the VM's deployed-sha stamp file > DIE — never git
  HEAD. Rollback skips build/stream when the tag still exists on the VM, streams
  from this machine when it does not, and never rewrites the stamp. A positional
  in rollback mode is the sha (given twice = error); any other positional is
  rejected.

Example: `kampodra deploy --host root@203.0.113.10 --rolling --env-file ./ops/env.production`

### deploy converge [&lt;sha7&gt;]

Finishes an interrupted `--rolling` deploy: retag → restart → health gate →
re-point to main → remove the shadow. Flags: `--host`, `--profile`,
`--ssh-key`, `--drain-timeout` (int, default 10). The sha comes from the
argument (must be a sha fragment) or the shadow's own image — never HEAD. A
leftover shadow must be converged before the next `--rolling`.
Example: `kampodra deploy converge --host root@203.0.113.10`

### deploy list

Deployment history: the VM's sha-tagged images (running one marked) merged with
the local ledger `~/.kampodra/deployments.jsonl`, plus a `Total deployments`
footer. Flags: `--host`, `--profile`, `--ssh-key`, `--all-profiles` (bool,
default false — one section per configured profile, sorted; errors when no
profiles exist), `--group <g>` (one section per profile in that group).
Same-tag rows merge to ONE row showing the NEWEST deploy timestamp. An
unreachable VM degrades to a ledger-only view with a warning, not an error.
Example: `kampodra deploy list --profile prod --all-profiles`

### deploy prune

Reclaims VM disk by removing OLD sha-tagged deploy images. ALWAYS kept: the
currently-running image, `ts-rollback`, and the newest `--keep` N.
Flags: `--host`, `--profile`, `--ssh-key`, `--keep` (string, default `"2"`,
non-negative integer), `--dry-run` (bool, default false — prints the exact
`podman rmi` commands and removes nothing). Refuses to prune if the image list
fails over ssh. After applying, reports disk on `/var/lib/containers` and warns
above 90%.
Example: `kampodra deploy prune --host root@203.0.113.10 --dry-run`

### deploy logs

Tails the running api container's logs. Flags: `--host`, `--profile`,
`--ssh-key`, `--lines` (string, default `"100"`, positive integer),
`--follow` (bool, default false — streams until ctrl-c with a clean exit; a
non-zero remote exit code propagates).
Example: `kampodra deploy logs --host root@203.0.113.10 --lines 200 --follow`

### deploy restart

Restarts the api service, init-aware: `rc-service` on Alpine (supervise-daemon
re-runs `podman run` on `:latest`), `systemctl` on systemd hosts, then a
best-effort status check. Flags: `--host`, `--profile`, `--ssh-key`.
Example: `kampodra deploy restart --profile prod`

### deploy shell

Interactive `sh` inside the running api container (forced tty), or a one-shot
via `exec -- <cmd>...`. The `--` separator is REQUIRED for one-shot mode (pflag
consumes it; the command vector is located via ArgsLenAtDash). Arguments are
POSIX sh-quoted before composition. Flags: `--host`, `--profile`, `--ssh-key`.
The container's exit code propagates.
Example: `kampodra deploy shell --host root@203.0.113.10 exec -- df -h`

## env

The remote app env file (project config's `envFile` path, 0600). Values are
NEVER printed — every display is a KEY + fingerprint table (value length +
first 2 chars). Raw values move only through `push`'s upload and `pull`'s
stdout/`--out` payload. Subcommand flags: `--host`, `--profile`, `--ssh-key`
(standard ladder) except where noted.

- `env list` — KEY + fingerprint table of the remote file.
- `env push --file <f>` — `--file` string, required; refuses files with no
  `KEY=VALUE` lines; uploads verbatim: remote 0600 from creation (umask 077) +
  atomic `mv`; prints the fingerprint table and a restart hint (does NOT
  restart itself).
- `env pull [--out <file>]` — `--out` string, default `""`. Without it the raw
  payload goes to stdout and the masked summary to stderr; with it the file is
  written 0600 and the summary goes to stdout.
- `env fingerprint [--file <f>]` — `--file` string, default stdin. Previews the
  masking for a LOCAL file; no host needed.
- `env diff <local-file>` — exactly one positional arg. Fingerprint-level diff
  of local vs remote; **exit 1 = differs** (GNU diff convention).
- `env from-schema [--repo <path>] [--schema <path>] [--out <file>]` —
  kampodra-native. Generates the env file from the repo's committed
  `.env.schema` via varlock (`node_modules/.bin/varlock` must exist in the
  repo; `pass()` refs resolve through varlock + the pass store, values never
  echo); `envClearKeys`-owned keys are dropped. Defaults: `--repo` `"."`
  (resolved via `git rev-parse --show-toplevel`; no git repo = error),
  `--schema` `"apps/api/.env.schema"`, `--out` `""` (= push through the push
  flow instead of writing locally, 0600).

Example: `kampodra env diff ./ops/env.production --host root@203.0.113.10`

## ssh

Host-level ssh passthrough (kampodra-native). Flags: `--host`, `--profile`,
`--ssh-key`. Flag parsing is NOT interspersed: everything after the first
non-flag argument belongs to the remote command verbatim. No command =
interactive login (forced tty). ssh's exit code propagates.
Example: `kampodra ssh --host root@203.0.113.10 df -h`

## config

Per-instance profiles in `~/.kampodra/config.json` (dir 0700 / file 0600).
FLAT — no inheritance; `group` is a cosmetic list filter. Persistent flags:
`--name` (string), `--host` (string), `--ssh-key` (string, path only, tilde
expanded), `--proxy-host` (string), `--group` (string), `--profile` (string),
`--set-default` (bool, false), `--force` (bool, false).

- `config init` — create or `--force`-refresh a profile. `--name` required:
  `[A-Za-z0-9][A-Za-z0-9_-]*`, max 64. The first profile auto-becomes the
  default. Re-init refreshes flat fields, keeps the tooling-owned cached init
  verdict unless host/ssh-key changed, and always preserves the profile's
  `project` block (config init never manages it).
- `config list [--group <g>]` — all profiles, default marked `*`.
- `config show [--profile <name>]` — one profile's fields (else the default).
- `config print [--profile <name>]` — the fully resolved effective project
  config with per-field provenance (flag/env/profile/repo-file/default). The
  debugging tool for the resolution ladder.
- `config set-default <name>` — repoint `defaultProfile` (max 1 positional).
- `config remove <name> [--force]` — removing the LAST profile requires
  `--force`; if the default was removed it reassigns sorted-first.

Example: `kampodra config init --name prod --host root@203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --proxy-host app.example.com --set-default`

## backup

OCI Object Storage backups via the `oci` CLI's own auth — kampodra passes
no auth flags unless the profile's `cloud` block overrides it, else the
oci CLI resolves natively; kampodra never accepts, stores, or logs
credential material. Persistent flags:

| Flag | Type | Default | Meaning |
|---|---|---|---|
| `--bucket` | string | `""` | bucket; ladder: `--bucket` > `KAMPODRA_BUCKET` > project config `bucket` |
| `--prefix` | string | `""` | object name prefix for `list` |
| `--out` | string | `""` | `download` destination (else the object's basename) |


| `--host` | string | `""` | VM target for `verify` |
| `--ssh-key` | string | `""` | identity file for `verify` |
| `--migrated-topology` | bool | `false` | `verify`: pass restore-verify's drill flag through |

- `backup list [--prefix <p>]` — objects in the bucket. LIST-denied by policy:
  prints the exact policy shape needed on stderr, **exit 1** (GET works without
  LIST once the object name is known).
- `backup download <object> [--out <file>]` — pre-creates the file 0600, fetches
  via `oci get`, then verifies sha256/md5 against the object's digest metadata;
  mismatch prints FAIL and **exits 1**; no metadata = integrity noted as skipped.
- `backup verify <local.db|.tgz>` — read-only drill. Accepts `.db`/`.sqlite`/`.sqlite3`
  or `.tgz`/`.tar.gz`. Needs a VM target (`--host` / the legacy instance-profile
  flag above / ladder). Each db is uploaded to a VM scratch file
  `/tmp/kampodra-verify-<n>-<crypto-random-hex>.db` (created under umask 077,
  ALWAYS removed — cleanup never masks the verdict), then the VM's
  `/usr/local/bin/restore-verify` runs as `restore-verify -db <path>`.
  `--migrated-topology` appends restore-verify's `--migrated-topology` drill
  flag (bulk-migration chain shape downgraded to warnings). For a `.tgz`, the
  archive is extracted to a local temp dir (path-traversal-refusing) and every
  `.db`/`.sqlite` member is verified sorted; `root/root.db` members are
  SKIPPED — the root db is schema-only, not in restore-verify scope. Never
  writes into the data dir. Any member failure fails the command.
- `backup restore-plan <object> [--bucket <name>]` — PRINTS the documented
  recovery sequence (stage → scp to scratch → verify → stop api → snapshot →
  swap in → start + smoke). Never executes anything: no oci call, no ssh, no
  mutation.

Example: `kampodra backup verify /tmp/restore.db --host root@203.0.113.10`

## vm-prepare

First-run bootstrap of a bare Alpine VM; idempotent, safe to re-run. Sequence:
sanity gates (UEFI boot, no systemd, OpenRC tooling) → sshd hardening (drop-in +
Include + restart, then gated) → apk community repo + podman stack (netavark) →
project state dir + `/etc/containers/registries.conf` → sysctl
`net.ipv4.ip_unprivileged_port_start=80` → OpenRC services (api container +
kamal-proxy via supervise-daemon, rc-update'd into the default runlevel) →
`kampodra-anchor` watcher (inert without its conf) → kamal-proxy image pulled +
service UP. The app container is intentionally NOT started on a fresh VM — the
first deploy provides image and env (a converge runs only when a previous
deploy left both). The first TLS certificate issues during the first deploy.
Readiness loops: ssh 60 × 5s, proxy container 20 × 3s. Accepts unknown host
keys (first contact).

| Flag | Type | Default |
|---|---|---|
| `--host` | string | `""` (required in practice for a new VM) |
| `--profile` | string | `""` |
| `--ssh-key` | string | `""` |

| `--ansible <playbook>` | string | `""` — run this ansible-playbook against the host AFTER bootstrap (temp inventory file, `--private-key` from the resolved key; failure is fatal) |

Example: `kampodra vm-prepare --host root@203.0.113.10`

## vm-wipe

kampodra-native teardown of the project stack on the target VM: stop + disable
every project service, remove the project containers (app, its rolling-deploy
shadow, kamal-proxy), `podman image prune -a -f`, delete the env file,
deployed-sha stamp, anchor conf, and state dir; the tenant data dir goes LAST.
DESTRUCTIVE — **`--yes` is REQUIRED**; without it nothing runs.

| Flag | Type | Default |
|---|---|---|
| `--yes` | bool | `false` — confirm the teardown (required) |
| `--keep-data` | bool | `false` — preserve the tenant data dir |
| `--force` | bool | `false` — override the services-mismatch refusal |
| `--host` | string | `""` |
| `--profile` | string | `""` |
| `--ssh-key` | string | `""` |

Match check: if NO project container is present AND no project service is
running, it refuses (`--force` to mean it — wrong host?). Individual step
failures are tolerated and reported, not fatal.
Example: `kampodra vm-wipe --profile prod --yes --keep-data`

## metrics

One-shot VM snapshot over SSH (load, memory, disk breakdown, containers, top
procs) — one round-trip per snapshot, busybox-safe remote commands. NOT
continuous monitoring.

| Flag | Type | Default |
|---|---|---|
| `--host` | string | `""` |
| `--profile` | string | `""` |
| `--ssh-key` | string | `""` |
| `--disk-threshold` | string | `"90"` (1–100) — **exit 1** when root disk usage >= this threshold |
| `--watch` | string | `""` — re-snapshot every N seconds (positive int; ctrl-c ends, exit 0) |
| `--count` | string | `""` — bound the snapshot count (positive int) |

Above 90% root disk always warns (exit 0); only the `--disk-threshold` threshold
fails. Requires a resolved target.
Example: `kampodra metrics --host root@203.0.113.10 --disk-threshold 85`

## dns

OCI DNS records via the `oci` CLI's own auth ONLY — no auth flags unless
the profile's `cloud` block overrides (else native resolution); never
credential material. A compartment is REQUIRED (block or `OCI_COMPARTMENT`
env), no default (a plain name resolves through `iam compartment list`; an `ocid1.`
value is used as-is). Zone resolution: `--zone` (ocid → name fetched; name →
as-is, trailing dot trimmed) else the compartment must hold EXACTLY one zone —
several → fail listing them (**exit 1**), zero → error. Record types are
pinned to `A | AAAA | CNAME`; all validation runs locally before any oci call.

| Flag (persistent) | Type | Default |
|---|---|---|
| `--zone` | string | `""` |
| `--name` | string | `""` — DNS label or fqdn inside the zone |
| `--type` | string | `""` — A, AAAA, or CNAME |
| `--value` | string | `""` — type-shaped target |
| `--ttl` | string | `"300"` (60..172800) |
| `--profile` | string | `""` — instance profile (carries the `cloud` block) |


- `dns records [--zone <id-or-name>]` — list records: domain / type / ttl / value.
- `dns add --name <label> --type <t> --value <v> [--ttl <s>]` — merges into the
  existing RRSet (round-robin records survive); already present = no-op.
- `dns rm --name <label> --type <t> --value <v>` — filters the value out of the
  RRSet (an emptied RRSet is removed entirely); no matching record = error.

Example: `kampodra dns add --name app --type A --value 203.0.113.10 --zone example.com`

## migrate

Tenant db migrations over SSH, applied THROUGH the repo checkout ON the VM
(the app's own script, path from the `migrateScript` config — default
`scripts/migrate-db.ts`; no parallel implementation; runs via `pnpm exec
tsx`). Order: root.db first
(`root/root.db` preferred, legacy flat `root.db` honored), then `tenant_*.db`
sorted, bounded-parallel. Preconditions are probed in one round-trip and fail
closed: repo root (must contain the migrate script), pnpm, tenant data dir.

| Flag | Type | Default |
|---|---|---|
| `--repo-root` | string | `""` — the repo checkout ON THE VM; required (beats `KAMPODRA_REPO_ROOT`) |
| `--jobs` | string | `""` → `MIGRATE_JOBS` env, else `4` (1–64) |
| `--allow-running` | bool | `false` — migrate while the api service runs (SQLITE_BUSY risk) |
| `--host` | string | `""` |
| `--profile` | string | `""` |
| `--ssh-key` | string | `""` |

Stop-first guard: refuses while the api service is running (init-aware:
`rc-service` on OpenRC, `systemctl` on systemd; neither found = loud warning and
proceed). `--allow-running` overrides with a warning. Per-file failures are
COLLECTED (one bad tenant must not block the others); **exit 1 if any failed —
do NOT start the API on a half-migrated estate**.
Example: `kampodra migrate --host root@203.0.113.10 --repo-root /srv/app --jobs 1`

## status

Live view: deployment count (ledger), health through the proxy
(`https://<proxyHost><healthPath>` + build-id), and — when a host resolves —
VM disk on `/var/lib/containers` (warn above 90%), sha-tagged image count +
prune estimate (keep 2), an init-aware service roll call, and with `--verbose`
the full metrics snapshot. Never fails on an unhealthy system — it renders
state.

| Flag | Type | Default |
|---|---|---|
| `--host` | string | `""` |
| `--profile` | string | `""` |
| `--ssh-key` | string | `""` |
| `--verbose` | bool | `false` (metrics snapshot; needs a target) |
| `--all-profiles` | bool | `false` — one full section per configured profile |

Example: `kampodra status --host root@203.0.113.10 --verbose`

## bluegreen

OCI reserved-IP pair for zero-downtime cutovers. One instance can start
alone; the pair shares one RESERVED public IP (derived from live OCI
state — no local state file). Instances are `<container>-blue/green`.
Auth: profile `cloud` block / env / native (no cloud flags).

- `status` — pair view: reserved IP + holder, both instances (ocid, AD),
  per-color app health. Read-only.
- `init` — create the DORMANT reserved IP (`kampodra-active`); idempotent.
- `provision <blue|green>` — launch the sibling from the other color's AD
  + subnet (`VM.Standard.A1.Flex` 2/12). Native: newest `<container>-alpine*`
  custom image with UEFI_64 firmware. Else inject: platform-image launch
  (`--platform-key` file / `OPS_SSH_PUBKEY`, `--platform-user` /
  `PLATFORM_SSH_USER` default `ubuntu`) + golden qcow2 streamed onto the
  boot disk (`--qcow2` / `ALPINE_QCOW2`; qemu-img required) + reboot +
  `/etc/alpine-release` verify. `--image-id` forces one image. Ends with
  `vm-prepare` next steps (does not run it).
- `flip --to <color> [--force]` — ACME-first: health gate on the target's
  own IP (unless `--force`) → anchor conf on the target guest (watcher
  configures it) → reserved IP assigned (waits ASSIGNED) → ACME on the
  target for the `<ip-dashes>.sslip.io` hostname → valid-cert HTTPS +
  served-sha verify through the reserved IP → FLIPPED. Post-assign failure
  auto-rolls back to the other color's EXISTING anchor (lookup-only) or
  to dormant, then cleans the failed target's guest. Bad `--to` = **exit 2**.
- `rollback` — unassign to DORMANT: holder resolved by anchor match
  (lookup-only), guest cleaned (conf + address), unassign waits AVAILABLE.

## image-import

Upload a packer-built qcow2 and import it as an OCI custom image (build
machine; self-supported Alpine, PARAVIRTUALIZED). `--image` required;
`--bucket` (`KAMPODRA_IMPORT_BUCKET`, else `<container>-image-import`);
`--name-prefix` (default `<container>-alpine` — provision looks this up);
`--compartment`; `--keep-object`. Polls AVAILABLE (30m cap, terminal
states fail); non-UEFI firmware warns loudly (A1 rejects, x86 launches);
staged object deleted unless kept; prints the image OCID.

## Exit codes

- `0` success (including degraded-but-loud views like `deploy list` without a VM).
- `2` unknown top-level command (the offender is named, the index reprinted).
- `1` everything else that errors, plus the defined semantic failures:
  `env diff` differing (GNU diff convention), `metrics --disk-threshold` threshold
  reached, `migrate` per-file failures (do-not-restart signal), `backup list`
  LIST-denied, `backup download` digest mismatch, `dns` ambiguous multi-zone
  compartment, `deploy logs --follow` / `ssh` / `deploy shell` propagating a
  non-zero remote exit code.

## Agent gotchas

- **Rollback is a FLAG, not a subcommand**: `kampodra deploy --rollback [<sha7>]`.
  Bare `--rollback` resolves from the VM's deployed-sha stamp and DIES without
  one — it never falls back to git HEAD.
- **Cloud auth is provider-native**: `backup`/`dns` take no auth flags.
  The instance profile's `cloud` block (`profile`/`compartment`/
  `instancePrincipal`) overrides; `OCI_PROFILE`/`OCI_COMPARTMENT` env are
  the escape hatch; with neither set the oci CLI resolves its default
  itself. `--profile` always means the kampodra INSTANCE profile.
- **`migrate` runs the app's own script**: path from the `migrateScript`
  config (default `scripts/migrate-db.ts`) inside the VM repo checkout,
  executed with `pnpm exec tsx`; `--repo-root`/`KAMPODRA_REPO_ROOT` is
  mandatory.
- **`vm-wipe` is destructive**: `--yes` gates everything; the host-mismatch
  refusal needs `--force`; `--keep-data` spares only the tenant data dir.
- `env push`/`deploy --env-file` overwrite the remote env atomically but do NOT
  restart — follow with `deploy restart` or a deploy.
- `env pull` without `--out` puts RAW VALUES on stdout (summary on stderr);
  never pipe it to a log.
- The manifest key is `envFile`; the profile `project` block's legacy key is
  `envFilePath`. Only `envFile` is valid in `kampodra.json` (strict parse).
- The bucket ladder is `--bucket` > `KAMPODRA_BUCKET` (the project ladder's
  own bucket layer) > project config `bucket`.
- `deploy prune` and `vm-wipe` both delete permanently — always `--dry-run` /
  `--keep-data` first when unsure.
- `bluegreen` (reserved-IP pair: `status | init | provision | flip --to | rollback`) and `image-import --image` are live; flips are ACME-first with auto-rollback, imports warn on non-UEFI firmware.
