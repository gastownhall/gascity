package scripts_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// The early-exit-reader guard: a reader at the end of a pipe that can exit
// before its input ends (grep -q, head, sed ...q, awk ... exit) lets the
// producer die of SIGPIPE (141). Under `set -o pipefail` that turns a MATCH
// into a miss, but only when the producer's output is larger than a few pipe
// writes, so small inputs pass and large ones silently flip. Two incidents in
// one week (upstream #6714, #6866) and a third fixed earlier (#3796) were all
// this one shape. Mechanism, measured flip rates, the scope rule and the
// rewrite table: engdocs/contributors/shell-pipefail-early-exit.md.
//
//	shell_pipefail_early_exit_test.go        this file: the finding type, the
//	                                         discovery and analyzer entry points,
//	                                         and the whole-repo test
//	shell_pipefail_early_exit_rules_test.go  table-driven fixtures: what is
//	                                         flagged, what is not, and the
//	                                         historical regressions verbatim
//
// The guard parses every tracked shell source with mvdan.cc/sh/v3/syntax (parse
// only, never interp) and has no exemption mechanism of any kind: no allow
// comment, no baseline file, no directory exclusion list. The remedy is always
// a one-token rewrite, and an escape hatch is what rots.

// shellDialect is the dialect a source is written for. It decides both whether
// the source is checked (a POSIX sh file is only in scope when pipefail is on
// for it) and which rewrite the finding recommends (here-strings and process
// substitution are bash-only).
type shellDialect int

const (
	// dialectBash is bash (shebang bash or env bash, *.bash, or a *.sh with no
	// shebang). It is ALWAYS checked, whether or not it sets pipefail: a library
	// inherits its sourcer's options and removing pipefail must never silence
	// the guard.
	dialectBash shellDialect = iota
	// dialectPOSIX is sh, dash, ash or ksh. It is checked only when it sets
	// pipefail itself or is sourced, transitively, from an in-scope file.
	dialectPOSIX
)

// shellSource is one parseable unit of shell: a whole script file, or one
// run: block of a GitHub Actions workflow.
type shellSource struct {
	// Path is the repo-relative, slash-separated path of the file.
	Path string
	// Dialect is the dialect the source is written for.
	Dialect shellDialect
	// Text is the shell text exactly as the parser should see it.
	Text string
	// LineOffset is added to a parsed line to give the file line. It is 0 for a
	// script file and, for a workflow run: block, the YAML line before the
	// block's first line, so a finding names the YAML line.
	LineOffset int
	// Workflow marks a run: block of a workflow file.
	Workflow bool
	// WorkflowShell is the step's explicit shell: value ("" when unspecified).
	// GitHub documents an unspecified shell as `bash -e {0}`, which has no
	// pipefail, and an explicit `shell: bash` as `bash -eo pipefail {0}`.
	WorkflowShell string
}

// pipefailFinding is one reader that can exit before its input ends.
type pipefailFinding struct {
	// File is the repo-relative path.
	File string
	// Line is the file line on which the reader command word starts.
	Line int
	// Reader is the reader command as written (grep, head, sed, awk, rg, ...).
	Reader string
	// Producer is the command word of the stage that feeds the reader.
	Producer string
	// Rewrite is the one-token rewrite for this reader and dialect.
	Rewrite string
}

// newShellSource decides whether one tracked file is a shell source and, if
// so, which dialect. Discovery by content: a first line `#!` naming sh, bash,
// dash, ash or ksh (directly or through env), or a *.sh/*.bash name (no
// shebang means bash). ok is false for any other file.
func newShellSource(_ string, _ []byte) (src shellSource, ok bool) {
	return shellSource{}, false
}

// workflowRunSources returns one source per run: block of a workflow file,
// with ${{ ... }} replaced by a placeholder so the block parses, and with the
// line offset and the step's shell recorded so findings name the YAML line.
func workflowRunSources(_ string, _ []byte) ([]shellSource, error) {
	return nil, nil
}

