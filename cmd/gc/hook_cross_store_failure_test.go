package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// These tests pin the multi-store half of "an empty queue and a failed query are
// different answers". The hook runs the work query against every store in its
// fan-out (hookWorkQueryStores). A secondary store whose read failed used to be
// skipped silently, so a primary that answered "[]" made the whole fan-out read
// as no work: plain `gc hook` exited 1 and `gc hook --claim` drained the seat
// as no_work, though a store that could hold this agent's work had never been
// read. A failed store is now a failed query unless another store has work.

// twoStoreFanOut is a primary store and one secondary (rig) store.
func twoStoreFanOut() []hookStore {
	return []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}},
	}
}

// primaryAnswersSecondaryFails runs a fan-out whose primary answers primaryOut
// and whose secondary store "riga" cannot be read.
func primaryAnswersSecondaryFails(t *testing.T, primaryOut string) hookStoreRunner {
	t.Helper()
	return func(_, dir string, _ []string) (string, error) {
		switch dir {
		case "city":
			return primaryOut, nil
		case "riga":
			return "", errTestStoreTimeout
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}
}

const routedCityRow = `[{"id":"hw-city","status":"open","metadata":{"gc.routed_to":"worker"}}]`

func TestBestStoreWithWorkReportsAFailedSecondaryWhenNoStoreHasWork(t *testing.T) {
	stores := twoStoreFanOut()
	out, selected, err := bestStoreWithWork("q", stores, stores[0], primaryAnswersSecondaryFails(t, "[]"))
	if err == nil {
		t.Fatalf("bestStoreWithWork = (%q, %+v, nil); a fan-out with an unread store and no work anywhere must be an error, not \"no work\"", out, selected)
	}
	if !strings.Contains(err.Error(), "riga") {
		t.Errorf("error = %q, want the failed store named", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("output = %q, want none: \"[]\" next to an error still reads as an empty queue to a careless caller", out)
	}
}

// TestHookDiscoveryReportsAFailedSecondaryStore is plain `gc hook`: the
// work-query-failed exit, not the no-work exit 1, with the store named.
func TestHookDiscoveryReportsAFailedSecondaryStore(t *testing.T) {
	stores := twoStoreFanOut()
	var stdout, stderr bytes.Buffer
	runner := hookDiscoveryRunner(stores, primaryAnswersSecondaryFails(t, "[]"), func(string, error) {}, &stderr)
	code := doHook("q", "", false, runner, &stdout, &stderr, hookVisibility{})
	if code != hookExitWorkQueryFailed {
		t.Fatalf("gc hook over a failed secondary store = exit %d, want %d; stdout=%q stderr=%s", code, hookExitWorkQueryFailed, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "[]") {
		t.Errorf("stdout = %q, want no empty-queue answer", stdout.String())
	}
	if !strings.Contains(stderr.String(), "riga") {
		t.Errorf("stderr = %q, want the failed store named", stderr.String())
	}
}

// TestHookDiscoveryServesWorkAndLogsAFailedSecondaryStore: when another store
// does have work, serving it is right, but the unread store is still reported.
func TestHookDiscoveryServesWorkAndLogsAFailedSecondaryStore(t *testing.T) {
	stores := twoStoreFanOut()
	var stdout, stderr bytes.Buffer
	runner := hookDiscoveryRunner(stores, primaryAnswersSecondaryFails(t, routedCityRow), func(string, error) {}, &stderr)
	code := doHook("q", "", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 0 {
		t.Fatalf("gc hook with work in the primary = exit %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hw-city") {
		t.Errorf("stdout = %q, want the primary's work served", stdout.String())
	}
	if !strings.Contains(stderr.String(), "riga") {
		t.Errorf("stderr = %q, want the unread store reported even though work was served", stderr.String())
	}
}

func secondaryFailureClaimOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}
}

func zeroClaimRetryPacing(t *testing.T) {
	t.Helper()
	prev := hookClaimQueryRetryInterval
	hookClaimQueryRetryInterval = 0
	t.Cleanup(func() { hookClaimQueryRetryInterval = prev })
}

// TestClaimHookWorkDoesNotDrainWhenASecondaryStoreFails is `gc hook --claim`:
// exit 1 with nothing on stdout, never a no_work drain or a drain-ack.
func TestClaimHookWorkDoesNotDrainWhenASecondaryStoreFails(t *testing.T) {
	zeroClaimRetryPacing(t)
	stores := twoStoreFanOut()
	acked := false
	ops := hookClaimOps{
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Fatal("nothing to claim, yet a claim ran")
			return beads.Bead{}, false, nil
		},
		DrainAck: func(io.Writer) error { acked = true; return nil },
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("q", "city", stores[0].env, stores, secondaryFailureClaimOpts(), ops, primaryAnswersSecondaryFails(t, "[]"), func(string, error) {}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("claim over a failed secondary store = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no protocol result: a drain here retires a seat on a read that never happened", stdout.String())
	}
	if acked {
		t.Fatal("the seat acknowledged a drain over an unread store")
	}
	if !strings.Contains(stderr.String(), "riga") {
		t.Errorf("stderr = %q, want the failed store named", stderr.String())
	}
}

// TestClaimHookWorkServesWorkAndLogsAFailedSecondaryStore: the claim takes the
// work it can see and still reports the store it could not read.
func TestClaimHookWorkServesWorkAndLogsAFailedSecondaryStore(t *testing.T) {
	zeroClaimRetryPacing(t)
	stores := twoStoreFanOut()
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("q", "city", stores[0].env, stores, secondaryFailureClaimOpts(), ops, primaryAnswersSecondaryFails(t, routedCityRow), func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claim with work in the primary = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != "hw-city" {
		t.Fatalf("claim result = %+v, want work hw-city", result)
	}
	if !strings.Contains(stderr.String(), "riga") {
		t.Errorf("stderr = %q, want the unread store reported even though work was claimed", stderr.String())
	}
}

// downRigFanOut is a primary store and one secondary rig leg, "riga", whose
// rig's managed Dolt runtime the tests declare down (stubRigRuntimeDown).
func downRigFanOut() []hookStore {
	return []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}, rig: "riga"},
	}
}

// stubRigRuntimeDown stands in for the runtime probe: the rigs in down have no
// running managed Dolt runtime. It returns how often each rig was asked.
func stubRigRuntimeDown(t *testing.T, down ...string) map[string]int {
	t.Helper()
	asked := map[string]int{}
	prev := hookRigRuntimeUnavailable
	hookRigRuntimeUnavailable = func(_ string, _ *config.City, rig string) bool {
		asked[rig]++
		return slices.Contains(down, rig)
	}
	t.Cleanup(func() { hookRigRuntimeUnavailable = prev })
	return asked
}

// TestBestStoreWithWorkAnswersWithoutARigWhoseRuntimeIsDown: a rig leg whose
// managed Dolt runtime is not running does not hold the answer up, so a seat
// with no other work drains; the failure is still noted. Every other failed rig
// still holds it (TestBestStoreWithWorkReportsAFailedSecondaryWhenNoStoreHasWork).
func TestBestStoreWithWorkAnswersWithoutARigWhoseRuntimeIsDown(t *testing.T) {
	stores := downRigFanOut()
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "riga" {
			return "", fmt.Errorf("%w: %w", errHookRigRuntimeUnavailable, errTestStoreTimeout)
		}
		return "[]", nil
	}
	var noted []*hookStoreReadError
	out, selected, err := bestStoreWithWork("q", stores, stores[0], run, func(f *hookStoreReadError) { noted = append(noted, f) })
	if err != nil {
		t.Fatalf("bestStoreWithWork = %v; a rig whose runtime is down must not fail the fan-out", err)
	}
	if out != "[]" || !isZeroHookStore(selected) {
		t.Fatalf("bestStoreWithWork = (%q, %+v), want the primary's empty answer", out, selected)
	}
	if len(noted) != 1 || noted[0].rig != "riga" || !errors.Is(noted[0], errTestStoreTimeout) {
		t.Fatalf("noted = %v, want riga's failed read", noted)
	}
}

