GOLANGCI_LINT_VERSION := 2.9.0
BUILDX_VERSION := 0.21.2

# Detect OS and arch for binary download.
GOOS   := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

BIN_DIR := $(shell go env GOPATH)/bin
GOLANGCI_LINT := $(BIN_DIR)/golangci-lint

BINARY     := gc
BUILD_DIR  := bin
INSTALL_DIR := $(BIN_DIR)

# Version metadata injected via ldflags.
VERSION    := $(shell tag=$$(git describe --tags --exact-match 2>/dev/null || true); if [ -n "$$tag" ]; then printf '%s' "$$tag" | sed 's/^v//'; else echo "dev"; fi)
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -X main.version=$(VERSION) \
           -X main.commit=$(COMMIT) \
           -X main.date=$(BUILD_TIME)

unique_words = $(if $1,$(firstword $1) $(call unique_words,$(filter-out $(firstword $1),$1)))

# macOS: icu4c (a transitive Dolt / go-icu-regex CGO build dependency) is
# keg-only under Homebrew, so its headers/libs are not on the default CGO
# search path. Point CGO at them when icu4c is present. This is a no-op on
# Linux and other platforms (where system ICU, e.g. libicu-dev, is found
# normally) and a no-op on macOS when icu4c is not installed.
ifeq ($(shell uname),Darwin)
ICU_PREFIX := $(shell brew --prefix icu4c 2>/dev/null)
ifneq ($(ICU_PREFIX),)
CGO_CPPFLAGS += -I$(ICU_PREFIX)/include
CGO_LDFLAGS += -L$(ICU_PREFIX)/lib
export CGO_CPPFLAGS
export CGO_LDFLAGS
endif
endif

# Linux: some non-system compilers (Nix, Flox, etc.) don't search /usr/include
# or /usr/lib by default. If system ICU headers exist but the compiler doesn't
# see them, intentionally let system paths participate in the whole CGO build.
# Set SYS_USR_CGO_FALLBACK=0 to disable this fallback for hermetic or cross-CGO
# builds.
ifeq ($(shell uname),Linux)
SYS_USR_CGO_FALLBACK ?= 1
ifneq ($(SYS_USR_CGO_FALLBACK),0)
SYS_USR_INCLUDE ?= /usr/include
SYS_USR_LIB_ROOT ?= /usr/lib
SYS_USR_LIB64_ROOT ?= /usr/lib64
ifneq ($(wildcard $(SYS_USR_INCLUDE)/unicode/uregex.h),)
ifeq ($(shell $(CC) -E -Wp,-v -x c /dev/null 2>&1 | sed 's/^[[:space:]]*//' | grep -F -x -q "$(SYS_USR_INCLUDE)" && echo yes),)
SYS_USR_MULTIARCH_CANDIDATES := $(strip $(shell dpkg-architecture -q DEB_HOST_MULTIARCH 2>/dev/null) $(shell $(CC) -print-multiarch 2>/dev/null))
SYS_USR_LIB_CANDIDATES := $(foreach arch,$(SYS_USR_MULTIARCH_CANDIDATES),$(SYS_USR_LIB_ROOT)/$(arch)) $(SYS_USR_LIB64_ROOT) $(SYS_USR_LIB_ROOT)
SYS_USR_LIB_DIRS := $(strip $(call unique_words,$(strip $(foreach dir,$(SYS_USR_LIB_CANDIDATES),$(if $(wildcard $(dir)),$(dir))))))
$(info Linux system CGO fallback active: adding -I$(SYS_USR_INCLUDE) $(addprefix -L,$(SYS_USR_LIB_DIRS)); set SYS_USR_CGO_FALLBACK=0 to disable)
CGO_CPPFLAGS += -I$(SYS_USR_INCLUDE)
CGO_LDFLAGS += $(addprefix -L,$(SYS_USR_LIB_DIRS))
export CGO_CPPFLAGS
export CGO_LDFLAGS
endif
endif
endif
endif

.PHONY: build check check-all check-bd check-docker check-docs check-dolt check-eventexport-isolation check-gomod-replace check-core-boundary check-native-dependency-surface check-routed-test-rows check-version-tag lint lint-full lint-new lint-changed fmt-check fmt vet test test-mac test-fast-parallel test-fsys-darwin-compile test-pack-registry-live test-native-doltlite-beads test-cmd-gc-process test-cmd-gc-process-shard test-cmd-gc-process-parallel test-worker-core test-worker-core-phase2 test-worker-core-phase2-real-transport setup-worker-inference test-worker-inference test-worker-inference-phase3 test-acceptance test-acceptance-b test-acceptance-c test-acceptance-all test-tutorial-goldens test-tutorial-regression test-tutorial test-integration test-integration-shards test-integration-shards-parallel test-integration-shards-cover test-integration-packages test-integration-packages-cover test-integration-review-formulas test-integration-review-formulas-cover test-integration-review-formulas-basic test-integration-review-formulas-basic-cover test-integration-review-formulas-retries test-integration-review-formulas-retries-cover test-integration-review-formulas-recovery test-integration-review-formulas-recovery-cover test-integration-bdstore test-integration-bdstore-cover test-integration-rest test-integration-rest-cover test-integration-rest-smoke test-integration-rest-smoke-cover test-integration-rest-full test-integration-rest-full-cover test-local-full-parallel test-mail-wisp-insert test-mcp-mail test-openclaw-bridge test-docker test-k8s test-cover test-cover-mac test-cover-noncmdgc test-cover-cmdgc-shard cover install install-tools install-buildx setup clean generate check-schema docker-base docker-agent docker-controller docs-dev diagrams-excalidraw dashboard-smoke

## build: compile gc binary with version metadata
build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/gc
ifeq ($(shell uname),Darwin)
	@scripts/sign-darwin-local.sh $(BUILD_DIR)/$(BINARY)
endif

