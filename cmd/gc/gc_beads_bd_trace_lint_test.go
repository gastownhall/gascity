package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bdForkSpellings are the two spellings of the chokepoint every bd fork in
// gc-beads-bd.sh goes through: the inline default and the bd_bin local the
// proxied paths resolve first.
var bdForkSpellings = []string{`"${BD_BIN:-bd}"`, `"$bd_bin"`}

// bdChokepointSpellings are the spellings a line may legitimately contain when
// it names the bd binary: the two fork chokepoints, and the assignment that
// resolves the local. Anything else — "$BD_BIN", ${BD_BIN}, $bd_bin unquoted —
// runs bd past the census bdForkSpellings counts.
var bdChokepointSpellings = []string{`"${BD_BIN:-bd}"`, `"$bd_bin"`, `bd_bin=`}

// bareBdFork matches `bd` used as a command: at the start of a line or after a
// separator, with nothing quoting it. The two chokepoints are quoted, so they
// are removed before this runs (see shellCode with dropQuoted).
var bareBdFork = regexp.MustCompile(`(?:^|[;&|(]|&&|\|\||\bexec\b|\bthen\b|\bdo\b)[[:space:]]*bd(?:[[:space:]]|$)`)

// jsonlTraceRedirect matches a redirection into the JSONL trace file in any
// spelling: >> or >, any spacing, braced or bare, quoted or not. Pinning the one
// byte sequence `>>"$GC_BD_TRACE_JSON"` left `>> "$GC_BD_TRACE_JSON"` and
// `>>"${GC_BD_TRACE_JSON}"` passing, which is the same hole the fork lint had.
var jsonlTraceRedirect = regexp.MustCompile(`>>?[[:space:]]*"?\$\{?GC_BD_TRACE_JSON`)

