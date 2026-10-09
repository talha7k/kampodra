# kampodra

kampodra is a registry-free deployment CLI for single-VM container stacks.
It builds your container image locally, streams it to any Linux host over
plain SSH (`podman save | ssh podman load`), health-gates it by its git sha,
and re-points the kamal-proxy TLS edge — no registry, no daemon-side agent.
Around deploys it covers the instance lifecycle: env files, metrics,
backups, DNS, tenant migrations, and deployment history.

## Highlights

- **Registry-free streaming deploys** — sha-tagged images, served-sha health verification, kamal-proxy re-point
- **Zero-downtime rolling deploys** — opt-in shadow-container double re-point (`deploy --rolling`)
- **Instant rollback** — `deploy --rollback [<sha7>]` flips the running tag (explicit sha, else the VM's deployed-sha stamp — never git HEAD)
- **Disk guard + prune** — deploy fails closed past a configurable disk threshold; `deploy prune` reclaims old images while protecting running and rollback tags
- **Env management** — push/pull/diff the remote env file; values are never printed (fingerprints only)
- **Tenant migrations** — `migrate` runs the app's own migration script over every db file on the VM: root first, tenants bounded-parallel, stop-first guard
- **Metrics** — one-shot CPU/memory/disk/container snapshot over SSH
- **Backups** — list/download/verify object-storage backups; `restore-plan` prints the recovery sequence and never executes it
- **DNS** — OCI DNS record management through the `oci` CLI's own auth
- **Profiles** — per-instance config in `~/.kampodra/config.json` (0700/0600), multi-host ready; auto-detects OpenRC vs systemd
- **Repo manifest** — a committed `kampodra.json` per project; `config print` renders the fully resolved effective config with per-field provenance
- **Machine-checked command surface** — a surface smoke test guards the command set

## Requirements

- **Go 1.27+** to build from source (the only install path today — the npm distribution is planned but not yet published)
- A **Linux host** with:
  - **Podman** (rootful or rootless) — no Docker daemon needed
  - **OpenSSH** access (key-based) from your machine
  - **kamal-proxy** as the TLS edge (optional but recommended)
- Local tools: `git`, `ssh`, `podman`

## Quick start

Build from source:

```bash
git clone https://github.com/talha7k/kampodra && cd kampodra
go build -o kampodra ./cmd/kampodra
```

Point it at a host, deploy an app from its git repo, check it:

```bash
kampodra config init --name prod --host root@203.0.113.10 \
  --ssh-key ~/.ssh/id_ed25519 --proxy-host app.example.com
kampodra deploy --dockerfile Dockerfile
kampodra status
```

The full zero-to-first-deploy walkthrough — including a fresh-VM bootstrap,
verification, and rollback — is in [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md).

## The resolution ladder

Every command resolves the deployed project's naming (container name,
env-file path, health endpoint, backup bucket, …) through one ladder:

```
flags > KAMPODRA_* env > profile "project" block > kampodra.json > built-in defaults
```

`kampodra.json` is a committed repo manifest, discovered upward from your
working directory (nearest file wins). It carries project naming only —
hosts, ssh keys, and secret material never belong there (those stay in
`~/.kampodra` profiles, 0600). A malformed manifest fails closed. The
built-in defaults are sample values of one app shape, not conventions —
override them for your project. `kampodra config print` shows the resolved
value and the winning source of every field.

## Documentation

- [docs/GETTING_STARTED.md](docs/GETTING_STARTED.md) — prerequisites, build, first deploy, rollback, troubleshooting
- [docs/cli.md](docs/cli.md) — full command reference
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — internal layout and design

## Status

Run `kampodra --version` for the installed version. Includes the OCI
reserved-IP blue/green pair (`bluegreen status | init | provision | flip
| rollback`) and golden-image import (`image-import`).

## License

[AGPL-3.0](LICENSE)
