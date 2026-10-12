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

// countAndDelayGateQuery records one gate query against counter and then blocks
// for delay. The call-counting stores share it so this file keeps a single
// direct sleep call site; see internal/testpolicy/resourcecensus, whose
// untagged fixed_sleep ledger is pinned and trips on a net-new direct sleep.
func countAndDelayGateQuery(mu *sync.Mutex, counter *int, delay time.Duration) {
	mu.Lock()
	*counter++
	mu.Unlock()
	time.Sleep(delay)
}

// isOrderGateIndexQuery reports whether q is the per-tick gate INDEX read: one
// unlabeled non-closed scan per gate store, folded by order-run label, that
// replaced the per-order label lists (ga-l7jdg).
//
// Every #2893 fixture in this file recognizes it as a gate query alongside the
// two label shapes it replaced. Recognizing only the old spellings would leave
// the fixtures delaying a query the dispatcher no longer issues — the store
// would answer instantly, the gate would never time out, and four fail-closed
// regression tests would pass while testing nothing.
func isOrderGateIndexQuery(q beads.ListQuery) bool {
	return q.AllowScan && q.Label == "" && q.Status == "" && q.Assignee == "" &&
		len(q.IDs) == 0 && len(q.Metadata) == 0 && !q.IncludeClosed && q.Limit == 0
}

// isOrderGateListQuery reports whether q is a read either open-work gate makes:
// the per-tick index scan, the strict `order-run:`-labeled fallback, or the
// open-tracking list.
func isOrderGateListQuery(q beads.ListQuery) bool {
	if isOrderGateIndexQuery(q) {
		return true
	}
	if q.IncludeClosed || q.Limit != 0 {
		return false
	}
	return strings.HasPrefix(q.Label, "order-run:") ||
		(q.Label == labelOrderTracking && q.Status == "open")
}

// gateTimeoutStore makes the open-work gate's store read block past the
// per-order gate timeout, reproducing the #2893 hang where
// storeHasOpenDescendants exceeds its budget under Dolt contention. Only a gate
// query shape is delayed; every other read stays fast.
type gateTimeoutStore struct {
	beads.Store
	delay time.Duration
}