// TestHookRigLegRunnerMarksOnlyARigWhoseRuntimeIsDown pins which failed reads
// are marked errHookRigRuntimeUnavailable: a secondary rig leg whose runtime the
// probe reports down, asked once per rig however often the leg is read, and
// never the agent's own (first) store, never a rig whose runtime is up, never a
// leg that answered.
func TestHookRigLegRunnerMarksOnlyARigWhoseRuntimeIsDown(t *testing.T) {
	asked := stubRigRuntimeDown(t, "riga")
	stores := []hookStore{
		{dir: "riga-own", env: []string{"GC_STORE=riga-own"}, rig: "riga"},
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}, rig: "riga"},
		{dir: "rigb", env: []string{"GC_STORE=rigb"}, rig: "rigb"},
		{dir: "rigc", env: []string{"GC_STORE=rigc"}, rig: "rigc"},
	}
	run := hookRigLegRunner("/city", nil, stores, func(_, dir string, _ []string) (string, error) {
		if dir == "rigc" {
			return "[]", nil
		}
		return "", errTestStoreTimeout
	})
	for round := 0; round < 3; round++ {
		for _, st := range stores {
			_, err := run("q", st.dir, st.env)
			marked := errors.Is(err, errHookRigRuntimeUnavailable)
			switch st.dir {
			case "riga":
				if !marked || !errors.Is(err, errTestStoreTimeout) {
					t.Fatalf("riga read = %v, want the read's own failure marked runtime-unavailable", err)
				}
			case "rigc":
				if err != nil {
					t.Fatalf("rigc read = %v, want its answer", err)
				}
			default:
				if marked || !errors.Is(err, errTestStoreTimeout) {
					t.Fatalf("%s read = %v, want its failure unmarked", st.dir, err)
				}
			}
		}
	}
	if asked["riga"] != 1 || asked["rigb"] != 1 || asked["rigc"] != 0 || len(asked) != 2 {
		t.Fatalf("runtime asked %v, want riga and rigb once each and nothing for a leg that answered", asked)
	}
}

