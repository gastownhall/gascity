package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// singleExecFakeBd logs every invocation's argv, one line each, and answers
// like bd: a bead array for show, an empty array for list, and an exit code
// of 3 for the id "demo-missing".
const singleExecFakeBd = `#!/bin/sh
printf '%s\n' "$*" >> "${BD_CALL_LOG}"
case " $* " in
  *" demo-missing "*) echo 'no issue found' >&2; echo 'not-found-stdout'; exit 3 ;;
esac
case "$1" in
  show) echo '[{"id":"demo-abc","title":"t","status":"open","issue_type":"task"}]' ;;
  list) echo '[]' ;;
  update) echo '{"id":"demo-abc"}' ;;
  *) : ;;
esac
`

// newSingleExecTestCity builds a city with a bound rig, a fake bd on PATH that
// logs its argv, and cwd at the city root. It returns the call log path.
func newSingleExecTestCity(t *testing.T) string {
	t.Helper()
	disableManagedDoltRecoveryForTest(t)
	origCityFlag, origRigFlag := cityFlag, rigFlag
	t.Cleanup(func() { cityFlag, rigFlag = origCityFlag, origRigFlag })
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
prefix = "demo"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(singleExecFakeBd), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "bd-calls.log")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_CALL_LOG", log)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_RIG", "")
	setCwd(t, cityDir)
	return log
}

// singleExecBdCalls returns the logged bd argvs, dropping `context` probes (preflight's
// bd context, which is not a bead read).
func singleExecBdCalls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" || strings.HasPrefix(line, "context") {
			continue
		}
		calls = append(calls, line)
	}
	return calls
}

// TestGcBdPassthroughRunsBdOnce pins that a `gc bd` read whose bead-ID
// auto-detect cannot change the resolved store invokes bd exactly once: the
// passthrough itself, with its stdout and exit code preserved. It used to run
// `bd show --json <id>` first, only to confirm the id lived in the store the
// command was going to use anyway.
func TestGcBdPassthroughRunsBdOnce(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
	}{
		{"show json", []string{"show", "demo-abc", "--json"}, 0, `"id":"demo-abc"`},
		{"show human", []string{"show", "demo-abc"}, 0, `"id":"demo-abc"`},
		{"show exit code", []string{"show", "demo-missing"}, 3, "not-found-stdout"},
		{"list", []string{"list", "--json"}, 0, "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := newSingleExecTestCity(t)
			var stdout, stderr bytes.Buffer
			if got := doBd(tc.args, &stdout, &stderr); got != tc.wantCode {
				t.Fatalf("doBd(%v) = %d, want %d; stderr=%q", tc.args, got, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout.String(), tc.wantStdout)
			}
			calls := singleExecBdCalls(t, log)
			if len(calls) != 1 || calls[0] != strings.Join(tc.args, " ") {
				t.Fatalf("bd invocations = %q, want exactly the passthrough %q", calls, strings.Join(tc.args, " "))
			}
		})
	}
}

// TestGcBdUpdateRunsPassthroughOnce pins the write path: the passthrough runs
// once and the only other bd call is the deliberate exact-ID collision guard
// (gcy-g4o), one read. The scope probe no longer adds a second read.
func TestGcBdUpdateRunsPassthroughOnce(t *testing.T) {
	log := newSingleExecTestCity(t)
	args := []string{"update", "demo-abc", "--status", "in_progress"}
	var stdout, stderr bytes.Buffer
	if got := doBd(args, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(update) = %d, want 0; stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"id":"demo-abc"`) {
		t.Fatalf("stdout = %q, want the passthrough's output", stdout.String())
	}
	var updates, reads int
	for _, call := range singleExecBdCalls(t, log) {
		switch {
		case strings.HasPrefix(call, "update "):
			updates++
		case strings.HasPrefix(call, "show "):
			reads++
		default:
			t.Fatalf("unexpected bd invocation %q", call)
		}
	}
	if updates != 1 || reads != 1 {
		t.Fatalf("bd update invocations = %d, guard reads = %d; want 1 and 1 (calls %q)", updates, reads, singleExecBdCalls(t, log))
	}
}

// TestResolveBdScopeTargetSkipsProbesThatCannotChangeTheAnswer pins the probe
// rule directly: an existence probe runs only when its answer could pick a
// different store than the rest of the chain would.
func TestResolveBdScopeTargetSkipsProbesThatCannotChangeTheAnswer(t *testing.T) {
	origProbe := bdBeadExists
	t.Cleanup(func() { bdBeadExists = origProbe })
	probes := 0
	exists := false
	bdBeadExists = func(string, *config.City, execStoreTarget, string) bool {
		probes++
		return exists
	}
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := func() *config.City {
		return &config.City{
			Workspace: config.Workspace{Name: "demo", Prefix: "demo"},
			Rigs:      []config.Rig{{Name: "frontend", Path: "frontend", Prefix: "fe"}},
		}
	}
	t.Setenv("GC_RIG", "")

	cases := []struct {
		name       string
		cwd        string
		gcRig      string
		args       []string
		exists     bool
		wantProbes int
		wantKind   string
	}{
		{"city id from city cwd", cityDir, "", []string{"show", "demo-abc"}, false, 0, "city"},
		{"rig id from rig cwd", rigDir, "", []string{"show", "fe-abc"}, false, 0, "rig"},
		// The b53658f shape: a city source bead read from a rig cwd still
		// probes, because the answer decides between the two stores.
		{"city id from rig cwd, present", rigDir, "", []string{"show", "demo-abc"}, true, 1, "city"},
		{"city id from rig cwd, absent", rigDir, "", []string{"show", "demo-abc"}, false, 1, "rig"},
		{"rig id from city cwd, present", cityDir, "", []string{"show", "fe-abc"}, true, 1, "rig"},
		{"rig id from city cwd, absent", cityDir, "", []string{"show", "fe-abc"}, false, 1, "city"},
		{"GC_RIG selects rig, city id", cityDir, "frontend", []string{"show", "demo-abc"}, true, 1, "city"},
		{"discarded GC_RIG keeps probing", cityDir, "nope", []string{"show", "demo-abc"}, true, 1, "city"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setCwd(t, tc.cwd)
			t.Setenv("GC_RIG", tc.gcRig)
			probes, exists = 0, tc.exists
			var stderr bytes.Buffer
			got, err := resolveBdScopeTarget(cfg(), cityDir, "", tc.args, false, &stderr)
			if err != nil {
				t.Fatalf("resolveBdScopeTarget() error = %v", err)
			}
			if probes != tc.wantProbes {
				t.Fatalf("probes = %d, want %d", probes, tc.wantProbes)
			}
			if got.ScopeKind != tc.wantKind {
				t.Fatalf("scope = %+v, want kind %q", got, tc.wantKind)
			}
		})
	}
}
