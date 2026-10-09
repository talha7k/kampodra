# kampodra — build, test, and release.
#
# `make publish` is the documented release flow; it NEVER auto-runs and is
# never wired into CI. It requires: a clean tree on a tagged commit
# (git tag v<version> == npm/package.json version), and npm auth for the
# @kampodra scope.

GO        ?= go
VERSION   := $(shell node -p "require('./npm/package.json').version")
PLATFORMS := darwin-arm64 darwin-x64 linux-amd64 linux-arm64

.PHONY: all test vet lint build cross-compile version-check npm-test publish clean

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
	node --test npm/test/shim.test.mjs

# cross-compile: every release target, staged into the npm platform
# packages' bin/ (what `npm publish` ships). GOOS/GOARCH are set EXPLICITLY
# per triple — without them `go build` produces HOST binaries mislabeled as
# other platforms (the CI matrix always did this right; the local target
# now matches). main.version is stamped from the npm package version so the
# shipped binaries report the release, not the main.go fallback.
cross-compile:
	@mkdir -p dist
	set -e; for triple in $(PLATFORMS); do \
		os=$${triple%%-*}; arch=$${triple##*-}; \
		case $$arch in x64) arch=amd64 ;; esac; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/kampodra-$$triple ./cmd/kampodra; \
		mkdir -p npm/platforms/$$triple/bin; \
		cp dist/kampodra-$$triple npm/platforms/$$triple/bin/kampodra; \
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
# npm package version (v<version>).
version-check:
	@test -n "$(VERSION)" || { echo "cannot read npm/package.json version"; exit 1; }
	@tag=$$(git tag --points-at HEAD | grep -E '^v' | head -n1); \
	echo "package version: $(VERSION)"; \
	echo "HEAD tag:        $${tag:-<none>}"; \
	test "$$tag" = "v$(VERSION)" || { \
		echo "FAIL: tag v$(VERSION) is not on HEAD — tag the release commit first (git tag v$(VERSION))"; exit 1; \
	}

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
		echo "== npm publish @kampodra/$$triple@$(VERSION)"; \
		npm publish --no-git-checks ./npm/platforms/$$triple; \
	done
	echo "== npm publish kampodra@$(VERSION)"
	npm publish --no-git-checks ./npm

clean:
	rm -rf dist