// shellCode returns line with comments removed, and — when dropQuoted is set —
// quoted text removed too.
//
// It is a lint's approximation of the shell's lexer, not the shell's lexer: it
// tracks single and double quotes and treats an unquoted `#` at the start of a
// word as a comment. That is enough to keep prose out of the scan. Without it
// `die "bd $version cannot initialize …"` reads as a bd fork, which is how the
// bare-word check would have cried wolf on its first run.
func shellCode(line string, dropQuoted bool) string {
	var out strings.Builder
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			if !dropQuoted {
				out.WriteByte(c)
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
			if dropQuoted {
				out.WriteByte(' ')
			} else {
				out.WriteByte(c)
			}
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return out.String()
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// unpinnedBdReference returns the leftover text of a line that names the bd
// binary outside the chokepoint spellings, or "".
func unpinnedBdReference(line string) string {
	code := shellCode(line, false)
	for _, spelling := range bdChokepointSpellings {
		code = strings.ReplaceAll(code, spelling, " ")
	}
	for _, name := range []string{"BD_BIN", "bd_bin"} {
		if index := strings.Index(code, name); index >= 0 {
			return strings.TrimSpace(code[index:])
		}
	}
	return ""
}

// forksBareBd reports whether line runs `bd` off PATH rather than through the
// chokepoint.
func forksBareBd(line string) bool {
	return bareBdFork.MatchString(shellCode(line, true))
}

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
		// The census is only a census if the chokepoint is the only door. A
		// fork written `"$BD_BIN" "$@"`, `${BD_BIN} "$@"` or as a bare `bd` runs
		// the same binary and is invisible to bdForkOffset, so the lint refuses
		// those spellings outright rather than pretending it counted them.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if leftover := unpinnedBdReference(line); leftover != "" {
			t.Errorf("gc-beads-bd.sh:%d names the bd binary outside the traced chokepoint:\n  %s\n  (%s)\n"+
				"use \"${BD_BIN:-bd}\" or the resolved \"$bd_bin\", or the fork is invisible to gc's bd census",
				i+1, strings.TrimSpace(line), leftover)
		}
		if forksBareBd(line) {
			t.Errorf("gc-beads-bd.sh:%d forks a bare `bd` off PATH:\n  %s\n"+
				"use \"${BD_BIN:-bd}\" so the fork passes the chokepoint the census counts",
				i+1, strings.TrimSpace(line))
		}
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
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if jsonlTraceRedirect.MatchString(line) {
			t.Errorf("gc-beads-bd.sh:%d writes the JSONL trace:\n  %s\n"+
				"that file is the fork census's, and writing it here double-counts every fork a recording BD_BIN shim already records",
				i+1, strings.TrimSpace(line))
		}
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

// TestBdForkLintSeesEveryWayOfNamingBd pins the negative half of the census: the
// spellings that would run bd without passing the chokepoint.
//
// bdForkOffset knows two spellings, so "a fork site added later fails this test"
// held only for forks written with them. `"$BD_BIN"`, `${BD_BIN}` and a bare `bd`
// are the same fork and were invisible. They are now refused by shape, and the
// matcher has to tell them from the prose that merely says the word.
func TestBdForkLintSeesEveryWayOfNamingBd(t *testing.T) {
	cases := []struct {
		name         string
		line         string
		wantUnpinned bool
		wantBare     bool
	}{
		{name: "the inline chokepoint", line: `        "${BD_BIN:-bd}" "$@"`},
		{name: "the resolved local", line: `        "$bd_bin" "$@"`},
		{name: "resolving the local", line: `        bd_bin="${BD_BIN:-bd}"`},
		{name: "an unquoted brace expansion", line: `        ${BD_BIN} "$@"`, wantUnpinned: true},
		{name: "the bare variable", line: `        "$BD_BIN" "$@"`, wantUnpinned: true},
		{name: "exec through the unpinned local", line: `        exec $bd_bin "$@"`, wantUnpinned: true},
		{name: "a bare bd off PATH", line: `        bd ping --json`, wantBare: true},
		{name: "a bare bd after exec", line: `        exec bd "$@"`, wantBare: true},
		{name: "a bare bd in a pipeline", line: `        printf '%s' "$x" | bd create --json`, wantBare: true},
		{name: "a bare bd in a subshell", line: `        (cd "$dir" && bd context >/dev/null)`, wantBare: true},
		// The prose the script is full of. A lint that flagged these would be
		// turned off within a week.
		{name: "bd named inside a message", line: `            die "bd $version cannot initialize the workspace at $dir (bd 1.0.0 or newer required)"`},
		{name: "a comment naming the chokepoint", line: `        # honor "${BD_BIN:-bd}" here too`},
		{name: "a trailing comment naming bd", line: `        run_init "$dir"   # bd init runs here`},
		{name: "a function whose name contains bd", line: `        run_bd_init_proxied "$dir" "$prefix"`},
		{name: "a single-quoted bd", line: `        printf '%s\n' 'bd ping'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unpinnedBdReference(tc.line) != ""; got != tc.wantUnpinned {
				t.Errorf("unpinnedBdReference(%q) = %q, want a finding: %v", tc.line, unpinnedBdReference(tc.line), tc.wantUnpinned)
			}
			if got := forksBareBd(tc.line); got != tc.wantBare {
				t.Errorf("forksBareBd(%q) = %v, want %v", tc.line, got, tc.wantBare)
			}
		})
	}
}

// TestJSONLTraceRedirectIsMatchedByShapeNotByBytes pins the other narrowed
// check: the script must not write the fork census's file, in any spelling.
func TestJSONLTraceRedirectIsMatchedByShapeNotByBytes(t *testing.T) {
	redirects := []string{
		`        >>"$GC_BD_TRACE_JSON" 2>/dev/null || true`,
		`        >> "$GC_BD_TRACE_JSON"`,
		`        >>"${GC_BD_TRACE_JSON}"`,
		`        >>${GC_BD_TRACE_JSON}`,
		`        printf '%s' "$rec" > $GC_BD_TRACE_JSON`,
	}
	for _, line := range redirects {
		if !jsonlTraceRedirect.MatchString(line) {
			t.Errorf("a write to the JSONL trace went unnoticed: %q", line)
		}
	}
	// Reading the variable is how the helper stands down, and must stay legal.
	for _, line := range []string{
		`    [ -z "${GC_BD_TRACE_JSON:-}" ] || return 0`,
		`        >>"$GC_BD_TRACE" 2>/dev/null || true`,
	} {
		if jsonlTraceRedirect.MatchString(line) {
			t.Errorf("the lint refused a line that writes no JSONL trace: %q", line)
		}
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
