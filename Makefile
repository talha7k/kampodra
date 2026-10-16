# kampodra — build, test, and release.
#
# `make bump VERSION=x.y.z[-pre.N]` is the release-prep entrypoint (DRY=1
# plans without touching anything). It edits the TWO hand-edited version
# homes — npm/package.json and the cmd/kampodra/main.go source pin (drift
# there is only caught by the pin test at publish time) — then stamps,
# commits "release <version>", and tags v<version>. The four platform
# packages + the root optionalDependencies pins are NOT hand-edited:
# cross-compile stamps them from npm/package.json.
#
# `make publish` is the documented release flow; it NEVER auto-runs and is
# never wired into CI. It requires: a clean tree on a tagged commit
# (git tag v<version> == npm/package.json version), and npm auth for the
# @talha7k scope.

GO        ?= go
VERSION   := $(shell node -p "require('./npm/package.json').version")
# Prerelease versions (0.7.0-beta.N) must NOT take npm's `latest` tag — npm
# itself refuses to publish them without an explicit tag. Derive it: any
# prerelease suffix -> `beta`; clean releases -> `latest`.
NPM_TAG   := $(if $(findstring -,$(VERSION)),beta,latest)
PLATFORMS := darwin-arm64 darwin-x64 linux-x64 linux-arm64

.PHONY: all test vet lint build bump cross-compile version-check npm-test publish publish-tagged hooks clean

all: test build

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

build:
	$(GO) build ./cmd/kampodra

npm-test:
	node --test npm/test/

# bump: release prep — validate (semver, forward-only, clean tree) and edit
# npm/package.json + the main.go pin via npm/bump.mjs, then stamp
# (cross-compile), verify the shipped binary + the pin test, and
# commit/tag. DRY=1 runs every validation and prints the plan, touching
# nothing. VERSION must come from the command line: the file default is the
# CURRENT package.json version, which would silently validate against
# itself (and fail forward-only, confusingly) instead of erroring on a
# missing argument.
bump:
	@test "$(origin VERSION)" = "command line" || { echo "usage: make bump VERSION=x.y.z[-pre.N] [DRY=1]"; exit 1; }
ifeq ($(DRY),1)
	node npm/bump.mjs $(VERSION) --dry-run
else
	node npm/bump.mjs $(VERSION)
	$(MAKE) cross-compile
	$(MAKE) release-check
	$(GO) test ./internal/command/ -run TestNpmPackageVersionMatchesBinary -count=1
	git add npm/package.json cmd/kampodra/main.go npm/platforms/darwin-arm64/package.json npm/platforms/darwin-x64/package.json npm/platforms/linux-arm64/package.json npm/platforms/linux-x64/package.json
	git commit -m "release $(VERSION)"
	git tag v$(VERSION)
	@echo "== bump $(VERSION) done: committed + tagged v$(VERSION)"
	@echo "== next: git push origin main v$(VERSION) && make publish"
endif

# cross-compile: every release target, staged into the npm platform
# packages' bin/ (what `npm publish` ships). GOOS/GOARCH are set EXPLICITLY
# per triple — without them `go build` produces HOST binaries mislabeled as
# other platforms (the CI matrix always did this right; the local target
# now matches). main.version is stamped from the npm package version so the
# shipped binaries report the release, not the main.go fallback.
cross-compile:
	@mkdir -p dist
	# Ship the agent skill inside the root npm package (files: skills/ is
	# npm/-relative and cannot reach the repo's skills/ — copy it in, like
	# the platform bins).
	@rm -rf npm/skills && mkdir -p npm/skills && cp -R skills/kampodra npm/skills/
	# Single source of truth for npm versions: every platform package.json
	# and the root package's optionalDependencies pins are STAMPED from
	# $(VERSION) at cross-compile time — static drift here once shipped a
	# root release depending on platform packages that never existed.
	node -e "const fs=require('fs');const f='npm/package.json';const p=JSON.parse(fs.readFileSync(f));for(const k of Object.keys(p.optionalDependencies||{}))p.optionalDependencies[k]='$(VERSION)';fs.writeFileSync(f,JSON.stringify(p,null,2)+'\n')"
	set -e; for triple in $(PLATFORMS); do \
		os=$${triple%%-*}; arch=$${triple##*-}; \
		case $$arch in x64) arch=amd64 ;; esac; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/kampodra-$$triple ./cmd/kampodra; \
		mkdir -p npm/platforms/$$triple/bin; \
		cp dist/kampodra-$$triple npm/platforms/$$triple/bin/kampodra; \
		node -e "const fs=require('fs');const f='npm/platforms/$$triple/package.json';const p=JSON.parse(fs.readFileSync(f));p.version='$(VERSION)';fs.writeFileSync(f,JSON.stringify(p,null,2)+'\n')"; \
		echo "built $$os/$$arch -> dist/kampodra-$$triple"; \
	done

