package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// retryWindowClock is a fake clock for the claim-read retry loop. Now reads it,
// Sleep advances it (by oversleep more than asked, to model a suspended or
// starved process), and each work-query read advances it by readCost, so the
// test controls exactly where every read and every pacing sleep lands relative
// to the claim window.
type retryWindowClock struct {
	now       time.Time
	readCost  time.Duration
	oversleep time.Duration
	sleeps    []time.Duration
	// readStarts records the invocation age at which each read began.
	readStarts []time.Duration
	invokedAt  time.Time
}

func newRetryWindowClock(readCost time.Duration) *retryWindowClock {
	start := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	return &retryWindowClock{now: start, invokedAt: start, readCost: readCost}
}

func (c *retryWindowClock) Now() time.Time { return c.now }

func (c *retryWindowClock) Sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d + c.oversleep)
}

// failingRun is a work-query runner whose every read fails after readCost.
func (c *retryWindowClock) failingRun(_, _ string, _ []string) (string, error) {
	c.readStarts = append(c.readStarts, c.now.Sub(c.invokedAt))
	c.now = c.now.Add(c.readCost)
	return "", errors.New("bd ready: exit status 1")
}

// retryWindowTestWindow is the production default claim window
// (hookWorkQueryTimeout + hookClaimMutationTimeout), pinned so the fake-clock
// arithmetic in these tests does not drift if either default is retuned.
const retryWindowTestWindow = 160 * time.Second

func (c *retryWindowClock) ops() *hookClaimOps {
	return &hookClaimOps{Now: c.Now, Sleep: c.Sleep, InvokedAt: c.invokedAt, ClaimWindow: retryWindowTestWindow}
}

func withProductionClaimRetryPacing(t *testing.T) {
	t.Helper()
	prevAttempts, prevInterval := hookClaimQueryRetryAttempts, hookClaimQueryRetryInterval
	hookClaimQueryRetryAttempts, hookClaimQueryRetryInterval = 3, 5*time.Second
	t.Cleanup(func() {
		hookClaimQueryRetryAttempts, hookClaimQueryRetryInterval = prevAttempts, prevInterval
	})
}

// A slow first read leaves less than one pacing interval of claim window. The
// loop must stop there: no sleep, no further read. Before the fix the loop slept
// and re-read three more times, carrying the process to ~640s against a 160s
// window.
func TestClaimReadRetryStopsWhenNextRetryWouldMissTheWindow(t *testing.T) {
	withProductionClaimRetryPacing(t)
	clock := newRetryWindowClock(156 * time.Second)
	ops := clock.ops()
	stores := []hookStore{{dir: "/rig"}}

	_, _, err := selectStoreWithWorkRetrying("query", stores, stores[0], clock.failingRun, ops)

	if err == nil {
		t.Fatal("selectStoreWithWorkRetrying returned no error for a leg that always fails")
	}
	if len(clock.readStarts) != 1 {
		t.Fatalf("reads = %d (started at %v), want 1: a retry that cannot start inside the window must not run", len(clock.readStarts), clock.readStarts)
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none: the loop must not sleep toward a window it cannot use", clock.sleeps)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("err = %q, want the underlying read failure preserved", err)
	}
	if !strings.Contains(err.Error(), "claim window") {
		t.Fatalf("err = %q, want it to say the claim window cut the retries short", err)
	}
}

// Reads slow enough that the window admits only part of the retry budget: every
// read the loop does run must start strictly inside the window, and it stops at
// the first retry that could not.
func TestClaimReadRetryRunsOnlyRetriesThatStartInsideTheWindow(t *testing.T) {
	withProductionClaimRetryPacing(t)
	window := retryWindowTestWindow
	clock := newRetryWindowClock(50 * time.Second)
	ops := clock.ops()
	stores := []hookStore{{dir: "/rig"}}

	if _, _, err := selectStoreWithWorkRetrying("query", stores, stores[0], clock.failingRun, ops); err == nil {
		t.Fatal("selectStoreWithWorkRetrying returned no error for a leg that always fails")
	}

	// 0s read -> 50s; sleep -> 55s read -> 105s; sleep -> 110s read -> 160s;
	// a further 5s sleep would end past the window, so stop.
	wantStarts := []time.Duration{0, 55 * time.Second, 110 * time.Second}
	if len(clock.readStarts) != len(wantStarts) {
		t.Fatalf("read starts = %v, want %v", clock.readStarts, wantStarts)
	}
	for i, got := range clock.readStarts {
		if got != wantStarts[i] {
			t.Fatalf("read starts = %v, want %v", clock.readStarts, wantStarts)
		}
		if got >= window {
			t.Fatalf("read %d started at invocation age %s, at or past the %s window", i, got, window)
		}
	}
	if len(clock.sleeps) != 2 {
		t.Fatalf("sleeps = %v, want 2 paced sleeps", clock.sleeps)
	}
}

// A pacing sleep that overruns (a suspended or starved process) must not be
// followed by a read once the window has closed during it.
func TestClaimReadRetryDoesNotReadAfterASleepThatOverranTheWindow(t *testing.T) {
	withProductionClaimRetryPacing(t)
	clock := newRetryWindowClock(time.Second)
	clock.oversleep = retryWindowTestWindow
	ops := clock.ops()
	stores := []hookStore{{dir: "/rig"}}

	_, _, err := selectStoreWithWorkRetrying("query", stores, stores[0], clock.failingRun, ops)

	if err == nil || !strings.Contains(err.Error(), "claim window") {
		t.Fatalf("err = %v, want the read failure annotated with the claim window", err)
	}
	if len(clock.readStarts) != 1 || len(clock.sleeps) != 1 {
		t.Fatalf("reads = %v, sleeps = %v; want exactly one read and one (overrun) sleep", clock.readStarts, clock.sleeps)
	}
}

// Control: the fence must not disable the retry. With window to spare the whole
// budget runs, a transient fault being exactly what the loop exists for.
func TestClaimReadRetryKeepsFullBudgetInsideTheWindow(t *testing.T) {
	withProductionClaimRetryPacing(t)
	clock := newRetryWindowClock(time.Second)
	ops := clock.ops()
	stores := []hookStore{{dir: "/rig"}}

	_, _, err := selectStoreWithWorkRetrying("query", stores, stores[0], clock.failingRun, ops)

	if err == nil {
		t.Fatal("selectStoreWithWorkRetrying returned no error for a leg that always fails")
	}
	if want := 1 + hookClaimQueryRetryAttempts; len(clock.readStarts) != want {
		t.Fatalf("reads = %d, want the full budget %d: an unspent window must not suppress the retry", len(clock.readStarts), want)
	}
	if strings.Contains(err.Error(), "claim window") {
		t.Fatalf("err = %q, want no claim-window annotation when the budget, not the window, ran out", err)
	}
}

// A direct caller that opened no invocation window (zero InvokedAt) has no turn
// to outlive, so the fence stays open, matching claimWindowSpent.
func TestClaimReadRetryWithoutAnInvocationWindowKeepsFullBudget(t *testing.T) {
	withProductionClaimRetryPacing(t)
	clock := newRetryWindowClock(10 * time.Minute)
	ops := &hookClaimOps{Now: clock.Now, Sleep: clock.Sleep}
	stores := []hookStore{{dir: "/rig"}}

	if _, _, err := selectStoreWithWorkRetrying("query", stores, stores[0], clock.failingRun, ops); err == nil {
		t.Fatal("selectStoreWithWorkRetrying returned no error for a leg that always fails")
	}
	if want := 1 + hookClaimQueryRetryAttempts; len(clock.readStarts) != want {
		t.Fatalf("reads = %d, want %d with no invocation window", len(clock.readStarts), want)
	}
}

