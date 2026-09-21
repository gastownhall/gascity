package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bdForkPattern matches the places gc-beads-bd.sh actually forks bd: the two
// spellings of the chokepoint it uses (`"${BD_BIN:-bd}"` and the `bd_bin`
// local the proxied initializer resolves first) followed by an argument list.
var bdForkPattern = regexp.MustCompile(`^\s*(?:"\$\{BD_BIN:-bd\}"|"\$bd_bin")\s`)

// TestGCBeadsBDScriptTracesEveryBDFork walks the script and requires a
// trace_bd_argv call immediately before every line that forks bd.
//
// A census with a hole in it is worse than no census: the fork budgets the
// proxied topology is measured against would read as met while the calls this
// script makes went uncounted, and the missing ones would be precisely the bd
// invocations the proxied path added. Checking the shape here rather than
// listing the known sites means a NEW fork site added later fails this test
// instead of quietly going unrecorded.
func TestGCBeadsBDScriptTracesEveryBDFork(t *testing.T) {
	scriptPath := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")

	forks := 0
	for i, line := range lines {
		if !bdForkPattern.MatchString(line) {
			continue
		}
		forks++
		previous := ""
		if i > 0 {
			previous = strings.TrimSpace(lines[i-1])
		}
		if !strings.HasPrefix(previous, "trace_bd_argv") {
			t.Errorf("gc-beads-bd.sh:%d forks bd without recording it first:\n  %s\n  %s\n"+
				"add `trace_bd_argv \"$@\"` immediately above, or the fork is invisible to gc's bd census",
				i+1, previous, strings.TrimSpace(line))
		}
	}
	if forks == 0 {
		t.Fatal("found no bd fork sites in gc-beads-bd.sh; the pattern this test scans for no longer matches the script")
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
	if strings.Contains(script, `"$GC_BD_TRACE_JSON"`) {
		t.Error("gc-beads-bd.sh writes the JSONL trace, which double-counts every fork a recording BD_BIN shim already records")
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
	harness := filepath.Join(dir, "harness.sh")
	body := "#!/bin/sh\nset -e\n" + helper + "\n" +
		"trace_bd_argv ping --json\n" +
		"GC_BD_TRACE=" + tracePath + "\nexport GC_BD_TRACE\n" +
		"trace_bd_argv create 'a title with spaces' --json\n" +
		"trace_bd_argv ping --json\n"
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
	if len(lines) != 2 {
		t.Fatalf("recorded %d line(s), want 2 (the call before GC_BD_TRACE was set must write nothing):\n%s", len(lines), recorded)
	}
	for _, want := range []string{"source=provider-script", "subcommand=create", "a title with spaces"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("record %q does not carry %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "subcommand=ping") {
		t.Errorf("record %q does not name the ping subcommand", lines[1])
	}
}
