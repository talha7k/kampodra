---
name: kampodra
description: "Operates the kampodra CLI, a kamal-alternative that deploys containers or binaries to Linux VMs over SSH with podman and kamal-proxy — no registry in the image path, zero-downtime by default. Use when deploying or rolling back a release, running the standby-update bluegreen flow (deploy to the idle color then flip), recovering an interrupted rolling deploy, reading deploy logs/status, pushing or diffing remote env files, generating env from a schema, managing instance profiles, listing or verifying backups, planning a restore, managing DNS records on OCI or GCP, staging golden images, running tenant database migrations, sampling VM metrics, preparing or wiping VMs, or running read-only database queries and snapshots (sqlite, MariaDB, self-hosted libsql/sqld). Triggers: deploy, rollback, podman, kamal-proxy, SSH, env, metrics, backup, DNS, gcp, gcloud, oci, bluegreen, flip, migrate, VM, Alpine, db query, db snapshot, libsql, sqld, mariadb, sqlite."
license: AGPL-3.0
compatibility: Requires podman, ssh, and git locally; the target VM needs a Linux userland (Alpine or Ubuntu 24.04) with podman and kamal-proxy (vm-prepare installs them). Cloud families authenticate through the provider CLI itself (oci or gcloud).
metadata:
  author: kampodra maintainers
  version: 0.8.0
---

# kampodra

## Overview

kampodra ships a git repository's app container straight to a Linux VM over SSH: `podman build` locally, `podman save | ssh podman load` to stream the image (no registry in the path), env push, init-aware restart, a served-sha health gate, a kamal-proxy re-point, then a public smoke check. It also owns the full instance lifecycle: rollback, deploy history and pruning, logs and shell, env management, metrics, OCI Object Storage backups, OCI DNS records, tenant database migrations, and VM prepare/wipe.

Every host-aware command resolves its target the same way (first match wins):

- profile selection: `--profile` flag > `KAMPODRA_PROFILE` env > the config's `defaultProfile`
- host: `--host` flag > profile `host` > `KAMPODRA_HOST`
- ssh key: `--ssh-key` flag > profile `sshKey` > `KAMPODRA_SSH_KEY` (empty = ssh agent / `~/.ssh/config`)

The project's shape (container name, env-file path, ports, bucket, ...) resolves through the ladder:

```
flags > KAMPODRA_* env > profile "project" block > kampodra.json > built-in defaults
```

`kampodra config print` renders the fully resolved effective project config with per-field provenance — always the first debugging stop when a value is not what you expect. Full flag-level detail for every command lives in [references/commands.md](references/commands.md).

## Prerequisites

- Local: Go 1.27+ to build; `git`, `podman`, and `ssh` on `PATH`.
- Remote: a Linux VM reachable over key-based SSH (kampodra targets Alpine + OpenRC; systemd hosts are detected for restart/stop paths). `vm-prepare` bootstraps a bare Alpine host with the podman stack, OpenRC services, and kamal-proxy.
- Build once: `go build -o kampodra ./cmd/kampodra` (source build is the supported install path). Verify with `kampodra --version`.

## Configure once, deploy forever

1. Create a profile (per-instance, stored in `~/.kampodra/config.json`, dir 0700 / file 0600; ssh keys are stored as paths only — never secrets):

   ```
   kampodra config init --name prod --host root@203.0.113.10 --ssh-key ~/.ssh/id_ed25519 --proxy-host app.example.com --set-default
   ```

   The first profile auto-becomes the default. Hosts/keys live only in profiles; the committed repo file `kampodra.json` carries project naming only.

2. (Recommended) Commit a `kampodra.json` at the repo root naming YOUR app. The built-in defaults are deliberately generic placeholders (`app`, `/etc/kampodra/env`, `app-backups`, …) — set your real naming in the manifest (19 optional camelCase fields: `container`, `shadowSuffix`, `envFile`, `dataDir`, `bucket`, `objectPrefix`, `healthPath`, `healthPaths`, `proxyHost`, `services`, `imagePrefix`, `port`, `network`, `shadowProbePort`, `deployedShaFile`, `envClearKeys`, `dockerfile`, `migrateScript`, plus a `unit.env` map rendering repo-owned -e flags into the app unit; an optional `"$schema"` key points at docs/kampodra.schema.json for editor autocomplete and is ignored). The parse is strict: malformed JSON or an unknown key FAILS CLOSED — a typo errors (naming the valid keys) instead of silently resolving to defaults. See the field table in [references/commands.md](references/commands.md).