// End to end through the claim loop: a read that would recover on retry, but
// only after the window closed, must not be retried into a claim. The
// invocation keeps the failed-read contract: no drain result, no drain-ack, no
// claim mutation, and the event bus still records the failure. The exit code
// follows the read's classification through the window annotation: 2 with the
// store-unavailable token for a timed-out or transport-class read, else 1. The
// typed-deadline read carries no timeout text, so only the annotation's %w
// wrap keeps it classified.
func TestHookClaimRetryCutByWindowExitsWithoutClaimOrDrain(t *testing.T) {
	cases := []struct {
		name      string
		readErr   error
		wantCode  int
		wantToken bool
	}{
		{
			name:     "application error keeps exit 1",
			readErr:  errors.New("bd ready: exit status 1"),
			wantCode: 1,
		},
		{
			name:      "transport error exits 2",
			readErr:   errors.New("bd ready: exit status 1: Error: dial tcp 127.0.0.1:3307: connect: connection refused"),
			wantCode:  2,
			wantToken: true,
		},
		{
			name:      "timed-out read exits 2",
			readErr:   fmt.Errorf("running work query %q: timed out after 150s: %w", "work-query", context.DeadlineExceeded),
			wantCode:  2,
			wantToken: true,
		},
		{
			name:      "typed deadline read exits 2",
			readErr:   fmt.Errorf("bd ready: %w", context.DeadlineExceeded),
			wantCode:  2,
			wantToken: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			clock := newRetryWindowClock(0)
			h := newFailureHookHarness()
			h.script("/rig",
				hookRunnerAnswer{err: tc.readErr},
				hookRunnerAnswer{out: routedRowJSON("wb-1")},
			)
			run := func(command, dir string, env []string) (string, error) {
				clock.readStarts = append(clock.readStarts, clock.now.Sub(clock.invokedAt))
				clock.now = clock.now.Add(156 * time.Second)
				return h.run(command, dir, env)
			}
			claimed := false
			ops := h.ops()
			ops.Claim = func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
				claimed = true
				return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
			}
			ops.Now, ops.Sleep = clock.Now, clock.Sleep
			ops.InvokedAt, ops.ClaimWindow = clock.invokedAt, retryWindowTestWindow
			stores := []hookStore{{dir: "/rig", env: []string{"BEADS_DIR=/rig"}}}

			var stdout, stderr bytes.Buffer
			code := claimHookWorkWithRunner("work-query", "/rig", stores[0].env, stores, failureClaimOptions(), ops,
				run, h.emitFailure, &stdout, &stderr)

			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			if got := strings.Contains(stderr.String(), hookStoreUnavailableToken); got != tc.wantToken {
				t.Fatalf("stderr carries %s = %v, want %v; stderr=%s", hookStoreUnavailableToken, got, tc.wantToken, stderr.String())
			}
			if len(clock.readStarts) != 1 || len(clock.sleeps) != 0 {
				t.Fatalf("reads = %v, sleeps = %v; want one read and no retry past the window", clock.readStarts, clock.sleeps)
			}
			if claimed {
				t.Fatal("a claim mutation ran after the claim window closed")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want no drain result for a failed read", stdout.String())
			}
			if h.drained {
				t.Fatal("drain was acknowledged for a failed read; the seat must be retained")
			}
			if len(h.events) == 0 {
				t.Fatal("no work-query failure event recorded")
			}
			if !strings.Contains(stderr.String(), "claim window") {
				t.Fatalf("stderr = %q, want the claim window named as why retries stopped", stderr.String())
			}
		})
	}
}

// budgetClaimHarness drives claimHookWorkWithRunner over a scripted fan-out on a
// fake clock: every read advances the clock by its leg's scripted cost, so a
// test places each read, and the CAS behind them, exactly against the claim
// window and the invocation read budget.
type budgetClaimHarness struct {
	*failureHookHarness
	clock *retryWindowClock
	// opts are the claim options every claim runs with (failureClaimOptions).
	opts hookClaimOptions
	// costs is consulted per (dir, attempt) like the scripted answers: the
	// last cost repeats.
	costs         map[string][]time.Duration
	reads         []string
	claims        []string
	claimFailed   []string
	windowExpired []hookClaimWindowExpiry
	// parents records the parent context the read guard gave each read that
	// claimWithReads ran, as the production work-query subprocess sees it.
	parents []budgetReadParent
}

// budgetReadParent is the parent context one read ran under: whether it carried
// a deadline, and which.
type budgetReadParent struct {
	dir      string
	deadline time.Time
	bounded  bool
}

func newBudgetClaimHarness() *budgetClaimHarness {
	return &budgetClaimHarness{
		failureHookHarness: newFailureHookHarness(),
		clock:              newRetryWindowClock(0),
		opts:               failureClaimOptions(),
		costs:              map[string][]time.Duration{},
	}
}

// leg scripts one leg: what each read of dir costs on the fake clock and what
// it answers.
func (b *budgetClaimHarness) leg(dir string, cost time.Duration, answers ...hookRunnerAnswer) {
	b.costs[dir] = []time.Duration{cost}
	b.script(dir, answers...)
}

func (b *budgetClaimHarness) run(command, dir string, env []string) (string, error) {
	n := b.calls[dir]
	b.reads = append(b.reads, dir)
	b.clock.readStarts = append(b.clock.readStarts, b.clock.now.Sub(b.clock.invokedAt))
	if costs := b.costs[dir]; len(costs) > 0 {
		if n >= len(costs) {
			n = len(costs) - 1
		}
		b.clock.now = b.clock.now.Add(costs[n])
	}
	return b.failureHookHarness.run(command, dir, env)
}

func (b *budgetClaimHarness) ops() hookClaimOps {
	ops := b.failureHookHarness.ops()
	ops.Claim = func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
		b.claims = append(b.claims, beadID)
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
	}
	ops.DrainPending = func(string) (bool, error) { return false, nil }
	ops.StampWorkMeta = func(context.Context, string, []string, string, string, map[string]string) error { return nil }
	ops.StampSessionClaim = func(string, string) error { return nil }
	ops.EmitClaimFailed = func(beadID string, _ hookClaimOptions, _ error) {
		b.claimFailed = append(b.claimFailed, beadID)
	}
	ops.EmitClaimWindowExpired = func(expiry hookClaimWindowExpiry) {
		b.windowExpired = append(b.windowExpired, expiry)
	}
	ops.Now, ops.Sleep = b.clock.Now, b.clock.Sleep
	ops.InvokedAt, ops.ClaimWindow = b.clock.invokedAt, retryWindowTestWindow
	return ops
}