## install: build and install gc to GOPATH/bin (same location as go install)
install: build
	@mkdir -p $(INSTALL_DIR)
	@set -e; \
		tmp="$(INSTALL_DIR)/.$(BINARY).tmp.$$$$"; \
		trap 'rm -f "$$tmp"' EXIT INT TERM HUP; \
		cp -f "$(BUILD_DIR)/$(BINARY)" "$$tmp"; \
		chmod 0755 "$$tmp"; \
		mv -f "$$tmp" "$(INSTALL_DIR)/$(BINARY)"; \
		trap - EXIT INT TERM HUP
	@# Migrate from old install location: replace stale binary with symlink
	@if [ "$(INSTALL_DIR)" != "$(HOME)/.local/bin" ]; then \
		if [ -f "$(HOME)/.local/bin/$(BINARY)" ] || [ -L "$(HOME)/.local/bin/$(BINARY)" ]; then \
			rm -f "$(HOME)/.local/bin/$(BINARY)"; \
		fi; \
		if [ -d "$(HOME)/.local/bin" ]; then \
			ln -sf "$(INSTALL_DIR)/$(BINARY)" "$(HOME)/.local/bin/$(BINARY)"; \
			echo "Symlinked $(HOME)/.local/bin/$(BINARY) -> $(INSTALL_DIR)/$(BINARY)"; \
		fi; \
	fi
	@echo "Installed $(BINARY) to $(INSTALL_DIR)/$(BINARY)"

## generate: regenerate JSON schemas and reference docs
generate:
	go run ./cmd/genschema

## check-schema: verify generated docs are up to date
check-schema: generate
	@git diff --exit-code docs/reference/ || \
		(echo "Error: generated docs stale. Run 'make generate'" && exit 1)

## clean: remove build artifacts
clean:
	rm -f $(BUILD_DIR)/$(BINARY)

## check: run fast quality gates (pre-commit: unit tests only)
check: fmt-check lint vet check-routed-test-rows test

## check-routed-test-rows: enforce the six-row matrix on read-path routed tests
## Prevents per-file read-path migrations (ga-h6w) from regressing below the
## six mandatory rows (api-happy-path, api-cache-not-live, api-500-fallback,
## api-404-error, controller-down, escape-hatch).
check-routed-test-rows:
	./scripts/check-routed-test-rows.sh

## check-gomod-replace: block unreleased replace directives (pseudo-version, local path, git ref)
## Tripwire for the 2026-06-11 incident where PR #3489 shipped a pseudo-version replace
## (=> v1.0.5-0.20260611054652-dc0561af28e9) that violated the public-project release policy.
## Policy: only released semver tags allowed; human-operator bypass required for exceptions.
check-gomod-replace:
	bash scripts/check-gomod-replace.sh go.mod

## check-core-boundary: guard the open-core boundary (no commercial coupling in the OSS module)
check-core-boundary:
	bash scripts/check-core-boundary.sh

## check-native-dependency-surface: guard native beads dependency and binary growth
check-native-dependency-surface:
	bash scripts/check-native-dependency-surface.sh

## check-eventexport-isolation: keep the OSS event-export surface brand-free, single-sourced, and internal-free
check-eventexport-isolation:
	bash scripts/check-eventexport-isolation.sh

## check-bd: verify bd (beads CLI) is installed
check-bd:
	@command -v bd >/dev/null 2>&1 || \
		(echo "Error: bd not found. See docs/getting-started/installation.md" && exit 1)

## check-docker: verify docker and buildx are available
check-docker:
	@command -v docker >/dev/null 2>&1 || \
		(echo "Error: docker not found. Install: https://docs.docker.com/engine/install/" && exit 1)
	@docker buildx version >/dev/null 2>&1 || \
		(echo "Error: docker buildx not found. Run: make install-buildx" && exit 1)

## check-dolt: verify dolt is installed
check-dolt:
	@command -v dolt >/dev/null 2>&1 || \
		(echo "Error: dolt not found. See docs/getting-started/installation.md" && exit 1)

## check-version-tag: verify HEAD's release tag (if any) is a clean stable vX.Y.Z
## No-op on untagged HEADs, so safe to run on every checkout. Used by release.yml
## to reject pre-release tags (vX.Y.Z-rc1, -beta, etc.) — the release workflow
## publishes stable releases only.
check-version-tag:
	@TAG=$$(git describe --tags --exact-match HEAD 2>/dev/null || true); \
	if [ -z "$$TAG" ]; then \
		echo "check-version-tag: HEAD is not a release tag, skipping"; \
		exit 0; \
	fi; \
	if echo "$$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$$'; then \
		echo "check-version-tag: OK ($$TAG is a stable release tag)"; \
		exit 0; \
	fi; \
	if echo "$$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+-'; then \
		echo "ERROR: tag '$$TAG' has a pre-release suffix"; \
		echo "The release workflow publishes stable releases only."; \
		echo "Pre-release tags should not trigger release.yml."; \
		exit 1; \
	fi; \
	echo "ERROR: tag '$$TAG' is not a vX.Y.Z release tag"; \
	echo "Release tags must match vMAJOR.MINOR.PATCH exactly."; \
	exit 1

## check-all: run all quality gates including integration tests (CI)
check-all: fmt-check lint vet check-bd check-dolt check-docker test-integration check-docs

LINT_BASE ?= origin/main
LINT_CHANGED_REF ?= HEAD
LINT_CHANGED_SCOPE ?= worktree
LINT_FLAGS ?=

## lint: run full-repo golangci-lint
lint: lint-full

## lint-full: run golangci-lint across all packages
lint-full: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run $(LINT_FLAGS) ./...

## lint-new: run golangci-lint for issues introduced since LINT_BASE
lint-new: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run $(LINT_FLAGS) --new-from-merge-base=$(LINT_BASE) --whole-files ./...

