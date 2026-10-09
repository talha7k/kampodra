# kampodra — Debts & Backlog

Living ledger. Check items off only with evidence (tests green, live-fire
proof where noted). Last updated: 2026-10-09 (Ubuntu 24.04 guest support,
7fc307e; repo pushed to GitHub).

## Status snapshot

- Version: `0.7.0-beta.2` · 14 packages green · CI matrix (4 platforms)
- Deploy proven live end-to-end: two real deploys + a full fresh-VM
  wipe-and-rebuild (21.5 min, `d54c3e1` live-fire fixes) — Alpine path;
  the Ubuntu 24.04 provisioner is fixture-proven only (live drill open)
- Neutral defaults; project config via repo `kampodra.json` + profiles +
  `KAMPODRA_*` env (see `config print` for provenance)

## Outstanding

### Big

- [x] **`bluegreen` port** — reserved-IP pair machinery (`status | init |
      provision | flip | rollback`), re-derived from the archived shell
      reference + the shipped anchor-watcher contract (neutral naming:
      `<container>-blue/green`, `/etc/kampodra/anchor.conf`). ACME-first
      flip with auto-rollback, inject-route provision, fixture tests for
      every path. Evidence: `internal/command/bluegreen_test.go` (15
      tests: happy flip sequence, ACME-failure rollback, native + inject
      provision, refusal paths); suite green. STILL NEEDED: one
      disposable-VM live drill before prod use (Guide rule for
      host-touching ports).
- [x] **`image-import` port** — OCI golden-image (qcow2 → custom image)
      import flow with the firmware verdict + keep-object. Evidence:
      `internal/command/imageimport_test.go` (happy/BIOS-warn/terminal/
      validation paths); suite green. Live drill: run once against a
      real tenancy (staging object, then delete).
- [x] **Secondary-image streaming** — the kampodra.json `images.sidecars`
      block makes deploy build+stream+tag declared sidecars as
      `<imagePrefix>-<name>:latest` (same platform + GIT_SHA identity,
      strict-parse + fail-closed validation, deploy mode only —
      rollback/`--sha` never touch sidecars). vm-prepare deliberately
      does NOT build images: it bootstraps the host; deploy is the only
      image-building path. Evidence:
      TestParseManifestImagesSidecars + TestParseManifestSidecarFailsClosed
      (strict block), TestSidecarImageRef (naming seam),
      TestDeployPipelineSidecarsStreamed (sequence + ORDER pins),
      TestDeployPipelineRollbackNeverTouchesSidecars (rollback purity);
      suite green. Live-fire proof pending (same drill as bluegreen:
      declare a backup sidecar, fresh-VM rebuild, confirm zero manual
      podman).
### Medium

- [ ] **Rolling deploy promotion** — `deploy --rolling` is opt-in and
      fixture-proven only. Flip to default after: (a) one live rolling
      drill (disposable VM or quiet window), (b) two consecutive clean
      rolling prod deploys. Keep `--in-place` documented as the escape
      hatch forever.
- [ ] **lint-debt burn-down** — ten pre-existing functions sit above the
      gocognit/gocyclo bar, allowlisted in `.golangci.yml` (the gate fails
      on anything new): `ResolveTraced`, `newDeployCommand`,
      `runDeployRoot`, `deployRun.execute`, `newEnvCommand`, `runMetrics`,
      `runMigrate`, `statusBody`, `runVMPrepare`, `runVMWipe`
      (+ `TestNoProjectMagicStringsOutsideProjectDotGo`). Burn down one by
      one, deleting an allowlist line each time. New code must stay under
      the bar at authoring time.
