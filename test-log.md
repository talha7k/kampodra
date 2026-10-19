# test-log.md — kampodra testing record

Living record of every real verification pass: what ran, where, what it
found, what it fixed. Orchestration-suite runs are logged per-release in
`test-results/*.log` (gitignored); this file carries the outcomes and the
findings that changed the code.

---

## 2026-10-18 — real-host vm-prepare (G5: first live hosts ever)

Environment: privileged podman container (ubuntu:24.04, **real systemd as
PID1**) over ssh; then a **live GCP VM** — ubuntu-minimal 24.04.5,
e2-micro, djib-kitchen, non-root ssh user. Both torn down after.

| check | container | GCP VM |
|---|---|---|
| ssh reach + key auth | ✓ (ssh-config alias, port 2222) | ✓ (metadata key, port 22) |
| systemd PID1 gate | ✓ (after fix F3) | ✓ (after fix F3) |
| apt operational | ✓ | ✓ |
| sshd hardening + gate | ✓ | ✓ (reaches here post-fix) |
| podman stack ≥4.9 + netavark | ✓ (after fix F1/F2) | ✓ |
| kamal-proxy edge + watcher active | ✓ | (not reached — non-root, F4) |
| **full PREPARED verdict** | **✓ first in project history** | blocked by F4 |

Findings (each fixed + committed):
- **F1** — the Ubuntu podman snippet never installed **iptables**; netavark
  shells out to it and doesn't declare the Depends. Minimal cloud images
  ship without it → first container run failed `netavark: No such file or
  directory`. Fix: iptables explicit in AptPodmanSnippet (26b7a23).
- **F2** — GateNetavark trusted `podman info`'s backend NAME without
  checking the binary exists. Gate now `test -x`'s the path podman itself
  reports (26b7a23).
- **F3** — the PID1 gate read `/proc/1/exe`, which is **root-only**
  (dumpable off): non-root ssh users (every GCP user) got EPERM →
  **vm-prepare could never run on a real cloud VM**. All systemd gates now
  read `/proc/1/comm` (world-readable) — verified green on BOTH hosts
  (bea638c, 9a3c769, 8265818).
