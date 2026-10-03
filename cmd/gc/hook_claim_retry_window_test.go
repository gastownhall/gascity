package main

import (
	"bytes"
	"context"
	"errors"
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
// invocation keeps the failed-read contract: exit 1, no drain result, no
// drain-ack, no claim mutation, and the event bus still records the failure.
func TestHookClaimRetryCutByWindowExitsWithoutClaimOrDrain(t *testing.T) {
	withProductionClaimRetryPacing(t)
	clock := newRetryWindowClock(0)
	h := newFailureHookHarness()
	h.script("/rig",
		hookRunnerAnswer{err: errors.New("bd ready: exit status 1")},
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

	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
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
}
