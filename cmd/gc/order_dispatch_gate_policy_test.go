package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// gateTimeoutStore makes the strict open-work gate scan (the
// `order-run:`-labeled, !IncludeClosed, Limit==0 List that hasOpenWorkStrict
// issues) block past the per-order gate timeout, reproducing the #2893 hang
// where storeHasOpenDescendants exceeds its budget under Dolt contention. Only
// that exact query shape is delayed; every other read stays fast.
type gateTimeoutStore struct {
	beads.Store
	delay time.Duration
}

func (s *gateTimeoutStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(query.Label, "order-run:") && !query.IncludeClosed && query.Limit == 0 {
		time.Sleep(s.delay)
	}
	return s.Store.List(query)
}

// TestOrderDispatchIdempotentFailsOpenOnGateTimeout is the #2893 #2'
// regression test: when the open-work gate exceeds its bound, an order marked
// idempotent must dispatch anyway (fail open) while a non-idempotent order
// must still be skipped (fail closed). Before the fix BOTH orders were skipped
// on gate timeout, starving the feeders fleet-wide.
func TestOrderDispatchIdempotentFailsOpenOnGateTimeout(t *testing.T) {
	prev := orderGateTimeout
	orderGateTimeout = 20 * time.Millisecond
	defer func() { orderGateTimeout = prev }()

	store := &gateTimeoutStore{Store: beads.NewMemStore(), delay: 300 * time.Millisecond}
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	aa := []orders.Order{
		{Name: "unrouted-feeder", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: true},
		{Name: "merge-loop-sweep", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: false},
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	if got := trackingBeads(t, store, "order-run:unrouted-feeder"); len(got) == 0 {
		t.Error("idempotent order should fail OPEN on gate timeout and dispatch, but no tracking bead was created (order was skipped — the starvation regression)")
	}
	if got := trackingBeads(t, store, "order-run:merge-loop-sweep"); len(got) != 0 {
		t.Errorf("non-idempotent order should fail CLOSED on gate timeout and skip; got %d tracking beads", len(got))
	}
}

// TestGateFailClosed covers the gate-error decision logic directly: a per-order
// gate timeout fails open only for idempotent orders, but a done dispatch
// context (shutdown / tick deadline) always blocks, even for idempotent orders.
func TestGateFailClosed(t *testing.T) {
	m := &memoryOrderDispatcher{stderr: lockedStderr(&bytes.Buffer{})}
	gateErr := fmt.Errorf("open-work gate for x timed out: %w", errGateTimeout)

	if m.gateFailClosed(context.Background(), orders.Order{Idempotent: true}, "feeder", gateErr) {
		t.Error("idempotent order on a live-context gate timeout should fail OPEN (not blocked)")
	}
	if !m.gateFailClosed(context.Background(), orders.Order{Idempotent: false}, "sweep", gateErr) {
		t.Error("non-idempotent order on gate timeout should fail CLOSED (blocked)")
	}
	if !m.gateFailClosed(context.Background(), orders.Order{Idempotent: true}, "feeder", errors.New("dolt: read failed")) {
		t.Error("idempotent order must fail CLOSED on a non-timeout gate error (only the bounded-gate timeout fails open)")
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if !m.gateFailClosed(canceledCtx, orders.Order{Idempotent: true}, "feeder", gateErr) {
		t.Error("a canceled dispatch context must block even idempotent orders (no dispatch into a dead context)")
	}
}

// openWorkGateCallCountStore is a gateTimeoutStore that also counts how many
// times the slow open-work gate path (the strict order-run label scan) is
// entered, so tests can assert the dispatcher avoided the gate on backoff ticks.
type openWorkGateCallCountStore struct {
	beads.Store
	delay     time.Duration
	mu        sync.Mutex
	gateCalls int
}

func (s *openWorkGateCallCountStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(q.Label, "order-run:") && !q.IncludeClosed && q.Limit == 0 {
		s.mu.Lock()
		s.gateCalls++
		s.mu.Unlock()
		time.Sleep(s.delay)
	}
	return s.Store.List(q)
}

func (s *openWorkGateCallCountStore) gateCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gateCalls
}

// TestOrderDispatchGateTimeoutBackoffPreventsRethrash is the gascity#3688
// regression test: when the open-work gate times out for a non-idempotent
// order (fail-closed), the dispatcher must set a gateBackoffUntil deadline so
// neither gate is reached on subsequent ticks — instead of hammering Dolt with
// a new 8-second gate query every tick.
func TestOrderDispatchGateTimeoutBackoffPreventsRethrash(t *testing.T) {
	prev := orderGateTimeout
	orderGateTimeout = 20 * time.Millisecond
	defer func() { orderGateTimeout = prev }()

	store := &openWorkGateCallCountStore{Store: beads.NewMemStore(), delay: 300 * time.Millisecond}
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	aa := []orders.Order{
		{Name: "merge-loop-sweep", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: false},
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	cityPath := t.TempDir() // must be stable across ticks so the store-key cache hits

	// Tick 1 at now: gate times out, fail-closed. With the fix, a gateBackoffUntil
	// deadline is set so neither gate is reached on any tick within the backoff window.
	ad.dispatch(context.Background(), cityPath, now)
	ad.drain(context.Background())

	if got := trackingBeads(t, store.Store, "order-run:merge-loop-sweep"); len(got) != 0 {
		t.Errorf("tick 1: non-idempotent order should fail CLOSED on gate timeout; got %d tracking beads", len(got))
	}
	afterTick1 := store.gateCallCount()
	if afterTick1 == 0 {
		t.Fatal("gate should have been called on tick 1 (to produce the timeout)")
	}

	// Tick 2: advance now by orderGateTimeout to mirror production reality — the
	// previous tick blocked for the full gate duration before returning, so the
	// next dispatchOrders call samples a fresh time.Now() ≥ tick1_start + gateTimeout.
	// gateBackoffActive must still return true because the deadline is anchored to
	// the actual wall clock at timeout (time.Now()+orderGateBackoffDuration), which
	// extends well beyond the tick-start offset. Without the fix this assertion
	// would fail: the deadline tick_start+gateTimeout is already in the past.
	ad.dispatch(context.Background(), cityPath, now.Add(orderGateTimeout))
	ad.drain(context.Background())

	if extra := store.gateCallCount() - afterTick1; extra > 0 {
		t.Errorf("tick 2 (within backoff window): gate was called %d extra time(s); backoff should have suppressed it (#3688)", extra)
	}
	if got := trackingBeads(t, store.Store, "order-run:merge-loop-sweep"); len(got) != 0 {
		t.Errorf("tick 2: order should not have dispatched during backoff; got %d tracking beads", len(got))
	}
}

// TestStoreHasOpenDescendantsSkipsTransientNotifications covers #2893 #3: a
// lingering open nudge/mail descendant must not keep the gate "open", but a real
// open work descendant still counts, and the nil-skip (sweeper) path keeps the
// original semantics where any open child counts.
func TestStoreHasOpenDescendantsSkipsTransientNotifications(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "wisp root", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "nudge:abc",
		Type:     nudgeBeadType,
		Status:   "open",
		ParentID: root.ID,
		Labels:   []string{nudgeBeadLabel},
	}); err != nil {
		t.Fatal(err)
	}

	// Gate semantics (skip notifications): a lone open nudge does not block.
	if has, err := storeHasOpenDescendants(store, root.ID, isTransientNotificationBead); err != nil {
		t.Fatal(err)
	} else if has {
		t.Error("a lone open nudge descendant must NOT count as open work (#2893 #3)")
	}

	// Sweeper semantics (nil skip): the open nudge still counts.
	if has, err := storeHasOpenDescendants(store, root.ID, nil); err != nil {
		t.Fatal(err)
	} else if !has {
		t.Error("nil skip must preserve original semantics: any open child counts")
	}

	// A real open work descendant still blocks even with the skip predicate.
	if _, err := store.Create(beads.Bead{Title: "real work", Type: "task", Status: "open", ParentID: root.ID}); err != nil {
		t.Fatal(err)
	}
	if has, err := storeHasOpenDescendants(store, root.ID, isTransientNotificationBead); err != nil {
		t.Fatal(err)
	} else if !has {
		t.Error("a real open work descendant must still count as open work")
	}
}

