# Getting started

Zero to first deploy: prerequisites, build, configure, deploy, verify,
roll back — plus the caveats and troubleshooting notes worth reading before
your first production deploy.

## Prerequisites

**Local machine:**

- **Go 1.27+** (see `go.mod`)
- `git` — deploys are stamped with the git sha; deploy mode requires a clean tree
- `ssh` — all remote work happens over SSH
- `podman` — the local build and the image stream

**Remote host:**

- Any Linux host reachable over **key-based SSH** (root, or a user with podman access)
- **Podman** (rootful or rootless) — no Docker daemon needed
- **kamal-proxy** — optional but recommended; it is the TLS edge that deploys re-point. `vm-prepare` can install it for you (note: `vm-prepare` targets a bare **Alpine** host — OpenRC, no systemd).

## Build from source

```bash
git clone https://github.com/talha7k/kampodra && cd kampodra
go build -o kampodra ./cmd/kampodra    # or: make build
./kampodra --version
```

Bare `./kampodra` prints the command index; every command supports `--help`
with usage and examples.

## 1. Point kampodra at a host

```bash
kampodra config init --name prod --host root@203.0.113.10 \
  --ssh-key ~/.ssh/id_ed25519 --proxy-host app.example.com
```

This writes a profile to `~/.kampodra/config.json` (dir 0700, file 0600).
The ssh key is stored as a **path** — secrets never live in the config. The
first profile you create automatically becomes the default, so plain
`kampodra deploy` targets it. Manage profiles with `config list`,
`config show`, `config set-default`, `config remove`; select a non-default
one per command with `--profile <name>`. Fleets: tag profiles with
`--group web`, then `config list --group web`, `status --group web`, and
`deploy list --group web` work the whole group; `config clone prod prod-2
--host root@<ip>` stamps out a sibling profile with explicit copies (the
new profile owns its values — no live link).

## 2. First deploy

From the app's git repository (it must have a `Dockerfile` and a clean
tree):

```bash
cd your-app
kampodra deploy --dockerfile Dockerfile
```

What happens, in order:

1. **Clean-tree gate** — the deploy sha comes from `git rev-parse HEAD`; deploy mode refuses a dirty tree (the tag must describe what was built). Not in a git repo? Use `deploy --sha <sha7>` to stream an existing local build.
2. **Build** — `podman build` with `--build-arg GIT_SHA=<sha>`; the context is the repo root; the image is tagged with the short sha.
3. **Stream** — `podman save | ssh podman load`. No registry: the image travels over SSH.
4. **Env push** (with `--env-file <file>`) — uploads the env file as 0600 via an atomic move, stamped with the sha.
5. **Restart** — init-aware (OpenRC `rc-service` or `systemctl`, auto-detected).
6. **Health gate** — the VM verifies the running container now actually serves *this* deploy's sha at the configured health path. A failed gate stops the pipeline before the proxy moves.
7. **Proxy re-point** — kamal-proxy targets the new container (on a fresh install, the first TLS certificate issues here).
8. **Smoke** — a public check through the edge (`--skip-smoke` to skip).
9. **Ledger + cleanup** — the deploy is recorded in `~/.kampodra/deployments.jsonl`, disk usage is reported, and old images beyond the keep-set are cleaned up.

Useful variants: `deploy --rolling` for the zero-downtime shadow-container
switch (`--drain-timeout`, default 10s, bounds the proxy switch);
`deploy --disk-threshold <pct>` to fail closed when the VM disk is at/over a
percentage **before** anything is built or streamed.

On a brand-new VM, bootstrap it first:

```bash
kampodra vm-prepare --host root@203.0.113.10
```

`vm-prepare` is idempotent: sanity gates, sshd hardening, the podman stack,
OpenRC services for the app container and kamal-proxy. The app container is
**not** started — the first deploy provides image and env. See the caveats
below before using it on a non-default project shape.

## 3. Verify

```bash
kampodra status                  # live health + deployment count + VM state
kampodra deploy list             # history (ledger + the VM's sha-tagged images) + total
kampodra deploy logs --lines 100 # tail the app container's logs (--follow to stream)
```

`status --verbose` adds the full metrics snapshot; `metrics` gives the same
one-shot snapshot on demand (`--disk-threshold <pct>` exits non-zero at the
threshold, default 90).

## 4. Roll back

Rollback is a **flag** on `deploy`, not a subcommand:

```bash
kampodra deploy --rollback <sha7>   # explicit target
kampodra deploy --rollback          # resolve from the VM's deployed-sha stamp
```

The target resolves: explicit sha argument, else the VM's deployed-sha
stamp (written by every successful deploy), else it **fails** — a bare
rollback never falls back to git HEAD. When the target image still exists
on the VM, the build/stream is skipped entirely: rollback is an instant
tag flip. If you interrupted a `--rolling` deploy, finish it first with
`kampodra deploy converge` (see troubleshooting below).

## 5. Where to next

- **`env`** — `env list | push | pull | fingerprint | diff` manage the remote env file; values are never printed, only fingerprints (key + length + first 2 chars). `env from-schema` generates the file from a committed `.env.schema` via varlock.
- **`metrics`** — one-shot load/memory/disk/container snapshot over SSH; `--watch <sec> --count <n>` re-snapshots.
- **`backup`** — `list | download | verify | restore-plan` against an OCI Object Storage bucket; `restore-plan` prints the recovery sequence and never executes it.
- **`dns`** — OCI DNS `records | add | rm` through the `oci` CLI's own auth; requires `OCI_COMPARTMENT`.
- **`migrate`** — tenant db migrations over SSH (`--repo-root` required); refuses to run while the app service is up unless `--allow-running`.