## lint-changed: run golangci-lint only for packages touched by changed Go files
lint-changed: $(GOLANGCI_LINT)
	@case "$(LINT_CHANGED_SCOPE)" in \
		staged) \
			files="$$(git diff --cached --name-only --diff-filter=ACMRT -- '*.go')"; \
			;; \
		tracked) \
			files="$$(git diff --name-only --diff-filter=ACMRT "$(LINT_CHANGED_REF)" -- '*.go')"; \
			;; \
		worktree) \
			files="$$( \
				git diff --name-only --diff-filter=ACMRT "$(LINT_CHANGED_REF)" -- '*.go'; \
				git diff --cached --name-only --diff-filter=ACMRT -- '*.go'; \
				git ls-files --others --exclude-standard -- '*.go'; \
			)"; \
			;; \
		*) \
			echo "unknown LINT_CHANGED_SCOPE=$(LINT_CHANGED_SCOPE); expected staged, tracked, or worktree" >&2; \
			exit 2; \
			;; \
	esac; \
	if [ -z "$$files" ]; then \
		echo "lint-changed: no changed Go files"; \
		exit 0; \
	fi; \
	pkgs="$$(printf '%s\n' "$$files" | sed '/^$$/d' | sort -u | while IFS= read -r file; do dirname "$$file"; done | sort -u | while IFS= read -r dir; do \
		if [ "$$dir" = "." ]; then pkg="."; else pkg="./$$dir"; fi; \
		if go list "$$pkg" >/dev/null 2>&1; then printf '%s\n' "$$pkg"; fi; \
	done | sort -u)"; \
	if [ -z "$$pkgs" ]; then \
		echo "lint-changed: no lintable Go packages"; \
		exit 0; \
	fi; \
	echo "lint-changed: $$(printf '%s\n' "$$pkgs" | tr '\n' ' ')"; \
	$(GOLANGCI_LINT) run $(LINT_FLAGS) $$pkgs

## fmt-check: fail if formatting would change files
fmt-check: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt --diff ./...

## fmt: auto-fix formatting
fmt: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) fmt ./...

## vet: run go vet
vet:
	go vet ./...

## TEST_ENV: env -i wrapper for `go test` invocations. Strips host env so
## agent-session vars (GC_CITY, GC_HOME, GC_SESSION_ID, ...) cannot leak into
## tests and corrupt live cities. Only the allowlist below survives. To opt
## extra vars through, set EXTRA_TEST_ENV='FOO=bar BAZ=qux' on the make line.
## See PR #746.
##
## Load-bearing: GC_DOLT_PORT and BEADS_DOLT_SERVER_PORT are deliberately NOT in
## the allowlist below. They point bd at the live shared city Dolt server, so
## letting them reach `go test` makes every bd-forking test write to PRODUCTION
## Dolt — 18+ parallel workers pegged the shared server and stalled bd writes
## city-wide (ga-w2kh1r). Do not add them. For a bare `go test` that bypasses
## this wrapper, internal/testenv scrubs these vars at test-binary init in every
## covered package (enforced by TestRequiresDedicatedTestenvImportFile).
GOPATH_VAL    := $(shell go env GOPATH)
GOCACHE_VAL   := $(shell go env GOCACHE)
GOMODCACHE_VAL := $(shell go env GOMODCACHE)
GOTMPDIR_VAL  := $(shell go env GOTMPDIR)
GOROOT_VAL    := $(shell go env GOROOT)
TEST_ENV = env -i \
	PATH="$$PATH" \
	HOME="$$HOME" \
	USER="$$USER" \
	LOGNAME="$$LOGNAME" \
	SHELL="$$SHELL" \
	LANG="$$LANG" \
	TMPDIR="$${TMPDIR:-/tmp}" \
	OBSERVABLE_TEST_LOG="$${OBSERVABLE_TEST_LOG-}" \
	OBSERVABLE_FAILURE_LINES="$${OBSERVABLE_FAILURE_LINES-}" \
	GC_TEST_NO_SLICE="$${GC_TEST_NO_SLICE-}" \
	XDG_RUNTIME_DIR="$$XDG_RUNTIME_DIR" \
	GOPATH="$(GOPATH_VAL)" \
	GOCACHE="$(GOCACHE_VAL)" \
	GOMODCACHE="$(GOMODCACHE_VAL)" \
	GOTMPDIR="$(GOTMPDIR_VAL)" \
	GOROOT="$${GOROOT:-$(GOROOT_VAL)}" \
	GOENV="$${GOENV-}" \
	GOFLAGS="$${GOFLAGS-}" \
	GO111MODULE="$${GO111MODULE-}" \
	GOEXPERIMENT="$${GOEXPERIMENT-}" \
	GOPROXY="$${GOPROXY-}" \
	GOPRIVATE="$${GOPRIVATE-}" \
	GONOPROXY="$${GONOPROXY-}" \
	GONOSUMDB="$${GONOSUMDB-}" \
	GOSUMDB="$${GOSUMDB-}" \
	GOINSECURE="$${GOINSECURE-}" \
	GOVCS="$${GOVCS-}" \
	GOWORK="$${GOWORK-}" \
	ANTHROPIC_BASE_URL="$${ANTHROPIC_BASE_URL-}" \
	ANTHROPIC_API_KEY="$${ANTHROPIC_API_KEY-}" \
	ANTHROPIC_AUTH_TOKEN="$${ANTHROPIC_AUTH_TOKEN-}" \
	ANTHROPIC_DEFAULT_HAIKU_MODEL="$${ANTHROPIC_DEFAULT_HAIKU_MODEL-}" \
	ANTHROPIC_DEFAULT_SONNET_MODEL="$${ANTHROPIC_DEFAULT_SONNET_MODEL-}" \
	ANTHROPIC_DEFAULT_OPUS_MODEL="$${ANTHROPIC_DEFAULT_OPUS_MODEL-}" \
	CLAUDE_CODE_SUBAGENT_MODEL="$${CLAUDE_CODE_SUBAGENT_MODEL-}" \
	CLAUDE_CODE_EFFORT_LEVEL="$${CLAUDE_CODE_EFFORT_LEVEL-}" \
	CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="$${CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC-}" \
	OLLAMA_API_KEY="$${OLLAMA_API_KEY-}" \
	CGO_CPPFLAGS="$${CGO_CPPFLAGS-}" \
	CGO_LDFLAGS="$${CGO_LDFLAGS-}" \
	$(EXTRA_TEST_ENV)