- [x] **Cloud auth without vendor flags** (replaces the old "OCI
      default-profile resolution" item) — the provider CLI resolves
      natively when kampodra passes no auth: instance profiles carry a
      `cloud` block (`profile`/`compartment`/`instancePrincipal`),
      `OCI_PROFILE`/`OCI_COMPARTMENT` env are the escape hatch.
      Evidence: TestDNSNativeResolutionOmitsProfileFlag,
      TestBackupListRendersTable, TestBackupCloudBlockSelectsConfigProfile;
      `--oci-profile`/`--instance-principal` flags deleted. The old dodge
      (campaign through default/DEFAULT/first variants) was the wrong
      layer — delegated instead of reimplemented.

### Small batch (one afternoon)

- [x] `vm-wipe`: prints `(profile: )` when unprofiled — now `(profile:
      <none>)` + the DONE line re-points via `--host` (no more empty
      `--profile`). Evidence: TestVMWipeFullSequence pins both.
- [x] `vm-wipe`: `rm -rf /etc/<envdir>` collateral — dropped. The wipe
      removes only managed files (env file, deployed-sha stamp,
      anchor.conf) and NAMES the left-in-place dir explicitly.
      Evidence: TestVMWipeFullSequence asserts no `rm -rf /etc/kampodra`
      + the left-in-place note.
- [x] `dbMembers` AppleDouble junk (`._*.db`) — dotfile-prefixed names
      (and dot dirs, via SkipDir) are filtered. Evidence:
      TestBackupAppleDoubleMembersSkipped.
- [x] `deploy list`: same-tag rows showed the earliest timestamp —
      same-tag rows merge to ONE row showing the NEWEST deploy timestamp
      (ledger-only duplicates collapse to the newest entry too).
      Evidence: TestDeployListSameTagLedgerOnlyMergesToLatest,
      TestLatestTagTimePrefersNewest; TestDeployListMergesVMTagsWithLedger
      re-pinned to latest.
- [x] `--pull-images` dead weight — REMOVED (flag, help, flow, test).
      Evidence: zero hits under `--pull-images` outside DEBTS/CHANGELOG;
      TestVMPreparePullImagesFlag deleted.

### Release hygiene

- [ ] **Publish to npm** — `make cross-compile && git tag v<version> &&
      make publish` (publishes platform packages first, then root; needs
      npm auth/OTP). First publish claims the `@kampodra` scope — verify
      it's free. (Repo is on GitHub since 2026-10-09:
      github.com/talha7k/kampodra, main pushed.)
- [x] **Version stamping** — main.version stamped from the npm package
      version (`-X main.version=$(VERSION)` in cross-compile), plus a
      `make release-check` gate wired into `make publish` asserting the
      shipped host binary reports it. Evidence: `make cross-compile &&
      make release-check` green (0.7.0-beta.2), npm version matches.

## Done (for orientation — do not re-add)

Deploy core (build→stream→health gate→proxy re-point→smoke→ledger) ·
`--sha` deploys + `--rollback` stamp ladder (never HEAD) · `--rolling`
shadow double re-point + `converge` · lifecycle family (list/prune/logs/
restart/shell) · env family (list/push/pull/fingerprint/diff/from-schema)
· config family + repo `kampodra.json` manifest + `config print`
provenance · backup family (list/download/verify/restore-plan) ·
`vm-wipe` · `vm-prepare` (+`--ansible`) · metrics · dns · migrate · ssh
passthrough · init auto-detection (openrc/systemd) · `--all-profiles` ·
CI (tests/vet/cross-compile) · npm per-platform packaging + shim ·
neutral defaults + flag unification + JSON schema doc · generic docs +
agent skill · cloud auth via profile `cloud` block + native provider
resolution (no vendor flags) · `--group` fan-out + `config clone` ·
`--sha`/`--disk-threshold` flag clarity · merge-latest deploy list.

## Guide

- **Definition of done** for any port: command live, fixture tests
  (exec-shim pattern in `internal/command/*_test.go`), help text with
  examples, CHANGELOG entry, and — for anything touching a real host —
  one live-fire proof recorded here.
- **No magic strings**: all naming flows through ProjectConfig
  (`internal/adapter/project`); the static guard enforces it.
- **Secrets**: never printed, never stored; fingerprints only. The
  no-credential gates (dns/backup/oci) must stay.
- **Testing**: TDD (watch RED first); `go test ./... -count=1` + vet +
  gofmt green before every commit; cross-compile before releases.
- **Repos**: this repo is self-contained; the retired shell predecessor
  is archived separately and referenced read-only only.