3. Push env (values are never printed; only KEY + fingerprint):

   ```
   kampodra env push --file ./ops/env.production            # or generate locally: kampodra env from-schema
   ```

4. First bootstrap on a fresh VM: `kampodra vm-prepare --host root@203.0.113.10` (idempotent; the app container is intentionally NOT started — the first deploy provides image and env).

## First deploy and verification

```
kampodra deploy                      # build HEAD (clean-tree gate) and run the full pipeline
kampodra status                      # live health through the proxy + deployment count + VM state
kampodra deploy logs --lines 100     # tail the app container (--since 2h catches boot-first lines)
```

Deploy stamps the git sha. In deploy mode the clean tree is gated, so what HEAD names is what ships. `kampodra deploy list` shows history (VM sha-tagged images merged with the local ledger `~/.kampodra/deployments.jsonl`); `kampodra metrics` snapshots load/memory/disk/containers; `deploy prune` reclaims disk from old sha-tagged images (always preview with `--dry-run`).

## Rollback: a flag, not a subcommand

There is NO `rollback` subcommand. Rollback is the flag `deploy --rollback [<sha7>]`:

```
kampodra deploy --rollback ccc3333   # explicit target
kampodra deploy --rollback           # resolves from the VM's deployed-sha stamp
```

Target resolution: the explicit sha, else the VM's deployed-sha stamp file, else it DIES — it never falls back to git HEAD (rolling back must not redeploy the very build you are rolling back from). Rollback skips the build/stream when the tag still exists on the VM and never rewrites the stamp.

## Zero-downtime deploys and recovery

`kampodra deploy --rolling` boots the new version as a shadow container, health-gates it BEFORE any traffic can reach it, then double re-points kamal-proxy (to the shadow, then back to the new main). Failure paths keep the safer state — but if an interrupted run exits loudly mid-switch, the shadow is left SERVING on purpose. Finish it with:

```
kampodra deploy converge [<sha7>]    # retag → restart → health gate → re-point to main → remove shadow
```

A leftover shadow must be converged before the next `--rolling` deploy. `--drain-timeout` (default 10s, rolling only) bounds the proxy-switch confirmation.

## Safety: destructive and guarded operations

| Operation | Behavior | Guard |
|---|---|---|
| `vm-wipe` | Stops/disables services, removes project containers, prunes images, deletes env/stamp/state | `--yes` REQUIRED; `--keep-data` spares the data dir; `--force` overrides a services-mismatch refusal (wrong host?) |
| `vm gc --apply` | Deletes ONLY stale scratch/scripts/residues from the typed sweep inventory | Dry run by default; hard-coded never-touch allowlists (tenant data, live chains, kampodra helpers, OS, build contexts, owner decisions); podman strictly report-only; in-use candidates blocked |
| `backup restore-plan <object>` | PRINTS the documented recovery sequence | Never executes anything |
| `backup verify <file>` | Read-only: uploads to VM scratch files and runs the VM's `restore-verify` | Never writes into the data dir |
| `deploy prune` | Removes old sha-tagged images (keeps running + ts-rollback + newest N) | `--dry-run` prints exact commands first |
| `migrate` | Writes db schema | Stop-first guard: refuses while the api service runs unless `--allow-running` |
| `env push` | Overwrites the remote env file atomically | Preview first with `env diff <local-file>` (exit 1 = differs) |
| `vm-prepare` | Mutates a bare host (packages, sshd, services) | Idempotent; safe to re-run |

## Read-only database access (db)

`kampodra db query` (one SELECT/WITH → rows on stdout) and
`kampodra db snapshot` (verified consistent copy, 0600) over ssh, three
engines behind `--engine auto` (flag > the VM env file's
KAMPODRA_DB_ENGINE > file magic):

- **sqlite** — `--db` is the file's path; rows as flat JSON; snapshot via
  `sqlite3 .backup` (consistent).
- **mariadb** — `--db` is the schema NAME; creds ride the VM env file
  (KAMPODRA_DB_NAME/HOST/USER/PASSWORD via `kampodra env set`), staged to
  a 0600 defaults file, never argv; snapshot = mariadb-dump
  --single-transaction with a "Dump completed" tail gate. Prefer a
  read-only role (GRANT SELECT) for agent queries.
- **postgres** — same env-file contract as mariadb; PGPASSFILE staged
  0600; pg_dump MVCC-consistent with a completeness tail gate.
