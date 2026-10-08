# Changelog

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
