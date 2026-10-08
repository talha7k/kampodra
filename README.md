# kampodra

A registry-free deployment CLI for single-VM container stacks — built with
**kamal-proxy**, **Podman**, and **Cobra**.

kampodra deploys container images to any Linux host over plain SSH (no
registry, no daemon-side agents): it builds locally, streams the image with
`podman save | ssh podman load`, tags every release with its git sha,
health-gates it, and re-points the kamal-proxy TLS edge. Rollback is an
instant image-tag flip. On top of deploys it manages the whole instance
lifecycle: env files, metrics, backups, DNS, and deployment history.

## Highlights

- **Registry-free streaming deploys** — `podman save | ssh podman load`, sha-tagged images, served-sha health verification
- **Zero-downtime rolling deploys** (roadmap: opt-in shadow-container swap through kamal-proxy's health-gated re-point)
- **Instant rollback** — `deploy rollback [<sha>]` flips the running tag
- **Deployment ledger** — every deploy recorded locally + derived from the host's image tags (`deploy list`, total count)
- **Disk guard + prune** — warns/fails over threshold; reclaims old images while protecting running/rollback tags
- **Env management** — push/pull the remote env file; values are never printed (fingerprints only)
- **Metrics** — one-shot CPU/memory/disk/container snapshot over SSH
- **Backups** — list/download/verify object-storage backups; `restore-plan` prints (never executes) the recovery sequence
- **Profiles** — per-instance config (`~/.kampodra/config.json`), flat and multi-host ready; auto-detects OpenRC vs systemd
- **Command-parity guard** — the shell-era CLI surface is a machine-checked ratchet (see `internal/command/parity_guard_test.go`)

## Requirements

- **Go 1.24+** (build from source) or **Node 18+** (npm distribution)
- A **Linux host** with:
  - **Podman** (rootful or rootless) — no Docker daemon needed
  - **OpenSSH** access (key-based) from your machine
  - **kamal-proxy** container as the TLS edge (optional but recommended)
- Local: `ssh`, `podman`, `git`

## Getting started

```bash
# From source
git clone https://github.com/talha7k/kampodra && cd kampodra
go build -o kampodra ./cmd/kampodra
./kampodra --help

# Via npm (when published)
npm install -g kampodra
kampodra --help
```

### 1. Point it at a host

```bash
kampodra config init --name prod --host root@203.0.113.10 \
  --ssh-key ~/.ssh/id_ed25519 --proxy-host app.example.com
```

### 2. Deploy

```bash
cd your-app && kampodra deploy --dockerfile Dockerfile
# → build, stream, health-gate, re-point the proxy, smoke, ledger entry
```

### 3. Operate

```bash
kampodra deploy list          # history + total deployments
kampodra deploy logs --lines 100
kampodra metrics              # CPU/mem/disk/containers snapshot
kampodra backup list          # object-storage backups
kampodra deploy rollback      # flip to the previous sha
```

## Architecture

Ports-and-adapters, strictly layered (enforced by a static test):

```
cmd/kampodra        thin CLI (Cobra)
internal/command    orchestration — no OS knowledge
internal/adapter    transport (ssh) · runtime (podman) · init (openrc/systemd)
                    probe · osfacts · cloud (OCI) · project · state
tools/paritygen     HISTORICAL: derived the frozen command-spec golden from
                    the shell predecessor (not a live step; kept auditable)
npm/                per-platform packages + JS bin shim (esbuild pattern)
```

Remote shell snippets live only inside adapters as fixture-tested templates —
the single place host-OS specifics (busybox vs GNU, OpenRC vs systemd) may
exist. The deployed project's naming (container name, env-file path, bucket,
health endpoint, …) lives in `internal/adapter/project` exactly once, with
profile (`config.json` `"project"` block) and `KAMPODRA_*` env overrides.

## History

kampodra is the Go successor of the shell-era **kampodine** CLI; the shell
line is retired (last release 0.6.0) and its command surface lives on as the
frozen spec in `internal/parity/golden.json` — the parity guard tracks that
spec, and kampodra-native additions beyond it are review-flagged, never
silent.

## Status

Pre-0.7 alpha — the command surface is being ported from the frozen shell
spec under a machine-checked parity ratchet. See `CHANGELOG.md`.

## License

MIT