## test: run fast unit tests (skip integration-tagged and GC_FAST_UNIT-gated process tests)
## The skipped cmd/gc process-backed scenarios remain covered by
## `make test-cmd-gc-process` locally and the CI `cmd/gc process suite` job.
## Bound package parallelism so subprocess-heavy packages do not starve each
## other into false 5s probe/condition timeouts. Use -count=1 so pre-commit
## reports actual test results instead of hanging after PASS while Go computes
## cache input hashes over local working files.
## Wrapped in $(TEST_ENV) — see comment above for why.
test: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 scripts/go-test-observable test -- -p=4 -count=1 -timeout 15m ./...

# MAC_UNIT_PKGS excludes cmd/gc from the Mac unit sweep; cmd/gc runs
# sharded via the mac-cmd-gc-process CI matrix job instead.
MAC_UNIT_PKGS = $(shell go list ./... | grep -v '/cmd/gc$$')

## test-mac: Mac unit sweep with cmd/gc excluded; cmd/gc covered by the Mac sharded job.
test-mac: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 scripts/go-test-observable test-mac -- -p=4 -count=1 -timeout 15m $(MAC_UNIT_PKGS)

LOCAL_TEST_JOBS ?= $(shell nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 8)

## test-fast-parallel: run the default fast suite with cmd/gc sharded locally
test-fast-parallel:
	$(TEST_ENV) LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) CMD_GC_PROCESS_TOTAL=$(CMD_GC_PROCESS_TOTAL) ./scripts/test-local-parallel fast

## test-fsys-darwin-compile: cross-compile internal/fsys for macOS so
## unix.Stat_t field-type regressions fail in the default fast test path.
test-fsys-darwin-compile:
	@tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	$(TEST_ENV) GOOS=darwin GOARCH=arm64 go test -c -o "$$tmp/fsys.test" ./internal/fsys

## test-pack-registry-live: run the opt-in gascity-packs registry canary
test-pack-registry-live:
	@if [ -z "$${GC_TEST_GASCITY_PACKS_REGISTRY:-}" ]; then \
		echo "Set GC_TEST_GASCITY_PACKS_REGISTRY to main or a gascity-packs registry.toml source"; \
		echo "Example: GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live"; \
		exit 2; \
	fi
	@# Keep the live canary portable on runners and local machines that do not
	@# have the optional ICU C headers needed by the default CGO build path.
	$(TEST_ENV) CGO_ENABLED=0 GC_TEST_GASCITY_PACKS_REGISTRY="$${GC_TEST_GASCITY_PACKS_REGISTRY}" go test ./cmd/gc -run '^TestPackRegistryLiveGascityPacksCatalog$$' -count=1
	$(TEST_ENV) CGO_ENABLED=0 GC_TEST_GASCITY_PACKS_REGISTRY="$${GC_TEST_GASCITY_PACKS_REGISTRY}" go test -tags acceptance_a -timeout 10m ./test/acceptance -run '^TestPackRegistryLiveImportsEveryCatalogPack$$' -count=1

## update-bundled-gastown-pack: pin the gastown module/constants/example to the latest registry release
update-bundled-gastown-pack:
	scripts/update-bundled-gastown-pack

## test-native-doltlite-beads: compile and run the native DoltLite read-store suite
test-native-doltlite-beads:
	$(TEST_ENV) CGO_ENABLED=0 go test -tags gascity_native_beads ./internal/beads -count=1

## sync-bd-corpus: vendor the bd contract corpus from a beads release (BD_CORPUS_TAG=vX.Y.Z)
sync-bd-corpus:
	scripts/sync-bd-corpus.sh

## test-cmd-gc-process: run the full non-short cmd/gc suite, including the
## process-backed lifecycle coverage routed out of the default fast loop
test-cmd-gc-process:
	$(TEST_ENV) GC_FAST_UNIT=0 scripts/go-test-observable test-cmd-gc-process -- -timeout 25m ./cmd/gc

CMD_GC_PROCESS_SHARD ?= 1
CMD_GC_PROCESS_TOTAL ?= 6
CMD_GC_COVER_TOTAL ?= 6
CMD_GC_COVER_SHARD ?= 1
test-cmd-gc-process-shard:
	$(TEST_ENV) GC_FAST_UNIT=0 GO_TEST_COUNT=1 GO_TEST_TIMEOUT=20m ./scripts/test-go-test-shard ./cmd/gc $(CMD_GC_PROCESS_SHARD) $(CMD_GC_PROCESS_TOTAL)

## test-cmd-gc-process-parallel: run all cmd/gc process shards concurrently
test-cmd-gc-process-parallel:
	LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) CMD_GC_PROCESS_TOTAL=$(CMD_GC_PROCESS_TOTAL) ./scripts/test-local-parallel cmd-gc-process

## test-worker-core: run deterministic worker transcript and continuation conformance
test-worker-core:
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./internal/worker/workertest -run '^TestPhase1'

## test-worker-core-phase2: run deterministic phase-2 worker conformance coverage
test-worker-core-phase2:
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./internal/worker/workertest -run '^TestPhase2'
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./internal/runtime/tmux -run '^TestPhase2'
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 ./cmd/gc -run '^TestPhase2(StartupMaterialization|InitialInputDelivery|InputResultFailureClassification)$$'

## test-worker-core-phase2-real-transport: run the live transport proof for phase 2
test-worker-core-phase2-real-transport:
	$(TEST_ENV) PROFILE="$${PROFILE-}" GC_WORKER_REPORT_DIR="$${GC_WORKER_REPORT_DIR-}" go test -count=1 -tags integration ./cmd/gc -run '^TestPhase2WorkerCoreRealTransportProof$$'

