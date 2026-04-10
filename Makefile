GOLANGCI_LINT_VERSION := 2.9.0

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

.PHONY: build check check-all check-bd check-docker check-docs check-dolt lint fmt-check fmt vet test test-acceptance test-acceptance-b test-acceptance-c test-acceptance-all test-tutorial-regression test-tutorial test-integration test-mcp-mail test-docker test-k8s test-worker-core test-worker-core-phase1 test-worker-core-phase2 test-worker-core-phase3 test-worker-inference setup-worker-inference test-cover cover install install-tools install-buildx setup clean generate check-schema docker-base docker-agent docker-controller docs-dev

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

## test: run unit tests (skip integration tests tagged with //go:build integration)
test:
	go test ./...

## test-acceptance: run acceptance tests (Tier A — fast, <5 min, every PR)
test-acceptance:
	go test -tags acceptance_a -timeout 5m ./test/acceptance/...

## test-acceptance-b: run Tier B acceptance tests (lifecycle, ~5 min, nightly)
test-acceptance-b:
	go test -tags acceptance_b -timeout 10m -v ./test/acceptance/tier_b/...

## test-acceptance-c: run Tier C acceptance tests (real inference, ~10 min, manual/nightly)
test-acceptance-c:
	go test -tags acceptance_c -timeout 15m -v ./test/acceptance/tier_c/...

## test-acceptance-all: run all acceptance tiers
test-acceptance-all: test-acceptance test-acceptance-b test-acceptance-c

## test-tutorial-regression: run manual tutorial regression tests (requires tmux, bd)
test-tutorial-regression:
	go test -tags 'integration acceptance' -timeout 10m -v -run TestTutorialRegression ./test/integration/

## test-integration: run all tests including integration (tmux, etc.)
test-integration:
	go test -tags integration -timeout 8m ./...


## test-tutorial: run tutorial acceptance tests (requires tmux, dolt, bd, claude authed)
## These exercise the full tutorial flow with real inference — run before each release.
test-tutorial:
	go test -tags 'integration acceptance' -timeout 10m -v ./test/integration/ -run 'TestTutorial'

## test-tutorial-regression: alias for test-tutorial
test-tutorial-regression: test-tutorial

## check-docs: verify docs sync tests
check-docs:
	go test ./test/docsync

# Packages for coverage — exclude noise:
#   session/tmux: integration-test-only, not meaningful for unit coverage
#   beadstest: conformance helper, runs under internal/beads coverage
COVER_PKGS := $(shell go list ./... | grep -v -e /session/tmux -e /beadstest)

## test-cover: run all tests with coverage output (excludes tmux)
test-cover:
	go test -tags integration -timeout 8m -coverprofile=coverage.txt $(COVER_PKGS)

## cover: run tests and show coverage report
cover: test-cover
	go tool cover -func=coverage.txt

## install-tools: install pinned golangci-lint
install-tools: $(GOLANGCI_LINT)

$(GOLANGCI_LINT):
	@echo "Installing golangci-lint v$(GOLANGCI_LINT_VERSION)..."
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | \
		sh -s -- -b $(BIN_DIR) v$(GOLANGCI_LINT_VERSION)

## install-buildx: install docker buildx plugin
install-buildx:
	@mkdir -p $(HOME)/.docker/cli-plugins
	curl -sSfL "https://github.com/docker/buildx/releases/download/v0.21.2/buildx-v0.21.2.$$(go env GOOS)-$$(go env GOARCH)" \
		-o $(HOME)/.docker/cli-plugins/docker-buildx
	chmod +x $(HOME)/.docker/cli-plugins/docker-buildx
	@echo "Installed docker-buildx v0.21.2"

## test-mcp-mail: run mcp_agent_mail live conformance test (auto-starts server)
test-mcp-mail:
	GC_TEST_MCP_MAIL=1 go test ./internal/mail/exec/ -run TestMCPMailConformanceLive -v -count=1

## test-docker: run Docker session provider integration tests
test-docker: check-docker
	./scripts/test-docker-session

## test-k8s: run K8s session provider conformance tests
test-k8s:
	go test -tags integration ./test/integration/ -run TestK8sSessionConformance -v -count=1

## test-worker-core-phase1: run phase-1 worker transcript/continuation conformance tests (set PROFILE=claude/tmux-cli|codex/tmux-cli|gemini/tmux-cli to narrow)
test-worker-core-phase1:
	PROFILE="$(PROFILE)" go test ./internal/worker/workertest -run '^TestPhase1' -count=1 -v

## test-worker-core: alias for the current phase-1 worker-core conformance suite
test-worker-core: test-worker-core-phase1

## test-worker-core-phase2: run phase-2 worker-core conformance tests (set PROFILE=claude/tmux-cli|codex/tmux-cli|gemini/tmux-cli to narrow)
test-worker-core-phase2:
	PROFILE="$(PROFILE)" go test ./internal/worker/workertest -run '^TestPhase2' -count=1 -v
	go test ./internal/runtime/tmux -run '^TestPhase2' -count=1 -v

## test-worker-core-phase3: run phase-3 worker-core conformance tests (set PROFILE=claude/tmux-cli|codex/tmux-cli|gemini/tmux-cli to narrow)
test-worker-core-phase3:
	PROFILE="$(PROFILE)" go test ./internal/worker/workertest -run '^TestPhase3' -count=1 -v
	PROFILE="$(PROFILE)" go test ./cmd/gc -run '^TestPhase3' -count=1 -v

## setup-worker-inference: install the live provider CLI for PROFILE=claude/tmux-cli|codex/tmux-cli|gemini/tmux-cli
setup-worker-inference:
	@test -n "$(PROFILE)" || (echo "Error: PROFILE is required (claude/tmux-cli|codex/tmux-cli|gemini/tmux-cli)" && exit 1)
	python3 scripts/worker_inference_setup.py install --profile "$(PROFILE)"

## test-worker-inference: run the live WorkerInference smoke suite (set PROFILE=claude/tmux-cli|codex/tmux-cli|gemini/tmux-cli)
test-worker-inference:
	@test -n "$(PROFILE)" || (echo "Error: PROFILE is required (claude/tmux-cli|codex/tmux-cli|gemini/tmux-cli)" && exit 1)
	PROFILE="$(PROFILE)" go test -tags acceptance_c -timeout 30m -count=1 -run '^TestWorkerInference' -v ./test/acceptance/worker_inference/...

## setup: install tools and git hooks
setup: install-tools
	git config core.hooksPath .githooks
	@echo "Done. Tools installed, pre-commit hook active."

## docs-dev: run the Mintlify docs locally
docs-dev:
	cd docs && npx --yes mint@latest dev

## docker-base: build base image with system dependencies (~2.5 min, rebuild rarely)
docker-base: check-docker
	docker build -f contrib/k8s/Dockerfile.base -t gc-agent-base:latest .

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
