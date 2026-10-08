# kampodra — build, test, and release.
#
# `make publish` is the documented release flow; it NEVER auto-runs and is
# never wired into CI. It requires: a clean tree on a tagged commit
# (git tag v<version> == npm/package.json version), and npm auth for the
# @kampodra scope.

GO        ?= go
VERSION   := $(shell node -p "require('./npm/package.json').version")
PLATFORMS := darwin-arm64 darwin-x64 linux-amd64 linux-arm64

.PHONY: all test vet build cross-compile version-check npm-test publish clean

all: test build

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build:
	$(GO) build ./cmd/kampodra

npm-test:
	node --test npm/test/shim.test.mjs

# cross-compile: every release target, staged into the npm platform
# packages' bin/ (what `npm publish` ships).
cross-compile:
	@mkdir -p dist
	set -e; for triple in $(PLATFORMS); do \
		os=$${triple%%-*}; arch=$${triple##*-}; \
		$(GO) build -trimpath -ldflags "-s -w" -o dist/kampodra-$$triple ./cmd/kampodra; \
		mkdir -p npm/platforms/$$triple/bin; \
		cp dist/kampodra-$$triple npm/platforms/$$triple/bin/kampodra; \
		echo "built $$os/$$arch -> dist/kampodra-$$triple"; \
	done

# version-check: the release gate — HEAD must carry the git tag matching the
# npm package version (v<version>).
version-check:
	@test -n "$(VERSION)" || { echo "cannot read npm/package.json version"; exit 1; }
	@tag=$$(git tag --points-at HEAD | grep -E '^v' | head -n1); \
	echo "package version: $(VERSION)"; \
	echo "HEAD tag:        $${tag:-<none>}"; \
	test "$$tag" = "v$(VERSION)" || { \
		echo "FAIL: tag v$(VERSION) is not on HEAD — tag the release commit first (git tag v$(VERSION))"; exit 1; \
	}

# publish: test → version-check → cross-compile → npm publish (per-platform
# packages first, then the root that optional-depends on them).
publish:
	@echo "== publish $(VERSION): test → version-check → cross-compile → npm publish =="
	$(MAKE) test
	$(MAKE) version-check
	$(MAKE) cross-compile
	set -e; for triple in $(PLATFORMS); do \
		echo "== npm publish @kampodra/$$triple@$(VERSION)"; \
		npm publish --no-git-checks ./npm/platforms/$$triple; \
	done
	echo "== npm publish kampodra@$(VERSION)"
	npm publish --no-git-checks ./npm

clean:
	rm -rf dist