WORKER_INFERENCE_PROFILE := $(if $(PROFILE),$(PROFILE),claude/tmux-cli)

## setup-worker-inference: install the provider CLI for PROFILE (default claude/tmux-cli)
setup-worker-inference:
	python3 scripts/worker_inference_setup.py install --profile "$(WORKER_INFERENCE_PROFILE)"

## test-worker-inference: run the live worker inference conformance package
test-worker-inference:
	$(TEST_ENV) PROFILE="$(WORKER_INFERENCE_PROFILE)" GC_WORKER_REPORT_DIR="$(GC_WORKER_REPORT_DIR)" go test -count=1 -tags acceptance_c -timeout 45m -v ./test/acceptance/worker_inference

## test-worker-inference-phase3: alias for the live worker inference conformance package
test-worker-inference-phase3: test-worker-inference

## test-acceptance: run acceptance tests (Tier A — command-level PR gate).
## ACCEPTANCE_TIMEOUT overrides the go-test timeout. The unsharded local/CI
## target runs the command-heavy Tier A package serially; RC gate shards it.
ACCEPTANCE_TIMEOUT ?= 15m
test-acceptance:
	$(TEST_ENV) go test -tags acceptance_a -timeout $(ACCEPTANCE_TIMEOUT) ./test/acceptance/...

## test-acceptance-b: run Tier B acceptance tests (lifecycle, ~5 min, nightly)
ACCEPTANCE_B_TIMEOUT ?= 10m
test-acceptance-b:
	$(TEST_ENV) go test -tags acceptance_b -timeout $(ACCEPTANCE_B_TIMEOUT) -v ./test/acceptance/tier_b/...

## test-acceptance-c: run Tier C acceptance tests (real inference, ~30-40 min, manual/nightly)
test-acceptance-c:
	$(TEST_ENV) go test -tags acceptance_c -timeout 45m -v ./test/acceptance/tier_c/...

## test-acceptance-all: run all acceptance tiers
test-acceptance-all: test-acceptance test-acceptance-b test-acceptance-c

## test-integration: run all tests including integration (tmux, etc.)
test-integration:
	$(TEST_ENV) go test -tags integration -timeout 30m ./...

## test-integration-huma: run just the Huma binary smoke test
test-integration-huma:
	$(TEST_ENV) go test -tags integration -timeout 2m -run TestHumaBinary ./test/integration/

## test-integration-shards: run the CI integration shards sequentially
test-integration-shards: test-integration-packages test-integration-review-formulas test-integration-bdstore test-integration-rest-smoke test-integration-rest-full

## test-integration-shards-parallel: run the CI integration shards concurrently
test-integration-shards-parallel:
	LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) ./scripts/test-local-parallel integration

## test-local-full-parallel: run fast unit, cmd/gc process, and integration shards concurrently
test-local-full-parallel:
	LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) CMD_GC_PROCESS_TOTAL=$(CMD_GC_PROCESS_TOTAL) ./scripts/test-local-parallel full

## test-integration-shards-cover: run the CI integration coverage shards sequentially
test-integration-shards-cover: test-integration-packages-cover test-integration-review-formulas-cover test-integration-bdstore-cover test-integration-rest-smoke-cover test-integration-rest-full-cover

## test-integration-packages: run all integration-tagged packages except ./test/integration
## cmd/gc package shards default to GC_FAST_UNIT=1; use test-cmd-gc-process for the slow process suite.
test-integration-packages:
	./scripts/test-integration-shard packages

## test-integration-packages-cover: run the packages shard with a CI coverage profile
test-integration-packages-cover:
	GO_TEST_COVERPROFILE=coverage.integration-packages.txt ./scripts/test-integration-shard packages

## test-integration-review-formulas: run the long-running workflow formula integration tests
test-integration-review-formulas:
	@status=0; \
	$(MAKE) test-integration-review-formulas-basic || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-review-formulas-retries || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-review-formulas-recovery || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	exit $$status

## test-integration-review-formulas-cover: run the review-formulas shard with a CI coverage profile
test-integration-review-formulas-cover:
	@status=0; \
	$(MAKE) test-integration-review-formulas-basic-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-review-formulas-retries-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-review-formulas-recovery-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	if [ $$status -eq 0 ]; then \
		./scripts/merge-coverprofiles coverage.integration-review-formulas.txt \
			coverage.integration-review-formulas-basic.txt \
			coverage.integration-review-formulas-retries.txt \
			coverage.integration-review-formulas-recovery.txt; \
	fi; \
	exit $$status

## test-integration-review-formulas-basic: run the core happy-path review-formulas tests
test-integration-review-formulas-basic:
	./scripts/test-integration-shard review-formulas-basic

## test-integration-review-formulas-basic-cover: run the basic review-formulas shard with coverage
test-integration-review-formulas-basic-cover:
	GO_TEST_COVERPROFILE=coverage.integration-review-formulas-basic.txt ./scripts/test-integration-shard review-formulas-basic

## test-integration-review-formulas-retries: run the retry/soft-fail review-formulas tests
test-integration-review-formulas-retries:
	./scripts/test-integration-shard review-formulas-retries

## test-integration-review-formulas-retries-cover: run the retry/soft-fail review-formulas shard with coverage
test-integration-review-formulas-retries-cover:
	GO_TEST_COVERPROFILE=coverage.integration-review-formulas-retries.txt ./scripts/test-integration-shard review-formulas-retries

## test-integration-review-formulas-recovery: run the crash/recovery review-formulas test
test-integration-review-formulas-recovery:
	./scripts/test-integration-shard review-formulas-recovery

## test-integration-review-formulas-recovery-cover: run the crash/recovery review-formulas shard with coverage
test-integration-review-formulas-recovery-cover:
	GO_TEST_COVERPROFILE=coverage.integration-review-formulas-recovery.txt ./scripts/test-integration-shard review-formulas-recovery

