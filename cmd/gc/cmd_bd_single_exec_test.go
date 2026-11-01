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

// TestResolveBdScopeTargetRoutesReleaseIfCurrentByItsBead pins the store gc's
// own `release-if-current <issue-id> <assignee>` runs in: the issue's, named by
// the issue id's prefix, with no existence probe. The second argument is the
// expected assignee, an identity rather than a subject. On a city whose rigs
// keep their own stores it is often a session bead id with the city's prefix,
// and that session bead exists in the city store, so a probe that took it as a
// candidate sent the release of a rig bead to the city store.
func TestResolveBdScopeTargetRoutesReleaseIfCurrentByItsBead(t *testing.T) {
	origProbe := bdBeadExists
	t.Cleanup(func() { bdBeadExists = origProbe })
	probes := 0
	bdBeadExists = func(string, *config.City, execStoreTarget, string) bool {
		probes++
		return true
	}
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "demo"},
		Rigs:      []config.Rig{{Name: "frontend", Path: "frontend", Prefix: "fe"}},
	}
	for _, tc := range []struct {
		name     string
		cwd      string
		gcRig    string
		args     []string
		wantKind string
	}{
		{"rig bead held by a city session, city cwd", cityDir, "", []string{"release-if-current", "fe-abc", "demo-wisp-8ft"}, "rig"},
		{"rig bead held by a city session, rig cwd", rigDir, "", []string{"release-if-current", "fe-abc", "demo-wisp-8ft"}, "rig"},
		{"rig bead held by a city session, GC_RIG", cityDir, "frontend", []string{"release-if-current", "fe-abc", "demo-session-0123456789abcdef"}, "rig"},
		{"city bead held by a rig-prefixed id, rig cwd", rigDir, "", []string{"release-if-current", "demo-abc", "fe-wisp-xyz"}, "city"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCwd(t, tc.cwd)
			t.Setenv("GC_RIG", tc.gcRig)
			probes = 0
			got, err := resolveBdScopeTarget(cfg, cityDir, "", tc.args, false, io.Discard)
			if err != nil {
				t.Fatalf("resolveBdScopeTarget(%v) error = %v", tc.args, err)
			}
			if got.ScopeKind != tc.wantKind || probes != 0 {
				t.Fatalf("resolveBdScopeTarget(%v) = %+v after %d probes, want kind %q and no probe (the issue id's prefix names its store)", tc.args, got, probes, tc.wantKind)
			}
		})
	}
}

// TestResolveBdScopeTargetNeverProbesReleaseIfCurrentsAssignee pins the probe
// chain's half of the same rule. Where the prefix alone cannot answer (here the
// city and a rig carry the same prefix), the existence probes ask about the
// issue id only. The holder's session bead exists in the city store, so a probe
// of the assignee would answer "city" for a bead the rig holds.
func TestResolveBdScopeTargetNeverProbesReleaseIfCurrentsAssignee(t *testing.T) {
	origProbe := bdBeadExists
	t.Cleanup(func() { bdBeadExists = origProbe })
	var probed []string
	bdBeadExists = func(_ string, _ *config.City, target execStoreTarget, id string) bool {
		probed = append(probed, target.ScopeKind+":"+id)
		return (target.ScopeKind == "city" && id == "demo-wisp-8ft") || (target.ScopeKind == "rig" && id == "demo-abc")
	}
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "alpha"), 0o700); err != nil {
		t.Fatal(err)
	}
	setCwd(t, cityDir)
	t.Setenv("GC_RIG", "")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo", Prefix: "demo"},
		Rigs:      []config.Rig{{Name: "alpha", Path: "alpha", Prefix: "demo"}},
	}
	got, err := resolveBdScopeTarget(cfg, cityDir, "", []string{"release-if-current", "demo-abc", "demo-wisp-8ft"}, false, io.Discard)
	if err != nil {
		t.Fatalf("resolveBdScopeTarget() error = %v", err)
	}
	if got.ScopeKind != "rig" || got.RigName != "alpha" {
		t.Fatalf("scope = %+v after probes %q, want rig alpha, the store that holds demo-abc", got, probed)
	}
	for _, p := range probed {
		if strings.HasSuffix(p, ":demo-wisp-8ft") {
			t.Fatalf("probes = %q: the assignee was probed as a bead id", probed)
		}
	}
}

