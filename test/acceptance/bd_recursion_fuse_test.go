//go:build acceptance_a && unix

// The bd recursion fuse (ga-kihcmff, incident ga-2t6u9dy). On 2026-10-25 a
// three-line stub `bd` on PATH that called `gc bd` recursed about 2,500 levels,
// used about 70 GiB of RAM and caused eight host OOM kills. Every Go path in
// gc that launches a bd binary now stamps GC_BD_DEPTH on the child and refuses
// at a fixed bound, so that loop ends at 16 launches. These tests run the real
// gc against such a stub.
//
// The fixtures cap themselves, so a regression is a failing assertion and not
// another outage: the stub appends to a log and exits at 64 lines without
// calling gc, and the outer gc runs in its own process group with a deadline
// and is killed on timeout.
package acceptance_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const (
	recursionFuseBound    = 16
	recursionStubBackstop = 64
	recursionDeadline     = 3 * time.Minute
)

// recursionFuseCity builds a minimal city directory with a bound rig-less
// scope; `gc bd` needs no running controller or store for a stubbed bd.
func recursionFuseCity(t *testing.T) string {
	t.Helper()
	city := helpers.TempDir(t)
	toml := "[workspace]\nname = \"fuse\"\nprefix = \"fuse\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	return city
}

// writeRecursiveStub writes a bd that logs the depth it was launched with and
// calls back into gc, the incident's shape. Only `list` recurses: version and
// every other probe gc makes are answered without calling gc, so the log counts
// the passthrough launches and nothing else. wrapPipe selects the incident's
// literal form, `gc bd ... 2>/dev/null | grep -v ...`, which hides both gc's
// stderr and its exit status from the stub.
func writeRecursiveStub(t *testing.T, dir, gcPath, log string, wrapPipe bool) string {
	t.Helper()
	call := fmt.Sprintf("exec %q bd \"$@\"", gcPath)
	if wrapPipe {
		call = fmt.Sprintf("%q bd \"$@\" 2>/dev/null | grep -v '^never-matches$'\nexit 0", gcPath)
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  version) echo 'bd version 1.3.2 (test)'; exit 0 ;;
  list) ;;
  *) exit 0 ;;
esac
n=0
if [ -f %[1]q ]; then while IFS= read -r _; do n=$((n+1)); done < %[1]q; fi
printf 'depth=%%s\n' "$GC_BD_DEPTH" >> %[1]q
# Backstop: stop at %[2]d lines without calling gc, so a missing fuse ends in
# a failed assertion rather than an unbounded recursion.
if [ "$n" -ge %[3]d ]; then exit 0; fi
%[4]s
`, log, recursionStubBackstop, recursionStubBackstop-1, call)
	stub := filepath.Join(dir, "bd")
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub
}

// runGCBounded runs `gc bd list --json` in the city under the stub, in its own
// process group with a deadline (helpers.RunGCBounded). A timeout means the
// recursion is unbounded and fails the test.
func runGCBounded(t *testing.T, city, stub string) helpers.BoundedGCRun {
	t.Helper()
	env := testEnv.Clone().
		With("PATH", filepath.Dir(stub)+string(os.PathListSeparator)+testEnv.Get("PATH")).
		With("BD_BIN", stub).
		// The acceptance env defaults to the file provider, which answers `gc bd`
		// itself; the fuse is for the bd-backed passthrough.
		With("GC_BEADS", "bd").
		With("GC_CITY_PATH", city)
	run := helpers.RunGCBounded(t, env, city, recursionDeadline, "bd", "list", "--json")
	if run.TimedOut {
		t.Fatalf("gc bd did not finish within %s: the recursion is unbounded; stderr tail:\n%s", recursionDeadline, tail(run.Stderr, 2000))
	}
	return run
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func readStubDepths(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func requireNoSurvivors(t *testing.T, pgid int) {
	t.Helper()
	if !helpers.ProcessGroupGone(pgid, 10*time.Second) {
		t.Fatalf("processes of the gc/bd chain (group %d) survived the run", pgid)
	}
}

func TestBdRecursionFuseStopsASelfCallingStub(t *testing.T) {
	city := recursionFuseCity(t)
	log := filepath.Join(helpers.TempDir(t), "stub.log")
	stub := writeRecursiveStub(t, helpers.TempDir(t), mustGCPath(t), log, false)

	run := runGCBounded(t, city, stub)
	stderr, runErr, pgid := run.Stderr, run.Err, run.PGID

	runs := readStubDepths(t, log)
	if len(runs) != recursionFuseBound {
		t.Fatalf("stub ran %d times (backstop %d), want %d; stderr tail:\n%s", len(runs), recursionStubBackstop, recursionFuseBound, tail(stderr, 1500))
	}
	for i, got := range runs {
		if want := fmt.Sprintf("depth=%d", i+1); got != want {
			t.Errorf("stub run %d saw %q, want %q", i+1, got, want)
		}
	}
	if runErr == nil {
		t.Errorf("gc bd exited 0 after hitting the bound, want a non-zero exit")
	}
	for _, want := range []string{stub, "GC_BD_DEPTH", fmt.Sprint(recursionFuseBound)} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not name %q:\n%s", want, tail(stderr, 1500))
		}
	}
	requireNoSurvivors(t, pgid)
}

// The incident's literal stub hides gc's stderr and exit status behind a
// pipeline, so nothing in the chain can notice a failure and stop. The bound
// has to hold anyway.
func TestBdRecursionFuseStopsTheIncidentShapedStub(t *testing.T) {
	city := recursionFuseCity(t)
	log := filepath.Join(helpers.TempDir(t), "stub.log")
	stub := writeRecursiveStub(t, helpers.TempDir(t), mustGCPath(t), log, true)

	run := runGCBounded(t, city, stub)
	stderr, pgid := run.Stderr, run.PGID

	if runs := readStubDepths(t, log); len(runs) != recursionFuseBound {
		t.Fatalf("incident-shaped stub ran %d times (backstop %d), want %d; stderr tail:\n%s", len(runs), recursionStubBackstop, recursionFuseBound, tail(stderr, 1500))
	}
	requireNoSurvivors(t, pgid)
}

// A bd wrapper that passes straight through to the real bd must not change the
// count: one gc launch is one level, whatever sits between gc and bd.
func TestBdRecursionFusePassThroughWrapperCountsOnce(t *testing.T) {
	city := recursionFuseCity(t)
	dir := helpers.TempDir(t)
	log := filepath.Join(helpers.TempDir(t), "stub.log")
	realBD := filepath.Join(dir, "real-bd")
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in version) echo 'bd version 1.3.2 (test)'; exit 0 ;; list) ;; *) exit 0 ;; esac\nprintf 'depth=%%s\\n' \"$GC_BD_DEPTH\" >> %q\necho '[]'\n", log)
	if err := os.WriteFile(realBD, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "bd")
	if err := os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nexec %q \"$@\"\n", realBD)), 0o755); err != nil {
		t.Fatal(err)
	}

	run := runGCBounded(t, city, wrapper)
	stderr, runErr, pgid := run.Stderr, run.Err, run.PGID

	if runErr != nil {
		t.Fatalf("gc bd through a pass-through wrapper failed: %v\n%s", runErr, tail(stderr, 1500))
	}
	if runs := readStubDepths(t, log); len(runs) != 1 || runs[0] != "depth=1" {
		t.Fatalf("real bd behind the wrapper saw %v, want exactly one run at depth=1", runs)
	}
	requireNoSurvivors(t, pgid)
}

func mustGCPath(t *testing.T) string {
	t.Helper()
	p, err := helpers.ResolveGCPath(testEnv)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
