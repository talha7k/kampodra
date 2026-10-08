# Changelog

## 0.7.0-beta.1 — 2026-10-09

- **Wave 4 — the INFRA families** (the frozen-spec port is complete except
  bluegreen / image-import / migrate, which stay honestly baselined):
  - **`config`** — `init | list | show | set-default | remove` against
    `~/.kampodra/config.json` (0700 dir / 0600 file, atomic same-dir tmp +
    mv). First profile auto-becomes the default; removing the LAST profile
    requires `--force`; the default reassigns sorted-first after a remove.
    Re-init (`--force`) refreshes the flat fields, keeps the cached init
    verdict while host/ssh-key are unchanged, and never touches the project
    block (tooling-owned). Values are validated (no whitespace/quotes/
    backslashes — never echoed); `~` expands in `--ssh-key`. The parity
    snapshot now sees cobra persistent flags (a snapshot gap, fixed).
  - **`backup`** — `list | download | verify | restore-plan` over the oci
    CLI's own auth (config-file `--profile` or `--instance-principal`; zero
    stored creds). LIST-denied policy prints the exact IAM shape; download
    lands 0600 and sha256/md5-verifies against the object's digest metadata
    (mismatch prints expected/got and fails closed); verify pipes the db
    image over ssh stdin into the VM's `/usr/local/bin/restore-verify`
    (.tgz archives extract to scratch, every .db member piped, traversal-
    safe); restore-plan PRINTS the stop/swap/start sequence and never
    executes.
  - **`vm-wipe` (kampodra-native)** — full teardown for the fresh-VM
    rebuild loop: stop + disable every project service (init-aware),
    remove the project containers (app + shadow + edge), prune images,
    delete env file / deployed-sha stamp / state dirs. `--yes` gates every
    mutation, `--keep-data` preserves the tenant data dir, and a host whose
    services don't match the profile is refused unless `--force`. Prints a
    full removed-summary; suggests `vm-prepare` next.
  - **`env from-schema` (kampodra-native)** — the deploy.sh varlock block:
    generates the env file from the repo's COMMITTED `.env.schema`
    (`git archive HEAD` → scratch dir inside the repo → the repo's own
    varlock resolves pass() refs), filters to KEY=VALUE (rc-script-owned
    clear keys dropped via the new `envClearKeys` project-config field,
    quotes stripped for `--env-file`), then writes locally (`--out`, 0600)
    or pushes through the env family's flow. Values NEVER echo —
    fingerprints only.
  - **`vm-prepare`** — the Alpine host bootstrap, ported faithfully:
    UEFI/no-systemd/OpenRC gates (fail-closed BEFORE mutations), sshd
    hardening ensure + gate, community repo + podman stack + cgroups,
    registries.conf + sysctl 80, OpenRC units for the api container +
    kamal-proxy (supervise-daemon), the kampodra-anchor watcher (inert
    without the conf), kamal-proxy pull + start + readiness, and the full
    post-gate battery. ALL naming routes through `vmbootstrap.Naming` ←
    ProjectConfig (magic-string guard holds). `--pull-images` pre-pulls
    the app image directly from the ImagePrefix registry (kampodine's
    Mac-local tunnel is gone — kampodra streams on deploy). NEW `--ansible
    <playbook>` runs ansible-playbook against the host after bootstrap
    (inventory + `--private-key` derived from the resolved target).
    Transport gains `StrictHostKeyChecking=accept-new` for first contact.
  - **`metrics`** — one-shot snapshot over one ssh round-trip (the osfacts
    renderers from wave 1); `--warn-disk <pct>` (default 90) exits 1 at or
    above the threshold using the deploy gate's verdict logic; `--watch N
    --count M` loops.
  - **`dns`** — `records | add | rm` over the oci CLI: types pinned to
    A|AAAA|CNAME, pure validators fire BEFORE any provider call, zone
    resolution requires exactly one compartment zone (else `--zone`),
    `add` merges the RRSet (round-robin survives) and is idempotent, `rm`
    filters and may empty the RRSet, and the no-credential static gate is
    ported (no credential material tokens anywhere in the dns surface).
- **Parity** — the NOT_YET_PORTED baseline is down to `bluegreen`,
  `image-import`, `migrate` (deliberate follow-ups, honestly baselined).
  Native additions review-flagged by the guard: `vm-wipe`, `env
  from-schema`, `vm-prepare --ansible`, plus the pre-existing
  `ssh`/`--rolling`/`converge`/`diff` surface.
- **Fixes** — the deploy pipeline test's `save|load` order pin raced the
  concurrent stream legs (pre-existing flake, ~1/6 runs); the pin now
  asserts the real invariants (both legs after build, retag after stream).

## 0.7.0-alpha.3 — 2026-10-08

- **`deploy` — the REAL pipeline** (the NOT_YET_PORTED gate is gone): clean-tree
  gate (auto-skipped for non-repo/`--version` use) → `podman build
  --build-arg GIT_SHA=<sha>` with the sha tag from `git rev-parse` →
  `podman save | ssh podman load` (registry-free stream) → env-file push via
  `--env-file` (the env family's 0600-from-creation temp + atomic `mv`,
  `API_GIT_SHA` stamped, fingerprints only — values never print) → init-aware
  restart (`init.ActionCommand` + ProjectConfig) → VM-side health gate
  (30 × 3s wget/curl with client-side served-sha verify against the deployed
  tag) → kamal-proxy re-point (`podman exec kamal-proxy kamal-proxy deploy
  <service> --host <proxyHost> --target <container>:<port> --tls
  --health-check-path <path>`) → public smoke (`/api/auth/ok` served-sha +
  `/up` + fresh `/build-id.txt`; `--skip-smoke` skips) → ledger append
  (`~/.kampodra/deployments.jsonl`; every failure after sha resolution records
  `failed` with the `FAILED — attempted tag` footer) → disk report
  (`--require-disk <pct>` fails closed BEFORE the build; >90% always warns) →
  keep-set image cleanup (running + ts-rollback + newest 3; tolerant).
- **`--rollback [<sha7>]` — the shell's latent bug FIXED**: the target
  resolves from the EXPLICIT argument, else the VM's deployed-sha stamp file,
  else it DIES — it NEVER silently falls back to git HEAD (the shell
  redeployed the very build being rolled back from). Skips build/stream when
  the tag already exists on the VM (`podman image exists`); streams from this
  machine when only local. Rollback never rewrites the stamp; the ledger line
  records `rollback` with the ORIGINAL deploy's subject
  (`ledger_subject_for_tag`). Pinned by a resolution-matrix test (explicit /
  stamp / die / never-HEAD / both-exist / neither).
- **`--rolling` (kampodra-native, opt-in) — zero-downtime shadow double
  re-point**: the new version boots as `<container>-shadow` from the SHA tag
  (never `:latest`) on the kamal network with a loopback-only probe port;
  health-gated BEFORE any traffic can reach it; kamal-proxy re-points to the
  shadow (first switch); drain wait (`--drain-timeout`, default 10s, timeout
  CONTINUES — the init stop terminates stragglers); init-system STOP of the
  old container (respawn discipline — never `podman stop`); retag `:latest`;
  start; health-gate the new main; re-point back (second switch); shadow
  removed LAST. Failure paths keep the safer state: a bad shadow never took
  traffic → removed + die (main untouched); a switch already made → the
  shadow stays SERVING, exit LOUD with the repair command. The sqlite
  two-writer overlap (both containers on one db during the switch) is
  documented — and bounded — in the ROLLING section of `deploy --help`
  (health + drain + stop budgets).
- **`deploy converge [<sha7>]` (kampodra-native)** — finish an interrupted
  `--rolling` deploy: retag → restart → health gate → re-point to main →
  remove the shadow. The sha comes from the argument, else from the shadow's
  own image config — never HEAD. A leftover shadow from an interrupted
  deploy makes the next `--rolling` refuse (converge first). A converged-but-
  unhealthy main keeps the shadow serving and exits LOUD.
- **Project config growth** — `port`, `network`, `shadowProbePort`,
  `deployedShaFile` join `project.Config` (profile `project` block >
  `KAMPODRA_PORT` / `KAMPODRA_NETWORK` / `KAMPODRA_SHADOW_PROBE_PORT` /
  `KAMPODRA_DEPLOYED_SHA_FILE` > named defaults) — VM-side deploy naming
  stays out of the command layer.
- **Adapters** — `state.AppendEntry` (the ledger_append port: validated
  fields, 0600, shell-identical line shape), `state.LedgerSubjectForTag`,
  `probe.BodyServesSha` (the health grep, prefix semantics pinned),
  `probe.Up` (the `/up` leg of the smoke), `envfile.RemoteTmpPath` (the env
  temp derivation shared by `env push` and deploy).
- **Parity** — the frozen spec's deploy surface is now fully covered (no
  baseline entry was needed; `--rolling`/`--env-file`/`--drain-timeout`/
  `converge` are review-flagged native additions). Version 0.7.0-alpha.3.

## 0.7.0-alpha.2 — 2026-10-08

- **`env` command family** — `list | push | pull | fingerprint` (frozen-spec
  parity: 0600 remote temp, atomic `mv`, fingerprints only, values never
  printed) plus kampodra-native `env diff <local-file>`: fingerprint-level
  local-vs-remote diff (added/removed/changed, both sides' fingerprints,
  exit 1 on drift, GNU diff convention).
- **`deploy` lifecycle** — `list` (VM sha-tags merged with the JSONL ledger,
  running marked, Total deployments footer), `prune` (keep-set: running +
  ts-rollback + newest `--keep N`; `--dry-run`), `logs` (`--lines N`;
  `--follow` streams until ctrl-c with a clean exit), `restart`
  (init-aware rc-service/systemctl), `shell` (interactive + `exec -- <cmd>`).
  The build/stream pipeline itself (and `--rollback`) remains NOT_YET_PORTED
  and fails closed with an honest message.
- **`kampodra ssh [<cmd>...]`** — kampodra-native host-level passthrough
  through the profile's host/key (no command = interactive login; ssh's exit
  code propagates).
- **`--all-profiles`** on `status` and `deploy list` — kampodra-native
  per-profile fan-out, one rendered section per configured profile.
- **Project config** — `internal/adapter/project` is the single source for
  the deployed project's naming (container, env-file path, data dir, bucket,
  health path, proxy host, services, image prefix). Resolution: profile
  `project` block > `KAMPODRA_*` env > named defaults; a static guard test
  fails on magic strings outside that file.
- **Frozen spec** — `internal/parity/golden.json` is pinned as final (shell
  0.6.0 @ `5bf07fe`; shell line retired 2026-10-08). The parity guard blocks
  on missing frozen-spec coverage and only WARNS on review-flagged
  kampodra-native additions. `tools/paritygen` is historical.
- **CI + release flow** — GitHub Actions (test/vet/build, 4-platform
  cross-compile matrix, explicit parity-guard step) and `make publish`
  (test → version-check → cross-compile → npm publish; never auto-run).

## 0.7.0-alpha.1

- Module bootstrap (ports-and-adapters layout), command-parity guard with
  golden + NOT_YET_PORTED baseline, `status` vertical slice, rename to
  kampodra, npm platform-package scaffolding.
