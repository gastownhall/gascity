package main

import (
	"bytes"
	"context"
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
	// costs is consulted per (dir, attempt) like the scripted answers: the
	// last cost repeats.
	costs         map[string][]time.Duration
	reads         []string
	claims        []string
	claimFailed   []string
	windowExpired []hookClaimWindowExpiry
}

func newBudgetClaimHarness() *budgetClaimHarness {
	return &budgetClaimHarness{
		failureHookHarness: newFailureHookHarness(),
		clock:              newRetryWindowClock(0),
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
	code := claimHookWorkWithRunner("work-query", stores[0].dir, stores[0].env, stores, failureClaimOptions(), ops,
		b.run, b.emitFailure, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
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
