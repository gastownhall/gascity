package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// claimFanoutClock is a fake invocation clock for whole-claim fan-out tests.
// Where retryWindowClock paces the retry loop's reads directly, this one layers
// over a failureHookHarness runner and advances once per work-query read, so a
// test states where every leg's read lands against the invocation's claim
// window without scripting the loop's internals.
type claimFanoutClock struct {
	now       time.Time
	invokedAt time.Time
	readCost  time.Duration
	// readCosts, when set, is consumed one entry per read in place of the
	// uniform readCost, so a test can price reads differently.
	readCosts []time.Duration
	sleeps    []time.Duration
}

func newClaimFanoutClock(readCost time.Duration) *claimFanoutClock {
	start := time.Date(2026, 11, 27, 7, 0, 0, 0, time.UTC)
	return &claimFanoutClock{now: start, invokedAt: start, readCost: readCost}
}

func (c *claimFanoutClock) Now() time.Time { return c.now }

func (c *claimFanoutClock) Sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
}

// nextReadCost is the cost of the next work-query read: the queued per-read
// cost when one is set, else the uniform readCost.
func (c *claimFanoutClock) nextReadCost() time.Duration {
	if len(c.readCosts) > 0 {
		cost := c.readCosts[0]
		c.readCosts = c.readCosts[1:]
		return cost
	}
	return c.readCost
}

// claimFanoutRecorder captures what one claimHookWorkWithRunner invocation did:
// every claim mutation it attempted, the claim-window expiries it emitted, the
// claim mutations that errored, and how many work-query reads ran.
type claimFanoutRecorder struct {
	claims        []string
	windowExpired []hookClaimWindowExpiry
	claimFailed   []string
	reads         int
}

// runClaimFanout drives claimHookWorkWithRunner over a failureHookHarness on
// the fake clock: every work-query read advances the clock by its cost, and the
// ops record the claims, window expiries and claim failures the assertions
// read. tune, when given, adjusts the ops after the harness seeds them (and
// before the recorders wrap the seams) — e.g. to make a CAS lose its race. It
// returns the exit code, the decoded JSON result (zero value when nothing was
// written), stderr, and the recorder.
func runClaimFanout(t *testing.T, clock *claimFanoutClock, h *failureHookHarness, stores []hookStore, tune ...func(*hookClaimOps)) (int, hookClaimJSONResult, string, *claimFanoutRecorder) {
	t.Helper()
	rec := &claimFanoutRecorder{}
	ops := h.ops()
	for _, f := range tune {
		f(&ops)
	}
	baseClaim := ops.Claim
	ops.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		rec.claims = append(rec.claims, beadID)
		return baseClaim(ctx, dir, env, beadID, assignee)
	}
	ops.EmitClaimWindowExpired = func(e hookClaimWindowExpiry) { rec.windowExpired = append(rec.windowExpired, e) }
	ops.EmitClaimFailed = func(beadID string, _ hookClaimOptions, _ error) { rec.claimFailed = append(rec.claimFailed, beadID) }
	ops.Now, ops.Sleep = clock.Now, clock.Sleep
	ops.InvokedAt, ops.ClaimWindow = clock.invokedAt, retryWindowTestWindow
	run := func(command, dir string, env []string) (string, error) {
		rec.reads++
		out, err := h.run(command, dir, env)
		clock.now = clock.now.Add(clock.nextReadCost())
		return out, err
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("work-query", stores[0].dir, stores[0].env, stores, failureClaimOptions(), ops,
		run, h.emitFailure, &stdout, &stderr)
	var result hookClaimJSONResult
	if trimmed := strings.TrimSpace(stdout.String()); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
			t.Fatalf("decoding claim result %q: %v", trimmed, err)
		}
	}
	return code, result, stderr.String(), rec
}