func (b *budgetClaimHarness) claim(stores []hookStore, ops hookClaimOps) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("work-query", stores[0].dir, stores[0].env, stores, b.opts, ops,
		b.run, b.emitFailure, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// read is run as the production entry's reader: it records the parent context
// the read guard gave the read, then runs the scripted read.
func (b *budgetClaimHarness) read(parent context.Context, command, dir string, env []string) (string, error) {
	deadline, bounded := parent.Deadline()
	b.parents = append(b.parents, budgetReadParent{dir: dir, deadline: deadline, bounded: bounded})
	return b.run(command, dir, env)
}

// claimWithReads is claim through claimHookWorkWithReads, the entry the
// production runner takes, so a test sees the parent context of every read.
func (b *budgetClaimHarness) claimWithReads(stores []hookStore, ops hookClaimOps) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithReads("work-query", stores[0].dir, stores[0].env, stores, b.opts, ops,
		b.read, b.emitFailure, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// ownBeadReadback is a canonical store that records every bead as this
// session's (failureClaimOptions' assignee), so an adoption is certified.
func ownBeadReadback(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, error) {
	return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, nil
}

// budgetLegs is a fan-out of the agent's own store followed by one rig leg per
// name.
func budgetLegs(rigs ...string) []hookStore {
	stores := []hookStore{{dir: "/city", env: []string{"BEADS_DIR=/city"}}}
	for _, rig := range rigs {
		stores = append(stores, hookStore{dir: "/rig-" + rig, rig: rig, env: []string{"BEADS_DIR=/rig-" + rig}})
	}
	return stores
}

var budgetEmpty = hookRunnerAnswer{out: "[]"}

// budgetRow is a leg's answer serving the one row bead.
func budgetRow(t *testing.T, bead beads.Bead) hookRunnerAnswer {
	t.Helper()
	rows, err := json.Marshal([]beads.Bead{bead})
	if err != nil {
		t.Fatalf("marshal row %s: %v", bead.ID, err)
	}
	return hookRunnerAnswer{out: string(rows)}
}

// budgetRoutedRow is an open row routed to the claimant's route target
// (failureClaimOptions), assigned to assignee, with the extra metadata.
func budgetRoutedRow(id, assignee string, metadata map[string]string) beads.Bead {
	meta := map[string]string{"gc.routed_to": "rig/worker"}
	maps.Copy(meta, metadata)
	return beads.Bead{ID: id, Status: "open", Type: "task", Assignee: assignee, Metadata: meta}
}

// readTraceLines returns the read-trace lines a claim wrote to stderr.
func readTraceLines(stderr string) []string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "gc hook --claim: reads (") {
			lines = append(lines, line)
		}
	}
	return lines
}

// The reported defect on the production claim path: two legs at the code's own
// measured worst-case read cost (~70s). Discovery finds the routed bead at 140s,
// inside the window. Nothing may spend the rest of the window between that
// answer and the CAS, so the bead is claimed rather than refused as "the claim
// window is spent".
func TestFederatedClaimClaimsRoutedWorkFoundBySlowLegs(t *testing.T) {
	withProductionClaimRetryPacing(t)
	b := newBudgetClaimHarness()
	stores := budgetLegs("alpha")
	b.leg("/city", 70*time.Second, budgetEmpty)
	b.leg("/rig-alpha", 70*time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})

	code, stdout, stderr := b.claim(stores, b.ops())

	if code != 0 {
		t.Fatalf("exit = %d, want 0; reads=%v starts=%v stderr=%s", code, b.reads, b.clock.readStarts, stderr)
	}
	if len(b.windowExpired) != 0 {
		t.Fatalf("claim window expired %d time(s) for work found inside the window: %+v", len(b.windowExpired), b.windowExpired)
	}
	if len(b.claims) != 1 || b.claims[0] != "work-1" {
		t.Fatalf("claims = %v, want [work-1]", b.claims)
	}
	if result := decodeTurnBoundResult(t, stdout); result.Action != "work" || result.BeadID != "work-1" {
		t.Fatalf("result = %+v, want action=work bead_id=work-1", result)
	}
}

// The selected leg's CAS failing with a transport-class error is a write
// failure, not an idle city: with no claim-time re-read in front of the CAS the
// failure reaches the claim loop, which records bead.claim_failed, drops the
// store, reselects across the rest and, finding nothing, drains claims_errored,
// never no_work.
func TestFederatedClaimTransportCASFailureDrainsClaimsErrored(t *testing.T) {
	withProductionClaimRetryPacing(t)
	b := newBudgetClaimHarness()
	stores := budgetLegs("alpha")
	b.leg("/city", time.Second, budgetEmpty)
	b.leg("/rig-alpha", time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
	ops := b.ops()
	ops.Claim = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
		b.claims = append(b.claims, beadID)
		return beads.Bead{}, false, errors.New("bd update: dial unix /rig-alpha/.beads/bd.sock: connect: connection refused")
	}

	code, stdout, stderr := b.claim(stores, ops)

	if code != 0 {
		t.Fatalf("exit = %d, want 0 (a drain); stderr=%s", code, stderr)
	}
	if len(b.claimFailed) != 1 || b.claimFailed[0] != "work-1" {
		t.Fatalf("bead.claim_failed = %v, want [work-1]", b.claimFailed)
	}
	result := decodeTurnBoundResult(t, stdout)
	if result.Action != "drain" || result.Reason != hookClaimReasonClaimsErrored {
		t.Fatalf("result = %+v, want a claims_errored drain, never no_work", result)
	}
}

// A claim that selected a store with claimable work goes straight to its CAS:
// one read per leg, never a second read of the selected leg.
func TestFederatedClaimRunsNoReadBetweenSelectionAndCAS(t *testing.T) {
	for _, tc := range []struct {
		name string
		rigs []string
	}{
		{name: "two legs", rigs: []string{"alpha"}},
		{name: "three legs", rigs: []string{"alpha", "beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			stores := budgetLegs(tc.rigs...)
			for _, st := range stores[:len(stores)-1] {
				b.leg(st.dir, time.Second, budgetEmpty)
			}
			last := stores[len(stores)-1]
			b.leg(last.dir, time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})

			code, _, stderr := b.claim(stores, b.ops())

			if code != 0 || len(b.claims) != 1 {
				t.Fatalf("exit = %d, claims = %v; want one claim; stderr=%s", code, b.claims, stderr)
			}
			if len(b.reads) != len(stores) {
				t.Fatalf("reads = %v, want exactly one per leg (%d)", b.reads, len(stores))
			}
			if got := hookClaimReadsPerTick(stores); got != len(stores) {
				t.Fatalf("hookClaimReadsPerTick = %d, want %d (one per leg)", got, len(stores))
			}
		})
	}
}

// The case the claim-time re-read used to guard: the selected store's row is
// lost to another claimant at CAS time while a later leg still holds routed
// work. The claim loop drops the store and reselects, so the later work is
// claimed and the seat does not drain no_work.
func TestFederatedClaimLostCASFallsThroughToLaterLeg(t *testing.T) {
	withProductionClaimRetryPacing(t)
	b := newBudgetClaimHarness()
	stores := budgetLegs("alpha")
	b.leg("/city", time.Second,
		hookRunnerAnswer{out: `[{"id":"own-1","status":"open","priority":0,"metadata":{"gc.routed_to":"rig/worker"}}]`})
	b.leg("/rig-alpha", time.Second,
		hookRunnerAnswer{out: `[{"id":"alpha-1","status":"open","priority":3,"metadata":{"gc.routed_to":"rig/worker"}}]`})
	ops := b.ops()
	ops.Claim = func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
		b.claims = append(b.claims, beadID)
		if beadID == "own-1" {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "gc__other-seat"}, false, nil
		}
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
	}

	code, stdout, stderr := b.claim(stores, ops)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}
	if want := []string{"own-1", "alpha-1"}; strings.Join(b.claims, ",") != strings.Join(want, ",") {
		t.Fatalf("claim attempts = %v, want %v (the lost own row, then the later leg)", b.claims, want)
	}
	result := decodeTurnBoundResult(t, stdout)
	if result.Action != "work" || result.BeadID != "alpha-1" {
		t.Fatalf("result = %+v, want action=work bead_id=alpha-1, never a no_work drain", result)
	}
	if b.drained {
		t.Fatal("drain was acknowledged although the later leg's work was claimed")
	}
}

