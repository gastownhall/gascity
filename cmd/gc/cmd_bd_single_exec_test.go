package main

import (
	"bytes"
	"io"
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
# The split-city allowlist asks which bd build will run; answered unlogged so
# the call count stays the passthrough's own.
case "$1" in version) echo 'bd version 1.3.2 (test)'; exit 0 ;; esac
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
// rule directly. An existence probe opens a store and reads the bead, a whole
// bd round trip, so it runs only when its answer could pick a different store
// than the rest of the chain would. An id a by-id verb addresses never needs
// one: its prefix names its store, and the probe only ever told bead ids from
// id-shaped tokens that are not subjects.
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
		// The b53658f shape: a city source bead read from a rig cwd. The id's
		// prefix names the city store, present or not.
		{"city id from rig cwd", rigDir, "", []string{"show", "demo-abc"}, false, 0, "city"},
		{"city id json from rig cwd", rigDir, "", []string{"show", "demo-abc", "--json"}, true, 0, "city"},
		{"rig id from city cwd", cityDir, "", []string{"show", "fe-abc"}, false, 0, "rig"},
		{"city id write from rig cwd", rigDir, "", []string{"update", "demo-abc", "--set-metadata", "k=v"}, false, 0, "city"},
		{"rig id close from city cwd", cityDir, "", []string{"close", "fe-abc", "--reason", "done"}, false, 0, "rig"},
		{"GC_RIG selects rig, city id", cityDir, "frontend", []string{"show", "demo-abc"}, false, 0, "city"},
		{"discarded GC_RIG, city id", cityDir, "nope", []string{"show", "demo-abc"}, false, 0, "city"},
		// An id-shaped token that is not an addressed subject still probes:
		// only the bead's existence tells it from free text.
		{"id-shaped flag value from rig cwd, present", rigDir, "", []string{"create", "--parent", "demo-abc", "--title", "x"}, true, 1, "city"},
		{"id-shaped flag value from rig cwd, absent", rigDir, "", []string{"create", "--parent", "demo-abc", "--title", "x"}, false, 1, "rig"},
		// Subjects in one store route by prefix together; subjects in two
		// different stores give no single answer by prefix.
		{"subjects in one rig store", cityDir, "", []string{"close", "fe-abc", "fe-def"}, false, 0, "rig"},
		{"subjects in two stores", cityDir, "", []string{"close", "demo-abc", "fe-abc"}, true, 1, "city"},
		// bd resolves its store from -C over the BEADS_DIR the target pins, so
		// an argv naming a directory keeps the chain, whose -C arm answers a
		// probe miss with the store bd will act on.
		{"city id write with -C rig dir", cityDir, "", []string{"update", "demo-abc", "-C", rigDir, "--set-metadata", "k=v"}, false, 1, "rig"},
		{"city id read with --directory= rig dir", cityDir, "", []string{"show", "demo-abc", "--directory=" + rigDir}, false, 1, "rig"},
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

// TestResolveBdScopeTargetProbesAPrefixTwoStoresShare pins where the prefix
// alone cannot answer: when the city and a rig, or two rigs, carry the same
// prefix, only the bead's existence tells their stores apart, so the prefix
// route declines and the probe chain decides.
func TestResolveBdScopeTargetProbesAPrefixTwoStoresShare(t *testing.T) {
	cases := []struct {
		name string
		id   string
		rigs []config.Rig
	}{
		{"two rigs", "shared-abc", []config.Rig{
			{Name: "alpha", Path: "alpha", Prefix: "shared"},
			{Name: "beta", Path: "beta", Prefix: "shared"},
		}},
		{"city and rig", "demo-abc", []config.Rig{
			{Name: "alpha", Path: "alpha", Prefix: "demo"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origProbe := bdBeadExists
			t.Cleanup(func() { bdBeadExists = origProbe })
			probes := 0
			bdBeadExists = func(string, *config.City, execStoreTarget, string) bool {
				probes++
				return false
			}
			cityDir := t.TempDir()
			for _, rig := range tc.rigs {
				if err := os.MkdirAll(filepath.Join(cityDir, rig.Path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			setCwd(t, cityDir)
			t.Setenv("GC_RIG", "")
			cfg := &config.City{Workspace: config.Workspace{Name: "demo", Prefix: "demo"}, Rigs: tc.rigs}
			if _, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"show", tc.id}, false, io.Discard); err != nil {
				t.Fatalf("resolveBdScopeTarget() error = %v", err)
			}
			if probes == 0 {
				t.Fatalf("no existence probe ran: the prefix route answered for a prefix two stores share")
			}
		})
	}
}

// TestResolveBdScopeTargetNeverAliasesAnUnboundRigByPrefix pins that a rig
// declared without a path binding owns no store the prefix route could name:
// its ids stay on the chain that never aliases an unbound rig to the city.
func TestResolveBdScopeTargetNeverAliasesAnUnboundRigByPrefix(t *testing.T) {
	origProbe := bdBeadExists
	t.Cleanup(func() { bdBeadExists = origProbe })
	bdBeadExists = func(string, *config.City, execStoreTarget, string) bool { return true }
	cityDir := t.TempDir()
	setCwd(t, cityDir)
	t.Setenv("GC_RIG", "")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "demo"},
		Rigs:      []config.Rig{{Name: "frontend", Prefix: "fe"}},
	}
	got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"show", "fe-abc"}, false, io.Discard)
	if err != nil {
		t.Fatalf("resolveBdScopeTarget() error = %v", err)
	}
	if got.ScopeKind != "city" || got.RigName != "" {
		t.Fatalf("scope = %+v, want the city store (an unbound rig is never a target)", got)
	}
}

