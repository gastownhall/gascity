package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// controllerTestExcludedHangDeadlineFuncs names the controller_test.go
// functions whose one raw-literal site is correct as it stands, per
// TESTING.md "Floors, ceilings, and inputs", and must NOT be migrated
// (ga-57b2dk). Exclusions are keyed by enclosing function, not line number,
// so an edit elsewhere in the file (such as an added import) cannot
// invalidate them.
var controllerTestExcludedHangDeadlineFuncs = map[string]string{
	"TestSendControllerCommandWithTimeoutsTimesOutOnRead":           "input the test feeds a fake server to define the scenario, not a hang detector",
	"TestWatchConfigDirs_CityRootDoesNotWatchUnrelatedNestedSubdir": "negative-assertion window (asserts no watcher poke arrives)",
	"TestWatchConfigDirs_CityRootIgnoresRuntimeTraceWrites":         "negative-assertion window (asserts no watcher poke arrives, loop body)",
	"TestResetSessionCircuitBreakerStateClearsRacingOpenPersist":    "bounded best-effort probe with no assertion on either branch",
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

func parseControllerTest(t *testing.T, path string, data []byte) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, data, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, file
}

// rawHangDeadlineLines returns the 1-indexed lines in [from, to] that match
// rawHangDeadlinePattern.
func rawHangDeadlineLines(lines []string, from, to int) []int {
	var matches []int
	for i := max(from, 1); i <= to && i <= len(lines); i++ {
		if rawHangDeadlinePattern.MatchString(lines[i-1]) {
			matches = append(matches, i)
		}
	}
	return matches
}

// rawHangDeadlineOffenders returns formatted offender strings for every line
// in [from, to] that matches rawHangDeadlinePattern and is not excluded.
func rawHangDeadlineOffenders(path string, lines []string, from, to int, excluded map[int]bool) []string {
	var offenders []string
	for _, i := range rawHangDeadlineLines(lines, from, to) {
		if !excluded[i] {
			offenders = append(offenders, formatOffender(path, i, lines[i-1]))
		}
	}
	return offenders
}

// excludedHangDeadlineLines resolves each function named in excludedFuncs to
// the line of its one raw-literal site, wherever edits have moved it. It also
// returns, sorted, a problem for every exclusion that no longer pins exactly
// one site: its function is gone, or holds no site (migrated) or several (one
// was added beside it). Such an exclusion excludes nothing.
func excludedHangDeadlineLines(fset *token.FileSet, file *ast.File, lines []string, excludedFuncs map[string]string) (excluded map[int]bool, problems []string) {
	excluded = make(map[int]bool, len(excludedFuncs))
	found := make(map[string]bool, len(excludedFuncs))
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		reason, listed := excludedFuncs[fn.Name.Name]
		if !listed {
			continue
		}
		found[fn.Name.Name] = true
		sites := rawHangDeadlineLines(lines, fset.Position(fn.Pos()).Line, fset.Position(fn.End()).Line)
		if len(sites) != 1 {
			problems = append(problems, fmt.Sprintf("%s (%s): want exactly 1 raw hang deadline, found %d at lines %v",
				fn.Name.Name, reason, len(sites), sites))
			continue
		}
		excluded[sites[0]] = true
	}
	for name, reason := range excludedFuncs {
		if !found[name] {
			problems = append(problems, fmt.Sprintf("%s (%s): function not found", name, reason))
		}
	}
	slices.Sort(problems)
	return excluded, problems
}

