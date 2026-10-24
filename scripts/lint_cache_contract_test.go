package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// golangci-lint's result cache is keyed by module-relative content, but a
// cached issue keeps the absolute path of the checkout that produced it. A
// cache shared across sibling or throwaway checkouts therefore replays paths
// into a tree that may no longer exist, and once it is gone the nolint and
// generated-file processors cannot read the source: every suppressed finding
// leaks and a clean tree lints red. The Makefile keeps the cache inside the
// checkout unless the caller chooses one.
func TestLintCacheDefaultsToTheCheckout(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	const cacheDefault = "GOLANGCI_LINT_CACHE ?= $(CURDIR)/.cache/golangci-lint"
	if !strings.Contains(string(makefile), cacheDefault) {
		t.Fatalf("Makefile must default GOLANGCI_LINT_CACHE to a per-checkout directory: want %q", cacheDefault)
	}
	const lintEnv = `LINT_ENV = GOFLAGS="$(QUALITY_GATE_GOFLAGS)" GOMEMLIMIT=$(LINT_GOMEMLIMIT) GOLANGCI_LINT_CACHE="$(GOLANGCI_LINT_CACHE)"`
	if !strings.Contains(string(makefile), lintEnv) {
		t.Fatalf("Makefile must bind GOLANGCI_LINT_CACHE through LINT_ENV: want %q", lintEnv)
	}
	for target, want := range map[string]string{
		"lint-golangci":     `$(LINT_ENV)`,
		"fmt-check":         `$(LINT_ENV)`,
		"fmt-check-changed": `$(LINT_ENV)`,
		"fmt":               `GOLANGCI_LINT_CACHE="$(GOLANGCI_LINT_CACHE)"`,
	} {
		t.Run(target, func(t *testing.T) {
			if body := makeTargetBody(t, string(makefile), target); !strings.Contains(body, want) {
				t.Fatalf("%s must scope GOLANGCI_LINT_CACHE to its subprocess tree", target)
			}
		})
	}
}

// Two checkouts of the same content must not share a cache by default, and an
// explicit GOLANGCI_LINT_CACHE still wins so CI and deliberate sharing keep
// working.
func TestLintCacheIsScopedPerCheckout(t *testing.T) {
	fixture := newPRStaticScopeFixture(t, map[string]string{
		"example.go": "package example\n\nfunc Value() int { return 1 }\n",
	})
	cacheLog := filepath.Join(t.TempDir(), "cache.log")
	recordingLint := filepath.Join(t.TempDir(), "golangci-lint")
	writeExecutable(t, recordingLint, `#!/bin/sh
set -eu
printf '%s\n' "${GOLANGCI_LINT_CACHE-unset}" >> "$LINT_CACHE_LOG"
`)

	run := func(t *testing.T, extraEnv ...string) string {
		t.Helper()
		if err := os.WriteFile(cacheLog, nil, 0o644); err != nil {
			t.Fatalf("reset cache log: %v", err)
		}
		cmd := makeCommand(
			"--no-print-directory",
			"-f", fixture.productionMakefile,
			"GOLANGCI_LINT="+recordingLint,
			"lint-golangci",
		)
		cmd.Dir = fixture.repoRoot
		env := make([]string, 0, len(os.Environ())+2)
		for _, entry := range fixture.commandEnv() {
			if strings.HasPrefix(entry, "GOLANGCI_LINT_CACHE=") {
				continue
			}
			env = append(env, entry)
		}
		cmd.Env = append(append(env, "LINT_CACHE_LOG="+cacheLog), extraEnv...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("lint-golangci failed: %v\n%s", err, output)
		}
		got, err := os.ReadFile(cacheLog)
		if err != nil {
			t.Fatalf("read cache log: %v", err)
		}
		return strings.TrimSpace(string(got))
	}

	t.Run("default lives inside the checkout", func(t *testing.T) {
		want := filepath.Join(fixture.repoRoot, ".cache", "golangci-lint")
		if got := run(t); got != want {
			t.Fatalf("GOLANGCI_LINT_CACHE = %q, want %q", got, want)
		}
	})
	t.Run("explicit cache wins", func(t *testing.T) {
		want := filepath.Join(t.TempDir(), "shared-cache")
		if got := run(t, "GOLANGCI_LINT_CACHE="+want); got != want {
			t.Fatalf("GOLANGCI_LINT_CACHE = %q, want the caller's %q", got, want)
		}
	})
}
