package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bdForkSpellings are the two spellings of the chokepoint every bd fork in
// gc-beads-bd.sh goes through: the inline default and the bd_bin local the
// proxied paths resolve first.
var bdForkSpellings = []string{`"${BD_BIN:-bd}"`, `"$bd_bin"`}

// bdForkSiteCount is how many places the script forks bd. It is pinned because
// it IS the census: a fork budget is only meaningful against a known number of
// call sites, and a site that appears without this number moving is a site
// nobody counted.
const bdForkSiteCount = 6

// bdForkOffset returns the byte offset at which line forks bd, or -1.
//
// It matches the chokepoint spelling ANYWHERE in the line rather than at the
// start of one. A line-anchored pattern missed two of the six real sites — the
// `bd version` read inside a command substitution and the `bd context` probe
// inside an if-condition subshell — so the lint's own claim, that a fork site
// added later fails the test, held only for forks written as the first word of a
// line, which is not the shape those two take.
//
// An assignment is not a fork: `bd_bin="${BD_BIN:-bd}"` resolves which binary
// to run and runs nothing, so an occurrence immediately preceded by `=` is
// skipped. Comment lines are skipped for the same reason.
func bdForkOffset(line string) int {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return -1
	}
	earliest := -1
	for _, spelling := range bdForkSpellings {
		for offset := 0; offset < len(line); {
			index := strings.Index(line[offset:], spelling)
			if index < 0 {
				break
			}
			at := offset + index
			offset = at + len(spelling)
			if at > 0 && line[at-1] == '=' {
				continue
			}
			if earliest < 0 || at < earliest {
				earliest = at
			}
		}
	}
	return earliest
}

// TestGCBeadsBDScriptTracesEveryBDFork walks the script and requires a
// trace_bd_argv call before every fork of bd.
//
// A census with a hole in it is worse than no census: the fork budgets the
// proxied topology is measured against would read as met while the calls this
// script makes went uncounted, and the missing ones would be precisely the bd
// invocations the proxied path added. Checking the shape here rather than
// listing the known sites means a NEW fork site added later fails this test
// instead of quietly going unrecorded — which is only true if the shape is
// matched by token anywhere in the line, since a mid-line fork is a shape the
// script already uses twice.
func TestGCBeadsBDScriptTracesEveryBDFork(t *testing.T) {
	scriptPath := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")

	forks := 0
	for i, line := range lines {
		offset := bdForkOffset(line)
		if offset < 0 {
			continue
		}
		forks++
		// The call may sit on the same line ahead of the fork, or on the
		// closest preceding line that is neither blank nor a comment.
		if strings.Contains(line[:offset], "trace_bd_argv") {
			continue
		}
		previous := previousCodeLine(lines, i)
		if !strings.HasPrefix(previous, "trace_bd_argv") {
			t.Errorf("gc-beads-bd.sh:%d forks bd without recording it first:\n  %s\n  %s\n"+
				"add `trace_bd_argv \"$@\"` immediately above, or the fork is invisible to gc's bd census",
				i+1, previous, strings.TrimSpace(line))
		}
	}
	if forks != bdForkSiteCount {
		t.Errorf("gc-beads-bd.sh has %d bd fork site(s), and this census pins %d; "+
			"a site added or removed here must move bdForkSiteCount deliberately, because the fork budget is stated against it",
			forks, bdForkSiteCount)
	}

	script := string(data)
	if !strings.Contains(script, "trace_bd_argv() {") {
		t.Fatal("gc-beads-bd.sh calls trace_bd_argv but does not define it")
	}
	// The variable is the contract with the operator, and it is deliberately
	// NOT the JSONL one: a test that also substitutes a recording BD_BIN shim
	// counts in GC_BD_TRACE_JSON, and writing here too would put every fork in
	// that file twice.
	if !strings.Contains(script, `[ -n "${GC_BD_TRACE:-}" ] || return 0`) {
		t.Error("trace_bd_argv is not gated on GC_BD_TRACE; an ordinary run must write nothing")
	}
	if strings.Contains(script, `>>"$GC_BD_TRACE_JSON"`) {
		t.Error("gc-beads-bd.sh writes the JSONL trace, which double-counts every fork a recording BD_BIN shim already records")
	}
	// The other half of that rule, and the one the in-process writer already
	// honors (bdstore.go newBDExecTrace): when the JSONL trace is claimed, this
	// one stands down, so the two formats can never share a file.
	if !strings.Contains(script, `[ -z "${GC_BD_TRACE_JSON:-}" ] || return 0`) {
		t.Error("trace_bd_argv is not suppressed when GC_BD_TRACE_JSON is set; two trace formats would interleave in one file")
	}
}