// TestFederatedClaimClaimsRoutedWorkFoundBySlowLegs is issue #5693's reported
// defect: on a two-leg city where every work-query read costs 70s — the loaded
// worst case the incident measured — discovery ends at 140s, inside the 160s
// default claim window, so the claim must take the routed bead the fan-out
// found. At base the claim re-read the selected leg before the CAS, spent the
// window on that second read, and the CAS was then refused with "the claim
// window is spent" on every patrol: routed unassigned beads starved for hours
// while a manual bd update --claim took the same rows instantly.
func TestFederatedClaimClaimsRoutedWorkFoundBySlowLegs(t *testing.T) {
	h := newFailureHookHarness()
	h.script("/own", hookRunnerAnswer{out: "[]"})
	h.script("/rig", hookRunnerAnswer{out: routedRowJSON("work-1")})
	stores := []hookStore{
		{dir: "/own", env: []string{"BEADS_DIR=/own"}},
		{dir: "/rig", env: []string{"BEADS_DIR=/rig"}, rig: "alpha"},
	}
	clock := newClaimFanoutClock(70 * time.Second)

	code, result, stderr, rec := runClaimFanout(t, clock, h, stores)

	if code != 0 || result.Action != "work" || result.BeadID != "work-1" {
		t.Fatalf("exit=%d result=%+v, want exit 0 claiming work-1 (discovery ended at 140s, inside the 160s window); stderr=%s", code, result, stderr)
	}
	if len(rec.claims) != 1 || rec.claims[0] != "work-1" {
		t.Fatalf("claims = %v, want exactly [work-1]", rec.claims)
	}
	if len(rec.windowExpired) != 0 {
		t.Fatalf("execution.claim_window_expired fired %d times; the reads fit the window, only the removed re-read did not", len(rec.windowExpired))
	}
}

// federatedClaimLegs builds the store list of a multi-leg city: the agent's
// own store first, then one leg per rig (named, so failures name their leg).
func federatedClaimLegs(rigs int) []hookStore {
	legs := []hookStore{{dir: "/own", env: []string{"BEADS_DIR=/own"}}}
	for _, name := range []string{"alpha", "beta"}[:rigs] {
		legs = append(legs, hookStore{dir: "/" + name, env: []string{"BEADS_DIR=/" + name}, rig: name})
	}
	return legs
}

// TestFederatedClaimRunsNoReadBetweenSelectionAndCAS pins the read count of a
// single-pass federated claim: exactly one work-query read per leg. The claim
// acts on the discovery output directly (#5693) — the CAS re-checks the row it
// takes — so between selecting a store and attempting its claim there is no
// further read, on any topology. hookClaimReadsPerTick is pinned to the same
// number so the two cannot drift.
func TestFederatedClaimRunsNoReadBetweenSelectionAndCAS(t *testing.T) {
	for _, tc := range []struct {
		name string
		legs int // rig legs beyond the agent's own store
	}{
		{name: "two legs", legs: 1},
		{name: "three legs", legs: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFailureHookHarness()
			legs := federatedClaimLegs(tc.legs)
			for _, st := range legs {
				h.script(st.dir, hookRunnerAnswer{out: "[]"})
			}
			h.script(legs[len(legs)-1].dir, hookRunnerAnswer{out: routedRowJSON("work-1")})
			clock := newClaimFanoutClock(time.Second)

			code, result, stderr, rec := runClaimFanout(t, clock, h, legs)

			if code != 0 || result.Action != "work" || result.BeadID != "work-1" {
				t.Fatalf("exit=%d result=%+v, want exit 0 claiming work-1; stderr=%s", code, result, stderr)
			}
			if rec.reads != len(legs) {
				t.Fatalf("work-query reads = %d, want exactly one per leg (%d): nothing runs between selection and the CAS", rec.reads, len(legs))
			}
			if got := hookClaimReadsPerTick(legs); got != len(legs) {
				t.Fatalf("hookClaimReadsPerTick = %d, want %d (one read per leg)", got, len(legs))
			}
		})
	}
}

