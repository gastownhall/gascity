package storebindingtest

// Bare Orders class conformance, over the closed storebinding.OrdersStore
// contract only. Nothing here reaches for a raw store, a ListQuery flag, or a
// physical row: an order run is created, tracked, ordered by recency, and
// closed, and that is the whole observable contract a dispatcher depends on.

import (
	"errors"
	"runtime"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// OrdersSuite configures one bare Orders class conformance run.
type OrdersSuite struct {
	// NewStore returns a fresh, empty Orders front door per assertion.
	NewStore func(TB) storebinding.OrdersStore
	// Capability is what the provider declares for the Orders class.
	Capability storebinding.ClassCapability
}

// RunOrdersStoreTests runs the bare Orders class conformance suite.
func RunOrdersStoreTests(r Runner, suite OrdersSuite) {
	r.Helper()
	if suite.NewStore == nil {
		r.Fatalf("storebindingtest: OrdersSuite.NewStore is required")
	}
	const scoped = "alpha/patrol"

	assertClassDeclaredAvailable(r, "Orders", suite.Capability)

	r.Run("CreateRunIsOpenAndReadable", func(r Runner) {
		store := suite.NewStore(r)
		run := mustCreateRun(r, store, scoped)
		if run.ID == "" {
			r.Fatalf("CreateRun returned an empty ID")
		}
		if !run.Open {
			r.Fatalf("a freshly created run is not open; the single-flight marker never engages")
		}
		if run.Scoped != scoped {
			r.Errorf("Scoped = %q, want %q", run.Scoped, scoped)
		}
		got, err := store.Get(run.ID)
		if err != nil {
			r.Fatalf("Get(%q): %v", run.ID, err)
		}
		if got.ID != run.ID || got.Scoped != scoped {
			r.Errorf("Get = %+v, want the created run %+v", got, run)
		}
	})

	r.Run("GetUnknownRunIsNotFound", func(r Runner) {
		store := suite.NewStore(r)
		if _, err := store.Get("gco-does-not-exist"); !errors.Is(err, beads.ErrNotFound) {
			r.Fatalf("Get(unknown) = %v, want a beads.ErrNotFound chain", err)
		}
	})

	r.Run("SetOutcomeIsReadBack", func(r Runner) {
		store := suite.NewStore(r)
		run := mustCreateRun(r, store, scoped)
		if err := store.SetOutcome(run.ID, orders.RunOutcomeExecFailed); err != nil {
			r.Fatalf("SetOutcome: %v", err)
		}
		got, err := store.Get(run.ID)
		if err != nil {
			r.Fatalf("Get after SetOutcome: %v", err)
		}
		if got.Outcome != orders.RunOutcomeExecFailed {
			r.Errorf("Outcome = %v, want %v", got.Outcome, orders.RunOutcomeExecFailed)
		}
	})

	r.Run("CloseRunEndsTheSingleFlight", func(r Runner) {
		store := suite.NewStore(r)
		run := mustCreateRun(r, store, scoped)
		open, found, err := store.LatestOpenRun(scoped)
		if err != nil {
			r.Fatalf("LatestOpenRun: %v", err)
		}
		if !found || open.ID != run.ID {
			r.Fatalf("LatestOpenRun = (%+v, %t), want the open run %q", open, found, run.ID)
		}
		if err := store.CloseRun(run.ID, "conformance"); err != nil {
			r.Fatalf("CloseRun: %v", err)
		}
		if got, err := store.Get(run.ID); err != nil {
			r.Fatalf("Get after CloseRun: %v", err)
		} else if got.Open {
			r.Errorf("the run still reports open after CloseRun")
		}
		if _, found, err := store.LatestOpenRun(scoped); err != nil {
			r.Fatalf("LatestOpenRun after close: %v", err)
		} else if found {
			r.Errorf("a closed run still answers LatestOpenRun; the single flight never clears")
		}
	})

	r.Run("RecentRunsAreNewestFirst", func(r Runner) {
		store := suite.NewStore(r)
		first := mustCreateRun(r, store, scoped)
		awaitDistinctCreatedAt(r, first.CreatedAt)
		second := mustCreateRun(r, store, scoped)
		awaitDistinctCreatedAt(r, second.CreatedAt)
		third := mustCreateRun(r, store, scoped)

		recent, err := store.RecentRuns(scoped, 3)
		if err != nil {
			r.Fatalf("RecentRuns: %v", err)
		}
		if len(recent) != 3 {
			r.Fatalf("RecentRuns returned %d runs, want 3", len(recent))
		}
		want := []string{third.ID, second.ID, first.ID}
		for index, id := range want {
			if recent[index].ID != id {
				r.Fatalf("RecentRuns = %v, want newest-first %v", runIDs(recent), want)
			}
		}
		bounded, err := store.RecentRuns(scoped, 1)
		if err != nil {
			r.Fatalf("RecentRuns(limit 1): %v", err)
		}
		if len(bounded) != 1 || bounded[0].ID != third.ID {
			r.Fatalf("RecentRuns(limit 1) = %v, want only the newest %q", runIDs(bounded), third.ID)
		}
	})

	r.Run("OpenRunsReportsOnlyOpenWork", func(r Runner) {
		store := suite.NewStore(r)
		open := mustCreateRun(r, store, scoped)
		closed := mustCreateRun(r, store, "alpha/other")
		if err := store.CloseRun(closed.ID, "conformance"); err != nil {
			r.Fatalf("CloseRun: %v", err)
		}
		running, err := store.OpenRuns()
		if err != nil {
			r.Fatalf("OpenRuns: %v", err)
		}
		ids := runIDs(running)
		if !containsID(ids, open.ID) {
			r.Errorf("OpenRuns = %v, want it to contain the open run %q", ids, open.ID)
		}
		if containsID(ids, closed.ID) {
			r.Errorf("OpenRuns = %v, want it to exclude the closed run %q", ids, closed.ID)
		}
	})

	r.Run("CursorRoundTrips", func(r Runner) {
		store := suite.NewStore(r)
		run := mustCreateRun(r, store, scoped)
		if err := store.SetCursor(run.ID, scoped, orders.EventCursor(42)); err != nil {
			r.Fatalf("SetCursor: %v", err)
		}
		if got := store.Cursor(scoped); got != orders.EventCursor(42) {
			r.Errorf("Cursor = %d, want 42", got)
		}
	})

	r.Run("DeleteRunRemovesIt", func(r Runner) {
		store := suite.NewStore(r)
		run := mustCreateRun(r, store, scoped)
		if err := store.DeleteRun(run.ID); err != nil {
			r.Fatalf("DeleteRun: %v", err)
		}
		if _, err := store.Get(run.ID); !errors.Is(err, beads.ErrNotFound) {
			r.Fatalf("Get after DeleteRun = %v, want a beads.ErrNotFound chain", err)
		}
	})
}

func mustCreateRun(r Runner, store storebinding.OrdersStore, scoped string) orders.OrderRun {
	r.Helper()
	run, err := store.CreateRun(scoped, orders.RunOpts{})
	if err != nil {
		r.Fatalf("CreateRun(%q): %v", scoped, err)
	}
	return run
}

func runIDs(list []orders.OrderRun) []string {
	ids := make([]string, 0, len(list))
	for _, run := range list {
		ids = append(ids, run.ID)
	}
	return ids
}

// awaitDistinctCreatedAt blocks until the wall clock has passed prior, so the
// next created row necessarily lands on a strictly later timestamp. Recency
// order is by creation time, so a tie would make the expected order genuinely
// ambiguous rather than merely unlucky. It is a condition wait rather than a
// fixed sleep: it returns on the first tick past prior instead of betting a
// constant against an unknown clock granularity.
func awaitDistinctCreatedAt(r Runner, prior time.Time) {
	r.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !time.Now().After(prior) {
		if time.Now().After(deadline) {
			r.Fatalf("the wall clock did not advance past %s", prior)
		}
		runtime.Gosched()
	}
}
