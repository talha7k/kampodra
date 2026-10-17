# gaps.md — kampodra known gaps, checklist & plan

Dated: 2026-10-18. Maintained by the esellar lead session; each gap carries its
own red→green checklist. Convention: work in `.worktrees/<branch>`, TDD, gates
(`go build ./... && go vet ./... && go test ./internal/... -count=1`, gofmt),
one concern per commit, never `--no-verify`.

## G1 — `vm image` surface (golden-image management) — PLANNED

The esellar golden image (Packer qcow2, `infra/alpine-host/` in the esellar
repo) is imported to OCI manually per its RUNBOOK. The artifact has no
versioned home; the import quirks (Alpine off OCI's supported list → the
boot-volume route) are tribal knowledge.

- [ ] `vm image publish` — take a built qcow2 → upload to OCI Object Storage
      `golden-image/<name>-<date>-<recipesha>/` via instance principal,
      sha256 + provenance manifest (recipe commit, packer/base versions)
- [ ] `vm image list` — bucket inventory (name, stamp, bytes, digest)
- [ ] `vm image verify` — integrity check against the committed SHA256SUMS
- [ ] `vm-prepare --image-manifest <file>` — resolve the image ref from the
      manifest (digest pins, never "latest"); encode the proven OCI import
      path (boot-volume route) as the tool's flow
- [ ] tests: manifest emit/verify, digest pinning, instance-principal path
      (dispatching-shim pattern), never-overwrite rule
- Plan: publish the CURRENT golden image once the surface lands; the recipe
  stays in esellar `infra/alpine-host/` (image is esellar-specific —
  growroot service, api unit); kampodra owns only the publish/resolve surface.

## G2 — dbq MariaDB drift in the main checkout — DECISION PENDING (owner)

The main checkout carries uncommitted `internal/adapter/dbq/maria*.go` +
edits (db.go, vm_prepare_*.go, vmbootstrap_snippets.go) from an expired
agent session — an unspecced MariaDB direction never reviewed.

- [ ] OWNER: keep (finish deliberately: spec + TDD wave in a worktree) or
      discard (restore the checkout to HEAD)
- [ ] if kept: the db surface grows a second engine — the SELECT/WITH gate,
      the streamed-helper pattern, and the snapshot flow need engine parity
      tests mirroring the sqlite path

## G3 — bluegreen test latency — BACKLOG

Two `internal/command` bluegreen tests make real network calls with ~30s
timeouts under machine load (verified passing in isolation).

- [ ] inject a clock/dial stub or fixture so the pair runs <2s
- [ ] keep at least one integration-flavored test behind a build tag as the
      live-wire canary

## G4 — release hygiene — STANDING

- [x] 0.8.8 published (db surface, vm gc, class-aware migrate Plan)
- [ ] next tag: fold in whatever merges from G1/G2 + any esellar-driven
      patches; publish remains the owner's (`make publish`, npm auth)
- [ ] consider pinning golangci-lint + a `make lint-go` baseline in this repo
      too (the esellar repo hit version drift red — prevention here)

## Standing rules (from the esellar estate, binding here)

- Agents work in `.worktrees/<branch>`; the main checkout stays clean.
- Tooling gaps become kampodra patches — never hand-run workarounds.
- The lead session merges to main + owns push/tag; `make publish` = owner.
