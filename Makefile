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

.PHONY: build check check-all check-bd check-docker check-docs check-dolt check-version-tag lint fmt-check fmt vet test test-fast-parallel test-fsys-darwin-compile test-cmd-gc-process test-cmd-gc-process-shard test-cmd-gc-process-parallel test-worker-core test-worker-core-phase2 test-worker-core-phase2-real-transport setup-worker-inference test-worker-inference test-worker-inference-phase3 test-acceptance test-acceptance-b test-acceptance-c test-acceptance-all test-tutorial-goldens test-tutorial-regression test-tutorial test-integration test-integration-shards test-integration-shards-parallel test-integration-shards-cover test-integration-packages test-integration-packages-cover test-integration-review-formulas test-integration-review-formulas-cover test-integration-review-formulas-basic test-integration-review-formulas-basic-cover test-integration-review-formulas-retries test-integration-review-formulas-retries-cover test-integration-review-formulas-recovery test-integration-review-formulas-recovery-cover test-integration-bdstore test-integration-bdstore-cover test-integration-rest test-integration-rest-cover test-integration-rest-smoke test-integration-rest-smoke-cover test-integration-rest-full test-integration-rest-full-cover test-local-full-parallel test-mcp-mail test-docker test-k8s test-cover cover install install-tools install-buildx setup clean generate check-schema docker-base docker-agent docker-controller docs-dev dashboard-smoke

## build: compile gc binary with version metadata
build:
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) ./cmd/gc
ifeq ($(shell uname),Darwin)
	@codesign -s - -f $(BUILD_DIR)/$(BINARY) 2>/dev/null || true
	@echo "Signed $(BINARY) for macOS"
endif

## install: build and install gc to GOPATH/bin (same location as go install)
install: build
	@mkdir -p $(INSTALL_DIR)
	@rm -f $(INSTALL_DIR)/$(BINARY)
	@cp $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)
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
	@git diff --exit-code docs/schema/ docs/reference/ || \
		(echo "Error: generated docs stale. Run 'make generate'" && exit 1)

## clean: remove build artifacts
clean:
	rm -f $(BUILD_DIR)/$(BINARY)

## check: run fast quality gates (pre-commit: unit tests only)
check: fmt-check lint vet test

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

## lint: run golangci-lint
lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run ./...

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
	$(TEST_ENV) GC_FAST_UNIT=1 scripts/go-test-observable test -- -p=4 -count=1 ./...

LOCAL_TEST_JOBS ?= $(shell nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 8)

## test-fast-parallel: run the default fast suite with cmd/gc sharded locally
test-fast-parallel:
	LOCAL_TEST_JOBS=$(LOCAL_TEST_JOBS) CMD_GC_PROCESS_TOTAL=$(CMD_GC_PROCESS_TOTAL) ./scripts/test-local-parallel fast

## test-fsys-darwin-compile: cross-compile internal/fsys for macOS so
## unix.Stat_t field-type regressions fail in the default fast test path.
test-fsys-darwin-compile:
	@tmp=$$(mktemp -d); \
	trap 'rm -rf "$$tmp"' EXIT; \
	$(TEST_ENV) GOOS=darwin GOARCH=arm64 go test -c -o "$$tmp/fsys.test" ./internal/fsys

## test-cmd-gc-process: run the full non-short cmd/gc suite, including the
## process-backed lifecycle coverage routed out of the default fast loop
test-cmd-gc-process:
	$(TEST_ENV) GC_FAST_UNIT=0 scripts/go-test-observable test-cmd-gc-process -- -timeout 25m ./cmd/gc

CMD_GC_PROCESS_SHARD ?= 1
CMD_GC_PROCESS_TOTAL ?= 6
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

## test-acceptance: run acceptance tests (Tier A — fast, <5 min, every PR).
## ACCEPTANCE_TIMEOUT overrides the go-test timeout (defaults to 5m on
## Linux; Mac CI bumps it because launchd-mediated supervisor start is
## noticeably slower than systemd).
ACCEPTANCE_TIMEOUT ?= 5m
test-acceptance:
	$(TEST_ENV) go test -tags acceptance_a -timeout $(ACCEPTANCE_TIMEOUT) ./test/acceptance/...

