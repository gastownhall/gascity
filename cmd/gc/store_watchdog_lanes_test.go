package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// storeWatchdogSeed is order-tracking history every store watchdog has work
// in: one stale open tracking bead (the tracking sweep closes it) and more
// closed tracking beads than the retention floor keeps, all past the TTL (the
// retention watchdog prunes them).
func storeWatchdogSeed(now time.Time) []beads.Bead {
	seed := []beads.Bead{{
		ID:        "stale-open",
		Title:     "order:stale",
		Status:    "open",
		Type:      "task",
		CreatedAt: now.Add(-time.Hour),
		Labels:    []string{"order-run:stale", labelOrderTracking},
	}}
	for i := range minClosedOrderTrackingRetained + 2 {
		seed = append(seed, beads.Bead{
			ID:        fmt.Sprintf("prune-%02d", i),
			Title:     "order:prune",
			Status:    "closed",
			Type:      "task",
			CreatedAt: now.Add(-8*24*time.Hour + time.Duration(i)*time.Minute),
			Labels:    []string{"order-run:prune", labelOrderTracking},
			Ephemeral: true,
		})
	}
	return seed
}

// assertStoreWatchdogSeedUntouched fails if any watchdog mutated the seed.
func assertStoreWatchdogSeedUntouched(t *testing.T, store beads.Store) {
	t.Helper()
	stale, err := store.Get("stale-open")
	if err != nil {
		t.Fatalf("get stale-open: %v", err)
	}
	if stale.Status != "open" {
		t.Fatalf("stale-open status = %q, want open: the tracking sweep ran", stale.Status)
	}
	closed, err := store.List(beads.ListQuery{Status: "closed", Label: labelOrderTracking, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("list closed tracking: %v", err)
	}
	if got, want := len(closed), minClosedOrderTrackingRetained+2; got != want {
		t.Fatalf("closed tracking beads = %d, want %d: the retention watchdog pruned", got, want)
	}
}

// assertStoreWatchdogClocksZero fails if any watchdog clock advanced.
func assertStoreWatchdogClocksZero(t *testing.T, cr *CityRuntime) {
	t.Helper()
	if !cr.orderSweepWatchdogLast.IsZero() {
		t.Fatalf("orderSweepWatchdogLast = %v, want zero", cr.orderSweepWatchdogLast)
	}
	if !cr.orderSweepWatchdogNotClosedDue.IsZero() {
		t.Fatalf("orderSweepWatchdogNotClosedDue = %v, want zero", cr.orderSweepWatchdogNotClosedDue)
	}
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want zero", cr.orderTrackingRetentionWatchdogLast)
	}
	if !cr.nudgeMailSweepWatchdogLast.IsZero() {
		t.Fatalf("nudgeMailSweepWatchdogLast = %v, want zero", cr.nudgeMailSweepWatchdogLast)
	}
}

// Order dispatch runs no store watchdog: with every watchdog due and work for
// each in the store, the startup dispatch and an orders-lane pass both reach
// the dispatcher without invoking a watchdog, advancing its clock or touching
// the records it would mutate. The watchdogs run on their own lanes, which
// this test never starts.
func TestOrdersPassRunsNoStoreWatchdogs(t *testing.T) {
	now := time.Now()
	store := beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	cr.standaloneCityStore = store
	cr.wispIndexMigrationApplied = true
	cr.orderRescanLast = now
	cr.managedDoltOwned = func(string) (bool, error) { return false, nil }

	cr.dispatchOrders(context.Background(), cr.cityPath)
	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)

	if got := od.calls.Load(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2 (startup dispatch and one lane pass)", got)
	}
	assertStoreWatchdogClocksZero(t, cr)
	assertStoreWatchdogSeedUntouched(t, store)
}
