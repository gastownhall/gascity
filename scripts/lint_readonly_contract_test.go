package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLintUsesReadonlyModuleDownloads(t *testing.T) {
	configPath := filepath.Join(repoRoot(t), ".golangci.yml")
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read %s: %v", configPath, err)
	}

	var config struct {
		Run struct {
			ModulesDownloadMode string `yaml:"modules-download-mode"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal(body, &config); err != nil {
		t.Fatalf("parse %s: %v", configPath, err)
	}
	if config.Run.ModulesDownloadMode != "readonly" {
		t.Fatalf("run.modules-download-mode = %q, want readonly", config.Run.ModulesDownloadMode)
	}

	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	const readonlyGOFlags = "QUALITY_GATE_GOFLAGS = $$(go env GOFLAGS | sed -E 's/(^|[[:space:]])-mod=[^[:space:]]+//g') -mod=readonly"
	if !strings.Contains(string(makefile), readonlyGOFlags) {
		t.Fatalf("Makefile must derive QUALITY_GATE_GOFLAGS from effective GOFLAGS")
	}
	for target, wantGOFLAGS := range map[string]string{
		"lint-full":     `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"lint-new":      `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"lint-changed":  `export GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"lint-affected": `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
	} {
		t.Run(target, func(t *testing.T) {
			body := makeTargetBody(t, string(makefile), target)
			for _, override := range []string{"--config", "--no-config"} {
				if strings.Contains(body, override) {
					t.Fatalf("%s overrides shared lint configuration with %q", target, override)
				}
			}
			if strings.Contains(body, "--modules-download-mode") {
				t.Fatalf("%s must not rely on a lint CLI module-mode override", target)
			}
			if !strings.Contains(body, wantGOFLAGS) {
				t.Fatalf("%s must scope QUALITY_GATE_GOFLAGS to its subprocess tree", target)
			}
		})
	}
}

func TestQualityGateTargetsUseReadonlyModuleDownloads(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	const readonlyGOFlags = "QUALITY_GATE_GOFLAGS = $$(go env GOFLAGS | sed -E 's/(^|[[:space:]])-mod=[^[:space:]]+//g') -mod=readonly"
	if !strings.Contains(string(makefile), readonlyGOFlags) {
		t.Fatalf("Makefile must normalize QUALITY_GATE_GOFLAGS from effective GOFLAGS")
	}

	for target, wantGOFLAGS := range map[string]string{
		"fmt-check":                `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"fmt-check-changed":        `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"vet":                      `GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"test":                     `$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
		"test-fsys-darwin-compile": `$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)"`,
	} {
		t.Run(target, func(t *testing.T) {
			if body := makeTargetBody(t, string(makefile), target); !strings.Contains(body, wantGOFLAGS) {
				t.Fatalf("%s must scope QUALITY_GATE_GOFLAGS to its subprocess tree", target)
			}
		})
	}
}

func TestFmtCheckDoesNotModifyGoSumWithAmbientWritableModuleMode(t *testing.T) {
	fixture := newPRStaticScopeFixture(t, map[string]string{
		"example.go": "package example\n\nfunc Value() int { return 1 }\n",
	})
	goSumPath := filepath.Join(fixture.repoRoot, "go.sum")
	writeTestFile(t, goSumPath, "example.com/dependency v1.0.0 h1:before\n")

	mutatingLint := filepath.Join(t.TempDir(), "golangci-lint")
	writeExecutable(t, mutatingLint, `#!/bin/sh
set -eu
case "${GOFLAGS-}" in
  *-tags=quality*) ;;
  *)
    echo "formatter lost non-module GOFLAGS" >&2
    exit 1
    ;;
esac
case "${GOFLAGS-}" in
  *-mod=readonly*) exit 0 ;;
esac
printf '%s\n' 'unexpected writable formatter resolution' >> go.sum
`)

	cmd := makeCommand(
		"--no-print-directory",
		"-f", fixture.productionMakefile,
		"GOLANGCI_LINT="+mutatingLint,
		"fmt-check",
	)
	cmd.Dir = fixture.repoRoot
	env := fixture.commandEnv()
	for index, entry := range env {
		if strings.HasPrefix(entry, "GOFLAGS=") {
			env[index] = "GOFLAGS=-tags=quality -mod=mod"
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fmt-check failed: %v\n%s", err, output)
	}

	got, err := os.ReadFile(goSumPath)
	if err != nil {
		t.Fatalf("read go.sum after fmt-check: %v", err)
	}
	const want = "example.com/dependency v1.0.0 h1:before\n"
	if string(got) != want {
		t.Fatalf("fmt-check modified go.sum under ambient -mod=mod:\nwant: %q\n got: %q", want, got)
	}
}

func TestLintChangedFailsClosedWhenReadonlyMetadataIsStale(t *testing.T) {
	fixture := newPRStaticScopeFixture(t, map[string]string{
		"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
	})
	writeTestFile(t, filepath.Join(fixture.repoRoot, "go.sum"), "example.com/dependency v1.0.0 h1:before\n")
	writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")

	goTool := filepath.Join(t.TempDir(), "go")
	writeExecutable(t, goTool, `#!/bin/sh
set -eu
case "${1-}" in
  env)
    if [ "${2-}" = "GOFLAGS" ]; then
      printf '%s\n' "${GOFLAGS-}"
    fi
    exit 0
    ;;
  list)
    case "${GOFLAGS-}" in
      *-mod=readonly*)
        echo "go: updates to go.sum needed; disabled by -mod=readonly" >&2
        exit 1
        ;;
    esac
    echo "unexpected writable module resolution" >> go.sum
    exit 0
    ;;
esac
echo "unexpected go invocation: $*" >&2
exit 1
`)

	before, err := os.ReadFile(filepath.Join(fixture.repoRoot, "go.sum"))
	if err != nil {
		t.Fatalf("read go.sum before lint: %v", err)
	}
	fixture.resetCalls(t)
	cmd := makeCommand(
		"--no-print-directory",
		"-f", fixture.productionMakefile,
		"GOLANGCI_LINT="+fixture.fakeLint,
		"LINT_CHANGED_SCOPE=tracked",
		"LINT_CHANGED_REF=HEAD",
		"LINT_FLAGS=",
		"lint-changed",
	)
	cmd.Dir = fixture.repoRoot
	env := fixture.commandEnv()
	for index, entry := range env {
		if strings.HasPrefix(entry, "GOFLAGS=") {
			env[index] = "GOFLAGS=-mod=mod"
		}
	}
	env = append(env, "PATH="+filepath.Dir(goTool)+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("lint-changed succeeded with stale readonly metadata:\n%s", output)
	}
	if !strings.Contains(string(output), "updates to go.sum needed") {
		t.Fatalf("lint-changed error did not preserve the module failure:\n%s", output)
	}
	after, err := os.ReadFile(filepath.Join(fixture.repoRoot, "go.sum"))
	if err != nil {
		t.Fatalf("read go.sum after lint: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("lint-changed modified go.sum under ambient -mod=mod:\nbefore: %q\nafter:  %q", before, after)
	}
	fixture.requireNoCalls(t)
}

func makeTargetBody(t *testing.T, makefile, target string) string {
	t.Helper()
	prefix := target + ":"
	start := strings.Index(makefile, "\n"+prefix)
	if start >= 0 {
		start++
	} else if strings.HasPrefix(makefile, prefix) {
		start = 0
	}
	if start < 0 {
		t.Fatalf("Makefile has no %s target", target)
	}
	body := makefile[start:]
	if next := strings.Index(body, "\n## "); next >= 0 {
		body = body[:next]
	}
	return body
}