// previousCodeLine returns the closest line before index that is neither blank
// nor a comment, trimmed.
func previousCodeLine(lines []string, index int) string {
	for i := index - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// TestBdForkOffsetSeesMidLineForksAndIgnoresAssignments pins the matcher itself,
// because a lint that cannot see a fork is a lint that reports none.
func TestBdForkOffsetSeesMidLineForksAndIgnoresAssignments(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{name: "a fork at the start of a line", line: `        "${BD_BIN:-bd}" "$@"`, want: true},
		{name: "the bd_bin local", line: `        "$bd_bin" "$@"`, want: true},
		{name: "inside a command substitution", line: `    if ! raw=$("${BD_BIN:-bd}" version 2>/dev/null); then`, want: true},
		{name: "inside an if-condition subshell", line: `        if (cd "$dir" && BEADS_DIR="$dir/.beads" "$bd_bin" context >/dev/null 2>&1); then`, want: true},
		{name: "resolving the binary is not running it", line: `        bd_bin="${BD_BIN:-bd}"`},
		{name: "a comment that names the chokepoint", line: `        # honor "${BD_BIN:-bd}" here too`},
		{name: "an ordinary line", line: `        run_bd_init_proxied "$dir" "$prefix"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bdForkOffset(tc.line) >= 0
			if got != tc.want {
				t.Fatalf("bdForkOffset(%q) >= 0 = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestGCBeadsBDTraceHelperIsOffByDefault runs the helper itself, both ways,
// through the same POSIX shell the script declares.
//
// The gating is the part worth proving: an unset variable has to write nothing
// at all, and a set one has to record the argv intact — including an argument
// with spaces in it, which is what a bead title looks like.
func TestGCBeadsBDTraceHelperIsOffByDefault(t *testing.T) {
	scriptPath := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	helper := extractShellFunction(t, string(data), "trace_bd_argv")

	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.log")
	jsonPath := filepath.Join(dir, "trace.jsonl")
	harness := filepath.Join(dir, "harness.sh")
	body := "#!/bin/sh\nset -e\n" + helper + "\n" +
		"trace_bd_argv ping --json\n" +
		"GC_BD_TRACE=" + tracePath + "\nexport GC_BD_TRACE\n" +
		"trace_bd_argv create 'a title with spaces' --json\n" +
		"trace_bd_argv ping --json\n" +
		// An argv carrying a newline: one fork must still be one line, or a
		// reader sees a record with no source= prefix.
		"trace_bd_argv create 'two\nlines' --json\n" +
		// And once the JSONL trace claims tracing, this one writes nothing.
		"GC_BD_TRACE_JSON=" + jsonPath + "\nexport GC_BD_TRACE_JSON\n" +
		"trace_bd_argv list --json\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil { //nolint:gosec // the harness must be executable
		t.Fatal(err)
	}

	// Run through gc's own provider-op runner rather than a fresh
	// exec.Command: it is the same spawn the script gets in production, and a
	// test that opens its own subprocess would grow the repo's shrink-only
	// resource census for no extra coverage.
	if err := runProviderOpWithEnv(harness, []string{"PATH=" + os.Getenv("PATH")}, "trace"); err != nil {
		t.Fatalf("trace harness: %v", err)
	}
	recorded, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("the helper wrote no trace with GC_BD_TRACE set: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	if len(lines) != 3 {
		t.Fatalf("recorded %d line(s), want 3 (the call before GC_BD_TRACE was set and the one after GC_BD_TRACE_JSON was set must write nothing):\n%s", len(lines), recorded)
	}
	for _, want := range []string{"source=provider-script", "subcommand=create", "a title with spaces"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("record %q does not carry %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "subcommand=ping") {
		t.Errorf("record %q does not name the ping subcommand", lines[1])
	}
	// The newline in the third fork's argv is folded, so the record stays one
	// line and still carries both words.
	for _, want := range []string{"source=provider-script", "two lines"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("record %q does not carry %q; an argv newline must not split the breadcrumb", lines[2], want)
		}
	}
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Errorf("the helper wrote %s (%v); the JSONL trace is the fork census's file, and writing both formats double-counts every fork", jsonPath, err)
	}
}
