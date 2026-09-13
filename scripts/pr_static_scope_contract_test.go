package scripts_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestChangedStaticTargetsScopeLintAndFormattingToTheDiff(t *testing.T) {
	t.Run("Go build-input suffix contract", func(t *testing.T) {
		want := []string{
			".go", ".c", ".cc", ".cpp", ".cxx", ".m", ".h", ".hh", ".hpp", ".hxx",
			".f", ".F", ".for", ".f90", ".s", ".S", ".sx", ".swig", ".swigcxx", ".syso",
		}
		selector := filepath.Join(repoRoot(t), "scripts", "ci-static-select")
		code := `import runpy
import sys

module = runpy.run_path(sys.argv[1], run_name="ci_static_select_contract")
want = frozenset(sys.argv[2:])
got = module["GO_BUILD_INPUT_SUFFIXES"]
classify = module["is_go_build_input"]
if got != want:
    raise SystemExit(f"build-input suffixes = {sorted(got)!r}, want {sorted(want)!r}")
if not all(classify("pkg/input" + suffix) for suffix in want):
    raise SystemExit("a required build-input suffix was not classified")
if any(classify(path) for path in ("README.md", "pkg/input.go.bak", "pkg/header.H")):
    raise SystemExit("a non-build input was classified")
`
		args := append([]string{"-c", code, selector}, want...)
		cmd := testCommand("python3", args...)
		cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("verify Go build-input suffix contract: %v\n%s", err, output)
		}
	})

	t.Run("changed Go file", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go":     "package alpha\n\nfunc Value() int { return 1 }\n",
			"alpha/unchanged.go": "package alpha\n\nfunc Unchanged() {}\n",
			"beta/beta.go":       "package beta\n\nfunc Value() int { return 1 }\n",
			"consumer/consumer.go": `package consumer

import "example.com/static-scope/alpha"

func Value() int { return alpha.Value() }
`,
			"README.md": "baseline\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-changed"); err != nil {
			t.Errorf("lint-changed failed for one changed Go file: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./alpha"})
		fixture.requireGoCalls(t)

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for one changed Go file: %v\n%s", err, output)
		}
		fixture.requireSingleRunCallWithUnorderedTail(t, "./alpha", "./consumer")
		fixture.requireGoCalls(t, []string{"vet", "./alpha", "./consumer"})

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("fmt-check-changed"); err != nil {
			t.Errorf("fmt-check-changed failed for one changed Go file: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"fmt", "--diff", "--", "alpha/alpha.go"})
		fixture.requireGoCalls(t)
	})

	t.Run("transitive and test-only reverse dependents", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
			"middle/middle.go": `package middle

import "example.com/static-scope/alpha"

func Value() int { return alpha.Value() }
`,
			"consumer/consumer.go": `package consumer

import "example.com/static-scope/middle"

func Value() int { return middle.Value() }
`,
			"internaltest/internaltest.go": "package internaltest\n\nfunc Value() int { return 1 }\n",
			"internaltest/internaltest_test.go": `package internaltest

import (
	"testing"

	"example.com/static-scope/alpha"
)

func TestValue(t *testing.T) { _ = alpha.Value() }
`,
			"externaltest/externaltest.go": "package externaltest\n\nfunc Value() int { return 1 }\n",
			"externaltest/externaltest_test.go": `package externaltest_test

import (
	"testing"

	"example.com/static-scope/alpha"
)

func TestValue(t *testing.T) { _ = alpha.Value() }
`,
			"unrelated/unrelated.go": "package unrelated\n\nfunc Value() int { return 1 }\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for reverse-dependent graph: %v\n%s", err, output)
		}
		fixture.requireSingleRunCallWithUnorderedTail(t,
			"./alpha",
			"./consumer",
			"./externaltest",
			"./internaltest",
			"./middle",
		)
		fixture.requireGoCalls(t, []string{
			"vet",
			"./alpha",
			"./consumer",
			"./externaltest",
			"./internaltest",
			"./middle",
		})
	})

	t.Run("broken package graph falls back to full lint", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
			"broken/broken.go": `package broken

import _ "example.com/static-scope/missing"
`,
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected did not fail closed for a broken package graph: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./..."})
		fixture.requireGoCalls(t, []string{"vet", "./..."})
	})

	t.Run("deleted Go file", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/delete.go": "package alpha\n\nfunc Deleted() {}\n",
			"alpha/keep.go":   "package alpha\n\nfunc Keep() {}\n",
		})
		if err := os.Remove(filepath.Join(fixture.repoRoot, "alpha", "delete.go")); err != nil {
			t.Fatalf("delete tracked Go file: %v", err)
		}

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for a deleted Go file: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./alpha"})
		fixture.requireGoCalls(t, []string{"vet", "./alpha"})

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("fmt-check-changed"); err != nil {
			t.Errorf("fmt-check-changed failed for a deleted Go file: %v\n%s", err, output)
		}
		fixture.requireNoCalls(t)
	})

	t.Run("deleted nested Go file beneath ancestor embed falls back to full", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": `package alpha

import "embed"

//go:embed child/**
var Data embed.FS
`,
			"alpha/child/delete.go": "package child\n\nfunc Delete() {}\n",
			"alpha/child/keep.go":   "package child\n\nfunc Keep() {}\n",
		})
		if err := os.Remove(filepath.Join(fixture.repoRoot, "alpha", "child", "delete.go")); err != nil {
			t.Fatalf("delete nested embedded Go file: %v", err)
		}

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected did not fail closed for a deleted nested embedded Go file: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./..."})
		fixture.requireGoCalls(t, []string{"vet", "./..."})
	})

	t.Run("cross-package rename with a spaced file name", func(t *testing.T) {
		const movedBody = `
func Moved() int {
	total := 0
	for i := 0; i < 10; i++ {
		total += i
	}
	return total
}
`
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"oldpkg/keep.go":       "package oldpkg\n\nfunc Keep() {}\n",
			"oldpkg/moved file.go": "package oldpkg\n" + movedBody,
		})
		newPath := filepath.Join(fixture.repoRoot, "newpkg", "moved file.go")
		if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
			t.Fatalf("create renamed package: %v", err)
		}
		if err := os.Rename(filepath.Join(fixture.repoRoot, "oldpkg", "moved file.go"), newPath); err != nil {
			t.Fatalf("rename tracked Go file across packages: %v", err)
		}
		writeTestFile(t, newPath, "package newpkg\n"+movedBody)
		runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git add -A")
		status := runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git diff --name-status -M HEAD --")
		if !strings.Contains(status, "R") || !strings.Contains(status, "oldpkg/moved file.go") || !strings.Contains(status, "newpkg/moved file.go") {
			t.Fatalf("fixture is not an across-package Git rename:\n%s", status)
		}

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for a cross-package rename: %v\n%s", err, output)
		}
		fixture.requireSingleRunCallWithUnorderedTail(t, "./newpkg", "./oldpkg")
		fixture.requireGoCalls(t, []string{"vet", "./newpkg", "./oldpkg"})

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("fmt-check-changed"); err != nil {
			t.Errorf("fmt-check-changed failed for a cross-package rename: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"fmt", "--diff", "--", "newpkg/moved file.go"})
		fixture.requireGoCalls(t)
	})

	t.Run("newline in changed file name", func(t *testing.T) {
		const name = "alpha/line\nbreak.go"
		fixture := newPRStaticScopeFixture(t, map[string]string{
			name: "package alpha\n\nfunc Value() int { return 1 }\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, name), "package alpha\n\nfunc Value() int { return 2 }\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("fmt-check-changed"); err != nil {
			t.Errorf("fmt-check-changed failed for a newline-containing file name: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"fmt", "--diff", "--", name})
		fixture.requireGoCalls(t)
	})

	t.Run("invalid ref falls back to full static checks", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTargetWithRef("lint-affected", "refs/heads/missing-static-base"); err != nil {
			t.Errorf("lint-affected did not fail closed for an invalid ref: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./..."})
		fixture.requireGoCalls(t, []string{"vet", "./..."})

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTargetWithRef("fmt-check-changed", "refs/heads/missing-static-base"); err != nil {
			t.Errorf("fmt-check-changed did not fail closed for an invalid ref: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"fmt", "--diff", "./..."})
		fixture.requireGoCalls(t)
	})

	t.Run("affected vet checks unchanged generated reverse dependent", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": `package alpha

func Printf(string, ...any) {}
`,
			"consumer/generated.go": `// Code generated by static-scope fixture. DO NOT EDIT.

package consumer

import "example.com/static-scope/alpha"

func Use() { alpha.Printf("%d", "not-an-int") }
`,
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "alpha.go"), `package alpha

import "fmt"

func Printf(format string, args ...any) { fmt.Printf(format, args...) }
`)

		fixture.resetCalls(t)
		output, err := fixture.runMakeTargetWithGo("lint-affected", fixture.realGo)
		if err == nil {
			t.Fatalf("lint-affected passed despite a new vet diagnostic in an unchanged generated reverse dependent:\n%s", output)
		}
		for _, marker := range []string{"consumer/generated.go", "format %d"} {
			if !strings.Contains(output, marker) {
				t.Errorf("affected vet output missing %q:\n%s", marker, output)
			}
		}
		fixture.requireSingleRunCallWithUnorderedTail(t, "./alpha", "./consumer")
		fixture.requireGoCalls(t)
	})

	t.Run("assembly-only diff selects its package", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value()\n",
			"alpha/value.s":  "#include \"textflag.h\"\n\nTEXT ·Value(SB), NOSPLIT, $0-0\n\tRET\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "value.s"), "#include \"textflag.h\"\n\n// changed\nTEXT ·Value(SB), NOSPLIT, $0-0\n\tRET\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for an assembly-only diff: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./alpha"})
		fixture.requireGoCalls(t, []string{"vet", "./alpha"})
	})

	t.Run("native include fragment selects its package and reverse dependents", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value()\n",
			"alpha/value.s":  "#include \"../shared/defs.inc\"\n\nTEXT ·Value(SB), $0-0\n\tRET\n",
			"consumer/consumer.go": `package consumer

import "example.com/static-scope/alpha"

func Value() { alpha.Value() }
`,
			"unrelated/unrelated.go": "package unrelated\n",
			"shared/defs.inc":        "#define VALUE 1\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "shared", "defs.inc"), "#define VALUE 2\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for a native include fragment: %v\n%s", err, output)
		}
		fixture.requireSingleRunCallWithUnorderedTail(t, "./alpha", "./consumer")
		fixture.requireGoCalls(t, []string{"vet", "./alpha", "./consumer"})
	})

	for _, testCase := range []struct {
		name          string
		sharedPackage bool
		wantPackages  []string
	}{
		{
			name:          "recognized shared native header beside a Go package",
			sharedPackage: true,
			wantPackages:  []string{"./alpha", "./consumer", "./shared"},
		},
		{
			name:         "recognized package-less shared native header",
			wantPackages: []string{"./alpha", "./consumer"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			files := map[string]string{
				"alpha/alpha.go": "package alpha\n\nfunc Value()\n",
				"alpha/value.s":  "#include \"../shared/defs.h\"\n\nTEXT ·Value(SB), $0-0\n\tRET\n",
				"consumer/consumer.go": `package consumer

import "example.com/static-scope/alpha"

func Value() { alpha.Value() }
`,
				"shared/defs.h":          "#define VALUE 1\n",
				"unrelated/unrelated.go": "package unrelated\n",
			}
			if testCase.sharedPackage {
				files["shared/shared.go"] = "package shared\n"
			}
			fixture := newPRStaticScopeFixture(t, files)
			writeTestFile(t, filepath.Join(fixture.repoRoot, "shared", "defs.h"), "#define VALUE 2\n")

			fixture.resetCalls(t)
			if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
				t.Errorf("lint-affected failed for a recognized shared native header: %v\n%s", err, output)
			}
			fixture.requireSingleRunCallWithUnorderedTail(t, testCase.wantPackages...)
			fixture.requireGoCalls(t, append([]string{"vet"}, testCase.wantPackages...))
		})
	}

	t.Run("embedded file diff selects its package", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": `package alpha

import _ "embed"

//go:embed data.txt
var Data string
`,
			"alpha/data.txt": "before\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "data.txt"), "after\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for an embedded-file diff: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./alpha"})
		fixture.requireGoCalls(t, []string{"vet", "./alpha"})
	})

	t.Run("package discovery requests read-only module mode", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")

		strictGo := filepath.Join(t.TempDir(), "go")
		writeExecutable(t, strictGo, `#!/bin/sh
set -eu
: "${STATIC_SCOPE_GO_LOG:?}"
: "${STATIC_SCOPE_REAL_GO:?}"
if [ "${1-}" = "list" ]; then
  readonly=0
  for arg in "$@"; do
    if [ "$arg" = "-mod=readonly" ]; then
      readonly=1
    fi
  done
  if [ "$readonly" -ne 1 ]; then
    echo "go list did not request -mod=readonly" >&2
    exit 97
  fi
  exec "$STATIC_SCOPE_REAL_GO" "$@"
fi
if [ "${1-}" = "vet" ]; then
  printf 'CALL\000' >> "$STATIC_SCOPE_GO_LOG"
  for arg in "$@"; do
    printf 'ARG\000%s\000' "$arg" >> "$STATIC_SCOPE_GO_LOG"
  done
  printf 'END\000' >> "$STATIC_SCOPE_GO_LOG"
  exit 0
fi
exec "$STATIC_SCOPE_REAL_GO" "$@"
`)

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTargetWithGo("lint-affected", strictGo); err != nil {
			t.Errorf("lint-affected failed with a read-only go list guard: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./alpha"})
		fixture.requireGoCalls(t, []string{"vet", "./alpha"})
	})

	for _, testPackage := range []struct {
		name        string
		packageName string
		fileName    string
		dataName    string
	}{
		{name: "internal test embed", packageName: "alpha", fileName: "alpha_internal_test.go", dataName: "internal.txt"},
		{name: "external test embed", packageName: "alpha_test", fileName: "alpha_external_test.go", dataName: "external.txt"},
	} {
		t.Run(testPackage.name, func(t *testing.T) {
			fixture := newPRStaticScopeFixture(t, map[string]string{
				"alpha/alpha.go": "package alpha\n",
				filepath.Join("alpha", testPackage.fileName): "package " + testPackage.packageName + `

import _ "embed"

//go:embed testdata/` + testPackage.dataName + `
var data string
`,
				filepath.Join("alpha", "testdata", testPackage.dataName): "before\n",
			})
			writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "testdata", testPackage.dataName), "after\n")

			fixture.resetCalls(t)
			if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
				t.Errorf("lint-affected failed for a %s diff: %v\n%s", testPackage.name, err, output)
			}
			fixture.requireCalls(t, []string{"run", "./alpha"})
			fixture.requireGoCalls(t, []string{"vet", "./alpha"})
		})
	}

	t.Run("embedded file selects every owning package", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": `package alpha

import _ "embed"

//go:embed child/data.txt
var Data string
`,
			"alpha/child/child.go": `package child

import _ "embed"

//go:embed data.txt
var Data string
`,
			"alpha/child/data.txt": "before\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "alpha", "child", "data.txt"), "after\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for a multiply owned embedded file: %v\n%s", err, output)
		}
		fixture.requireSingleRunCallWithUnorderedTail(t, "./alpha", "./alpha/child")
		fixture.requireGoCalls(t, []string{"vet", "./alpha", "./alpha/child"})
	})

	t.Run("deleted required embedded file falls back to full", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": `package alpha

import _ "embed"

//go:embed data.txt
var Data string
`,
			"alpha/data.txt": "before\n",
		})
		if err := os.Remove(filepath.Join(fixture.repoRoot, "alpha", "data.txt")); err != nil {
			t.Fatalf("delete embedded fixture: %v", err)
		}

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected did not fail closed for a missing embedded file: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./..."})
		fixture.requireGoCalls(t, []string{"vet", "./..."})
	})

	t.Run("deleted embedded glob member falls back to full", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": `package alpha

import "embed"

//go:embed data/*.txt
var Data embed.FS
`,
			"alpha/data/first.txt":  "first\n",
			"alpha/data/second.txt": "second\n",
		})
		if err := os.Remove(filepath.Join(fixture.repoRoot, "alpha", "data", "first.txt")); err != nil {
			t.Fatalf("delete embedded glob member: %v", err)
		}

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected did not fail closed for a deleted embed glob member: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./..."})
		fixture.requireGoCalls(t, []string{"vet", "./..."})
	})

	t.Run("deleted recognized embedded glob member falls back before native shortcut", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": `package alpha

import "embed"

//go:embed data/*.h
var Data embed.FS
`,
			"alpha/data/first.h":  "#define FIRST 1\n",
			"alpha/data/second.h": "#define SECOND 2\n",
			"consumer/consumer.go": `package consumer

import "example.com/static-scope/alpha"

var Data = alpha.Data
`,
			"native/native.go": "package native\n\nfunc Value()\n",
			"native/value.s":   "TEXT ·Value(SB), $0-0\n\tRET\n",
		})
		if err := os.Remove(filepath.Join(fixture.repoRoot, "alpha", "data", "first.h")); err != nil {
			t.Fatalf("delete recognized embedded glob member: %v", err)
		}

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected did not fail closed before the native shortcut: %v\n%s", err, output)
		}
		fixture.requireCalls(t, []string{"run", "./..."})
		fixture.requireGoCalls(t, []string{"vet", "./..."})
	})

	t.Run("changed Go symlink is not formatted", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
		})
		outside := filepath.Join(t.TempDir(), "outside.go")
		writeTestFile(t, outside, "package outside\n")
		path := filepath.Join(fixture.repoRoot, "alpha", "alpha.go")
		if err := os.Remove(path); err != nil {
			t.Fatalf("replace Go file with symlink: %v", err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatalf("create Go symlink: %v", err)
		}

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("fmt-check-changed"); err != nil {
			t.Errorf("fmt-check-changed failed for a Go symlink: %v\n%s", err, output)
		}
		fixture.requireNoCalls(t)
	})

	t.Run("non-Go diff", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
			"README.md":      "baseline\n",
		})
		writeTestFile(t, filepath.Join(fixture.repoRoot, "README.md"), "documentation only\n")

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for a non-Go diff: %v\n%s", err, output)
		}
		fixture.requireNoCalls(t)

		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("fmt-check-changed"); err != nil {
			t.Errorf("fmt-check-changed failed for a non-Go diff: %v\n%s", err, output)
		}
		fixture.requireNoCalls(t)

		if err := os.Remove(filepath.Join(fixture.repoRoot, "README.md")); err != nil {
			t.Fatalf("delete non-Go fixture: %v", err)
		}
		fixture.resetCalls(t)
		if output, err := fixture.runMakeTarget("lint-affected"); err != nil {
			t.Errorf("lint-affected failed for a deleted non-build, non-embedded file: %v\n%s", err, output)
		}
		fixture.requireNoCalls(t)
	})
}

func TestCIStaticScopeClassifierFailsClosedOutsideValidatedPullRequestMerge(t *testing.T) {
	classifier := filepath.Join(repoRoot(t), "scripts", "ci-static-scope")
	body, err := os.ReadFile(classifier)
	if err != nil {
		t.Fatalf("read executable static-scope classifier: %v", err)
	}
	info, err := os.Stat(classifier)
	if err != nil {
		t.Fatalf("stat executable static-scope classifier: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("static-scope classifier mode = %o, want executable", info.Mode().Perm())
	}
	for _, protected := range []string{
		"go.mod",
		"go.sum",
		"go.work",
		"go.work.sum",
		".golangci.",
		"Makefile",
		".github/workflows/",
		".github/actions/",
		".githooks/",
		"vendor/",
		"scripts/cipolicy/",
		"scripts/ci-static-scope",
		"scripts/ci-static-select",
	} {
		if !strings.Contains(string(body), protected) {
			t.Errorf("static-scope protected paths must explicitly include %q", protected)
		}
	}
	for _, unsafeBase := range []string{"origin/main", "merge-base"} {
		if strings.Contains(string(body), unsafeBase) {
			t.Errorf("static-scope classifier uses %q instead of validating the exact synthetic-merge base parent", unsafeBase)
		}
	}

	t.Run("ordinary synthetic pull request merge", func(t *testing.T) {
		fixture, baseSHA := newSyntheticPRStaticScopeFixture(t, "")
		fixture.requireClassification(t, classifier, "pull_request", baseSHA, "changed")
	})

	t.Run("protected configuration paths", func(t *testing.T) {
		for _, protectedPath := range []string{
			"go.mod",
			"go.sum",
			".golangci.json",
			".golangci.toml",
			".golangci.yaml",
			".golangci.yml",
			".github/actions/static-scope/action.yml",
			".github/workflows/ci.yml",
			".githooks/pre-commit",
			"Makefile",
			"go.work",
			"go.work.sum",
			"scripts/cipolicy/policy.go",
			"vendor/example.com/dependency/file.go",
			"scripts/ci-static-scope",
			"scripts/ci-static-select",
		} {
			t.Run(strings.ReplaceAll(protectedPath, "/", "_"), func(t *testing.T) {
				fixture, baseSHA := newSyntheticPRStaticScopeFixture(t, protectedPath)
				fixture.requireClassification(t, classifier, "pull_request", baseSHA, "full")
			})
		}
	})

	t.Run("deleted protected path", func(t *testing.T) {
		fixture, baseSHA := newSyntheticPRStaticScopeFixtureWithMutation(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
			".golangci.yml":  "version: '2'\n",
		}, func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, ".golangci.yml")); err != nil {
				t.Fatalf("delete protected fixture: %v", err)
			}
		})
		fixture.requireClassification(t, classifier, "pull_request", baseSHA, "full")
	})

	t.Run("missing and wrong base", func(t *testing.T) {
		fixture, baseSHA := newSyntheticPRStaticScopeFixture(t, "")
		fixture.requireClassification(t, classifier, "pull_request", "", "full")
		fixture.requireClassification(t, classifier, "pull_request", strings.Repeat("0", 40), "full")
		wrongBase := strings.TrimSpace(runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git rev-parse HEAD^2"))
		if wrongBase == baseSHA {
			t.Fatalf("wrong-base fixture unexpectedly equals synthetic merge base %s", baseSHA)
		}
		fixture.requireClassification(t, classifier, "pull_request", wrongBase, "full")
	})

	t.Run("missing shallow history", func(t *testing.T) {
		fixture, baseSHA := newSyntheticPRStaticScopeFixture(t, "")
		cloneRoot := filepath.Join(t.TempDir(), "shallow")
		cmd := testCommand("git", "clone", "-q", "--depth=1", "file://"+fixture.repoRoot, cloneRoot)
		cmd.Dir = fixture.repoRoot
		cmd.Env = fixture.commandEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("clone shallow fixture: %v\n%s", err, output)
		}
		shallow := fixture
		shallow.repoRoot = cloneRoot
		shallow.requireClassification(t, classifier, "pull_request", baseSHA, "full")
	})

	t.Run("pull request without a synthetic merge", func(t *testing.T) {
		fixture := newPRStaticScopeFixture(t, map[string]string{
			"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
		})
		baseSHA := strings.TrimSpace(runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git rev-parse HEAD"))
		fixture.requireClassification(t, classifier, "pull_request", baseSHA, "full")
	})

	t.Run("non-pull-request and unknown events", func(t *testing.T) {
		fixture, baseSHA := newSyntheticPRStaticScopeFixture(t, "")
		for _, event := range []string{"push", "workflow_dispatch", "schedule", "unknown", ""} {
			t.Run(eventNameForTest(event), func(t *testing.T) {
				fixture.requireClassification(t, classifier, event, baseSHA, "full")
			})
		}
	})
}

func newSyntheticPRStaticScopeFixture(t *testing.T, protectedPath string) (prStaticScopeFixture, string) {
	t.Helper()
	return newSyntheticPRStaticScopeFixtureWithMutation(t, map[string]string{
		"alpha/alpha.go": "package alpha\n\nfunc Value() int { return 1 }\n",
		"README.md":      "baseline\n",
	}, func(t *testing.T, root string) {
		t.Helper()
		if protectedPath == "" {
			writeTestFile(t, filepath.Join(root, "alpha", "alpha.go"), "package alpha\n\nfunc Value() int { return 2 }\n")
		} else {
			writeTestFile(t, filepath.Join(root, protectedPath), "name: static-scope fixture\n")
		}
	})
}

func newSyntheticPRStaticScopeFixtureWithMutation(
	t *testing.T,
	files map[string]string,
	mutate func(*testing.T, string),
) (prStaticScopeFixture, string) {
	t.Helper()
	fixture := newPRStaticScopeFixture(t, files)
	baseSHA := strings.TrimSpace(runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git rev-parse HEAD"))
	runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(), "git checkout -qb feature")
	mutate(t, fixture.repoRoot)
	runGitFixtureCommands(t, fixture.repoRoot, fixture.commandEnv(),
		"git add -A",
		"git commit -qm feature",
		"git checkout -q main",
		"git merge -q --no-ff feature -m synthetic-merge",
	)
	return fixture, baseSHA
}

func (f prStaticScopeFixture) requireClassification(t *testing.T, classifier, event, baseSHA, want string) {
	t.Helper()
	driver := filepath.Join(t.TempDir(), "classify.mk")
	writeTestFile(t, driver, `.PHONY: classify
classify:
	@"$(CLASSIFIER)"
`)
	cmd := makeCommand(
		"--no-print-directory",
		"-f", driver,
		"CLASSIFIER="+classifier,
		"classify",
	)
	cmd.Dir = f.repoRoot
	cmd.Env = append(f.commandEnv(), "EVENT_NAME="+event, "PR_BASE_SHA="+baseSHA)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("classify event %q base %q: %v\n%s", event, baseSHA, err, output)
	}
	if got := strings.TrimSpace(string(output)); got != want {
		t.Fatalf("classify event %q base %q = %q, want %q", event, baseSHA, got, want)
	}
}

func eventNameForTest(event string) string {
	if event == "" {
		return "empty"
	}
	return event
}

type prStaticScopeFixture struct {
	repoRoot           string
	productionMakefile string
	fakeLint           string
	fakeGo             string
	lintLog            string
	goLog              string
	realGo             string
	homeDir            string
}

func newPRStaticScopeFixture(t *testing.T, files map[string]string) prStaticScopeFixture {
	t.Helper()

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("create temporary repository: %v", err)
	}
	writeTestFile(t, filepath.Join(repo, "go.mod"), "module example.com/static-scope\n\ngo 1.23\n")
	for name, content := range files {
		writeTestFile(t, filepath.Join(repo, name), content)
	}

	toolDir := t.TempDir()
	lintLog := filepath.Join(toolDir, "golangci.calls")
	goLog := filepath.Join(toolDir, "go.calls")
	fakeLint := filepath.Join(toolDir, "golangci-lint")
	writeExecutable(t, fakeLint, `#!/bin/sh
set -eu
: "${STATIC_SCOPE_LINT_LOG:?}"
printf 'CALL\000' >> "$STATIC_SCOPE_LINT_LOG"
for arg in "$@"; do
  printf 'ARG\000%s\000' "$arg" >> "$STATIC_SCOPE_LINT_LOG"
done
printf 'END\000' >> "$STATIC_SCOPE_LINT_LOG"
`)
	realGo := "go"
	fakeGo := filepath.Join(toolDir, "go")
	writeExecutable(t, fakeGo, `#!/bin/sh
set -eu
: "${STATIC_SCOPE_GO_LOG:?}"
: "${STATIC_SCOPE_REAL_GO:?}"
if [ "${1-}" = "vet" ]; then
  printf 'CALL\000' >> "$STATIC_SCOPE_GO_LOG"
  for arg in "$@"; do
    printf 'ARG\000%s\000' "$arg" >> "$STATIC_SCOPE_GO_LOG"
  done
  printf 'END\000' >> "$STATIC_SCOPE_GO_LOG"
  exit 0
fi
exec "$STATIC_SCOPE_REAL_GO" "$@"
`)

	fixture := prStaticScopeFixture{
		repoRoot:           repo,
		productionMakefile: filepath.Join(repoRoot(t), "Makefile"),
		fakeLint:           fakeLint,
		fakeGo:             fakeGo,
		lintLog:            lintLog,
		goLog:              goLog,
		realGo:             realGo,
		homeDir:            t.TempDir(),
	}
	setupMakefile := filepath.Join(t.TempDir(), "git-init.mk")
	writeTestFile(t, setupMakefile, `.PHONY: init
init:
	@git init -q -b main
	@git config user.email static-scope@example.invalid
	@git config user.name static-scope-test
	@git add .
	@git commit -qm baseline
`)
	cmd := makeCommand("--no-print-directory", "-C", repo, "-f", setupMakefile, "init")
	cmd.Env = fixture.commandEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("initialize temporary Git repository: %v\n%s", err, output)
	}
	return fixture
}

func (f prStaticScopeFixture) runMakeTarget(target string) (string, error) {
	return f.runMakeTargetWithOptions(target, "HEAD", f.fakeGo)
}

func (f prStaticScopeFixture) runMakeTargetWithRef(target, ref string) (string, error) {
	return f.runMakeTargetWithOptions(target, ref, f.fakeGo)
}

func (f prStaticScopeFixture) runMakeTargetWithGo(target, goTool string) (string, error) {
	return f.runMakeTargetWithOptions(target, "HEAD", goTool)
}

func (f prStaticScopeFixture) runMakeTargetWithOptions(target, ref, goTool string) (string, error) {
	cmd := makeCommand(
		"--no-print-directory",
		"-f", f.productionMakefile,
		"GOLANGCI_LINT="+f.fakeLint,
		"CI_STATIC_GO="+goTool,
		"LINT_CHANGED_SCOPE=tracked",
		"LINT_CHANGED_REF="+ref,
		"LINT_FLAGS=",
		"SYS_USR_CGO_FALLBACK=0",
		target,
	)
	cmd.Dir = f.repoRoot
	cmd.Env = f.commandEnv()
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (f prStaticScopeFixture) commandEnv() []string {
	env := make([]string, 0, len(os.Environ())+7)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" ||
			name == "STATIC_SCOPE_LINT_LOG" ||
			name == "STATIC_SCOPE_GO_LOG" ||
			name == "STATIC_SCOPE_REAL_GO" ||
			name == "SYS_USR_CGO_FALLBACK" ||
			name == "EVENT_NAME" ||
			name == "PR_BASE_SHA" ||
			name == "GOFLAGS" ||
			name == "GOENV" ||
			name == "GOWORK" ||
			name == "LINT_FLAGS" ||
			name == "GIT_CONFIG" ||
			strings.HasPrefix(name, "GIT_CONFIG_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HOME="+f.homeDir,
		"STATIC_SCOPE_LINT_LOG="+f.lintLog,
		"STATIC_SCOPE_GO_LOG="+f.goLog,
		"STATIC_SCOPE_REAL_GO="+f.realGo,
		"SYS_USR_CGO_FALLBACK=0",
		"GOFLAGS=-mod=readonly",
		"GOENV=off",
		"GOWORK=off",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
}

func (f prStaticScopeFixture) resetCalls(t *testing.T) {
	t.Helper()
	for label, path := range map[string]string{"golangci": f.lintLog, "go": f.goLog} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("reset fake %s log: %v", label, err)
		}
	}
}

func (f prStaticScopeFixture) calls(t *testing.T) [][]string {
	t.Helper()
	return readFramedCalls(t, f.lintLog, "golangci")
}

func (f prStaticScopeFixture) goCalls(t *testing.T) [][]string {
	t.Helper()
	return readFramedCalls(t, f.goLog, "go")
}

func readFramedCalls(t *testing.T, path, label string) [][]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read fake %s log: %v", label, err)
	}
	if len(body) == 0 {
		return nil
	}
	fields := bytes.Split(body, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	calls := make([][]string, 0)
	for index := 0; index < len(fields); {
		if string(fields[index]) != "CALL" {
			t.Fatalf("malformed fake %s log token %q at %d", label, fields[index], index)
		}
		index++
		call := make([]string, 0)
		for {
			if index >= len(fields) {
				t.Fatalf("unterminated fake %s call", label)
			}
			switch string(fields[index]) {
			case "END":
				index++
				calls = append(calls, call)
				goto nextCall
			case "ARG":
				if index+1 >= len(fields) {
					t.Fatalf("missing fake %s argument after token %d", label, index)
				}
				call = append(call, string(fields[index+1]))
				index += 2
			default:
				t.Fatalf("malformed fake %s call token %q at %d", label, fields[index], index)
			}
		}
	nextCall:
		continue
	}
	return calls
}

func (f prStaticScopeFixture) requireCalls(t *testing.T, want ...[]string) {
	t.Helper()
	got := f.calls(t)
	if len(got) != len(want) {
		t.Errorf("golangci calls = %v, want %v", got, want)
		return
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("golangci call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func (f prStaticScopeFixture) requireNoCalls(t *testing.T) {
	t.Helper()
	if got := f.calls(t); len(got) != 0 {
		t.Errorf("golangci calls = %v, want no-op", got)
	}
	if got := f.goCalls(t); len(got) != 0 {
		t.Errorf("go calls = %v, want no-op", got)
	}
}

func (f prStaticScopeFixture) requireGoCalls(t *testing.T, want ...[]string) {
	t.Helper()
	got := f.goCalls(t)
	if len(got) != len(want) {
		t.Errorf("go calls = %v, want %v", got, want)
		return
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("go call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func (f prStaticScopeFixture) requireSingleRunCallWithUnorderedTail(t *testing.T, wantTail ...string) {
	t.Helper()
	got := f.calls(t)
	if len(got) != 1 || len(got[0]) == 0 {
		t.Errorf("golangci calls = %v, want one run call with %v", got, wantTail)
		return
	}
	if got[0][0] != "run" {
		t.Errorf("golangci call = %v, want leading argument %q", got[0], "run")
		return
	}
	gotTail := slices.Clone(got[0][1:])
	wantSorted := slices.Clone(wantTail)
	slices.Sort(gotTail)
	slices.Sort(wantSorted)
	if !slices.Equal(gotTail, wantSorted) {
		t.Errorf("golangci run arguments = %v, want %v", got[0][1:], wantTail)
	}
}

func runGitFixtureCommands(t *testing.T, repo string, env []string, commands ...string) string {
	t.Helper()
	makefile := filepath.Join(t.TempDir(), "git-fixture.mk")
	var body strings.Builder
	body.WriteString(".PHONY: run\nrun:\n")
	for _, command := range commands {
		body.WriteString("\t@")
		body.WriteString(command)
		body.WriteByte('\n')
	}
	writeTestFile(t, makefile, body.String())
	cmd := makeCommand("--no-print-directory", "-C", repo, "-f", makefile, "run")
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run Git fixture command: %v\n%s", err, output)
	}
	return string(output)
}