- **libsql (self-hosted sqld)** — no --db needed; KAMPODRA_DB_URL
  (default localhost:8080) + optional KAMPODRA_DB_TOKEN, staged to a 0600
  header file; rows as flat JSON (same shape as sqlite); snapshot needs
  KAMPODRA_DB_PATH and gates on PRAGMA integrity_check.

Server engines on their own VM: `--db-host` / profile `dbHost` moves the
flows' ssh to the DB VM (its env file carries the creds). TSV engines
emit string values (batch text has no types); file engines emit typed
JSON.

`kampodra db grant-ro --db <schema>` provisions the read-only role:
mariadb executes end-to-end (random password, env keys written on the
VM, password never printed); `--engine postgres` prints the ready SQL.

The gate rejects non-SELECT/WITH statements (exit 2) BEFORE any ssh on
every engine — a rejected statement costs zero round-trips.

```
kampodra db query --db /data/root.db --sql "SELECT count(*) FROM org" --profile prod
kampodra db query --db esellar --sql "SELECT id, total FROM pos_order LIMIT 5"   # mariadb, after env set
kampodra db snapshot --db esellar --out ./esellar.sql
kampodra db snapshot --out ./app.db                                              # libsql, after env set
```

## Gotchas

- Defaults are GENERIC placeholders (`app`, `/etc/kampodra/env`, `app-backups`, `/up`), not your app's names — commit a `kampodra.json` before the first deploy against a new tree, or check `config print`.
- `migrate` runs the app's own script on the VM: the path comes from the `migrateScript` config (default `scripts/migrate-db.ts`), executed with `pnpm exec tsx` inside `--repo-root` (or `KAMPODRA_REPO_ROOT`). Point `migrateScript` at your repo's real layout.
- `dns`/`backup`/`bluegreen` are multi-provider (oci | gcp behind the neutral dispatch; one `RunCloud` seam): they authenticate through the provider CLI's own auth, and kampodra passes no auth flags unless the instance profile's `cloud` block overrides it (oci: `profile`/`compartment`/`instancePrincipal`; gcp: `provider: gcp` + `project`/`account`); `OCI_PROFILE`/`OCI_COMPARTMENT` env work as the oci escape hatch, else the provider CLI resolves natively; gcp scopes natively via the block's `project` (dns also takes `dnsCompartment` + `--dns-compartment` for split-compartment zones). `image-import` is OCI-only by design (a `gcp` profile fails closed — Cloud Build trust-shape). `bluegreen provision` pins `VM.Standard.A1.Flex` on oci, `e2-standard-2` on gcp. `--profile` always means the kampodra INSTANCE profile.
- Fleet profiles: `--group web` filters `config list` / `status` / `deploy list`; `config clone prod prod-2 --host root@<ip>` stamps out a sibling with explicit copies (no live link).
- `bluegreen` manages the reserved-IP pair (`status | init | provision <color> | deploy --to <color> | flip --to <color> [--force] | rollback`) — pairs + reserved IP derive from LIVE provider state, no manual-IP fallback. `deploy --to <color>` is the standby-update composite: build once → push to the idle color → health-gate it there → flip (pipeline failure = no flip). A flip warns when the CURRENT holder is unhealthy (no healthy fallback after cutover). `image-import --image <qcow2>` stages+imports golden images on oci (warns on non-UEFI firmware). See [references/commands.md](references/commands.md).
- `deploy` health-gates and smokes EVERY path in the manifest's `healthPaths[]` (default: exactly [`healthPath`]) — pin the routes that must answer, not just the shallow one. Binary fast-pushes are sha256-fingerprinted and byte-verified on the VM before the swap; a stale rebuild (bytes identical to the previous push under a new sha) warns loudly.
- No command ever prints env values — only KEY + fingerprint tables (value length + first 2 chars). Raw values move only through `env push`'s upload and `env pull`'s stdout/`--out` payload.

## Common flows

- Bad release, revert now: `kampodra deploy --rollback` (stamp-resolved), confirm with `kampodra status`.
- Disk pressure: `kampodra metrics --disk-threshold 90` → `kampodra deploy prune --dry-run` → apply.
- Env drift suspected: `kampodra env diff ./ops/env.production` → `kampodra env push --file ./ops/env.production` → `kampodra deploy restart`.
- One-off inspection on the VM: `kampodra ssh df -h` (argv passes through verbatim), or inside the container `kampodra deploy shell exec -- df -h`.
- Schema change: stop-first `kampodra migrate --repo-root /srv/app` (failures are collected per tenant db; exit 1 means do NOT restart the API on a half-migrated estate).

For every command's full flags, defaults, and exit codes, read [references/commands.md](references/commands.md).
