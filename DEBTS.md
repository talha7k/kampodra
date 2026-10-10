# kampodra — Debts & Backlog

Living ledger. Check items off only with evidence (tests green, live-fire
proof where noted). Last updated: 2026-10-10 (live-fire proof session:
bluegreen + image-import + sidecars + rolling on one disposable OCI VM;
green never touched).

## Status snapshot

- Version: `0.7.0-beta.3` · 14 packages green · CI matrix (4 platforms)
- Deploy proven live end-to-end: two real deploys + a full fresh-VM
  wipe-and-rebuild (21.5 min, `d54c3e1` live-fire fixes) — Alpine path;
  the Ubuntu 24.04 provisioner is fixture-proven only (live drill open)
- 2026-10-10 live-fire session: the 0.7.0-beta.3 binary drove a full
  disposable-VM lifecycle against prod-adjacent OCI (bluegreen pair,
  image-import, sidecar deploy, rolling) — green (84.13.128.216)
  read-only throughout; findings fixed forward (5 commits, all suites
  green); evidence under `test-results/livefire-2026-10-09/`
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
      provision, refusal paths); suite green. LIVE DRILL DONE
      (2026-10-10, disposable VM): status pair view, provision blue via
      the inject route (4m04s: launch→platform ssh→qcow2→raw→gzip|dd→
      reboot→Alpine verified), flip --to blue (22.3s, ACME issued for
      84-8-104-247.sslip.io, served sha through the reserved IP while
      green kept serving its own), rollback (6.0s → dormant + guest
      cleanup), forced-flip auto-rollback (51.2s: post-assign failure →
      dormant + guest cleaned, green lookup-only). Live findings FIXED:
      ReservedIPListArgs never passed its parser's --query (status saw NO
      reserved IP); pair instance naming now pairInstancePrefix-derivable
      (legacy estates: esellar-green vs esellar-api-green); every guest
      ssh leg now rides the resolved --ssh-key; the inject platform legs
      authenticate with the resolved private key (--platform-key is
      authorization only).
- [x] **`image-import` port** — OCI golden-image (qcow2 → custom image)
      import flow with the firmware verdict + keep-object. Evidence:
      `internal/command/imageimport_test.go` (happy/BIOS-warn/terminal/
      validation paths); suite green. LIVE DRILL DONE (2026-10-10): full
      207MB qcow2 upload → import → AVAILABLE in 7m10s against the real
      tenancy; firmware verdict = BIOS with the loud A1 WARN (OCI pins
      imports); staged object auto-deleted; the custom image deleted at
      teardown; provision's route detection SKIPPED the BIOS image live
      (inject route taken).
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
      suite green. LIVE-FIRE PROOF DONE (2026-10-10, fresh-VM rebuild on
      the disposable blue): manifest
      `{"name":"backup","dockerfile":"apps/api-go/Dockerfile.backup"}` —
      deploy built+streamed+verified the sidecar image alongside the
      primary with ZERO manual podman; the ansible backups role
      (backup_image=<sidecar ref>) started the daemon from the streamed
      image; /healthz responded (documented ok:false noise on an empty
      tenant dir); backupd logged buildSha=<deploy sha> (sidecar identity
      proven live). Live finding FIXED: sidecar builds now use the
      DOCKERFILE's directory as context (repo-root context failed on the
      first real sidecar Dockerfile — COPY go.mod go.sum).
### Medium

- [ ] **Rolling deploy promotion** — `deploy --rolling` is opt-in.
      (a) one live rolling drill — DONE (2026-10-10, disposable VM): two
      clean `--rolling` runs (43.5s then 34.4s; build/stream → shadow
      boot+health on the loopback probe port → re-point+drain → init stop
      → retag+start+gate → re-point back → converge), background public
      probe loop at ~2/s: ZERO failed probes in either swap window (79/79
      then 105/105 all 200, max 989ms); the failed-shadow safe-abort path
      also proven live (run 1 died at the shadow gate, nothing took
      traffic, main untouched). Live findings FIXED: the shadow run now
      replicates the init unit's clear env via a shared
      vmbootstrap.ClearEnvArgs (a shadow without PORT booted the app's
      compiled default port and failed its probe); the drain-confirm poll
      now uses `kamal-proxy list` (`kamal-proxy ls <svc>` is not a verb —
      the confirm never confirmed). (b) two consecutive clean rolling
      PROD deploys — STILL OPEN. Keep `--in-place` documented as the
      escape hatch forever.
- [x] **lint-debt burn-down** — COMPLETE (2026-10-09): all ten grandfathered
      functions refactored under the gocognit 25 / gocyclo 20 bar
      (ResolveTraced → per-layer resolver methods; newDeployCommand →
      per-subcommand builders; newEnvCommand likewise; (*deployRun).execute
      → preflight/primaryImage/inPlaceSwitch phases; runDeployRoot →
      validateDeployArgs + assembleDeployRun + positionalRollbackSha;
      runMigrate → flags/stop-guard/probe-check/run-all helpers;
      statusBody → section renderers; runMetrics → flag-parse + snapshot
      helpers; runVMWipe → match-check + teardown-phase helpers;
      runVMPrepare was already under the bar from the Ubuntu dispatch
      refactor; the guard test → walk/scan helpers). The grandfather
      exclusion is DELETED from .golangci.yml — the gate now fails on any
      function above the bar, new or old. Behavior byte-identical
      throughout; evidence: full suite 14/14 ok
      (test-results/burndown-integration.log), golangci 0 issues with the
      allowlist gone.
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

- [x] **Publish to npm** — `make cross-compile && git tag v<version> &&
      make publish` (publishes platform packages first, then root; needs
      npm auth/OTP). Published under the personal `@talha7k` scope
      (2026-10-10, 0.7.0-beta.4: root + 4 platform packages, `beta` tag). (Repo is on GitHub since 2026-10-09:
      github.com/talha7k/kampodra, main pushed.)
- [x] **Version stamping** — main.version stamped from the npm package
      version (`-X main.version=$(VERSION)` in cross-compile), plus a
      `make release-check` gate wired into `make publish` asserting the
      shipped host binary reports it. Evidence: `make cross-compile &&
      make release-check` green (0.7.0-beta.3), npm version matches.

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