// checkShellSources applies the scope rule across the whole source set (a
// POSIX file sourced from an in-scope file is itself in scope, so scope needs
// every source at once) and returns every finding sorted by file then line.
// A source that fails to parse is an error naming the file and line; it is
// never skipped.
func checkShellSources(_ []shellSource) ([]pipefailFinding, error) {
	return nil, nil
}

// discoverShellSources returns every shell source under root: the tracked files
// from `git ls-files -z` when root is a work tree, otherwise a walk of the
// tree with the residency prune rules. Both must give the same set.
func discoverShellSources(_ string) ([]shellSource, error) {
	return nil, nil
}

// walkShellSources is the non-git discovery: a walk of root that prunes the
// directory names residencyPruned prunes.
func walkShellSources(_ string) ([]shellSource, error) {
	return nil, nil
}

// gitShellSources is the git discovery: git ls-files -z under root.
func gitShellSources(_ string) ([]shellSource, error) {
	return nil, nil
}

// formatPipefailFinding renders the one report line for a finding:
// file:line: <reader> can exit before its input ends; <producer> may die of
// SIGPIPE (141) under pipefail and turn a match into a miss. Rewrite: ... See
// engdocs/contributors/shell-pipefail-early-exit.md.
func formatPipefailFinding(_ pipefailFinding) string {
	return ""
}

// pipefailGuardDoc is the contributor document every finding points at.
const pipefailGuardDoc = "engdocs/contributors/shell-pipefail-early-exit.md"

// pipefailSourceFloor is the shrink alarm: roughly 75% of the 395 sources the
// guard parsed when it was written. A scan set that quietly shrinks below it
// means the guard is checking less than it claims.
const pipefailSourceFloor = 296

// pipefailCanaryDirs each need at least one discovered source, so a scan that
// loses a whole area (a Bazel data gap, a moved directory, a broken prune)
// fails loudly instead of passing on what is left.
var pipefailCanaryDirs = []string{
	".githooks/",
	".github/workflows/",
	"internal/bootstrap/packs/core/",
	"scripts/",
	"contrib/",
	"examples/",
	"schemas/",
	"test/agents/",
	"tools/rbe/",
}

// TestShellNoEarlyExitReaderUnderPipefail is the whole-repo guard. It lists
// EVERY finding, sorted, then fails once.
func TestShellNoEarlyExitReaderUnderPipefail(t *testing.T) {
	root := repoRoot(t)
	sources, err := discoverShellSources(root)
	if err != nil {
		t.Fatalf("discovering shell sources under %s: %v", root, err)
	}

	if len(sources) < pipefailSourceFloor {
		t.Errorf("scan set shrank: %d shell sources, floor %d", len(sources), pipefailSourceFloor)
	}
	for _, dir := range pipefailCanaryDirs {
		found := false
		for _, src := range sources {
			if strings.HasPrefix(src.Path, dir) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("canary: no shell source discovered under %s", dir)
		}
	}

	findings, err := checkShellSources(sources)
	if err != nil {
		t.Fatalf("analyzing shell sources: %v", err)
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	if len(findings) == 0 {
		return
	}
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, formatPipefailFinding(f))
	}
	t.Errorf("%d early-exit reader(s) at the end of a pipe under pipefail:\n%s",
		len(findings), strings.Join(lines, "\n"))
}

// lineOfNeedle returns the 1-based line of text containing needle, failing the
// test when there is none or more than one, so a fixture expectation names its
// line by content instead of by a hand-counted number that rots.
func lineOfNeedle(t *testing.T, text, needle string) int {
	t.Helper()
	found := 0
	at := 0
	for i, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			found++
			at = i + 1
		}
	}
	if found != 1 {
		t.Fatalf("needle %q appears on %d lines of the fixture, want exactly 1:\n%s", needle, found, text)
	}
	return at
}

// describeFindings renders findings for a failure message.
func describeFindings(findings []pipefailFinding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("%s:%d %s<-%s", f.File, f.Line, f.Reader, f.Producer))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}