func (s *gateTimeoutStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if isOrderGateListQuery(query) {
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

// gateErrorStore returns a supplied error (not a hang) from the open-work gate
// reads, reproducing vp-gprv where the wisp-tier bd query fails with "bd query:
// timed out after 30s" under Dolt contention. Unlike gateTimeoutStore (which
// sleeps until the per-order bound fires and yields an errGateTimeout), the
// error arrives promptly and reaches gateFailClosed as a store-read error.
//
// The gate reads the store on TWO legs, and both must fail for the gate itself
// to fail, because they are each other's fallback:
//
//   - the per-tick batched order-run index (orderDispatchTrackingIndex's
//     entriesForStore), a LABEL-LESS AllowScan read of the store's whole
//     non-closed corpus, folded by label for every order at once; and
//   - hasOpenWorkStrict, the per-order "order-run:<scoped>" label query the
//     index falls back to when its scan errors, so an index hole is not read as
//     an absence of work.
//
// Failing only the label query lets the batched scan succeed and answer "no open
// work" for every order, so the gate never errors, every order dispatches, and
// the test passes for entirely the wrong reason while exercising none of the
// timeout path. Every other read stays live: notably the order-run history query
// behind lastRun must succeed, or dispatch bails at its lastRunErr check before
// gateFailClosed is ever consulted.
type gateErrorStore struct {
	beads.Store
	err error
}

// isGateIndexScan reports whether the query is the batched, label-less order-run
// index scan the tracking index issues once per store per tick.
func isGateIndexScan(query beads.ListQuery) bool {
	return query.AllowScan && query.Label == ""
}

// isStrictGateQuery reports whether the query is the per-order single-flight
// "order-run:<scoped>" label read the index falls back to.
func isStrictGateQuery(query beads.ListQuery) bool {
	return strings.HasPrefix(query.Label, "order-run:") && !query.IncludeClosed && query.Limit == 0
}

func (s *gateErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if isGateIndexScan(query) || isStrictGateQuery(query) {
		return nil, s.err
	}
	return s.Store.List(query)
}

// TestOrderDispatchIdempotentFailsOpenOnStoreTimeout is the vp-gprv regression:
// when the open-work gate's wisp bd query TIMES OUT (returns a timeout error
// rather than merely hanging past the per-order bound), an idempotent order
// must still fail OPEN and dispatch, while a non-idempotent order fails CLOSED.
// code-review-gate (idempotent) was starved fleet-wide because this store-layer
// timeout was misclassified as a genuine read failure and failed closed even
// though the order opted into idempotent fail-open.
func TestOrderDispatchIdempotentFailsOpenOnStoreTimeout(t *testing.T) {
	store := &gateErrorStore{
		Store: beads.NewMemStore(),
		err:   fmt.Errorf("bd list both tiers: bd query: %w", errors.New("timed out after 30s")),
	}
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	aa := []orders.Order{
		{Name: "code-review-gate", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: true},
		{Name: "merge-loop-sweep", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: false},
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	if got := trackingBeads(t, store, "order-run:code-review-gate"); len(got) == 0 {
		t.Error("idempotent order should fail OPEN on a store-query timeout and dispatch, but no tracking bead was created (vp-gprv starvation)")
	}
	if got := trackingBeads(t, store, "order-run:merge-loop-sweep"); len(got) != 0 {
		t.Errorf("non-idempotent order should fail CLOSED on a store-query timeout and skip; got %d tracking beads", len(got))
	}
}

// TestOrderDispatchIdempotentFailsOpenOnBothTiersDown pins the end-to-end
// consequence of the mixed-chain classification decision, at the layer where
// starvation actually shows up.
//
// When BOTH list tiers fail, mergeListTierResults returns
// errors.Join(primaryErr, ephemeralErr) — in practice a timeout leaf joined to a
// hard-failure leaf. Requiring every leaf to be timeout-shaped before relaxing
// the gate would fail an idempotent order CLOSED here, and both-tiers-down is
// strictly WORSE store contention than the single-tier timeout that starved
// code-review-gate fleet-wide in the first place. That would reinstate vp-gprv
// at exactly the moment the fail-open matters most, and would make this
// classifier stricter than isBdAmbiguousWriteError, which already treats
// "timed out after" and "connection reset" as one transient family.
//
// So an idempotent order must still dispatch, and a non-idempotent order must
// still be skipped — single-flight is not relaxed for anyone who did not opt in.
func TestOrderDispatchIdempotentFailsOpenOnBothTiersDown(t *testing.T) {
	store := &gateErrorStore{
		Store: beads.NewMemStore(),
		err: fmt.Errorf("bd list both tiers: %w", errors.Join(
			errors.New("bd query: timed out after 30s"),
			errors.New("dolt: read failed"),
		)),
	}
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	aa := []orders.Order{
		{Name: "code-review-gate", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: true},
		{Name: "merge-loop-sweep", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: false},
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	if got := trackingBeads(t, store, "order-run:code-review-gate"); len(got) == 0 {
		t.Error("idempotent order should fail OPEN when both list tiers fail with a joined timeout and dispatch, but no tracking bead was created (vp-gprv starvation under maximal contention)")
	}
	if got := trackingBeads(t, store, "order-run:merge-loop-sweep"); len(got) != 0 {
		t.Errorf("non-idempotent order should still fail CLOSED when both tiers are down; got %d tracking beads", len(got))
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
		t.Error("idempotent order must fail CLOSED on a non-timeout gate error (only a timeout fails open)")
	}

	// A raw store/bd query timeout (the wisp "bd query: timed out after 30s"
	// case, vp-gprv) is the same store-contention signal as the per-order gate
	// bound, just surfaced from a different layer: an idempotent order must fail
	// OPEN on it, a non-idempotent order still fails CLOSED. Before the fix this
	// reached gateFailClosed as a non-errGateTimeout error and blocked even
	// idempotent orders, starving code-review-gate fleet-wide.
	storeTimeoutErr := fmt.Errorf("checking open work: %w", errors.New("bd list both tiers: bd query: timed out after 30s"))
	if m.gateFailClosed(context.Background(), orders.Order{Idempotent: true}, "review", storeTimeoutErr) {
		t.Error("idempotent order on a store-query timeout should fail OPEN (vp-gprv)")
	}
	if !m.gateFailClosed(context.Background(), orders.Order{Idempotent: false}, "sweep", storeTimeoutErr) {
		t.Error("non-idempotent order on a store-query timeout should still fail CLOSED")
	}

	// Both list tiers failing at once produces errors.Join(timeout, read-failure)
	// from mergeListTierResults, and an idempotent order fails OPEN on it. That
	// is the reviewed decision, pinned here so a later "only fail open when every
	// leaf is a timeout" tightening cannot quietly restore the vp-gprv
	// starvation: both-tiers-down is strictly worse contention than the
	// single-tier failure that starved code-review-gate, so it is precisely where
	// an idempotent order must keep dispatching. Non-idempotent orders are
	// unaffected — they still fail CLOSED and keep single-flight.
	bothTiersDownErr := fmt.Errorf("checking open work: %w",
		errors.Join(errors.New("bd query: timed out after 30s"), errors.New("dolt: read failed")))
	if m.gateFailClosed(context.Background(), orders.Order{Idempotent: true}, "review", bothTiersDownErr) {
		t.Error("idempotent order on a both-tiers-down join carrying a timeout leaf should fail OPEN (vp-gprv); failing CLOSED here starves it under the worst contention")
	}
	if !m.gateFailClosed(context.Background(), orders.Order{Idempotent: false}, "sweep", bothTiersDownErr) {
		t.Error("non-idempotent order on a both-tiers-down join should still fail CLOSED")
	}
	// Control: the same join shape with NO timeout leaf is a plain store failure
	// and must block even an idempotent order, so the assertion above pins the
	// timeout leaf rather than "any joined error".
	noTimeoutJoinErr := fmt.Errorf("checking open work: %w",
		errors.Join(errors.New("dolt: read failed"), errors.New("connection reset by peer")))
	if !m.gateFailClosed(context.Background(), orders.Order{Idempotent: true}, "review", noTimeoutJoinErr) {
		t.Error("idempotent order must fail CLOSED on a joined store failure with no timeout leaf")
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
	if isOrderGateListQuery(q) {
		countAndDelayGateQuery(&s.mu, &s.gateCalls, s.delay)
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
	if isOrderGateIndexQuery(query) || (query.Label == labelOrderTracking && query.Status == "open" && !query.IncludeClosed && query.Limit == 0) {
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

// bothGatesCallCountStore counts every List call that belongs to an open-work
// gate query — the first gate (listCanonicalOpenOrderTrackingBeads: Label ==
// labelOrderTracking, Status open, !IncludeClosed, Limit 0) and the second
// gate (hasOpenWorkStrict: Label order-run:*, !IncludeClosed, Limit 0). Each
// such call also sleeps past orderGateTimeout so a non-opt-out order is
// skipped (fail-closed) — reproducing the #2893 dispatch starvation that
// NoWorkGate exists to bypass.
type bothGatesCallCountStore struct {
	beads.Store
	delay time.Duration
	mu    sync.Mutex
	calls int
}

func (s *bothGatesCallCountStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.isGateQuery(q) {
		countAndDelayGateQuery(&s.mu, &s.calls, s.delay)
	}
	return s.Store.List(q)
}

func (s *bothGatesCallCountStore) isGateQuery(q beads.ListQuery) bool {
	return isOrderGateListQuery(q)
}

func (s *bothGatesCallCountStore) gateCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestOrderDispatchNoWorkGateSkipsGatesUnderStoreDelay is the vp-cixi.6
// regression test: a pure cooldown probe that tracks no beads sets
// NoWorkGate, so the dispatcher must NOT run either open-work gate for it —
// not even under a store so slow the gate would time out and skip the probe
// every cycle (#2893 dispatch starvation -> stale provider-health cache ->
// fail-closed provider health). The probe still dispatches on its cooldown,
// and a plain (gate-protected) order under the same slow store is still
// skipped (fail-closed) as before.
func TestOrderDispatchNoWorkGateSkipsGatesUnderStoreDelay(t *testing.T) {
	prev := orderGateTimeout
	orderGateTimeout = 20 * time.Millisecond
	defer func() { orderGateTimeout = prev }()

	store := &bothGatesCallCountStore{Store: beads.NewMemStore(), delay: 300 * time.Millisecond}
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	aa := []orders.Order{
		{Name: "provider-health-probe", Trigger: "cooldown", Interval: "1m", Exec: "true", NoWorkGate: true},
		{Name: "merge-loop-sweep", Trigger: "cooldown", Interval: "1m", Exec: "true"},
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	// The NoWorkGate probe must dispatch (fail-closed starvation bypassed).
	if got := trackingBeads(t, store.Store, "order-run:provider-health-probe"); len(got) == 0 {
		t.Error("NoWorkGate order should dispatch without entering the gate, but no tracking bead was created (the #2893 starvation this fixes)")
	}
	// The plain order must still be skipped (fail-closed) under the slow store.
	if got := trackingBeads(t, store.Store, "order-run:merge-loop-sweep"); len(got) != 0 {
		t.Errorf("plain order should fail CLOSED on gate timeout and skip; got %d tracking beads", len(got))
	}
	// No gate query should have run for the NoWorkGate order. The plain order's
	// first gate (hasOpenTracking) runs once before timing out, so the total is
	// exactly one gate call — NOT one per order, and NOT the second gate.
	if got := store.gateCalls(); got != 1 {
		t.Errorf("expected exactly 1 gate query (the plain order's first gate, timed out); got %d — NoWorkGate must skip both gates entirely (#2893)", got)
	}
}

// TestOrderDispatchNoWorkGateSkipsTrackingGateDirectly narrows the NoWorkGate
// behavior to the first gate site: a NoWorkGate order must skip the tracking
// gate and dispatch, issuing ZERO gate queries.
func TestOrderDispatchNoWorkGateSkipsTrackingGateDirectly(t *testing.T) {
	store := &bothGatesCallCountStore{Store: beads.NewMemStore(), delay: 0}
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	aa := []orders.Order{
		{Name: "provider-health-probe", Trigger: "cooldown", Interval: "1m", Exec: "true", NoWorkGate: true},
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	if got := trackingBeads(t, store.Store, "order-run:provider-health-probe"); len(got) == 0 {
		t.Fatal("NoWorkGate order should dispatch without entering either gate")
	}
	if got := store.gateCalls(); got != 0 {
		t.Errorf("NoWorkGate order must issue ZERO gate queries; got %d", got)
	}
}
