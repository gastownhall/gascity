package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// These tests pin the bounded order-run root gate: an open root-only wisp used
// to count as in-flight order work forever, so a root left behind by a stalled
// pool or a controller restart held its order's open-work gate shut for good.

func seedRootOnlyOrderWisp(t *testing.T, store beads.Store, order string, createdAt time.Time) beads.Bead {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title:     "vapor-order-root",
		Type:      "task",
		Labels:    []string{"order-run:" + order},
		Metadata:  map[string]string{"gc.kind": "wisp"},
		CreatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestOrderRunRootInFlightBound(t *testing.T) {
	for _, tt := range []struct {
		name string
		a    orders.Order
		want time.Duration
	}{
		{"long cooldown doubles the interval", orders.Order{Trigger: "cooldown", Interval: "1h"}, 2 * time.Hour},
		{"daily cooldown", orders.Order{Trigger: "cooldown", Interval: "24h"}, 48 * time.Hour},
		{"short cooldown uses the floor", orders.Order{Trigger: "cooldown", Interval: "1m"}, orderRunRootInFlightFloor},
		{"cron has no interval", orders.Order{Trigger: "cron", Schedule: "0 9 * * *"}, orderRunRootInFlightFloor},
		{"condition has no interval", orders.Order{Trigger: "condition", Check: "true"}, orderRunRootInFlightFloor},
		{"event has no interval", orders.Order{Trigger: "event", On: "bead.closed"}, orderRunRootInFlightFloor},
		{"unparseable interval uses the floor", orders.Order{Trigger: "cooldown", Interval: "soon"}, orderRunRootInFlightFloor},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := orderRunRootInFlightBound(tt.a); got != tt.want {
				t.Fatalf("orderRunRootInFlightBound = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestHasOpenWorkStrictBounded_StaleRootOnlyWispDoesNotBlock(t *testing.T) {
	now := time.Date(2030, 10, 1, 12, 0, 0, 0, time.UTC)
	store := &createdAtOverrideStore{Store: beads.NewMemStore()}
	seedRootOnlyOrderWisp(t, store, "digest", now.Add(-3*time.Hour))

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrictBounded(store, "digest", now, 2*time.Hour)
	if err != nil {
		t.Fatalf("hasOpenWorkStrictBounded: %v", err)
	}
	if has {
		t.Fatal("an unclaimed root-only wisp older than the bound must not block the order forever")
	}
}

func TestHasOpenWorkStrictBounded_FreshRootOnlyWispBlocks(t *testing.T) {
	now := time.Date(2030, 10, 1, 12, 0, 0, 0, time.UTC)
	store := &createdAtOverrideStore{Store: beads.NewMemStore()}
	seedRootOnlyOrderWisp(t, store, "digest", now.Add(-10*time.Minute))

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrictBounded(store, "digest", now, 2*time.Hour)
	if err != nil {
		t.Fatalf("hasOpenWorkStrictBounded: %v", err)
	}
	if !has {
		t.Fatal("a root-only wisp younger than the bound is still in flight (tr-kds01)")
	}
}

func TestHasOpenWorkStrictBounded_StaleClaimedRootOnlyWispBlocks(t *testing.T) {
	now := time.Date(2030, 10, 1, 12, 0, 0, 0, time.UTC)
	store := &createdAtOverrideStore{Store: beads.NewMemStore()}
	root := seedRootOnlyOrderWisp(t, store, "digest", now.Add(-3*time.Hour))
	if err := store.Update(root.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatal(err)
	}

	ad := &memoryOrderDispatcher{}
	has, err := ad.hasOpenWorkStrictBounded(store, "digest", now, 2*time.Hour)
	if err != nil {
		t.Fatalf("hasOpenWorkStrictBounded: %v", err)
	}
	if !has {
		t.Fatal("a claimed (in_progress) root-only wisp is live work and must block re-dispatch at any age")
	}
}

func TestOrderDispatchStaleRootOnlyWispNoLongerBlocks(t *testing.T) {
	now := time.Now()
	store := &createdAtOverrideStore{Store: beads.NewMemStore()}
	seedRootOnlyOrderWisp(t, store, "my-pool-order", now.Add(-2*time.Hour))

	ran := false
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		ran = true
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "my-pool-order",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/run.sh",
	}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, nil)

	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	if !ran {
		t.Fatal("order must dispatch: its only open order-run root is an unclaimed root-only wisp " +
			"two hours old, past the 30m bound for a 1m order")
	}
}

// TestOrderDispatchFreshRootOnlyWispBlocksAndReportsSuppression pins the other
// side of the bound: a root-only wisp younger than the bound still holds the
// gate, and a long enough streak of refusals is reported through the existing
// order.suppressed event (no second channel). Once the root closes, the order
// dispatches and the streak ends.
func TestOrderDispatchFreshRootOnlyWispBlocksAndReportsSuppression(t *testing.T) {
	now := time.Now()
	store := &createdAtOverrideStore{Store: beads.NewMemStore()}
	root := seedRootOnlyOrderWisp(t, store, "my-pool-order", now.Add(-10*time.Minute))

	runs := 0
	fakeExec := func(_ context.Context, _, _ string, _ []string) ([]byte, error) {
		runs++
		return nil, nil
	}
	aa := []orders.Order{{
		Name:     "my-pool-order",
		Trigger:  "cooldown",
		Interval: "1m",
		Exec:     "scripts/run.sh",
	}}
	rec := &memRecorder{}
	m := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, rec).(*memoryOrderDispatcher)
	cityPath := t.TempDir()

	// The root stays inside its 30m bound for every tick below.
	tick := now
	for i := 0; i < orderOpenWorkSuppressionAlertAfter; i++ {
		m.dispatch(context.Background(), cityPath, tick)
		m.drain(context.Background())
		tick = tick.Add(10 * time.Second)
	}

	if runs != 0 {
		t.Fatalf("exec ran %d time(s); a 10-minute-old root-only wisp is still in flight", runs)
	}
	got := rec.suppressedEvents()
	if len(got) != 1 {
		t.Fatalf("order.suppressed events after %d blocked ticks = %d, want 1", orderOpenWorkSuppressionAlertAfter, len(got))
	}
	if got[0].Subject != "my-pool-order" {
		t.Errorf("event subject = %q, want %q", got[0].Subject, "my-pool-order")
	}
	if p := decodeSuppressedPayload(t, got[0]); p.Consecutive != orderOpenWorkSuppressionAlertAfter {
		t.Errorf("payload consecutive = %d, want %d", p.Consecutive, orderOpenWorkSuppressionAlertAfter)
	}

	if err := store.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	m.dispatch(context.Background(), cityPath, tick.Add(time.Minute))
	m.drain(context.Background())
	if runs != 1 {
		t.Fatalf("exec ran %d time(s) after the root closed, want 1", runs)
	}
	m.cacheMu.Lock()
	_, stillSuppressed := m.openWorkSuppression["my-pool-order"]
	m.cacheMu.Unlock()
	if stillSuppressed {
		t.Fatal("suppression streak not cleared after the order dispatched")
	}
}