- **F4 (open, gaps.md G5)** — vm-prepare's privileged steps assume root
  ssh (OCI's shape); root-disabled clouds need a sudo-wrap pass.

Teardown: GCP instance deleted, runtime key file removed, smoke containers
and images removed. Nothing left running.

## 2026-10-18 — real-engine db smoke matrix (local pods + env files only)

Environment: this mac — podman machine (mariadb:11, postgres:16-alpine,
sqld via ghcr.io/tursodatabase/libsql-server), native Go, host clients via
brew (mariadb, libpq). Every command the flows compose ran against REAL
servers, pod and ssh modes both.

| engine | pod mode (real) | ssh/host mode (real) |
|---|---|---|
| sqlite | native-built helper vs hostile file: typed JSON (numbers/blobs-base64/escapes), gate exit 2 on DELETE, SQL errors exit 1 | — (file is co-located) |
| mariadb | staged 0600 cnf query with `--binary-as-hex`; grant-ro executed live (role reads, **DELETE denied**); **mariabackup backup→prepare→tar verified** | host client → exposed port; genuine `Dump completed` stamp |
| libsql | real sqld: Hrana query → `[{"id":7,"name":"seven"}]` | N/A by design (URL-based) |
| postgres | in-container `psql --csv` + `pg_dump` (PGPASSFILE env-prefix, `--user postgres`), tail-stamp gate refused an empty dump live | libpq 18.6 → exposed port, dump stamp ✓ |

Findings (each fixed + committed, 09001d9):
- **F5** — MariaDB batch emits blobs as RAW bytes (no `0x`): non-UTF-8
  through json.Marshal corrupts to U+FFFD. Fixed: `--binary-as-hex` +
  UTF-8 validity net in the normalizer; empty blobs render bare `0x`
  (gate widened).
- **F6** — the container image authenticates root by PASSWORD, not
  unix_socket (the Debian-package shape): in-container cnf staging from
  the env file, root forced for grant-ro/mariabackup.
- **F7** — real Hrana sends integer values as STRINGS (JS precision):
  decoder emits digits as raw JSON numbers — precision-preserving,
  sqlite-leg type parity. Golden test re-encoded with the real envelope.
- **F8** — psql unaligned corrupts multi-line values: postgres legs moved
  to `--csv` (RFC4180 quoting) + stdlib CSV normalizer; NULLs render
  empty in csv mode — documented protocol loss.
- **F9** — `pg_dump` has no `--set` flag (that's psql's): PGPASSFILE
  moved to an env prefix. The tail-stamp gate refused the empty dump
  exactly as designed while this was being found.
- **F10** — `podman exec` runs as root and the DB role "root" does not
  exist: `--user postgres` added to the pg container execs.

Kept as durable assets: the env-gated real-smoke test
(`libsql_smoke_real_test.go`, skips without KAMPODRA_SMOKE_SQLD) and
golden TSV/CSV/Hrana fixtures captured verbatim from the live engines
(`TestNormalizeTSVGoldenRealMaria`, `TestNormalizeCSVGoldenRealPsql`).

## 2026-10-16 → 2026-10-17 — release trains (orchestration + registry)

Every release: full go suite → version-check (tag on HEAD) → cross-compile
×4 (skills staged) → release-check → npm publish ×5 → publish-verify
(registry downloads re-probed for version + commit sha).

| release | suites | publishes | verify | notes |
|---|---|---|---|---|
| 0.8.3 | green | 5/5 | manual (pre-gate) | first skills-in-tarball; install smoke 0.8.3 |
| 0.8.4 | green | 5/5 | manual string-probe (pre-gate) | first `@kampodra/*` naming; propagation-lag skip explained |
| 0.8.5 | tagged, never published | — | — | superseded by 0.8.6 |
| 0.8.6 | green | 5/5 | **FAIL→fixed**: probe grepped the runtime-concatenated `version (commit sha)` phrase that cannot exist in a binary; artifacts verified correct by hand (tag sha ×3); probe redesigned to require version + sha separately | first commit-stamped binaries; fresh install prints `0.8.6 (commit 0e26016)` |
| 0.8.7 | green | 5/5 | **FAIL→fixed**: `npm view` without `--prefer-online` re-read the same cached pre-publish packument for all retries — cache bug, not staleness; CDN convergence confirmed; probe now cache-bypasses | fresh install prints `0.8.7 (commit 10edf23)` |
| 0.8.8 | green | 5/5 | **GREEN first run** — both earlier fixes held; all four platforms verified in-loop | carries the real-smoke hardening |

Pre-release deprecation hygiene: all nine `@talha7k/*` strays deprecated
with rename pointers (raw packument PUT — the registry accepts the
granular token; the npm-CLI EOTP demand was client-side only).

## Standing suites

- `go test ./...` — **17 packages, green** at every landing since the db
  slice; the command package is the heavy one (~350s, hundreds of
  exec-shim cases driving real cobra flows through fake ssh/go binaries).
- `npm/test/` (node --test) — 17 cases: shim resolution, bump atomicity,
  naming; green at every npm change.
- golangci-lint — errcheck/staticcheck zero; four complexity findings
  deliberately left (gate.go, vmgc.go, resolve_test.go, project.go —
  named for their authors).
- Gates that exist BECAUSE of this log: commit-stamped binaries,
  release-check's stamp assertion, publish-verify's registry re-probe,
  the netavark binary check, the comm-based PID1 gates.

## 2026-10-18 (late) — the sudo-wrap's live GCP iterations + the lockout lesson

Environment: three GCP ubuntu-minimal VMs (provisioned + torn down each),
non-root ssh user throughout.

| iteration | result |
|---|---|
| 1 | gates green as non-root through sshd hardening (the sudo-wrap worked); failed at the drop-in upload — /etc/ssh is root-writable only |
| 2 | the sh -c escape for elevated uploads broke in transport (sudo usage error) — replaced with the quoting-free `sudo tee` + chmod form |
| 3 | tee uploads verified manually (exit 0), then the sshd RESTART failed: sshd -t invalid — root cause was MY OWN diagnostic litter (stray drop-ins with junk content from manual debugging, invalid as sshd directives) |

Incident + fix: the failed restart took the running sshd down = the VM
locked out (deleted; disposable). The lockout class is now closed:
**sshd -t validates the config before the restart** — on failure the
drop-in is removed, ssh is restored, and the run dies naming the config
error (128b0f0). A broken render can never lock an operator out again.

Also proven live: the privilege detection (root vs sudo -n) and the
compound-elevation (runRoot) fix — the sshd Include grep got sudo while
its sed ran bare (prefix-scoping bug, now runRoot-elevated as one shell).

## 2026-10-18 — first real deploy → rolling update → rollback (local systemd container)

Environment: the G5 container (ubuntu:24.04, systemd PID1, vm-prepared —
every gate green), a busybox smoke app in a throwaway git repo, deploy
run for real (podman build → save|ssh load → env → shadow → health gate
→ kamal-proxy switch).

| step | result |
|---|---|
| fresh-estate deploy | **FAILED → real finding F11**: the app unit's `--env-file` parse fails on a missing env file; vm-prepare created the dir but not the file. Fixed: prepare touches the env file 0600 |
| health gate | **FAILED → real finding F12**: "unreachable" ×30 while the app 404'd — the VM-side fetcher (wget/curl) was never ensured by vm-prepare, and an HTTP-error body printed as "unreachable" instead of "serving". Fixed: wget in the Ubuntu snippet + the gate distinguishes up-and-wrong from unreachable; the shadow's last logs are now captured BEFORE its removal (the rm deleted the only evidence) |
| health contract | the app must serve `"git":"<sha>"` JSON (probe.BodyServesSha) — three app-side iterations to discover the exact contract (prefix text → bare sha → JSON field); the contract deserves a doc |
| first-deploy bootstrap | gate passed → proxy switched → `systemctl stop smokeapp` found no unit (this host never had one) → the flow's own `deploy converge` path finished it: **smokeapp active, serving through the proxy** |
| rolling update (v2) | **full pass**: shadow → gate → switch → verified `served git sha via /up + public smoke` |
| rollback | **ran, then real finding F13 — FIXED same day**: it restored a FAILED attempt's image (presence-on-VM selection). The ladder is now: explicit sha > the ledger's last `result=success` excluding what's serving > die naming the empty ledger; failed deployments never qualify, and the alternation works because the rollback records a success at its own sha |

Also noted: converge/rollback's public smoke reads the PROFILE's
proxyHost over the manifest's (local runs see the profile's production
hostname) — precedence to revisit alongside F13.