// TestClaimHookWorkDrainsWhenTheOnlyFailedRigHasItsRuntimeDown is the claim
// door: with no work anywhere and only a down rig unread, the seat drains
// no_work and the rig is named on stderr once, though the bounded retry and
// re-reads run.
func TestClaimHookWorkDrainsWhenTheOnlyFailedRigHasItsRuntimeDown(t *testing.T) {
	zeroClaimRetryPacing(t)
	stubRigRuntimeDown(t, "riga")
	stores := downRigFanOut()
	h := newFailureHookHarness()
	var emitted []error
	var stdout, stderr bytes.Buffer
	run := hookRigLegRunner("/city", nil, stores, primaryAnswersSecondaryFails(t, "[]"))
	code := claimHookWorkWithRunner("q", "city", stores[0].env, stores, secondaryFailureClaimOpts(), h.ops(), run, func(_ string, err error) { emitted = append(emitted, err) }, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claim with only a down rig unread = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork || !h.drained {
		t.Fatalf("claim result = %+v drained=%v, want a no_work drain acknowledged", result, h.drained)
	}
	if got := strings.Count(stderr.String(), `rig "riga"`); got != 1 {
		t.Fatalf("stderr names riga %d times, want once: %s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), errHookRigRuntimeUnavailable.Error()) {
		t.Errorf("stderr = %s, want the reason riga was not waited on", stderr.String())
	}
	if len(emitted) != 0 {
		t.Errorf("emitted %v, want no failed-query event: the query answered", emitted)
	}
}

// TestClaimHookWorkRecordsEachFailedRigOnceAcrossRetries: a rig that keeps
// failing the claim's bounded retries is one failed query, recorded as one
// event naming that rig, with a bounded reason.
func TestClaimHookWorkRecordsEachFailedRigOnceAcrossRetries(t *testing.T) {
	zeroClaimRetryPacing(t)
	stubRigRuntimeDown(t)
	stores := []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}, rig: "riga"},
		{dir: "rigb", env: []string{"GC_STORE=rigb"}, rig: "rigb"},
	}
	reads := map[string]int{}
	run := hookRigLegRunner("/city", nil, stores, func(_, dir string, _ []string) (string, error) {
		reads[dir]++
		if dir == "city" {
			return "[]", nil
		}
		return "", fmt.Errorf("running work query %q: exit status 1: bd: %s store unreadable\nsecond line", strings.Repeat("q", 4096), dir)
	})
	ops := hookClaimOps{DrainAck: func(io.Writer) error { t.Fatal("drain acknowledged over unread rigs"); return nil }}
	rec := events.NewFake()
	emit := func(command string, err error) { emitWorkQueryFailure(rec, "gcs-1", "worker", command, err) }
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("q", "city", stores[0].env, stores, secondaryFailureClaimOpts(), ops, run, emit, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("claim over failing rigs = %d stdout=%q, want 1 with no result; stderr=%s", code, stdout.String(), stderr.String())
	}
	if reads["riga"] < 2 {
		t.Fatalf("riga read %d times, want the bounded retry to re-read it", reads["riga"])
	}
	if len(rec.Events) != 2 {
		t.Fatalf("events = %+v, want one per failed rig", rec.Events)
	}
	for i, rig := range []string{"riga", "rigb"} {
		ev := rec.Events[i]
		if ev.Type != events.SessionWorkQueryFailed {
			t.Fatalf("event %d type = %q, want %q", i, ev.Type, events.SessionWorkQueryFailed)
		}
		want := fmt.Sprintf("work query against rig %q store %s failed: exit status 1: bd: %s store unreadable", rig, rig, rig)
		if ev.Message != want {
			t.Fatalf("event %d reason = %q, want %q", i, ev.Message, want)
		}
	}
	if strings.Contains(stderr.String(), strings.Repeat("q", 64)) || strings.Contains(stderr.String(), "second line") {
		t.Fatalf("stderr carries the quoted work query or stderr past its first line: %s", stderr.String())
	}
}