// trackingGateTimeoutStore makes the first open-work gate
// (OpenRuns, which queries Label==labelOrderTracking)
// block past the per-order gate timeout, reproducing the first-gate timeout
// path that gateBackoffUntil must suppress on subsequent ticks.
type trackingGateTimeoutStore struct {
	beads.Store
	delay     time.Duration
	gateCount atomic.Int32
}

func (s *trackingGateTimeoutStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == labelOrderTracking && query.Status == "open" && !query.IncludeClosed && query.Limit == 0 {
		s.gateCount.Add(1)
		time.Sleep(s.delay)
	}
	return s.Store.List(query)
}

// TestOrderDispatchEventTriggeredBackoffOnTrackingGateTimeout verifies that
// the gate-timeout backoff is trigger-agnostic: an event-triggered (non-cron)
// non-idempotent order whose first open-work gate (hasOpenTracking) times out
// is suppressed on subsequent ticks via gateBackoffUntil, exactly as for
// cooldown-triggered orders. With the old rememberLastRun approach this path
// was unprotected because orderTriggerUsesLastRun returned false for event
// triggers (#3688, event-trigger gap).
func TestOrderDispatchEventTriggeredBackoffOnTrackingGateTimeout(t *testing.T) {
	prev := orderGateTimeout
	orderGateTimeout = 20 * time.Millisecond
	defer func() { orderGateTimeout = prev }()

	store := &trackingGateTimeoutStore{Store: beads.NewMemStore(), delay: 50 * time.Millisecond}
	now := time.Date(2026, 6, 27, 17, 0, 0, 0, time.UTC)
	orderName := "cascade-nudge-on-event"

	aa := []orders.Order{{
		Name:    orderName,
		Trigger: "event",
		On:      "bead.closed",
		Exec:    "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	cityPath := t.TempDir()

	// Tick 1: first gate times out, fail-closed. gateBackoffUntil is set so
	// tick 2 skips without re-entering the gate, regardless of trigger type.
	ad.dispatch(context.Background(), cityPath, now)
	ad.drain(context.Background())
	if got := trackingBeads(t, store.Store, "order-run:"+orderName); len(got) != 0 {
		t.Fatalf("tick 1: event-triggered order must be skipped on tracking gate timeout; got %d tracking beads", len(got))
	}
	countAfterTick1 := store.gateCount.Load()
	if countAfterTick1 == 0 {
		t.Fatal("tick 1 did not reach the open order-tracking gate")
	}

	// Tick 2: advance now by orderGateTimeout to mirror production reality — the
	// previous tick blocked for the full gate duration, so the next tick's
	// dispatchOrders samples now' ≥ tick1_start + orderGateTimeout. The deadline
	// is anchored to actual wall clock + orderGateBackoffDuration, so the backoff
	// is still active. Without the fix (deadline = tick_start + gateTimeout ≈ now),
	// this assertion would fail.
	ad.dispatch(context.Background(), cityPath, now.Add(orderGateTimeout))
	ad.drain(context.Background())

	if got := store.gateCount.Load(); got != countAfterTick1 {
		t.Fatalf("tick 2 re-entered the tracking gate for event-triggered order: got %d calls, want %d (#3688 event-trigger gap)", got, countAfterTick1)
	}
	if got := trackingBeads(t, store.Store, "order-run:"+orderName); len(got) != 0 {
		t.Fatalf("tick 2: no tracking bead expected while gate-timeout backoff is active; got %d", len(got))
	}
}

// TestOrderDispatchNonIdempotentBackoffOnOpenTrackingTimeout verifies that when
// the first open-work gate (hasOpenTracking / OpenRuns)
// times out for a non-idempotent order, gateBackoffUntil is set and suppresses
// re-entry into that gate on subsequent ticks (#3688, first-gate site).
func TestOrderDispatchNonIdempotentBackoffOnOpenTrackingTimeout(t *testing.T) {
	prev := orderGateTimeout
	orderGateTimeout = 20 * time.Millisecond
	defer func() { orderGateTimeout = prev }()

	store := &trackingGateTimeoutStore{Store: beads.NewMemStore(), delay: 50 * time.Millisecond}
	now := time.Date(2026, 6, 27, 17, 0, 0, 0, time.UTC)
	orderName := "cascade-nudge-on-blocker-close"

	aa := []orders.Order{{
		Name:     orderName,
		Trigger:  "cooldown",
		Interval: "5m",
		Exec:     "true",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	cityPath := t.TempDir()

	// Tick 1: gate times out, fail-closed (non-idempotent). gateBackoffUntil
	// deadline is set so tick 2 is skipped before reaching the gate.
	ad.dispatch(context.Background(), cityPath, now)
	ad.drain(context.Background())
	if got := trackingBeads(t, store.Store, "order-run:"+orderName); len(got) != 0 {
		t.Fatalf("tick 1: non-idempotent order must be skipped on tracking gate timeout; got %d tracking beads", len(got))
	}
	countAfterTick1 := store.gateCount.Load()
	if countAfterTick1 == 0 {
		t.Fatal("tick 1 did not reach the open order-tracking gate")
	}

	// Tick 2: advance now by orderGateTimeout to mirror production reality — the
	// previous tick blocked for the full gate duration. The deadline is anchored
	// to actual wall clock + orderGateBackoffDuration, so the backoff is still
	// active. Without the fix this assertion would fail.
	ad.dispatch(context.Background(), cityPath, now.Add(orderGateTimeout))
	ad.drain(context.Background())

	if got := store.gateCount.Load(); got != countAfterTick1 {
		t.Fatalf("tick 2 re-entered the open order-tracking gate after tick 1 timed out: got %d calls, want %d (#3688 first-gate site)", got, countAfterTick1)
	}
	if got := trackingBeads(t, store.Store, "order-run:"+orderName); len(got) != 0 {
		t.Fatalf("tick 2: no tracking bead expected while gate-timeout backoff is active; got %d", len(got))
	}
}