# release-check: the shipped host binary must report the release version,
# not the main.go fallback (cd into cross-compile's dist/ and ask it).
release-check:
	@os=$$(uname -s | tr '[:upper:]' '[:lower:]'); march=$$(uname -m); \
	case $$march in x86_64) march=amd64 ;; arm64|aarch64) march=arm64 ;; esac; \
	triple=$$os-$$march; case $$triple in darwin-amd64) triple=darwin-x64 ;; linux-amd64) triple=linux-amd64 ;; esac; \
	bin=dist/kampodra-$$triple; test -x $$bin || { echo "no host binary at $$bin — run make cross-compile"; exit 1; }; \
	got=$$($$bin --version); echo "binary version: $$got (want $(VERSION))"; \
	test "$$got" = "$(VERSION)" || { echo "FAIL: shipped binary reports $$got — check the -X main.version ldflag"; exit 1; }

# version-check: the release gate — HEAD must carry the git tag matching the
# npm package version (v<version>). Two distinct failure shapes, named
# separately: HEAD UNTAGGED (tag the release commit) vs TAG BEHIND HEAD
# (normal trunk development after a release — publish from the pinned
# worktree via `make publish-tagged`, or cut the next release with
# `make bump`).
version-check:
	@test -n "$(VERSION)" || { echo "cannot read npm/package.json version"; exit 1; }
	@tag=$$(git tag --points-at HEAD | grep -E '^v' | head -n1); \
	echo "package version: $(VERSION)"; \
	echo "HEAD tag:        $${tag:-<none>}"; \
	if [ "$$tag" = "v$(VERSION)" ]; then exit 0; fi; \
	if git rev-parse -q --verify "refs/tags/v$(VERSION)" >/dev/null 2>&1; then \
		behind=$$(git rev-list "v$(VERSION)"..HEAD --count); \
		echo "FAIL: tag v$(VERSION) exists but is $$behind commit(s) behind HEAD — normal trunk development after a release."; \
		echo "  republish THIS release:  make publish-tagged   (runs publish from a worktree pinned at the tag)"; \
		echo "  ship the new commits:    make bump VERSION=x.y.z && git push origin main v<version> && make publish"; \
	else \
		echo "FAIL: tag v$(VERSION) is not on HEAD — tag the release commit first (git tag v$(VERSION))"; \
	fi; \
	exit 1

# publish-tagged: republish an EXISTING release without disturbing trunk.
# Runs the full publish flow from a throwaway worktree pinned at the
# version's tag — version-check sees tag==HEAD there. The worktree is
# removed on success AND failure.
publish-tagged:
	@test -n "$(VERSION)" || { echo "cannot read npm/package.json version"; exit 1; }
	@git rev-parse -q --verify "refs/tags/v$(VERSION)" >/dev/null 2>&1 || { echo "no tag v$(VERSION) — nothing to republish"; exit 1; }
	@test -z "$$(git worktree list --porcelain | grep 'refs/tags/v$(VERSION)')" || { echo "a worktree for v$(VERSION) already exists — remove it first"; exit 1; }
	git worktree add .worktrees/publish-v$(VERSION) "v$(VERSION)"
	@status=0; $(MAKE) -C .worktrees/publish-v$(VERSION) publish || status=$$?; \
	git worktree remove --force .worktrees/publish-v$(VERSION) || true; \
	exit $$status

# hooks: install the git pre-commit hook (idempotent — sets core.hooksPath).
hooks:
	git config core.hooksPath .githooks
	@echo "git hooks installed (.githooks) — pre-commit now guards release-tag drift + staged-Go gofmt"


# publish: test → version-check → cross-compile → release-check → npm
# publish (per-platform packages first, then the root that
# optional-depends on them).
publish:
	@echo "== publish $(VERSION): test → version-check → cross-compile → release-check → npm publish =="
	$(MAKE) test
	$(MAKE) version-check
	$(MAKE) cross-compile
	$(MAKE) release-check
	set -e; for triple in $(PLATFORMS); do \
		echo "== npm publish @talha7k/$$triple@$(VERSION)"; \
		npm publish ./npm/platforms/$$triple --tag $(NPM_TAG) --access public; \
	done
	echo "== npm publish kampodra@$(VERSION)"
	npm publish ./npm --tag $(NPM_TAG) --access public

clean:
	rm -rf dist