## test-integration-bdstore: run the bd store conformance shard in isolation
test-integration-bdstore:
	./scripts/test-integration-shard bdstore

## test-integration-bdstore-cover: run the bdstore shard with a CI coverage profile
test-integration-bdstore-cover:
	GO_TEST_COVERPROFILE=coverage.integration-bdstore.txt ./scripts/test-integration-shard bdstore

## test-integration-rest-smoke: run the PR smoke subset of the remaining ./test/integration tests
test-integration-rest-smoke:
	./scripts/test-integration-shard rest-smoke

## test-integration-rest-smoke-cover: run the smoke rest shard with a CI coverage profile
test-integration-rest-smoke-cover:
	GO_TEST_COVERPROFILE=coverage.integration-rest-smoke.txt ./scripts/test-integration-shard rest-smoke

## test-integration-rest-full: run the heavier rest shard kept for nightly/RC and targeted PRs
test-integration-rest-full:
	./scripts/test-integration-shard rest-full

## test-integration-rest-full-cover: run the full rest shard with a CI coverage profile
test-integration-rest-full-cover:
	GO_TEST_COVERPROFILE=coverage.integration-rest-full.txt ./scripts/test-integration-shard rest-full

## test-integration-rest: run the combined rest smoke+full suite
test-integration-rest:
	@status=0; \
	$(MAKE) test-integration-rest-smoke || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-rest-full || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	exit $$status

## test-integration-rest-cover: run the combined rest smoke+full coverage shards
test-integration-rest-cover:
	@status=0; \
	$(MAKE) test-integration-rest-smoke-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	$(MAKE) test-integration-rest-full-cover || { st=$$?; [ $$status -ne 0 ] || status=$$st; }; \
	exit $$status

## test-chaos-dolt: run the opt-in managed Dolt chaos integration test
## Set GC_DOLT_CHAOS_DURATION and GC_DOLT_CHAOS_SEED to control runtime and replay failures.
test-chaos-dolt:
	$(TEST_ENV) \
		GC_DOLT_CHAOS_DURATION=$${GC_DOLT_CHAOS_DURATION:-2m} \
		GC_DOLT_CHAOS_SEED="$${GC_DOLT_CHAOS_SEED-}" \
		go test -tags 'integration chaos_dolt' -timeout 45m -run 'TestManagedDoltChaos_CityAndRigCallersRemainConsistent' -count=1 ./test/integration


## test-tutorial-goldens: run tutorial golden acceptance tests (requires tmux, dolt, bd, claude auth)
## These exercise the published tutorial flow with real inference — run before each release.
test-tutorial-goldens:
	$(TEST_ENV) go test -tags acceptance_c -timeout 90m -v ./test/acceptance/tutorial_goldens/...

## test-tutorial: alias for tutorial goldens
test-tutorial: test-tutorial-goldens

## test-tutorial-regression: alias for tutorial goldens
test-tutorial-regression: test-tutorial-goldens

## check-docs: verify docs sync tests
check-docs:
	$(TEST_ENV) go test ./test/docsync

# Packages for coverage — exclude noise:
#   session/tmux: integration-test-only, not meaningful for unit coverage
#   beadstest: conformance helper, runs under internal/beads coverage
# cmd/gc excluded: it runs sharded below in test-cover to stay under per-package timeout
UNIT_COVER_PKGS_NONCMDGC = $(shell go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v -e /session/tmux -e /beadstest -e '/cmd/gc$$')

## test-cover: run fast unit-test coverage without the integration-tagged package sweep.
## cmd/gc is sharded CMD_GC_COVER_TOTAL (default 6) ways via test-go-test-shard so each
## shard lands well under the per-package timeout; profiles are merged via merge-coverprofiles.
## The skipped cmd/gc process-backed scenarios remain covered by
## `make test-cmd-gc-process` locally and the CI `cmd/gc process suite` job.
test-cover: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 go test -timeout 10m -coverprofile=coverage.noncmdgc.txt $(UNIT_COVER_PKGS_NONCMDGC)
	@rm -f coverage.cmdgc.*.txt
	@for s in $$(seq 1 $(CMD_GC_COVER_TOTAL)); do \
		$(TEST_ENV) GO_TEST_COVERPROFILE="coverage.cmdgc.$$s.txt" \
		GC_FAST_UNIT=1 GO_TEST_COUNT=1 GO_TEST_TIMEOUT=10m \
		./scripts/test-go-test-shard ./cmd/gc "$$s" $(CMD_GC_COVER_TOTAL) || exit 1; \
	done
	./scripts/merge-coverprofiles coverage.txt coverage.noncmdgc.txt coverage.cmdgc.*.txt

## test-cover-mac: Mac coverage sweep with cmd/gc excluded; cmd/gc runs via the Mac sharded job.
## Running the full test-cover cmd/gc shards sequentially on Mac would exceed the 25m job cap.
test-cover-mac: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 go test -timeout 10m -coverprofile=coverage.txt $(UNIT_COVER_PKGS_NONCMDGC)

## test-cover-noncmdgc: run unit coverage for all packages except cmd/gc (CI parallel half).
test-cover-noncmdgc: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 go test -timeout 10m -coverprofile=coverage.noncmdgc.txt $(UNIT_COVER_PKGS_NONCMDGC)

## test-cover-cmdgc-shard: run unit coverage for one cmd/gc shard (CMD_GC_COVER_SHARD of CMD_GC_COVER_TOTAL).
test-cover-cmdgc-shard:
	$(TEST_ENV) GO_TEST_COVERPROFILE=coverage.cmdgc.$(CMD_GC_COVER_SHARD).txt GC_FAST_UNIT=1 GO_TEST_COUNT=1 GO_TEST_TIMEOUT=10m ./scripts/test-go-test-shard ./cmd/gc $(CMD_GC_COVER_SHARD) $(CMD_GC_COVER_TOTAL)

