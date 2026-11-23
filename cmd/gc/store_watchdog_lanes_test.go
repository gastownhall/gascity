package main

import (
	"context"
	"fmt"
	"sync"
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

// closedHistoryBlockingStore parks every closed-status List (the retention
// prune's history read) until release is closed.
type closedHistoryBlockingStore struct {
	beads.Store
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
}

func newClosedHistoryBlockingStore(inner beads.Store) *closedHistoryBlockingStore {
	return &closedHistoryBlockingStore{Store: inner, entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *closedHistoryBlockingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "closed" {
		s.enteredOnce.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.Store.List(q)
}

// storeWatchdogTestRuntime is ordersLaneTestRuntime over store, with every
// watchdog due and the order rescan, index migration and managed-Dolt
// preflight out of the way. Its empty city path skips the retention
// watchdog's backup-age check, as on every directly-built test runtime.
func storeWatchdogTestRuntime(t *testing.T, store beads.Store, od orderDispatcher, patrol string) *CityRuntime {
	t.Helper()
	cr := ordersLaneTestRuntime(t, od, patrol, nil)
	cr.cityPath = ""
	cr.standaloneCityStore = store
	cr.wispIndexMigrationApplied = true
	cr.orderRescanLast = time.Now()
	cr.managedDoltOwned = func(string) (bool, error) { return false, nil }
	return cr
}

// A retention prune blocked in its closed-history read holds nothing else:
// the startup dispatch, an orders-lane pass and the tracking and nudge/mail
// lanes each finish their work while it stays parked.
func TestStoreWatchdogLanesRunWhileRetentionIsBlocked(t *testing.T) {
	now := time.Now()
	seed := append(storeWatchdogSeed(now), mailSeed("mail-stale", now.Add(-nudgeMailSweepDefaultMailTTL-time.Minute)))
	store := newClosedHistoryBlockingStore(beads.NewMemStoreFrom(100, seed, nil))
	od := &recordingOrderDispatcher{}
	cr := storeWatchdogTestRuntime(t, store, od, "1h")
	ctx := context.Background()

	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)
		cr.runStoreWatchdogLanePass(ctx, cr.storeWatchdogLaneOf(storeWatchdogOrderTrackingRetention))
	}()
	t.Cleanup(func() {
		close(store.release)
		awaitClose(t, retentionDone, "the retention pass finishing after release")
	})
	awaitClose(t, store.entered, "the retention watchdog reaching its closed-history read")

	othersDone := make(chan struct{})
	go func() {
		defer close(othersDone)
		cr.dispatchOrders(ctx, cr.cityPath)
		cr.runOrdersLanePass(ctx, cr.cityPath, ordersLaneReasonCadence)
		cr.runStoreWatchdogLanePass(ctx, cr.storeWatchdogLaneOf(storeWatchdogOrderTrackingSweep))
		cr.runStoreWatchdogLanePass(ctx, cr.storeWatchdogLaneOf(storeWatchdogNudgeMailSweep))
	}()
	awaitClose(t, othersDone, "dispatch and the tracking and nudge/mail lanes while retention is blocked")

	select {
	case <-retentionDone:
		t.Fatal("the retention pass finished before release: the test did not hold it")
	default:
	}
	if got := od.calls.Load(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2 (startup dispatch and one lane pass)", got)
	}
	for id, want := range map[string]string{"stale-open": "closed", "mail-stale": "closed"} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != want {
			t.Fatalf("%s status = %q, want %q while retention is blocked", id, got.Status, want)
		}
	}
}
