# Architecture

kampodra is a registry-free deployment CLI: it builds a container image
locally, streams it to a Linux VM over plain SSH (`podman save | ssh
podman load`), health-gates it by git sha, and re-points the kamal-proxy
TLS edge. This document is the contributor-facing view of how the code is
laid out and why. For usage, start with [GETTING_STARTED.md](GETTING_STARTED.md)
and the command reference in [cli.md](cli.md); the user-level overview is
[../README.md](../README.md).

## Layering: ports and adapters

The tree is a strict three-layer ports-and-adapters architecture:

```
cmd/kampodra        thin entrypoint: version stamp + command.Execute
internal/command    Cobra command tree: orchestration + rendering
internal/adapter/*  every boundary to the outside world
```

- **cmd/kampodra** builds nothing and knows nothing; it imports
  `internal/command` only (plus stdlib for `os.Exit`).
- **internal/command** parses flags, resolves configuration, sequences
  adapter calls, and renders output. It has *no* OS knowledge: no ssh
  invocations, no podman, no remote-host specifics. Its only entry into
  the world is the injected `command.Deps` (home dir, working dir, env
  lookup, stdio, a `transport.Runner`, a prober) — tests substitute
  fakes, so no command test touches the network, filesystem, or real
  environment.
- **internal/adapter/** is the only OS-touching code. Each subpackage
  owns one boundary:

  | adapter      | boundary                                                              |
  |--------------|-----------------------------------------------------------------------|
  | `transport`  | the `ssh` binary (no SSH library): captured runs, stdin upload, streams |
  | `runtime`    | podman on the VM: sha-tagged images, containers, prune keep-set math   |
  | `init`       | service supervision: OpenRC vs systemd detection, service actions      |
  | `probe`      | the public edge: app health endpoint + served build id via kamal-proxy |
  | `osfacts`    | remote OS facts over ssh: `df -P`, disk-guard verdicts, metrics snapshot |
  | `cloud`      | the `oci` CLI: DNS records, object-storage backups                     |
  | `migrate`    | tenant-db migration composition: probe, plan (root first), per-file cmd |
  | `project`    | the deployed project's shape (naming) + the `kampodra.json` manifest   |
  | `state`      | local state: `config.json` profiles + the `deployments.jsonl` ledger   |
  | `envfile`    | env-file grammar, fingerprint masking, local-vs-remote diff            |
  | `envschema`  | env generation from a committed `.env.schema` via the repo's varlock   |
  | `vmbootstrap`| Alpine bootstrap payloads: managed files, OpenRC units, remote snippets |

The layering is enforced statically by `internal/arch/arch_test.go`:

- `TestEntrypointIsThin` — `cmd/kampodra` may import `internal/command`
  only among internal packages.
- `TestAdapterLayering` — adapters import stdlib and external libraries
  only: never `internal/command`, never the parent `internal/adapter`,
  and never a sibling adapter sideways. Adapter-to-adapter communication
  goes *up* through the command layer or through an injected seam (for
  example `init.RunFunc`, closed over the transport runner by the
  command layer).

## The two resolution ladders

All configuration is resolved before any adapter runs. There are two
independent ladders: *where* to deploy, and *what* the deployed project
is shaped like.

### Target resolution (where)

Implemented once in `internal/command/resolve.go` (`ResolveTarget`) and
shared by every host-aware command:

```
profile:  --profile flag > KAMPODRA_PROFILE > config.json defaultProfile
host:     --host flag    > profile.host     > KAMPODRA_HOST
ssh key:  --ssh-key flag > profile.sshKey   > KAMPODRA_SSH_KEY
```

An explicit per-invocation flag always wins; session env beats the
persisted profile layers. The proxy host follows the project ladder
(profile `project.proxyHost` > `KAMPODRA_PROXY_HOST` > project default;
the profile's legacy flat `proxyHost` field still wins when set).
Selection fails closed: an unknown profile name is an error that names
the known set.

### Project-shape resolution (what)

Implemented in `internal/adapter/project/project.go` (`ResolveTraced`).
One ladder covers every naming field — container name, env-file path,
health endpoint, backup bucket, image prefix, ports, networks, …:

```
flags > KAMPODRA_* env > profile "project" block > kampodra.json > built-in defaults
```

Each layer overrides only the fields it actually provides; unprovided
fields fall through to the next layer. `config print` renders every
resolved field together with its winning source (flag / env / profile /
repo-file / default) using the per-field provenance traces the same
function returns.

The two middle layers have a deliberate fail-open vs fail-closed
asymmetry:

- **`kampodra.json` (repo manifest, `internal/adapter/project/manifest.go`)
  fails closed.** It is a committed config file — a typo must error, not
  silently resolve to defaults. It is parsed with unknown-key rejection,
  and discovery walks upward from the working directory (nearest file
  wins). It carries project *naming only*: hosts, ssh keys, and any
  secret material never belong in a committed file.
- **The profile `project` block fails open.** A corrupt block is ignored
  and the lower layers (manifest, env, defaults) apply. It is an
  optional override layer, not data; failing open keeps a hand-edited
  `config.json` from bricking every command.

Built-in defaults are named values in `project.LoadDefault()` — one
sample app shape, not conventions. Override them per project; nothing
else hardcodes them (a guard test keeps raw literals out of the
adapter).

## Remote shell snippets live only in adapters

Everything host-OS-specific — busybox vs GNU utilities, OpenRC vs
systemd, apk, podman invocation shapes — exists only inside adapter
packages, as exported command templates and render functions pinned by
fixture tests (for example the vmbootstrap snippet/gate tests, or the
init-detection probe). Adapters do not ssh by themselves: they expose
pure template/parser functions and receive execution through seams (a
`RunFunc` or an injected runner) that the command layer closes over the
resolved host and transport.

The consequence: supporting a new host flavor means extending exactly
one adapter (and its fixtures), while the command layer stays
host-agnostic and its tests stay offline.

## The command surface guard

The CLI's command set is machine-checked by one plain test — no frozen
spec, no golden files:

- `internal/command/surface_test.go` (`TestCommandSurface`) walks the
  live Cobra tree and asserts the exact set of implemented commands
  (each with a `Use` and a `Short`). Registering a command silently, or
  advertising one that errors "not implemented", fails the test.
- Unimplemented ideas are roadmap items, not placeholder commands: they
  appear nowhere in the CLI (blue-green deploys and image import are the
  current roadmap).

Historically the surface was guarded by a frozen-spec "parity ratchet"
(a golden JSON derived from the project's retired shell-era predecessor,
with a baseline of not-yet-ported commands and a generator tool). That
apparatus was retired once the port completed — the predecessor lineage
is a fact of history, not a live dependency: behavior contracts
(timeouts, exit codes, byte shapes) live in ordinary tests now.

## State on disk

- **`~/.kampodra/config.json`** — per-instance profiles
  (`internal/adapter/state`). The directory is created 0700 and the
  file written 0600, atomically (same-dir temp file + rename, so a
  reader never observes a partial file). Profiles carry host, ssh key
  (a *path* — the file never holds secret material), proxy host, group,
  a cached init-system verdict, and the raw `project` override block. A
  missing file is an empty config; a corrupt one fails closed.
- **`~/.kampodra/deployments.jsonl`** — the append-only deploy ledger.
  One JSON line per deploy attempt (timestamp, host, sha, tag, result,
  duration, subject), fields validated *before* the write and emitted
  in a fixed key order so readers can string-match safely. There is no
  repair: render paths skip malformed lines, and count paths still
  count them.
- **`kampodra.json`** — the committed repo manifest, discovered upward
  from the working directory (nearest file wins). Project naming only;
  see the project-shape ladder above.

## Adding a command

1. **Where the Cobra command goes.** A new file in
   `internal/command/<name>.go` with a `new<Name>Command(d Deps)`
   constructor, registered in `NewRoot`. Give it a help heredoc (usage
   + examples) and declare it in the root command index. Wire
   `--host` / `--ssh-key` / `--profile` through `ResolveTarget` rather
   than hand-rolling target logic. Per-command flag semantics belong in
   the CLI reference, not here.
2. **Why orchestration stays OS-free.** `RunE` should only validate
   flags, resolve the target, sequence adapter calls, and render. Any
   decision logic (ordering, keep-sets, thresholds, verdicts) belongs in
   an adapter function or a pure helper in the command package — both
   unit-testable without a VM. The static arch test blocks any shortcut
   that reaches past the adapters.
3. **What the surface test does to you.** `TestCommandSurface` fails on
   a command registered without a `Short`, and on any drift between the
   root index and the implemented command set — in both directions.
   Update the index and the test together.
4. **What tests to add.** A command test that drives the Cobra command
   with fake `Deps` (injected env lookup, runner, home, working dir —
   no ssh, no network), and adapter fixture tests for any new remote
   snippets or parsers. If you added a new remote shell template, its
   fixture test is the only place the host-OS wording gets pinned.