// TestHookStoreReadErrorIsBounded runs a real failing work query: the report
// names the store, how the read ended and the first stderr line, and drops the
// command line (a generated work query runs to kilobytes) and later lines.
func TestHookStoreReadErrorIsBounded(t *testing.T) {
	command := ": " + strings.Repeat("x", 16<<10) + "; echo 'bd: store unreadable' >&2; echo 'second line' >&2; exit 3"
	_, err := shellWorkQueryWithEnv(command, t.TempDir(), nil)
	if err == nil {
		t.Fatal("precondition: the work query must fail")
	}
	failure := &hookStoreReadError{dir: "/city/rigs/riga", rig: "riga", err: err}
	want := `work query against rig "riga" store /city/rigs/riga failed: exit status 3: bd: store unreadable`
	if got := failure.Error(); got != want {
		t.Fatalf("report = %q, want %q", got, want)
	}
	if got := (&hookSecondaryStoreFailure{failures: []*hookStoreReadError{failure}}).Error(); got != want {
		t.Fatalf("aggregate report = %q, want %q", got, want)
	}
	if !strings.Contains(err.Error(), strings.Repeat("x", 64)) {
		t.Fatalf("precondition: the read's own error should still quote the command: %.200s", err)
	}

	long := &hookStoreReadError{dir: "d", err: errors.New(strings.Repeat("é", 400))}
	if got := workQueryFailureSummary(long.err); len(got) > hookStoreFailureSummaryMax+len("…") || !utf8.ValidString(got) {
		t.Fatalf("summary of a long one-line error = %d bytes (valid UTF-8: %v), want at most %d plus the ellipsis", len(got), utf8.ValidString(got), hookStoreFailureSummaryMax)
	}
}

// TestSecondaryStoreFailureClassifiesEachStoreWhole: the aggregate's message is
// bounded to each store's first stderr line, but whether the store answered is
// read from the whole failure, so a connection error on a later line still
// carries the store-unavailable token and exit 2.
func TestSecondaryStoreFailureClassifiesEachStoreWhole(t *testing.T) {
	refused := errors.New("running work query \"q\": exit status 1: bd: cannot read issues\ndial tcp 127.0.0.1:3307: connect: connection refused")
	aggregate := &hookSecondaryStoreFailure{failures: []*hookStoreReadError{{dir: "riga", rig: "riga", err: refused}}}
	if strings.Contains(aggregate.Error(), "connection refused") {
		t.Fatalf("precondition: the bounded report should stop at the first stderr line: %q", aggregate.Error())
	}
	if !workQueryStoreUnavailable(aggregate) {
		t.Fatalf("aggregate over a refused connection classifies as answered: %v", aggregate)
	}
	var stderr bytes.Buffer
	if code := reportClaimWorkQueryFailure(hookClaimRetryWindowClosed(aggregate, &hookClaimOps{}), &stderr); code != 2 || !strings.Contains(stderr.String(), hookStoreUnavailableToken) {
		t.Fatalf("claim report = %d %q, want exit 2 with %s", code, stderr.String(), hookStoreUnavailableToken)
	}

	refusal := &hookSecondaryStoreFailure{failures: []*hookStoreReadError{{dir: "rigb", err: errors.New("running work query \"q\": exit status 1: bd: unknown flag --frobnicate")}}}
	if workQueryStoreUnavailable(refusal) {
		t.Fatalf("a store that answered with a refusal classifies as unavailable: %v", refusal)
	}
}

