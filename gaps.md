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
- [ ] follow-up: sudo-wrap for vm-prepare on root-disabled clouds (GCP
      ssh users are never root; privileged steps assume root today)
- Rule: local pods first; cloud only when containers cannot represent the
  surface; EVERYTHING torn down after

## Working rules (corrected to the repo's git discipline)

- Work in the main checkout when changes are isolated (per AGENTS.md);
  worktrees only for isolation needs, never ritual.
- Tooling gaps become kampodra patches — never hand-run workarounds.
- The lead session owns merge/push/tag; `make publish` = owner.
