package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawHangDeadlinePattern is ga-57b2dk's acceptance-check regex: the raw-
// literal-duration shapes that #4638/#4639 replaced with awaitClose (for a
// channel drain) or awaitCond (for a polled condition) everywhere else in
// this package's cmd/gc tests. The third alternative catches
// waitForNamedMode's two call-site literals, which are invisible to the
// first two alternatives because the raw duration is an argument rather than
// a direct time.After/time.Now().Add call.
var rawHangDeadlinePattern = regexp.MustCompile(`time\.After\([0-9]|time\.Now\(\)\.Add\([0-9]|waitForNamedMode\([^)]*,\s*[0-9]`)

// controllerTestExcludedHangDeadlineLines are the raw-literal sites in
// controller_test.go that are correct as they stand, per
// TESTING.md:1364-1371, and must NOT be migrated (ga-57b2dk). Line numbers
// are 1-indexed.
var controllerTestExcludedHangDeadlineLines = map[int]string{
	421:  "input the test feeds a fake server to define the scenario, not a hang detector",
	877:  "negative-assertion window (asserts no watcher poke arrives)",
	927:  "negative-assertion window (asserts no watcher poke arrives, loop body)",
	1456: "bounded best-effort probe with no assertion on either branch",
}

func controllerTestPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootForLint(t), "cmd", "gc", "controller_test.go")
}

func controllerTestLines(t *testing.T) (path string, data []byte, lines []string) {
	t.Helper()
	path = controllerTestPath(t)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return path, data, strings.Split(string(data), "\n")
}

// rawHangDeadlineOffenders returns formatted offender strings for every line
// in [from, to] that matches rawHangDeadlinePattern and is not one of
// controllerTestExcludedHangDeadlineLines.
func rawHangDeadlineOffenders(path string, lines []string, from, to int) []string {
	var offenders []string
	if from < 1 {
		from = 1
	}
	for i := from; i <= to && i <= len(lines); i++ {
		line := lines[i-1]
		if !rawHangDeadlinePattern.MatchString(line) {
			continue
		}
		if _, excluded := controllerTestExcludedHangDeadlineLines[i]; excluded {
			continue
		}
		offenders = append(offenders, formatOffender(path, i, line))
	}
	return offenders
}

// TestControllerTestHasNoUnmigratedRawHangDeadlines pins ga-57b2dk's primary
// acceptance check: every sub-10s raw-literal timer in controller_test.go
// that isn't one of the four documented exclusions must be migrated to
// awaitClose/awaitCond, exactly as #4638 already did for the rest of the
// package.
func TestControllerTestHasNoUnmigratedRawHangDeadlines(t *testing.T) {
	path, _, lines := controllerTestLines(t)

	offenders := rawHangDeadlineOffenders(path, lines, 1, len(lines))
	if len(offenders) > 0 {
		t.Fatalf("controller_test.go has %d raw-literal hang deadline(s); replace with awaitClose "+
			"(channel drain) or awaitCond (polled condition) per ga-57b2dk:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}

	// Guard against the exclusion list silently going stale (e.g. the code
	// around an excluded line moved or was migrated without updating this
	// map) by requiring every documented exclusion to still match.
	for lineNo, reason := range controllerTestExcludedHangDeadlineLines {
		if lineNo > len(lines) || !rawHangDeadlinePattern.MatchString(lines[lineNo-1]) {
			t.Errorf("expected excluded raw hang deadline at %s:%d (%s) but it no longer matches; "+
				"update controllerTestExcludedHangDeadlineLines if the code moved or was migrated",
				path, lineNo, reason)
		}
	}
}

// TestControllerTestNoFunctionMixesHangBudgetWithRawDeadline pins ga-57b2dk's
// second acceptance check: a test function that already uses hangBudget for
// some of its waits must not also carry an unmigrated raw-literal deadline —
// the exact same-function inconsistency #4638 left behind in four functions.
func TestControllerTestNoFunctionMixesHangBudgetWithRawDeadline(t *testing.T) {
	path, data, lines := controllerTestLines(t)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var violations []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Pos()).Line
		end := fset.Position(fn.End()).Line

		usesHangBudget := false
		for i := start; i <= end && i <= len(lines); i++ {
			if strings.Contains(lines[i-1], "hangBudget") {
				usesHangBudget = true
				break
			}
		}
		if !usesHangBudget {
			continue
		}
		if offenders := rawHangDeadlineOffenders(path, lines, start, end); len(offenders) > 0 {
			violations = append(violations, fmt.Sprintf("%s: %s", fn.Name.Name, strings.Join(offenders, "; ")))
		}
	}

	if len(violations) > 0 {
		t.Fatalf("functions mix hangBudget with a raw-literal hang deadline (ga-57b2dk):\n  %s",
			strings.Join(violations, "\n  "))
	}
}