## test-acceptance-b: run Tier B acceptance tests (lifecycle, ~5 min, nightly)
test-acceptance-b:
	$(TEST_ENV) go test -tags acceptance_b -timeout 10m -v ./test/acceptance/tier_b/...

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
UNIT_COVER_PKGS := $(shell go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v -e /session/tmux -e /beadstest)

## test-cover: run fast unit-test coverage without the integration-tagged package sweep
## The skipped cmd/gc process-backed scenarios remain covered by
## `make test-cmd-gc-process` locally and the CI `cmd/gc process suite` job.
test-cover: test-fsys-darwin-compile
	$(TEST_ENV) GC_FAST_UNIT=1 go test -timeout 8m -coverprofile=coverage.txt $(UNIT_COVER_PKGS)

## cover: run tests and show coverage report
cover: test-cover
	go tool cover -func=coverage.txt

## install-tools: install pinned golangci-lint + oapi-codegen
install-tools: $(GOLANGCI_LINT) install-oapi-codegen

$(GOLANGCI_LINT):
	@echo "Installing golangci-lint v$(GOLANGCI_LINT_VERSION)..."
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION)

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

## test-mcp-mail: run mcp_agent_mail live conformance test (auto-starts server)
test-mcp-mail:
	$(TEST_ENV) GC_TEST_MCP_MAIL=1 go test ./internal/mail/exec/ -run TestMCPMailConformanceLive -v -count=1

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

## docs-dev: run the Mintlify docs locally
docs-dev:
	./mint.sh dev

## dashboard-build: regenerate SPA types + compile the dist bundle
dashboard-build:
	cd cmd/gc/dashboard/web && npm ci --silent && npm run gen && npm run build

## dashboard-dev: Vite dev server (HMR) for SPA iteration
dashboard-dev:
	cd cmd/gc/dashboard/web && npm run dev

## dashboard-check: typecheck + build the SPA, then go test the static handler
dashboard-check: dashboard-build
	cd cmd/gc/dashboard/web && npm run typecheck
	$(TEST_ENV) go test ./cmd/gc/dashboard/...

## dashboard-smoke: serve the built SPA bundle via Vite preview and verify it responds
dashboard-smoke: dashboard-build
	@PORT=$$(python3 -c 'import socket; sock = socket.socket(); sock.bind(("127.0.0.1", 0)); print(sock.getsockname()[1]); sock.close()'); \
	LOG=$$(mktemp); \
	( cd cmd/gc/dashboard/web && exec npm run preview -- --host 127.0.0.1 --strictPort --port $$PORT >"$$LOG" 2>&1 ) & \
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

## dashboard-ci: rebuild the SPA bundle and fail if the tracked dist/ is stale.
## Used by CI to enforce that cmd/gc/dashboard/web/dist/ matches the source.
dashboard-ci: dashboard-check
	@if ! git diff --quiet -- cmd/gc/dashboard/web/dist; then \
		echo "ERROR: cmd/gc/dashboard/web/dist/ is stale — run 'make dashboard-build' and commit." >&2; \
		git --no-pager diff --stat -- cmd/gc/dashboard/web/dist; \
		exit 1; \
	fi

## spec-ci: regenerate the OpenAPI spec + generated Go client, fail on drift.
## Used by CI to enforce that internal/api/openapi.json, docs/schema/openapi.{json,txt},
## docs/schema/events.{json,txt}, and internal/api/genclient/client_gen.go are
## all in lock-step with Huma.
spec-ci: install-oapi-codegen
	go run ./cmd/genspec
	go generate ./internal/api/genclient
	@if ! git diff --quiet -- internal/api/openapi.json docs/schema/openapi.json docs/schema/openapi.txt docs/schema/events.json docs/schema/events.txt internal/api/genclient/client_gen.go; then \
		echo "ERROR: spec/client artifacts drifted — run 'make spec-ci' locally and commit." >&2; \
		git --no-pager diff --stat -- internal/api/openapi.json docs/schema/openapi.json docs/schema/openapi.txt docs/schema/events.json docs/schema/events.txt internal/api/genclient/client_gen.go; \
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