## cover: run tests and show coverage report
cover: test-cover
	go tool cover -func=coverage.txt

## install-tools: install pinned golangci-lint + oapi-codegen
install-tools: $(GOLANGCI_LINT) install-oapi-codegen

$(GOLANGCI_LINT):
	@echo "Installing golangci-lint v$(GOLANGCI_LINT_VERSION)..."
	@attempt=1; max_attempts=5; delay=2; \
	while [ $$attempt -le $$max_attempts ]; do \
		echo "golangci-lint install attempt $$attempt/$$max_attempts"; \
		if GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION); then \
			exit 0; \
		fi; \
		if [ $$attempt -lt $$max_attempts ]; then \
			echo "golangci-lint install failed; retrying in $${delay}s..." >&2; \
			sleep $$delay; \
		fi; \
		attempt=$$((attempt + 1)); \
		delay=$$((delay * 2)); \
	done; \
	echo "ERROR: failed to install golangci-lint v$(GOLANGCI_LINT_VERSION) after $$max_attempts attempts" >&2; \
	exit 1

## install-oapi-codegen: install pinned oapi-codegen so the spec→client drift
## test (TestGeneratedClientInSync) can regenerate client_gen.go without skipping.
.PHONY: install-oapi-codegen
install-oapi-codegen:
	@if ! command -v oapi-codegen >/dev/null; then \
		echo "Installing oapi-codegen..." >&2; \
		go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.6.0; \
	fi

## install-buildx: install docker buildx plugin
install-buildx:
	@mkdir -p $(HOME)/.docker/cli-plugins
	@case "$(GOOS)-$(GOARCH)" in \
		linux-amd64|linux-arm64) ;; \
		*) echo "Unsupported docker-buildx platform: $(GOOS)-$(GOARCH)" >&2; exit 1 ;; \
	esac; \
	tmp="$$(mktemp)"; \
	checksums="$$(mktemp)"; \
	trap 'rm -f "$$tmp" "$$checksums"' EXIT; \
	curl -sSfL "https://github.com/docker/buildx/releases/download/v$(BUILDX_VERSION)/checksums.txt" \
		-o "$$checksums"; \
	asset="buildx-v$(BUILDX_VERSION).$(GOOS)-$(GOARCH)"; \
	expected_sha="$$(awk -v asset="*$$asset" '$$2 == asset {print $$1}' "$$checksums")"; \
	if [ -z "$$expected_sha" ]; then echo "Missing checksum for $$asset" >&2; exit 1; fi; \
	curl -sSfL "https://github.com/docker/buildx/releases/download/v$(BUILDX_VERSION)/buildx-v$(BUILDX_VERSION).$(GOOS)-$(GOARCH)" \
		-o "$$tmp"; \
	echo "$$expected_sha  $$tmp" | sha256sum -c -; \
	install -m 0755 "$$tmp" $(HOME)/.docker/cli-plugins/docker-buildx
	@echo "Installed docker-buildx v$(BUILDX_VERSION)"

## test-mail-wisp-insert: run the beads version-skew regression tests for gc mail send (wisp_events INSERT path)
## Tripwire for the 2026-06-11 P0: covers both NativeDoltStore and BdStore → Dolt paths.
test-mail-wisp-insert:
	@echo "=== NativeDoltStore ephemeral mail (go.mod beads library) ==="
	$(TEST_ENV) go test -tags integration ./internal/beads/ -run TestNativeDoltStoreEphemeralMailSend -v -count=1
	@echo "=== BdStore mail wisp INSERT (bd CLI → Dolt SQL) ==="
	$(TEST_ENV) go test -tags integration ./test/integration/ -run TestBdStoreMailWispInsert -v -count=1

## test-mcp-mail: run mcp_agent_mail live conformance test (auto-starts server)
test-mcp-mail:
	$(TEST_ENV) GC_TEST_MCP_MAIL=1 go test ./internal/mail/exec/ -run TestMCPMailConformanceLive -v -count=1

## test-openclaw-bridge: install + run the contrib/openclaw-bridge Node test suite
test-openclaw-bridge:
	cd contrib/openclaw-bridge && npm ci --no-audit --no-fund && npm test

## test-docker: run Docker session provider integration tests
test-docker: check-docker
	./scripts/test-docker-session

## test-k8s: run K8s session provider conformance tests
test-k8s:
	$(TEST_ENV) go test -tags integration ./test/integration/ -run TestK8sSessionConformance -v -count=1

## setup: install tools and git hooks
setup: install-tools
	git config core.hooksPath .githooks
	@echo "Done. Tools installed, pre-commit hook active."

