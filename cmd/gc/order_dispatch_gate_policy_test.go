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