// TestControllerTestHasNoUnmigratedRawHangDeadlines pins ga-57b2dk's primary
// acceptance check: every sub-10s raw-literal timer in controller_test.go
// that isn't one of the four documented exclusions must be migrated to
// awaitClose/awaitCond, exactly as #4638 already did for the rest of the
// package.
func TestControllerTestHasNoUnmigratedRawHangDeadlines(t *testing.T) {
	path, data, lines := controllerTestLines(t)
	fset, file := parseControllerTest(t, path, data)

	// Guard against the exclusion list silently going stale (an excluded site
	// was migrated, or its function renamed, without updating this map) or
	// loose (a second raw deadline was added beside an excluded one) by
	// requiring every documented exclusion to pin exactly one site.
	excluded, problems := excludedHangDeadlineLines(fset, file, lines, controllerTestExcludedHangDeadlineFuncs)
	for _, problem := range problems {
		t.Errorf("%s: hang-deadline exclusion %s; update controllerTestExcludedHangDeadlineFuncs if the site "+
			"was migrated or its function renamed, or migrate any raw deadline added beside it", path, problem)
	}

	offenders := rawHangDeadlineOffenders(path, lines, 1, len(lines), excluded)
	if len(offenders) > 0 {
		t.Fatalf("controller_test.go has %d raw-literal hang deadline(s); replace with awaitClose "+
			"(channel drain) or awaitCond (polled condition) per ga-57b2dk:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestControllerTestNoFunctionMixesHangBudgetWithRawDeadline pins ga-57b2dk's
// second acceptance check: a test function that already uses hangBudget for
// some of its waits must not also carry an unmigrated raw-literal deadline —
// the exact same-function inconsistency #4638 left behind in four functions.
func TestControllerTestNoFunctionMixesHangBudgetWithRawDeadline(t *testing.T) {
	path, data, lines := controllerTestLines(t)
	fset, file := parseControllerTest(t, path, data)
	// Stale or loose exclusions are reported by
	// TestControllerTestHasNoUnmigratedRawHangDeadlines; here an exclusion
	// with a problem simply excludes nothing.
	excluded, _ := excludedHangDeadlineLines(fset, file, lines, controllerTestExcludedHangDeadlineFuncs)

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
		if offenders := rawHangDeadlineOffenders(path, lines, start, end, excluded); len(offenders) > 0 {
			violations = append(violations, fmt.Sprintf("%s: %s", fn.Name.Name, strings.Join(offenders, "; ")))
		}
	}

	if len(violations) > 0 {
		t.Fatalf("functions mix hangBudget with a raw-literal hang deadline (ga-57b2dk):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestExcludedHangDeadlineLinesFollowFunctionNotLineNumber pins the
// exclusion resolver's contract on a synthetic file: an exclusion follows its
// function when an edit above it shifts every line, and an exclusion that no
// longer pins exactly one site is reported and excludes nothing.
func TestExcludedHangDeadlineLinesFollowFunctionNotLineNumber(t *testing.T) {
	const body = `
func TestKept() {
	<-time.After(200 * time.Millisecond)
}

func TestMigrated() {}

func TestLoose() {
	<-time.After(250 * time.Millisecond)
	<-time.After(250 * time.Millisecond)
}
`
	excludedFuncs := map[string]string{
		"TestKept":     "still holds its one raw site",
		"TestMigrated": "its site was migrated",
		"TestLoose":    "a second site was added beside it",
		"TestRenamed":  "the function was renamed away",
	}
	for _, tc := range []struct{ name, header string }{
		{"one import", "package p\n\nimport \"time\"\n"},
		{"added import shifts every line", "package p\n\nimport (\n\t\"os\"\n\t\"time\"\n)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.header + body
			lines := strings.Split(src, "\n")
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "synthetic_test.go", src, 0)
			if err != nil {
				t.Fatalf("parse synthetic source: %v", err)
			}

			excluded, problems := excludedHangDeadlineLines(fset, file, lines, excludedFuncs)

			keptLine := slices.Index(lines, "\t<-time.After(200 * time.Millisecond)") + 1
			if want := map[int]bool{keptLine: true}; !maps.Equal(excluded, want) {
				t.Errorf("excluded lines = %v, want only TestKept's site %v", excluded, want)
			}
			reported := strings.Join(problems, "\n")
			for _, fn := range []string{"TestMigrated", "TestLoose", "TestRenamed"} {
				if !strings.Contains(reported, fn) {
					t.Errorf("problems %q do not report %s", problems, fn)
				}
			}
			if len(problems) != 3 {
				t.Errorf("problems = %q, want exactly 3 (TestKept resolves cleanly)", problems)
			}
		})
	}
}