## Not yet covered (honest ledger)

- deploy → bluegreen flip → rollback against a REAL VM (the flows are
  shim-tested; the estate's live-fire pre-dates the session's changes)
- alpine container-mode vm-prepare (the alpine leg is root-ssh OCI shape)
- grant-ro against real postgres (SQL printed, not executed — admin path)
- the GCP container-mode db flows over a real dbHost hop

## The sudo-wrap closed — the full non-root GCP pass (2026-10-19)
GCP ubuntu-minimal 24.04, e2-micro, non-root ssh user: vm-prepare
completed end-to-end. Three more live findings fixed en route:
- F14: the 60s proxy wait lost the race to a cold first pull (5min now)
- F15: the readiness probe read the ROOTFUL podman store unprivileged —
  the probe carries the elevation prefix
- the compound podman gates, the &&-chained registries gate, and the
  `!` negation gate all carried shell syntax the blanket sudo prefix
  cannot parse — elevated forms go through runRoot's sh -c now
Lesson: elevation composition has TWO failure modes — under-elevation
(privileged reads denied) and mis-elevation (sudo parsing shell
syntax). Every remote command is now classified: read (no elevation),
run (simple argv), runRoot (shell syntax).

## The 0.8.10→0.8.13 release train — two release-pipeline findings (2026-10-19)
- F16: the platform binaries shipped 0644 — macOS `cp` over an EXISTING
  destination keeps the destination's mode, and gitignored stale bins
  from earlier builds kept reinfecting the tarballs. Fix: chmod 755
  after the cp (963986e). Caught by the fresh-install smoke (EACCES).
- F17: `make bump` builds BEFORE the release commit exists — the stamped
  sha is the PRE-release commit, so publish-verify's tag-sha probe
  rightly failed. The designed path is `make publish-tagged` (worktree
  at the tag → the stamp matches). Caught by our own provenance gate.
- The plain `publish` target after a bump is a TRAP; the train now is:
  bump → push → publish-tagged. Transient registry E404s on PUT need a
  retry INSIDE the tagged worktree — never a manual retry from main.
- 0.8.10 (exec bit) + 0.8.11/0.8.12 (stamps) deprecated with pointers
  to 0.8.13. 0.8.13 verified: all four platforms + root, fresh-install
  smoke prints 0.8.13 (commit 67d6836).

## The OCI leg — the second real cloud (2026-10-19)
OCI A1.Flex (ARM, 1 OCPU/6GB), Ubuntu 24.04 Minimal, me-riyadh-1,
launched via the `esellar-api` API-key profile (the default profiles'
session tokens had expired — the durable API-key profile was the key).
ubuntu@ (non-root, passwordless sudo): vm-prepare passed ALL gates
first try — the sudo-wrap is now proven on TWO clouds and TWO
architectures (GCP x86 + OCI ARM). The esellar infra's OCI auth is
instance-principal (keyless, on-host only) — nothing to mirror into
varlock/pass for local CLI use; the API-key profile already covers it.