// Work only a CAS can take must be in hand by the invocation read deadline
// (InvokedAt + window - reserve). The CAS on such work answered at or after it
// is refused as a timed-out read: exit 2 with the store-unavailable token, a
// work-query-failure event naming the leg that was cut, no drain, and never a
// window refusal, even once the window is spent too.
func TestClaimReadsStopAtTheInvocationReadBudget(t *testing.T) {
	cases := []struct {
		name string
		// cost of every read; work is only in the last leg.
		cost time.Duration
		// primaryFree makes the agent's own (empty) leg answer instantly.
		primaryFree bool
		window      time.Duration
		rigs        []string
		// cutLeg is the rig whose work-query-failure event must be recorded.
		cutLeg string
	}{
		{name: "late answer on the last leg", cost: 60 * time.Second, rigs: []string{"alpha", "beta"}, cutLeg: "beta"},
		// alpha's empty answer at 160s is kept, so beta is still read; the work
		// it answers at 240s is cut.
		{name: "leg started past the deadline answers work", cost: 80 * time.Second, rigs: []string{"alpha", "beta"}, cutLeg: "beta"},
		// A 12s override leaves a 6s budget (reserve = window/2), which the
		// rig leg's 7s read overruns.
		{name: "override window below the mutation reserve", cost: 7 * time.Second, primaryFree: true, window: 12 * time.Second, rigs: []string{"alpha"}, cutLeg: "alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			stores := budgetLegs(tc.rigs...)
			if tc.primaryFree {
				b.leg("/city", 0, budgetEmpty)
			}
			for _, st := range stores[:len(stores)-1] {
				if _, scripted := b.costs[st.dir]; !scripted {
					b.leg(st.dir, tc.cost, budgetEmpty)
				}
			}
			last := stores[len(stores)-1]
			b.leg(last.dir, tc.cost, hookRunnerAnswer{out: routedRowJSON("work-1")})
			ops := b.ops()
			if tc.window > 0 {
				ops.ClaimWindow = tc.window
			}

			code, stdout, stderr := b.claim(stores, ops)

			if code != 2 || !strings.Contains(stderr, hookStoreUnavailableToken) {
				t.Fatalf("exit = %d, want 2 with %s; reads=%v starts=%v stderr=%s", code, hookStoreUnavailableToken, b.reads, b.clock.readStarts, stderr)
			}
			if len(b.windowExpired) != 0 {
				t.Fatalf("a read that ran out of budget was reported as a window refusal: %+v", b.windowExpired)
			}
			if len(b.claims) != 0 {
				t.Fatalf("claims = %v, want none: an answer past the read deadline must not reach a CAS", b.claims)
			}
			if stdout != "" || b.drained {
				t.Fatalf("stdout = %q, drained = %v; want no drain result and drain-ack unconsumed", stdout, b.drained)
			}
			named := false
			for _, ev := range b.events {
				if strings.Contains(ev.Message, fmt.Sprintf("rig %q", tc.cutLeg)) {
					named = true
				}
			}
			if !named {
				t.Fatalf("no work-query-failure event names rig %q; events=%+v", tc.cutLeg, b.events)
			}
			for _, st := range stores {
				if b.calls[st.dir] != 1 {
					t.Fatalf("leg %s was read %d time(s), want once: with no work in hand every leg is read; reads=%v", st.dir, b.calls[st.dir], b.reads)
				}
			}
		})
	}
}

// The read deadline exists so found work can start its CAS inside the claim
// window. An answer that needs no CAS is not cut by it: an empty one still
// drains no_work, however late it arrives, and a leg that starts past the
// deadline is still read while no work is in hand, since its answer may be
// empty too. A failed read or a cut answer would hold the no_work answer up and
// end the claim with exit 2, so an idle fan-out that overran the budget would
// never drain.
func TestClaimReadPastTheBudgetStillDrainsAnIdleFanOut(t *testing.T) {
	type leg struct {
		dir  string
		cost time.Duration
	}
	cases := []struct {
		name   string
		window time.Duration
		rigs   []string
		legs   []leg
	}{
		{
			name: "every leg answers empty, the last one past the deadline",
			rigs: []string{"alpha", "beta"},
			legs: []leg{{"/city", 60 * time.Second}, {"/rig-alpha", 60 * time.Second}, {"/rig-beta", 60 * time.Second}},
		},
		{
			name: "a leg that starts past the deadline is still read",
			rigs: []string{"alpha"},
			legs: []leg{{"/city", 155 * time.Second}, {"/rig-alpha", 10 * time.Second}},
		},
		{
			// A 75s override leaves a 65s read budget, which the one 70s read
			// overruns.
			name:   "one leg slower than an override's read budget",
			window: 75 * time.Second,
			legs:   []leg{{"/city", 70 * time.Second}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			stores := budgetLegs(tc.rigs...)
			for _, l := range tc.legs {
				b.leg(l.dir, l.cost, budgetEmpty)
			}
			ops := b.ops()
			if tc.window > 0 {
				ops.ClaimWindow = tc.window
			}

			code, stdout, stderr := b.claim(stores, ops)

			if code != 0 {
				t.Fatalf("exit = %d, want 0 (a no_work drain); starts=%v stderr=%s", code, b.clock.readStarts, stderr)
			}
			if result := decodeTurnBoundResult(t, stdout); result.Action != "drain" || result.Reason != "no_work" {
				t.Fatalf("result = %+v, want a no_work drain", result)
			}
			if !b.drained {
				t.Fatal("drain-ack was not consumed by the no_work drain")
			}
			if len(b.events) != 0 {
				t.Fatalf("work-query-failure events = %+v, want none: every leg answered", b.events)
			}
			if lines := readTraceLines(stderr); len(lines) != 0 {
				t.Fatalf("a no_work drain wrote a read trace: %q", lines)
			}
			for _, st := range stores {
				if b.calls[st.dir] != 1 {
					t.Fatalf("leg %s was read %d time(s), want once; reads=%v", st.dir, b.calls[st.dir], b.reads)
				}
			}
		})
	}
}