// TestGcBdCrossScopeByIDRunsBdOnce pins the cross-scope shape agents hit most:
// a rig worktree reading or writing a city source bead (and the reverse). The
// subject's prefix names its store, so a read is the passthrough alone and a
// write is the passthrough plus the deliberate exact-ID guard read; neither
// pays an existence probe first. Each probe was a full bd round trip (~0.46 s
// against a remote serve, ~1 s against a Dolt rig opened native).
func TestGcBdCrossScopeByIDRunsBdOnce(t *testing.T) {
	cases := []struct {
		name      string
		fromRig   bool
		args      []string
		wantCalls []string
		wantScope string
	}{
		{"city bead read from a rig cwd", true, []string{"show", "demo-abc", "--json"}, []string{"show demo-abc --json"}, "city"},
		{"rig bead read from the city cwd", false, []string{"show", "fe-abc"}, []string{"show fe-abc"}, `rig "frontend"`},
		{"city bead write from a rig cwd", true, []string{"update", "demo-abc", "--status", "in_progress"}, []string{"show --json demo-abc", "update demo-abc --status in_progress"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := newSingleExecTestCity(t)
			if tc.fromRig {
				setCwd(t, filepath.Join(os.Getenv("GC_CITY_PATH"), "frontend"))
			}
			var stdout, stderr bytes.Buffer
			if got := doBd(tc.args, &stdout, &stderr); got != 0 {
				t.Fatalf("doBd(%v) = %d, want 0; stderr=%q", tc.args, got, stderr.String())
			}
			calls := singleExecBdCalls(t, log)
			if strings.Join(calls, "\n") != strings.Join(tc.wantCalls, "\n") {
				t.Fatalf("bd invocations = %q, want %q", calls, tc.wantCalls)
			}
			if tc.wantScope != "" && !strings.Contains(stderr.String(), "answering from the "+tc.wantScope+" store") {
				t.Fatalf("stderr = %q, want the %s store to answer", stderr.String(), tc.wantScope)
			}
		})
	}
}

// TestGcBdPassthroughLoadsCityConfigOnce pins the passthrough's config cost: a
// `gc bd` invocation composes the city config (packs, includes, site binding)
// once, and every guard and env builder before the exec shares it. The
// passthrough used to compose it once per helper (doBd, the by-id twin census,
// the pinned-bd resolver twice, the hosted-credential probe), about 60 ms each
// on a production-sized city, which was most of gc's own share of the command.
//
// The probe's own load count keeps it on the measured path: its one load runs
// inside the scope. That the load is a memo answer rather than a composition of
// its own is pinned by TestHostedCredentialProbeReadsThroughTheInvocationMemo,
// because these counts cannot tell the two apart: a direct
// config.LoadWithIncludesOptions call moves them exactly as a memo answer
// does, the probe's by one and the composed count by none.
func TestGcBdPassthroughLoadsCityConfigOnce(t *testing.T) {
	for _, args := range [][]string{
		{"show", "demo-abc"},
		{"show", "demo-abc", "--json"},
		{"list", "--json"},
		{"show", "fe-abc"},
		{"update", "demo-abc", "--status", "in_progress"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			newSingleExecTestCity(t)
			// The probe's cache is process-wide; start it empty so this
			// passthrough's probe loads.
			resetHostedCredentialProbeCache()
			t.Cleanup(resetHostedCredentialProbeCache)
			before := composedCityConfigLoads.Load()
			probesBefore := hostedCredentialProbeLoads.Load()
			var stdout, stderr bytes.Buffer
			if got := doBd(args, &stdout, &stderr); got != 0 {
				t.Fatalf("doBd(%v) = %d, want 0; stderr=%q", args, got, stderr.String())
			}
			if loads := composedCityConfigLoads.Load() - before; loads != 1 {
				t.Fatalf("doBd(%v) composed the city config %d times, want 1", args, loads)
			}
			if probes := hostedCredentialProbeLoads.Load() - probesBefore; probes != 1 {
				t.Fatalf("doBd(%v) ran the hosted-credential probe's load %d times, want 1", args, probes)
			}
			if currentInvocationConfigMemo() != nil {
				t.Fatal("doBd left its invocation config memo open")
			}
		})
	}
}