// TestFederatedClaimLostCASFallsThroughToLaterLeg pins the case the claim loop
// itself must cover now that the claim acts on the discovery output directly
// (#5693): the discovery-selected store's best row is lost to another claimant
// at CAS time (ok=false, no error) and a later leg still holds routed work. A
// lost CAS is non-terminal, the exhausted store is dropped, and reselection
// claims the later leg's work — so this must end in a claim, never a no_work
// drain that strands routed work behind a lost race.
func TestFederatedClaimLostCASFallsThroughToLaterLeg(t *testing.T) {
	h := newFailureHookHarness()
	// Priority 1 outranks the default 2, so discovery deterministically selects
	// the own store first.
	h.script("/own", hookRunnerAnswer{out: routedRowJSONPriority("own-1", 1)})
	h.script("/rig", hookRunnerAnswer{out: routedRowJSON("rig-1")})
	stores := []hookStore{
		{dir: "/own", env: []string{"BEADS_DIR=/own"}},
		{dir: "/rig", env: []string{"BEADS_DIR=/rig"}, rig: "alpha"},
	}
	clock := newClaimFanoutClock(time.Second)
	loseOwnRace := func(o *hookClaimOps) {
		base := o.Claim
		o.Claim = func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
			if beadID == "own-1" {
				return beads.Bead{}, false, nil // lost the race: taken between discovery and the CAS
			}
			return base(ctx, dir, env, beadID, assignee)
		}
	}

	code, result, stderr, rec := runClaimFanout(t, clock, h, stores, loseOwnRace)

	if code != 0 || result.Action != "work" || result.BeadID != "rig-1" {
		t.Fatalf("exit=%d result=%+v, want exit 0 claiming rig-1 after the lost race; stderr=%s", code, result, stderr)
	}
	if len(rec.claims) != 2 || rec.claims[0] != "own-1" || rec.claims[1] != "rig-1" {
		t.Fatalf("claims = %v, want [own-1 rig-1]: the lost CAS first, then the later leg", rec.claims)
	}
}

// TestClaimReadsStopAtTheInvocationReadBudget pins the one-invocation read
// budget (#5693): claim-path reads must finish by
// InvokedAt+ClaimWindow−reserve (150s of the default 160s window), so a
// three-leg fan-out whose last leg would answer with work at 180s is cut as a
// timed-out read, not a claim that spent the window finding work it cannot
// take. The invocation keeps the failed-read contract: exit 2 with the
// store-unavailable token, a work-query-failure event naming the leg that was
// cut, no drain result, no drain-ack, no claim mutation, and no
// execution.claim_window_expired — at base the incident's shape: work
// discovered at 180s, CAS refused at 180s.
func TestClaimReadsStopAtTheInvocationReadBudget(t *testing.T) {
	withProductionClaimRetryPacing(t)
	h := newFailureHookHarness()
	h.script("/own", hookRunnerAnswer{out: "[]"})
	h.script("/alpha", hookRunnerAnswer{out: "[]"})
	h.script("/beta", hookRunnerAnswer{out: routedRowJSON("work-1")})
	stores := federatedClaimLegs(2) // /own, /alpha, /beta
	clock := newClaimFanoutClock(60 * time.Second)

	code, result, stderr, rec := runClaimFanout(t, clock, h, stores)

	if code != 2 {
		t.Fatalf("exit=%d, want 2: the third read's answer lands at 180s, at/after the 150s read deadline, so it is a timed-out read; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, hookStoreUnavailableToken) {
		t.Fatalf("stderr = %q, want the store-unavailable token for a read cut by the budget", stderr)
	}
	if !strings.Contains(stderr, "read budget") {
		t.Fatalf("stderr = %q, want the failure to name the read budget", stderr)
	}
	if !strings.Contains(stderr, "beta") {
		t.Fatalf("stderr = %q, want the failure to name the leg that was cut", stderr)
	}
	if result.Action != "" || h.drained {
		t.Fatalf("result=%+v drained=%v; a cut read keeps the failed-read contract: no drain result, no drain-ack", result, h.drained)
	}
	if len(rec.claims) != 0 {
		t.Fatalf("claims = %v, want none: no answer reached a CAS", rec.claims)
	}
	if len(rec.windowExpired) != 0 {
		t.Fatalf("execution.claim_window_expired fired %d times; a cut read is a read failure, not a window refusal", len(rec.windowExpired))
	}
	if len(h.events) == 0 || !strings.Contains(h.events[0].Message, "beta") {
		t.Fatalf("events = %+v, want a work-query failure naming the cut leg", h.events)
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