// This session's own in-progress work needs no CAS either: adoption resumes it,
// and the claim window deliberately does not fence adoption. So a late leg
// holding it is read and the bead resumed (existing_assignment), however long
// the reads before it took, and even after a late answer with fresh work: that
// work arrived too late for its CAS, so it holds no later read to the deadline.
func TestClaimReadPastTheBudgetStillResumesOwnInProgressWork(t *testing.T) {
	cases := []struct {
		name string
		// costs are the per-leg read costs, own store first; the last leg holds
		// the in-progress row and every other leg answers empty.
		costs []time.Duration
		// workBefore makes the leg before the in-progress one answer fresh
		// work instead.
		workBefore bool
	}{
		{name: "answered past the deadline", costs: []time.Duration{60 * time.Second, 60 * time.Second, 60 * time.Second}},
		{name: "in a leg that starts past the deadline", costs: []time.Duration{155 * time.Second, 10 * time.Second}},
		{name: "after a late answer with fresh work", costs: []time.Duration{100 * time.Second, 60 * time.Second, 10 * time.Second}, workBefore: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			stores := budgetLegs("alpha", "beta")[:len(tc.costs)]
			for i, st := range stores {
				answer := budgetEmpty
				switch {
				case i == len(stores)-1:
					answer = hookRunnerAnswer{out: inProgressRowJSON("resume-1")}
				case i == len(stores)-2 && tc.workBefore:
					answer = hookRunnerAnswer{out: routedRowJSON("work-1")}
				}
				b.leg(st.dir, tc.costs[i], answer)
			}
			ops := b.ops()
			ops.ReadWorkMeta = ownBeadReadback

			code, stdout, stderr := b.claim(stores, ops)

			if code != 0 {
				t.Fatalf("exit = %d, want 0; starts=%v stderr=%s", code, b.clock.readStarts, stderr)
			}
			result := decodeTurnBoundResult(t, stdout)
			if result.Action != "work" || result.Reason != "existing_assignment" || result.BeadID != "resume-1" {
				t.Fatalf("result = %+v, want resume-1 resumed as existing_assignment", result)
			}
			if len(b.claims) != 0 || len(b.windowExpired) != 0 {
				t.Fatalf("claims = %v, window expiries = %d; want a resume with no CAS and no window refusal", b.claims, len(b.windowExpired))
			}
			if b.drained {
				t.Fatal("drain-ack was consumed although the session resumed its work")
			}
		})
	}
}

// A late answer is still acted on wherever the claim needs no mutation for it:
// work the claim skips (a build-budget deferral, another seat's worktree) is
// left, and a workflow whose input is already done is retired. So an idle seat
// whose slowest leg serves only such rows drains no_work, as it did when the
// claim re-read the selected leg, rather than exit 2 on every re-drive while
// the done workflow keeps counting as demand.
func TestClaimReadPastTheBudgetSkipsAndRetiresWithoutAMutation(t *testing.T) {
	deferredUntil := newRetryWindowClock(0).now.Add(time.Hour).Format(time.RFC3339)
	bound := managedWorktreeCandidate("bound-1", "/wt/seat-b")
	bound.Metadata["gc.routed_to"] = "rig/worker"
	cases := []struct {
		name string
		row  beads.Bead
		seat hookPoolSeat
		// retire makes the row's workflow input already done.
		retire     bool
		wantStderr string
	}{
		{
			name: "a row inside its build-budget deferral",
			row:  budgetRoutedRow("deferred-1", "", map[string]string{beadmeta.BudgetDeferredUntilMetadataKey: deferredUntil}),
		},
		{
			name:       "a row bound to another seat's managed worktree",
			row:        bound,
			seat:       hookPoolSeat{WorkDir: "/wt/seat-a"},
			wantStderr: "left bound-1 to a session in its managed worktree /wt/seat-b",
		},
		{
			name:       "a step of a workflow whose input is already done",
			row:        budgetRoutedRow("step-1", "", map[string]string{beadmeta.RootBeadIDMetadataKey: "root-1"}),
			retire:     true,
			wantStderr: "workflow root-1 input convoy work is already terminal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			b.opts.PoolSeat = tc.seat
			stores := budgetLegs("alpha", "beta")
			// beta answers at 180s, past the 150s read deadline and the window.
			b.leg("/city", 60*time.Second, budgetEmpty)
			b.leg("/rig-alpha", 60*time.Second, budgetEmpty)
			b.leg("/rig-beta", 60*time.Second, budgetRow(t, tc.row))
			ops := b.ops()
			ops.InputVerdict = func(_ context.Context, _ string, _ []string, candidate beads.Bead, _ []string, _ workflowInputAbsenceProver) (workflowInputVerdict, error) {
				if !tc.retire {
					return workflowInputVerdict{}, nil
				}
				return workflowInputVerdict{RootID: candidate.Metadata[beadmeta.RootBeadIDMetadataKey], State: workflowInputComplete}, nil
			}
			var retired []string
			ops.RetireWorkflow = func(_ context.Context, _ string, _ []string, verdict workflowInputVerdict) error {
				retired = append(retired, verdict.RootID)
				return nil
			}

			code, stdout, stderr := b.claim(stores, ops)

			if code != 0 {
				t.Fatalf("exit = %d, want 0 (a no_work drain); starts=%v stderr=%s", code, b.clock.readStarts, stderr)
			}
			if result := decodeTurnBoundResult(t, stdout); result.Action != "drain" || result.Reason != "no_work" {
				t.Fatalf("result = %+v, want a no_work drain", result)
			}
			if !b.drained {
				t.Fatal("drain-ack was not consumed by the no_work drain")
			}
			if len(b.claims) != 0 || len(b.windowExpired) != 0 {
				t.Fatalf("claims = %v, window expiries = %d; want neither for rows the claim takes no CAS on", b.claims, len(b.windowExpired))
			}
			if len(b.events) != 0 {
				t.Fatalf("work-query-failure events = %+v, want none: every leg answered", b.events)
			}
			if lines := readTraceLines(stderr); len(lines) != 0 {
				t.Fatalf("a no_work drain wrote a read trace: %q", lines)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr, tc.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tc.wantStderr)
			}
			var wantRetired []string
			if tc.retire {
				wantRetired = []string{"root-1"}
			}
			if fmt.Sprint(retired) != fmt.Sprint(wantRetired) {
				t.Fatalf("retired workflows = %v, want %v", retired, wantRetired)
			}
		})
	}
}