// releaseRouteFakeBd is a bd that answers per store, told apart by the
// BEADS_DIR gc pins for each scope: the rig store holds the work bead fe-abc,
// in progress and held by demo-wisp-8ft; the city store holds that holder's
// (closed) session bead and no fe-abc. Every invocation is logged as
// "<store>|<argv>".
const releaseRouteFakeBd = `#!/bin/sh
case "$1" in version) echo 'bd version 1.3.2 (test)'; exit 0 ;; esac
store=city
case "${BEADS_DIR}" in */frontend/.beads) store=rig ;; esac
printf '%s|%s\n' "$store" "$*" >> "${BD_CALL_LOG}"
case " $* " in
  *" fe-abc "*)
    if [ "$store" = rig ]; then
      case "$1" in
        show) echo '[{"id":"fe-abc","title":"t","status":"in_progress","assignee":"demo-wisp-8ft","issue_type":"task"}]'; exit 0 ;;
        update)
          case " $* " in *" --if-assignee demo-wisp-8ft "*) exit 0 ;; esac
          echo 'precondition failed: assignee mismatch' >&2; exit 13 ;;
      esac
    fi
    case "$1" in
      show) echo 'Issue fe-abc not found' >&2; exit 1 ;;
      update) echo 'Error resolving fe-abc: no issue found matching "fe-abc"' >&2; exit 1 ;;
    esac ;;
  *" demo-wisp-8ft "*)
    if [ "$store" = city ] && [ "$1" = show ]; then
      echo '[{"id":"demo-wisp-8ft","title":"s","status":"closed","issue_type":"session"}]'; exit 0
    fi
    echo 'Issue demo-wisp-8ft not found' >&2; exit 1 ;;
esac
case "$1" in query|list) echo '[]' ;; esac
exit 0
`

// newReleaseRouteTestCity is newSingleExecTestCity with releaseRouteFakeBd
// first on PATH. It returns the call log path.
func newReleaseRouteTestCity(t *testing.T) string {
	t.Helper()
	log := newSingleExecTestCity(t)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(releaseRouteFakeBd), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// TestGcBdReleaseIfCurrentReleasesARigBeadHeldByACitySession is the orphan
// sweep's release end to end: an unscoped `gc bd release-if-current` of a rig
// bead whose dead holder is a session bead in the city store. It must release
// the claim in the rig store; it used to run against the city store, which
// holds no such bead, and print "skipped" with exit 0, which the sweep reads as
// "someone else holds it" and keeps the dead claim forever.
func TestGcBdReleaseIfCurrentReleasesARigBeadHeldByACitySession(t *testing.T) {
	log := newReleaseRouteTestCity(t)
	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"release-if-current", "fe-abc", "demo-wisp-8ft"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(release-if-current) = %d, want 0; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "released" {
		t.Fatalf("stdout = %q, want released; bd calls = %q", stdout.String(), singleExecBdCalls(t, log))
	}
	var released bool
	for _, call := range singleExecBdCalls(t, log) {
		if strings.HasPrefix(call, "city|update fe-abc") {
			t.Fatalf("the release ran against the city store: %q", call)
		}
		if strings.HasPrefix(call, "rig|update fe-abc --if-assignee demo-wisp-8ft") {
			released = true
		}
	}
	if !released {
		t.Fatalf("no conditional release reached the rig store; bd calls = %q", singleExecBdCalls(t, log))
	}
}

// TestGcBdReleaseIfCurrentRefusesABeadTheStoreDoesNotHold pins the verdict's
// honesty: "skipped" means the store holds the bead and someone else does now.
// A release against a store that holds no such bead is an error, as `gc bd
// show` and `gc bd update` of that id are, never "skipped" with exit 0.
func TestGcBdReleaseIfCurrentRefusesABeadTheStoreDoesNotHold(t *testing.T) {
	newReleaseRouteTestCity(t)
	cityDir := os.Getenv("GC_CITY_PATH")
	var stdout, stderr bytes.Buffer
	got := doBd([]string{"--city", cityDir, "release-if-current", "fe-abc", "demo-wisp-8ft"}, &stdout, &stderr)
	if got != 1 {
		t.Fatalf("doBd(--city release-if-current <rig bead>) = %d, want 1; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
	if out := strings.TrimSpace(stdout.String()); out != "" {
		t.Fatalf("stdout = %q, want nothing: no verdict for a bead the store does not hold", out)
	}
	if !strings.Contains(stderr.String(), `"fe-abc"`) || !strings.Contains(stderr.String(), "the city store") {
		t.Fatalf("stderr = %q, want it to name the bead and the store that does not hold it", stderr.String())
	}
}

// TestGcBdReleaseIfCurrentSkipsAClaimSomeoneElseHolds is the other half: a bead
// the store holds under a different assignee is the CAS miss, "skipped" with
// exit 0, as before.
func TestGcBdReleaseIfCurrentSkipsAClaimSomeoneElseHolds(t *testing.T) {
	newReleaseRouteTestCity(t)
	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"release-if-current", "fe-abc", "worker-9"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(release-if-current) = %d, want 0; stdout=%q stderr=%q", got, stdout.String(), stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "skipped" {
		t.Fatalf("stdout = %q, want skipped", stdout.String())
	}
}