// TestHookWorkQueryStoresSkipsSuspendedRigs: a city-scoped agent's fan-out
// leaves out a suspended rig — the frame gc ready reads — and names the rig of
// every rig leg it builds.
func TestHookWorkQueryStoresSkipsSuspendedRigs(t *testing.T) {
	prev := hookStoreQueryEnv
	hookStoreQueryEnv = func(_ string, _ *config.City, a *config.Agent) (map[string]string, error) {
		return map[string]string{"GC_STORE": "leg-" + a.Dir}, nil
	}
	t.Cleanup(func() { hookStoreQueryEnv = prev })
	cityDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "riga", Path: filepath.Join(cityDir, "rigs", "riga")},
		{Name: "rigb", Path: filepath.Join(cityDir, "rigs", "rigb")},
	}}
	agent := &config.Agent{Name: "worker", Scope: "city"}
	rigsOf := func(stores []hookStore) []string {
		var rigs []string
		for _, st := range stores[1:] {
			rigs = append(rigs, st.rig)
		}
		return rigs
	}

	all := hookWorkQueryStores(cityDir, cfg, agent, "worker", cityDir, nil, nil, nil)
	if got := rigsOf(all); !slices.Equal(got, []string{"riga", "rigb"}) {
		t.Fatalf("fan-out rigs = %v, want [riga rigb]", got)
	}
	serving := hookWorkQueryStores(cityDir, cfg, agent, "worker", cityDir, nil, nil, map[string]bool{"riga": true})
	if got := rigsOf(serving); !slices.Equal(got, []string{"rigb"}) {
		t.Fatalf("fan-out rigs with riga suspended = %v, want [rigb]", got)
	}
}

// TestHookCommandDoesNotSearchASuspendedRig drives plain `gc hook` over a city
// whose only rig store cannot be read: while the rig is serving, the failed
// read is the answer (exit 2, the rig named); once the rig is suspended, the
// hook does not search it and the empty queue is the answer (exit 1).
func TestHookCommandDoesNotSearchASuspendedRig(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCityAgents(t, `[[rigs]]
name = "riga"
path = "rigs/riga"

[[agent]]
name = "worker"
scope = "city"
`)
	if err := os.MkdirAll(filepath.Join(cityDir, "rigs", "riga"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", cityDir)
	fakeBin := t.TempDir()
	script := "#!/bin/sh\ncase \"$GC_RIG_ROOT\" in */rigs/riga) printf 'bd: rig store unreadable\\n' >&2; exit 1;; esac\nprintf '[]'\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	const readFailure = `work query against rig "riga"`
	var stdout, stderr bytes.Buffer
	if code := cmdHookWithOptions([]string{"worker"}, hookCommandOptions{}, &stdout, &stderr); code != hookExitWorkQueryFailed || !strings.Contains(stderr.String(), readFailure) {
		t.Fatalf("gc hook over a serving rig that cannot be read = exit %d, want %d naming riga's read; stdout=%q stderr=%s", code, hookExitWorkQueryFailed, stdout.String(), stderr.String())
	}

	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityDir, "riga", &suspended); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdHookWithOptions([]string{"worker"}, hookCommandOptions{}, &stdout, &stderr); code != 1 {
		t.Fatalf("gc hook with the unreadable rig suspended = exit %d, want 1 (empty queue); stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), readFailure) || strings.Contains(stderr.String(), "rig store unreadable") {
		t.Errorf("stderr = %s, want the suspended rig not searched at all", stderr.String())
	}
}

// TestHookCommandEnvBuildFailureIsAFailedQuery: a work-query environment that
// cannot be built means the query never ran. Plain `gc hook` reports that as a
// failed query, not the empty-queue exit 1 a caller drains on, and the claim
// door writes no drain result.
func TestHookCommandEnvBuildFailureIsAFailedQuery(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_CITY", writeFenceTestCity(t))
	prev := hookStoreQueryEnv
	hookStoreQueryEnv = func(string, *config.City, *config.Agent) (map[string]string, error) {
		return nil, errors.New("resolving the store's Dolt endpoint: invalid canonical config")
	}
	t.Cleanup(func() { hookStoreQueryEnv = prev })

	var stdout, stderr bytes.Buffer
	if code := cmdHookWithOptions([]string{"worker"}, hookCommandOptions{}, &stdout, &stderr); code != hookExitWorkQueryFailed {
		t.Fatalf("gc hook with an unbuildable query env = exit %d, want %d; stderr=%s", code, hookExitWorkQueryFailed, stderr.String())
	}
	if !strings.Contains(stderr.String(), "building work query env") {
		t.Errorf("stderr = %q, want the failure named", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdHookWithOptions([]string{"worker"}, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr); code == 0 || stdout.Len() != 0 {
		t.Fatalf("gc hook --claim with an unbuildable query env = exit %d stdout=%q, want a failure with no result; stderr=%s", code, stdout.String(), stderr.String())
	}
}