// A claim mutation on work answered at or after the read deadline is refused
// before it starts, whichever claim tier would run it: a fresh claim, the
// promotion of a ready assignment, or a stale-lease reclaim. It is refused as
// the read the budget cut: exit 2 with the store-unavailable token, a
// work-query-failure event naming the leg, a read trace listing that leg as
// budget-cut, no drain and no window refusal. The leg answers inside the
// window, so only this refusal stands between the late answer and the
// mutation.
func TestClaimReadPastTheBudgetRefusesEveryClaimMutation(t *testing.T) {
	cases := []struct {
		name    string
		row     beads.Bead
		reclaim bool
	}{
		{name: "a fresh claim", row: budgetRoutedRow("work-1", "", nil)},
		{name: "a ready assignment's promotion", row: budgetRoutedRow("ready-1", "gc__worker-1", nil)},
		{name: "a stale-lease reclaim", row: budgetRoutedRow("stale-1", "gc__gone-seat", nil), reclaim: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			b.opts.AutoReclaimStaleClaims = tc.reclaim
			stores := budgetLegs("alpha", "beta")
			// beta answers at 152s: past the 150s read deadline, inside the
			// 160s window.
			b.leg("/city", 50*time.Second, budgetEmpty)
			b.leg("/rig-alpha", 50*time.Second, budgetEmpty)
			b.leg("/rig-beta", 52*time.Second, budgetRow(t, tc.row))
			ops := b.ops()
			var reclaimed []string
			ops.ReclaimStale = func(_ context.Context, _ string, _ []string, beadID string) (bool, string, error) {
				reclaimed = append(reclaimed, beadID)
				return true, "gc__gone-seat", nil
			}

			code, stdout, stderr := b.claim(stores, ops)

			if code != 2 || !strings.Contains(stderr, hookStoreUnavailableToken) {
				t.Fatalf("exit = %d, want 2 with %s; starts=%v stderr=%s", code, hookStoreUnavailableToken, b.clock.readStarts, stderr)
			}
			if len(b.claims) != 0 || len(reclaimed) != 0 {
				t.Fatalf("claims = %v, reclaims = %v; want no mutation on an answer past the read deadline", b.claims, reclaimed)
			}
			if len(b.windowExpired) != 0 {
				t.Fatalf("the late answer was refused as a spent window: %+v", b.windowExpired)
			}
			if stdout != "" || b.drained {
				t.Fatalf("stdout = %q, drained = %v; want no drain result and drain-ack unconsumed", stdout, b.drained)
			}
			named := false
			for _, ev := range b.events {
				if strings.Contains(ev.Message, `rig "beta"`) {
					named = true
				}
			}
			if !named {
				t.Fatalf("no work-query-failure event names rig \"beta\"; events=%+v", b.events)
			}
			lines := readTraceLines(stderr)
			if len(lines) != 1 {
				t.Fatalf("trace lines = %q, want exactly one; stderr=%s", lines, stderr)
			}
			for _, part := range []string{"rig:beta +1m40s 52s budget-cut", "no claim started"} {
				if !strings.Contains(lines[0], part) {
					t.Fatalf("trace line %q is missing %q", lines[0], part)
				}
			}
		})
	}
}

// Once a read has found work only a CAS can take, that work must reach its CAS
// inside the window, so every later read of the selection must end by the read
// deadline: one that would start past it does not start, and one that answers
// past it is cut, whatever it answers, as production kills that read at the
// deadline. The found work is then claimed.
func TestClaimReadWithWorkInHandEndsAtTheReadDeadline(t *testing.T) {
	cases := []struct {
		name string
		rigs []string
		// alphaCost and alphaAnswer script the rig leg read after the own
		// store's work is found at 100s.
		alphaCost   time.Duration
		alphaAnswer hookRunnerAnswer
		// unread lists legs the runner must never be called for.
		unread []string
	}{
		// Kept, the late resume row would outrank the found work.
		{name: "a read still running at the deadline is cut", rigs: []string{"alpha"}, alphaCost: 55 * time.Second, alphaAnswer: hookRunnerAnswer{out: inProgressRowJSON("resume-1")}},
		{name: "a read that would start past the deadline does not start", rigs: []string{"alpha", "beta"}, alphaCost: 50 * time.Second, alphaAnswer: budgetEmpty, unread: []string{"/rig-beta"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			stores := budgetLegs(tc.rigs...)
			b.leg("/city", 100*time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
			b.leg("/rig-alpha", tc.alphaCost, tc.alphaAnswer)
			b.leg("/rig-beta", time.Second, budgetEmpty)
			ops := b.ops()
			ops.ReadWorkMeta = ownBeadReadback

			code, stdout, stderr := b.claim(stores, ops)

			if code != 0 || len(b.claims) != 1 || b.claims[0] != "work-1" {
				t.Fatalf("exit = %d, claims = %v; want work-1 claimed; starts=%v stderr=%s", code, b.claims, b.clock.readStarts, stderr)
			}
			if len(b.windowExpired) != 0 {
				t.Fatalf("the found work was refused by the window: %+v", b.windowExpired)
			}
			if result := decodeTurnBoundResult(t, stdout); result.Action != "work" || result.BeadID != "work-1" {
				t.Fatalf("result = %+v, want action=work bead_id=work-1", result)
			}
			for _, dir := range tc.unread {
				if b.calls[dir] != 0 {
					t.Fatalf("leg %s was read %d time(s) after the deadline, with work waiting on its CAS", dir, b.calls[dir])
				}
			}
		})
	}
}

// In production the same rule reaches the work-query subprocess through its
// parent context: a read runs under the read deadline only once claimable work
// is in hand, so it is killed there; before that it runs to its own
// hookWorkQueryTimeout.
func TestClaimReadParentCarriesTheReadDeadlineOnlyWithWorkInHand(t *testing.T) {
	withProductionClaimRetryPacing(t)
	b := newBudgetClaimHarness()
	stores := budgetLegs("alpha", "beta")
	b.leg("/city", time.Second, budgetEmpty)
	b.leg("/rig-alpha", time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
	b.leg("/rig-beta", time.Second, budgetEmpty)
	ops := b.ops()

	code, _, stderr := b.claimWithReads(stores, ops)

	if code != 0 || len(b.claims) != 1 {
		t.Fatalf("exit = %d, claims = %v; want work-1 claimed; stderr=%s", code, b.claims, stderr)
	}
	readDeadline := b.clock.invokedAt.Add(retryWindowTestWindow - hookClaimMutationTimeout)
	want := []budgetReadParent{
		{dir: "/city"},
		{dir: "/rig-alpha"},
		{dir: "/rig-beta", deadline: readDeadline, bounded: true},
	}
	if len(b.parents) != len(want) {
		t.Fatalf("read parents = %+v, want %+v", b.parents, want)
	}
	for i, got := range b.parents {
		if got.dir != want[i].dir || got.bounded != want[i].bounded || !got.deadline.Equal(want[i].deadline) {
			t.Fatalf("read parents = %+v, want %+v", b.parents, want)
		}
	}
}

// A claim whose override window leaves a budget the read fits in still claims:
// the reserve never eats the whole window.
func TestClaimReadBudgetUnderAWindowOverrideStillClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		cost   time.Duration
	}{
		{name: "window below the reserve keeps half", window: 12 * time.Second, cost: 5 * time.Second},
		{name: "window above the reserve keeps window minus reserve", window: 30 * time.Second, cost: 19 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			stores := budgetLegs()
			b.leg("/city", tc.cost, hookRunnerAnswer{out: routedRowJSON("work-1")})
			ops := b.ops()
			ops.ClaimWindow = tc.window

			code, _, stderr := b.claim(stores, ops)

			if code != 0 || len(b.claims) != 1 {
				t.Fatalf("exit = %d, claims = %v; want the read inside the budget claimed; stderr=%s", code, b.claims, stderr)
			}
		})
	}
}

// In production the read deadline is also the parent of each work-query
// subprocess, so a read still running when the budget runs out is killed there,
// not at its own later hookWorkQueryTimeout, and its error says which fired.
func TestClaimReadContextKillsAReadAtTheReadDeadline(t *testing.T) {
	ops := hookClaimOps{InvokedAt: time.Now(), ClaimWindow: 400 * time.Millisecond}
	ctx, cancel := ops.claimReadContext()
	defer cancel()

	started := time.Now()
	_, err := shellWorkQueryWithEnvContext(ctx, "sleep 30", t.TempDir(), nil)

	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("read ran %s, want it killed at the ~200ms read deadline", elapsed)
	}
	if !errors.Is(err, errHookClaimReadBudgetSpent) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the read-budget cut (wrapping context.DeadlineExceeded)", err)
	}
	if !strings.Contains(err.Error(), "timed out after the invocation's claim read deadline") {
		t.Fatalf("err = %q, want it to name the claim read deadline", err)
	}
	if !workQueryStoreUnavailable(err) {
		t.Fatalf("err = %v, want it classified store-unavailable like any timed-out read", err)
	}
}