Full command reference: [cli.md](cli.md).

## Caveats: override the built-in defaults

The out-of-box defaults are deliberately generic placeholders (`app`,
`/etc/kampodra/env`, `app-backups`, …) — set your project's real naming in
a committed `kampodra.json` and let the defaults be the fallback they are
meant to be:

| Field | Built-in default |
|---|---|
| `container` | `app` |
| `envFile` | `/etc/kampodra/env` |
| `bucket` | `app-backups` |
| `healthPath` | `/up` |
| `imagePrefix` | `127.0.0.1:5000/app` |
| `envClearKeys` | `NODE_ENV`, `PORT` |
| `deployedShaFile` | `/etc/kampodra/deployed-sha` |
| `dataDir` | `/data` |
| `services` | `app`, `kamal-proxy` |
| `proxyHost` | `app.example.com` |

(Also defined: `shadowSuffix`, `objectPrefix`, `port`, `network`,
`shadowProbePort`, `dockerfile`, `migrateScript`.)

Override them at whichever layer fits — highest wins:

1. **`kampodra.json`** (repo manifest, committed): 17 optional camelCase
   fields — `container`, `shadowSuffix`, `envFile`, `dataDir`, `bucket`,
   `objectPrefix`, `healthPath`, `proxyHost`, `services`, `imagePrefix`,
   `port`, `network`, `shadowProbePort`, `deployedShaFile`, `envClearKeys`,
   `dockerfile`, `migrateScript`. Parsed strictly: malformed JSON or unknown
   keys **fail closed** (the error lists the valid keys). For editor
   autocomplete, point `"$schema"` at
   [docs/kampodra.schema.json](kampodra.schema.json).
2. **Profile `project` block** in `~/.kampodra/config.json` (same field
   keys; the env-file key `envFile` is accepted here too).
3. **`KAMPODRA_*` env vars** — one per field (`KAMPODRA_CONTAINER`,
   `KAMPODRA_ENV_FILE`, `KAMPODRA_IMAGE_PREFIX`, …).

`kampodra config print` renders the fully resolved effective config with
per-field provenance — the definitive answer to "where did this value come
from?".

Additional shape-specific caveats:

- **`migrate` runs the app's own script on the VM** — resolved from the
  `migrateScript` config (default `scripts/migrate-db.ts`) inside
  `--repo-root` (or `KAMPODRA_REPO_ROOT`), executed with `pnpm exec tsx`
  on the VM. Point `migrateScript` at your repo's real layout.
- **`vm-prepare` writes a generic OpenRC unit** — `NODE_ENV=production`,
  `PORT=<port>` plus your resolved project naming. On an unusual project
  shape, review the generated unit before relying on it.
- **`backup`/`dns` are OCI today** (auth via the `oci` CLI; the adapter is
  isolated in `internal/adapter/cloud`) — everything else is cloud-agnostic.

## Troubleshooting

**Disk guard.** `deploy --disk-threshold <pct>` fails closed before the build
when the VM disk is at/over the percentage; above 90% used, deploys,
`status`, and `metrics` all warn loudly regardless. `metrics
--disk-threshold <pct>` (default 90) exits 1 at the threshold. The remedy is
when the VM disk is at/over the percentage; above 90% used, deploys,
`status`, and `metrics` all warn loudly regardless. `metrics
--disk-threshold <pct>` (default 90) exits 1 at the threshold. The remedy is
almost always `kampodra deploy prune --dry-run` first — it removes old
sha-tagged images while always keeping the running image, the ts-rollback
tag, and the newest `--keep N` (default 2).

**Interrupted rolling deploy.** A `deploy --rolling` that dies mid-switch
leaves a shadow container behind, and the failure paths are designed so the
safer state survives (a bad shadow never took traffic; a completed switch
leaves the shadow serving). Finish the deploy explicitly:

```bash
kampodra deploy converge          # or: kampodra deploy converge <sha7>
```

Converge retags, restarts, health-gates, re-points to the main container,
and removes the shadow. A leftover shadow must be converged before the next
`--rolling`.

**"target required" / wrong host.** Host-aware commands resolve the target
through `--host` > profile `host` > `KAMPODRA_HOST`, the key through
`--ssh-key` > profile `sshKey` > `KAMPODRA_SSH_KEY`, and the profile
through `--profile` > `KAMPODRA_PROFILE` > `defaultProfile`. Setting
`KAMPODRA_HOST=root@203.0.113.10` (and optionally `KAMPODRA_SSH_KEY`) is
the quickest way to work profile-free; `KAMPODRA_PROFILE` switches the
default for a session.

**Backup bucket / OCI.** The backup bucket resolves `--bucket` >
`KAMPODRA_BUCKET` > project config. The `dns` and `backup` families
authenticate through the `oci` CLI only, and kampodra passes no auth flags
unless the instance profile's `cloud` block overrides it
(`{"profile": "…", "compartment": "…", "instancePrincipal": true}` in
`~/.kampodra/config.json`); `OCI_PROFILE` / `OCI_COMPARTMENT` env are the
escape hatch. A `dns` compartment is still **required** — from the block or
the env, no default.

**Anything else.** Every command supports `--help`; bare `kampodra` prints
the command index. `kampodra config print` is the debugging tool for any
"why is it using *that* value" question.
