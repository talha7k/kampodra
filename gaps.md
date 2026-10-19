# gaps.md — kampodra known gaps — VALIDATED 2026-10-18 (lead session)

Each gap re-verified against the tree this date; stale claims closed.

## G1 — golden-image surface — OPEN (reframed: Ubuntu minimal is the base)

VALIDATED remainder. `imageimport.go` (OCI boot-volume import + the
golden-image lookup prefixes) already exists; what is missing is the
artifact-publish side and the Ubuntu base strategy:

- [ ] STRATEGY (decided this session): the golden minimal Ubuntu image is
      the OFFICIAL ubuntu-minimal cloud image (systemd included, cloud-init
      for first-boot keys) + `vm-prepare` convergence — no custom Packer
      build to maintain for Ubuntu; custom recipes stay esellar/Alpine-only
- [ ] `vm image publish` — artifact → Object Storage
      `golden-image/<name>-<date>-<digest>/`, sha256 + provenance manifest
      (source URL, import date)
- [ ] `vm image list` / `verify` — bucket inventory + digest check
- [ ] `--image-manifest` pinning (digests, never "latest")
- [ ] tests: manifest emit/verify, digest pinning, never-overwrite

## G2 — dbq MariaDB "drift" — CLOSED (was stale)

The claim ("uncommitted drift from an expired session, decision pending")
was made against a mid-flight tree and is FALSE today: the MariaDB/db
surface is committed (489519b…09001d9), spec'd in commit messages,
real-engine smoked (mariadb:11 container, live grant-ro, physical
mariabackup), and shipped in 0.8.7/0.8.8. Engine-parity tests exist
(gate ×3 engines, shim flows, golden TSV/CSV/Hrana fixtures).

## G3 — bluegreen test latency — CLOSED (not reproducing)

Measured this date: bluegreen tests complete in 0.00–1.28s in isolation;
no network waits. The ~330s command-suite duration is suite VOLUME
(hundreds of exec-shim cases), not bluegreen. Reopen only with a
reproduction.

## G4 — release hygiene — STANDING

- [x] 0.8.8 published (real-smoke hardened db surface, provenance gates)
- [x] 0.8.7 published (three engines, container mode, grant-ro, dbHost)
- [ ] next tag folds in the profile-db block + G1 remainder
- [ ] pin golangci-lint + a `make lint-go` baseline

## G5 — integration testing depth — OPEN (this date's finding)

The exec-shim suite proves orchestration, not hosts. The real-engine db
smoke (local pods, env-gated, torn down) is the pattern to extend:

- [x] vm-prepare against a REAL systemd host — DONE this date: an
      ubuntu:24.04 privileged container (systemd PID1) cleared every gate
      after two live findings (iptables missing from the Ubuntu snippet;
      the netavark gate trusted a name without a binary) — both fixed
- [x] real cloud leg — DONE this date: GCP ubuntu-minimal 24.04.5 (live
      VM, torn down): found the PID1 gate's /proc/1/exe is unreadable to
      non-root (comm-based gate now) — vm-prepare could never have run
      on a real GCP VM before this
- [x] SELECTIVE ELEVATION landed: the 14 read-only gates run via read()
      (no sudo prefix — the live finding: elevation broke their shapes);
      mutating steps keep sudo -n / runRoot
- [ ] OCI throwaway leg — BLOCKED on OCI CLI auth (401: the config's
      credentials are stale; rotate via varlock: oci session authenticate
      or a fresh API key). The recipe, ready to run: AD from
      `oci iam availability-domain list`, e2.micro (always-free) with the
      Ubuntu-minimal or Alpine image, root ssh (the OCI shape — no sudo
      needed), `vm-prepare --host`, teardown
- [ ] follow-up (IN PROGRESS, live-found): the sudo-wrap landed
      (52e279c→4ad2eaf) and clears every gate through podman-install as a
      non-root user; the REMAINING defect: read-only GATES also elevate
      (the podman-version gate under sudo printed sudo's own usage) —
      the design refinement is selective elevation: only MUTATING steps
      take the prefix, read-only gates run as the user. LANDED for most
      gates (22149d9) + the privileged-read re-filings (ssh -T, podman
      info — 388601e, 464152c). REMAINING: GatePodmanVersion under
      sudo -n prints sudo's OWN usage on ubuntu-minimal (the exact
      remote command works via manual ssh) — debug the sudoers/secure_path
      interaction on the minimal image, then the final end-to-end pass.
- Rule: local pods first; cloud only when containers cannot represent the
  surface; EVERYTHING torn down after

## G6 — sqlq helper cache trusts presence — OPEN (live-fired 2026-10-18, esellar prod)

`ensureSQLHelper` reuses the VM copy and the local build cache on EXISTENCE
alone (`test -x` / `os.Stat`) — no content check. A 22-byte ASCII stub
("fake-sqlq-helper-bytes", leaked from a test run that wrote into the REAL
user cache instead of an injected dir) poisoned both layers:
`~/Library/Caches/kampodra/dbq/kampodra-sqlq-<goos>-<goarch>-v<ver>` + the
VM's `/tmp/kampodra-sqlq-v<ver>-<arch>`. Every `db query`/`db snapshot`
then failed with `fake-sqlq-helper-bytes: not found`, and re-streaming
re-shipped the poisoned local bytes. `vm gc` cannot help (its never-touch
allowlist protects `/tmp/kampodra-*` by design).

- [ ] fingerprint-verify both cache layers before trust (sha256 stamped
      beside the artifact at build time — the same discipline the binary
      push already has); on mismatch, rebuild + re-stream
- [ ] audit tests for cache-dir injection: every test that touches
      `HelperLocalPath` must pass an injected `CacheDir` (the Deps field
      exists for exactly this) — find the leak that wrote the real cache
- [ ] recovery runbook: delete the local stub (rebuild is automatic) AND
      the VM copy (kampodra ssh rm; gc will never do it)

## G7 — no migration-write / migrate-stage surface — OPEN (live-fired 2026-10-18, esellar prod)

`migrate` RUNS what is already staged on the VM (probe + per-file runner —
no streaming), and `db query` is SELECT/WITH-gated by design. The esellar
support-billing phase-1 rollout therefore needed TWO hand-surfaces:
(a) staging the migrater + migrations bundle (tar stream over
`kampodra ssh`, now pinned by the repo manifest's
`migrateScript=migrate/migrater` + `KAMPODRA_MIGRATE_MODE/DIR` env), and
(b) the binding one-time subscription row move (a one-off static Go helper
built/uploaded/run/removed — the "requeue16 precedent"). The runbook's
"kampodra migrate streams the fresh migrations bundle + migrater" is
aspirational.

- [ ] `migrate stage` (or deploy-block-driven): build the migrater,
      stream it + the migrations dirs to `<repo-root>` on the VM with
      byte-verify — the runbook sentence becomes true
- [ ] `db exec`-equivalent for ONE gated write statement (explicit
      `--i-am-writing`, refuse multi-statement strings, print rows-affected)
      — the sanctioned escape from ad-hoc helpers for one-time data moves

## Working rules (corrected to the repo's git discipline)

- Work in the main checkout when changes are isolated (per AGENTS.md);
  worktrees only for isolation needs, never ritual.
- Tooling gaps become kampodra patches — never hand-run workarounds.
- The lead session owns merge/push/tag; `make publish` = owner.