// Through the production claim entry, a leg read once work is in hand runs its
// work-query subprocess under the read deadline: the own leg answers the step at
// once, the rig leg's subprocess would sleep 30s, and it is killed at the 2s
// read budget of a 4s window, so the step is claimed inside the window. A
// subprocess left to its own timeout would hold the claim past the window,
// which then refuses the step.
func TestClaimHookWorkKillsALaterLegAtTheReadDeadline(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	city := newHookClaimEventsCity(t)
	t.Setenv("GC_HOOK_CLAIM_WINDOW", "4s")
	step, err := city.mem.Get(city.stepID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := json.Marshal([]beads.Bead{step})
	if err != nil {
		t.Fatal(err)
	}
	workQuery := `if [ -n "$GC_TEST_SLOW_LEG" ]; then exec '` + sleep + `' 30; fi; printf '%s' '` + string(rows) + `'`
	env := []string{"BEADS_DIR=" + filepath.Join(city.dir, ".beads")}
	rigDir := t.TempDir()
	stores := []hookStore{
		{dir: city.dir, env: env},
		{dir: rigDir, rig: "slow", env: []string{"BEADS_DIR=" + filepath.Join(rigDir, ".beads"), "GC_TEST_SLOW_LEG=1"}},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                env,
		JSON:               true,
	}
	cfg, err := loadCityConfig(city.dir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	started := time.Now()
	code := claimHookWork(newHookCity(city.dir, cfg), workQuery, city.dir, env, stores, opts,
		func(string, error) {}, &stdout, &stderr)
	elapsed := time.Since(started)

	if code != 0 {
		t.Fatalf("claimHookWork = %d after %s, want 0; stdout=%s stderr=%s", code, elapsed, stdout.String(), stderr.String())
	}
	if elapsed > 15*time.Second {
		t.Fatalf("the claim took %s, want the slow leg killed at the ~2s read deadline", elapsed)
	}
	if result := decodeTurnBoundResult(t, stdout.String()); result.Action != "work" || result.BeadID != city.stepID {
		t.Fatalf("result = %+v, want action=work bead_id=%s", result, city.stepID)
	}
	if !strings.Contains(stderr.String(), `rig "slow"`) {
		t.Fatalf("stderr = %q, want the cut slow leg reported", stderr.String())
	}
}

// A paced retry that answers work at or after the read deadline must not carry
// it into a CAS, whether the retry started inside the read budget or past it:
// the claim is refused as a timed-out read.
func TestClaimReadRetryCannotLandFoundWorkPastTheReadBudget(t *testing.T) {
	t.Run("retry answers past the deadline", func(t *testing.T) {
		withProductionClaimRetryPacing(t)
		b := newBudgetClaimHarness()
		stores := budgetLegs()
		b.script("/city", hookRunnerAnswer{err: errors.New("bd ready: exit status 1")}, hookRunnerAnswer{out: routedRowJSON("work-1")})
		b.costs["/city"] = []time.Duration{100 * time.Second, 70 * time.Second}

		code, _, stderr := b.claim(stores, b.ops())

		if code != 2 || !strings.Contains(stderr, hookStoreUnavailableToken) {
			t.Fatalf("exit = %d, want the timed-out-read exit 2; starts=%v stderr=%s", code, b.clock.readStarts, stderr)
		}
		if len(b.claims) != 0 || len(b.windowExpired) != 0 {
			t.Fatalf("claims = %v, window expiries = %d; want no CAS and no window refusal of found work", b.claims, len(b.windowExpired))
		}
		if want := []time.Duration{0, 105 * time.Second}; len(b.clock.readStarts) != 2 || b.clock.readStarts[1] != want[1] {
			t.Fatalf("read starts = %v, want %v", b.clock.readStarts, want)
		}
	})
	t.Run("retry starts past the deadline", func(t *testing.T) {
		withProductionClaimRetryPacing(t)
		b := newBudgetClaimHarness()
		stores := budgetLegs()
		b.script("/city",
			hookRunnerAnswer{err: errors.New("bd ready: exit status 1: Error: dial tcp 127.0.0.1:3307: connect: connection refused")},
			hookRunnerAnswer{out: routedRowJSON("work-1")})
		b.costs["/city"] = []time.Duration{147 * time.Second, time.Second}

		code, _, stderr := b.claim(stores, b.ops())

		if code != 2 || !strings.Contains(stderr, hookStoreUnavailableToken) {
			t.Fatalf("exit = %d, want 2 with %s; stderr=%s", code, hookStoreUnavailableToken, stderr)
		}
		if len(b.claims) != 0 || len(b.windowExpired) != 0 {
			t.Fatalf("claims = %v, window expiries = %d; want no CAS and no window refusal of found work", b.claims, len(b.windowExpired))
		}
		if want := []time.Duration{0, 152 * time.Second}; fmt.Sprint(b.clock.readStarts) != fmt.Sprint(want) {
			t.Fatalf("read starts = %v, want %v: the retry starts inside the window", b.clock.readStarts, want)
		}
	})
}

// While no work is in hand a paced retry runs up to the claim window, even when
// it starts past the read deadline: its answer may need no claim mutation. An
// empty one drains no_work and this session's own in-progress work is resumed,
// where retries stopped at the read deadline would end an idle seat's claim
// with exit 2.
func TestClaimReadRetryPastTheReadBudgetStillDrainsOrResumes(t *testing.T) {
	cases := []struct {
		name       string
		retry      hookRunnerAnswer
		wantAction string
		wantReason string
		wantBead   string
	}{
		{name: "the retry answers empty", retry: budgetEmpty, wantAction: "drain", wantReason: "no_work"},
		{
			name:       "the retry answers this session's own in-progress work",
			retry:      hookRunnerAnswer{out: inProgressRowJSON("held-1")},
			wantAction: "work", wantReason: "existing_assignment", wantBead: "held-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			stores := budgetLegs()
			b.script("/city",
				hookRunnerAnswer{err: errors.New("bd ready: exit status 1: Error: dial tcp 127.0.0.1:3307: connect: connection refused")},
				tc.retry)
			// The failed read ends at 146s, so the retry starts at 151s: past
			// the 150s read deadline, inside the 160s window.
			b.costs["/city"] = []time.Duration{146 * time.Second, time.Second}
			ops := b.ops()
			ops.ReadWorkMeta = ownBeadReadback

			code, stdout, stderr := b.claim(stores, ops)

			if code != 0 {
				t.Fatalf("exit = %d, want 0; starts=%v stderr=%s", code, b.clock.readStarts, stderr)
			}
			if want := []time.Duration{0, 151 * time.Second}; fmt.Sprint(b.clock.readStarts) != fmt.Sprint(want) {
				t.Fatalf("read starts = %v, want %v", b.clock.readStarts, want)
			}
			result := decodeTurnBoundResult(t, stdout)
			if result.Action != tc.wantAction || result.Reason != tc.wantReason || result.BeadID != tc.wantBead {
				t.Fatalf("result = %+v, want action=%s reason=%s bead_id=%q", result, tc.wantAction, tc.wantReason, tc.wantBead)
			}
			if len(b.claims) != 0 || len(b.windowExpired) != 0 {
				t.Fatalf("claims = %v, window expiries = %d; want neither", b.claims, len(b.windowExpired))
			}
			if want := tc.wantAction == "drain"; b.drained != want {
				t.Fatalf("drain-ack consumed = %v, want %v", b.drained, want)
			}
		})
	}
}

// Every claim that ends without work for a reason other than "every leg
// answered empty" writes one read-trace line naming each read; a claim and a
// no_work drain write none.
func TestClaimFailureExitsCarryOneReadTrace(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(b *budgetClaimHarness, ops *hookClaimOps) []hookStore
		wantCode  int
		wantTrace bool
		// wantParts must all appear in the trace line.
		wantParts []string
		wantReads int
	}{
		{
			name: "spent read budget",
			setup: func(b *budgetClaimHarness, _ *hookClaimOps) []hookStore {
				b.leg("/city", 80*time.Second, budgetEmpty)
				b.leg("/rig-alpha", 80*time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
				return budgetLegs("alpha")
			},
			wantCode:  2,
			wantTrace: true,
			wantParts: []string{"budget 2m30s of 2m40s window", "own +0s 1m20s empty", "rig:alpha +1m20s 1m20s budget-cut", "no claim started"},
			wantReads: 2,
		},
		{
			name: "non-transport read failure",
			setup: func(b *budgetClaimHarness, _ *hookClaimOps) []hookStore {
				b.leg("/city", time.Second, hookRunnerAnswer{err: errors.New("bd ready: exit status 1: unknown flag")})
				return budgetLegs()
			},
			wantCode:  1,
			wantTrace: true,
			wantParts: []string{"own +0s 1s error", "own +6s 1s error", "own +12s 1s error", "own +18s 1s error", "no claim started"},
			wantReads: 4,
		},
		{
			name: "window refusal",
			setup: func(b *budgetClaimHarness, ops *hookClaimOps) []hookStore {
				b.leg("/city", time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
				// The drain probe is the last store read before the CAS; a
				// stall there spends the window without any work-query read.
				ops.DrainPending = func(string) (bool, error) {
					b.clock.now = b.clock.now.Add(3 * time.Minute)
					return false, nil
				}
				stores := budgetLegs()
				stores[0].env = append(stores[0].env, "GC_SESSION_ID=gcs-1")
				return stores
			},
			wantCode:  1,
			wantTrace: true,
			wantParts: []string{"own +0s 1s work", "claim refused at +3m1s: window spent"},
			wantReads: 1,
		},
		{
			name: "claims_errored drain",
			setup: func(b *budgetClaimHarness, ops *hookClaimOps) []hookStore {
				b.leg("/city", time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
				ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
					return beads.Bead{}, false, errors.New("bd update: exit status 1: database is locked")
				}
				return budgetLegs()
			},
			wantCode:  0,
			wantTrace: true,
			wantParts: []string{"own +0s 1s work", "claims errored"},
			wantReads: 1,
		},
		{
			// The own store's row is lost at its CAS; the reselection then reads
			// only the failing rig leg, once and three paced retries.
			name: "reselection read failure after a claim that took nothing",
			setup: func(b *budgetClaimHarness, ops *hookClaimOps) []hookStore {
				b.leg("/city", time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
				b.leg("/rig-alpha", time.Second, hookRunnerAnswer{err: errors.New("bd ready: exit status 1: unknown flag")})
				ops.Claim = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, bool, error) {
					return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "gc__other-seat"}, false, nil
				}
				return budgetLegs("alpha")
			},
			wantCode:  1,
			wantTrace: true,
			wantParts: []string{"own +0s 1s work", "rig:alpha +1s 1s error", "rig:alpha +20s 1s error", "reselection read failed after a claim that took nothing"},
			wantReads: 6,
		},
		{
			name: "reselection read failure after claims errored",
			setup: func(b *budgetClaimHarness, ops *hookClaimOps) []hookStore {
				b.leg("/city", time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
				b.leg("/rig-alpha", time.Second, hookRunnerAnswer{err: errors.New("bd ready: exit status 1: unknown flag")})
				ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
					return beads.Bead{}, false, errors.New("bd update: exit status 1: database is locked")
				}
				return budgetLegs("alpha")
			},
			wantCode:  1,
			wantTrace: true,
			wantParts: []string{"own +0s 1s work", "reselection read failed after claims errored"},
			wantReads: 6,
		},
		{
			name: "claim",
			setup: func(b *budgetClaimHarness, _ *hookClaimOps) []hookStore {
				b.leg("/city", time.Second, hookRunnerAnswer{out: routedRowJSON("work-1")})
				return budgetLegs()
			},
			wantCode: 0,
		},
		{
			name: "no_work drain",
			setup: func(b *budgetClaimHarness, _ *hookClaimOps) []hookStore {
				b.leg("/city", time.Second, budgetEmpty)
				b.leg("/rig-alpha", time.Second, budgetEmpty)
				return budgetLegs("alpha")
			},
			wantCode: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			ops := b.ops()
			stores := tc.setup(b, &ops)

			code, _, stderr := b.claim(stores, ops)

			if code != tc.wantCode {
				t.Fatalf("exit = %d, want %d; stderr=%s", code, tc.wantCode, stderr)
			}
			lines := readTraceLines(stderr)
			if !tc.wantTrace {
				if len(lines) != 0 {
					t.Fatalf("trace lines = %q, want none for this outcome", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("trace lines = %q, want exactly one; stderr=%s", lines, stderr)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(lines[0], part) {
					t.Fatalf("trace line %q is missing %q", lines[0], part)
				}
			}
			if got := strings.Count(lines[0], "; ") + 1; got != tc.wantReads+1 {
				t.Fatalf("trace line %q lists %d entries, want %d reads plus the outcome", lines[0], got, tc.wantReads)
			}
		})
	}
}

// A single-leg claim whose one read answers inside the budget is unchanged:
// one read, the same result line, the same exit code. Under a window override
// the budget is the override less the reserve.
func TestSingleLegClaimReadUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		cost   time.Duration
	}{
		{name: "default window", cost: 140 * time.Second},
		// A 75s override leaves a 65s read budget.
		{name: "window override", window: 75 * time.Second, cost: 60 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withProductionClaimRetryPacing(t)
			b := newBudgetClaimHarness()
			stores := budgetLegs()
			b.leg("/city", tc.cost, hookRunnerAnswer{out: routedRowJSON("work-1")})
			ops := b.ops()
			if tc.window > 0 {
				ops.ClaimWindow = tc.window
			}

			code, stdout, stderr := b.claim(stores, ops)

			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
			}
			if len(b.reads) != 1 {
				t.Fatalf("reads = %v, want exactly one", b.reads)
			}
			result := decodeTurnBoundResult(t, stdout)
			if result.Action != "work" || result.BeadID != "work-1" || !result.OK {
				t.Fatalf("result = %+v, want ok action=work bead_id=work-1", result)
			}
			if lines := readTraceLines(stderr); len(lines) != 0 {
				t.Fatalf("a claim wrote a read trace: %q", lines)
			}
		})
	}
}