## diagrams-excalidraw: render docs/diagrams/excalidraw/*.excalidraw to excalidraw-rendered/*.svg (idempotent)
diagrams-excalidraw:
	@set -e; \
	src_dir=docs/diagrams/excalidraw; \
	out_dir=docs/diagrams/excalidraw-rendered; \
	mkdir -p "$$out_dir"; \
	shopt -s nullglob 2>/dev/null || true; \
	rendered=0; \
	for f in "$$src_dir"/*.excalidraw; do \
		[ -e "$$f" ] || continue; \
		base=$$(basename "$$f" .excalidraw); \
		out="$$out_dir/$$base.svg"; \
		if [ ! -e "$$out" ] || [ "$$f" -nt "$$out" ]; then \
			echo "excalidraw -> $$out"; \
			npx -y @swiftlysingh/excalidraw-cli convert "$$f" --format svg --padding 16 --output "$$out"; \
			rendered=$$((rendered+1)); \
		fi; \
	done; \
	echo "excalidraw: rendered $$rendered file(s)"

## docs-dev: run the Mintlify docs locally
docs-dev:
	./mint.sh dev

## dashboard-build: compile the SPA bundle and sync it into the embedded dist/
dashboard-build:
	cd internal/api/dashboardspa/web && npm ci --silent && npm run build && rm -rf ../dist && cp -rf frontend/dist ../dist

## dashboard-dev: Vite dev server (HMR) for SPA iteration
dashboard-dev:
	cd internal/api/dashboardspa/web && npm run --workspace gas-city-dashboard-frontend dev

## dashboard-check: typecheck (src + test files) + build the SPA, then go test the embedded handler + BFF
dashboard-check: dashboard-build
	cd internal/api/dashboardspa/web && npm run typecheck && npm run --workspace gas-city-dashboard-frontend typecheck:test
	$(TEST_ENV) go test ./internal/api/dashboardspa/... ./internal/api/dashboardbff/...

## dashboard-smoke: serve the built SPA bundle via Vite preview and verify it responds
dashboard-smoke: dashboard-build
	@PORT=$$(python3 -c 'import socket; sock = socket.socket(); sock.bind(("127.0.0.1", 0)); print(sock.getsockname()[1]); sock.close()'); \
	LOG=$$(mktemp); \
	( cd internal/api/dashboardspa/web/frontend && exec npm run preview -- --host 127.0.0.1 --strictPort --port $$PORT >"$$LOG" 2>&1 ) & \
	PID=$$!; \
	trap 'kill $$PID >/dev/null 2>&1 || true; wait $$PID >/dev/null 2>&1 || true; rm -f "$$LOG"' EXIT INT TERM; \
	for attempt in $$(seq 1 40); do \
		if curl -fsS "http://127.0.0.1:$$PORT/" >/dev/null; then \
			exit 0; \
		fi; \
		sleep 0.25; \
	done; \
	cat "$$LOG" >&2; \
	exit 1

## dashboard-ci: rebuild the SPA bundle and fail if the embedded dist/ is stale.
## Used by CI to enforce that internal/api/dashboardspa/dist/ matches the source.
dashboard-ci: dashboard-check
	@if ! git diff --quiet -- internal/api/dashboardspa/dist; then \
		echo "ERROR: internal/api/dashboardspa/dist/ is stale — run 'make dashboard-build' and commit." >&2; \
		git --no-pager diff --stat -- internal/api/dashboardspa/dist; \
		exit 1; \
	fi

## spec-ci: regenerate the OpenAPI spec + generated Go client, fail on drift.
## Used by CI to enforce that internal/api/openapi.json, docs/reference/schema JSON
## artifacts, compatibility .txt mirrors, and internal/api/genclient/client_gen.go
## are all in lock-step with Huma.
spec-ci: install-oapi-codegen
	go run ./cmd/genspec
	go generate ./internal/api/genclient
	@if ! git diff --quiet -- internal/api/openapi.json docs/reference/schema/openapi.json docs/reference/schema/openapi.txt docs/reference/schema/events.json docs/reference/schema/events.txt internal/api/genclient/client_gen.go; then \
		echo "ERROR: spec/client artifacts drifted — run 'make spec-ci' locally and commit." >&2; \
		git --no-pager diff --stat -- internal/api/openapi.json docs/reference/schema/openapi.json docs/reference/schema/openapi.txt docs/reference/schema/events.json docs/reference/schema/events.txt internal/api/genclient/client_gen.go; \
		exit 1; \
	fi

## docker-base: build base image with system dependencies (~2.5 min, rebuild rarely)
docker-base: check-docker
	. ./deps.env && docker build -f contrib/k8s/Dockerfile.base \
		--build-arg DOLT_VERSION=$$DOLT_VERSION \
		-t gc-agent-base:latest .

## docker-agent: build base agent image (~5s on top of base). For prebaked images use: gc build-image
docker-agent: check-docker
	docker build -f contrib/k8s/Dockerfile.agent -t gc-agent:latest .
	@if kubectl config current-context 2>/dev/null | grep -q '^kind-'; then \
		cluster=$$(kubectl config current-context | sed 's/^kind-//'); \
		echo "Loading gc-agent:latest into kind cluster '$$cluster'..."; \
		kind load docker-image gc-agent:latest --name "$$cluster"; \
	fi

## docker-controller: build controller image for K8s deployment (~10s on top of agent)
docker-controller: check-docker
	docker build -f contrib/k8s/Dockerfile.controller -t gc-controller:latest .
	@if kubectl config current-context 2>/dev/null | grep -q '^kind-'; then \
		cluster=$$(kubectl config current-context | sed 's/^kind-//'); \
		echo "Loading gc-controller:latest into kind cluster '$$cluster'..."; \
		kind load docker-image gc-controller:latest --name "$$cluster"; \
	fi

## k8s-secret: create K8s secret with Claude credentials
## Usage: make k8s-secret CLAUDE_CONFIG_SRC=~/.claude [GC_K8S_NAMESPACE=gc]
## Source dir must contain .credentials.json (required) and optionally
## .claude.json (onboarding state) and settings.json.
k8s-secret:
	@if [ -z "$${CLAUDE_CONFIG_SRC:-}" ]; then \
		echo "Usage: make k8s-secret CLAUDE_CONFIG_SRC=<path-to-claude-config-dir>" >&2; \
		echo "  The directory must contain .credentials.json" >&2; \
		exit 1; \
	fi; \
	ns="$${GC_K8S_NAMESPACE:-gc}"; \
	src="$$CLAUDE_CONFIG_SRC"; \
	if [ ! -f "$$src/.credentials.json" ]; then \
		echo "Error: $$src/.credentials.json not found." >&2; \
		exit 1; \
	fi; \
	args="--from-file=.credentials.json=$$src/.credentials.json"; \
	[ -f "$$src/.claude.json" ] && args="$$args --from-file=.claude.json=$$src/.claude.json"; \
	[ -f "$$src/settings.json" ] && args="$$args --from-file=settings.json=$$src/settings.json"; \
	kubectl -n "$$ns" delete secret claude-credentials --ignore-not-found >/dev/null 2>&1; \
	kubectl -n "$$ns" create secret generic claude-credentials $$args; \
	echo "Secret 'claude-credentials' created in namespace '$$ns'"

## help: show this help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':'
