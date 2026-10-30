package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// Order dispatch pours each recipe into the store its class demands
// (ga-fk1a5), and the order gate reads the order-run evidence wherever that
// put it (R17).
//
// A v1 POURED order formula compiles to a molecule container plus plain
// assigned task steps, all work class. The dispatcher used to put every order
// molecule in the graph binding, where `gc hook`'s work-scope query never sees
// the assigned steps. Routed to the work store, the root also carries the
// order's run evidence (the order-run label, and the order:/seq: cursor of an
// event order), so the gate must read that store too or the cooldown, the
// event cursor and the single-flight gate all regress.
//
// Both topologies; the split leg's binding is a real SQLite store.

func newWorkClassOrderDispatcher(t *testing.T, e splitEnv, a orders.Order, ep events.Provider) (*memoryOrderDispatcher, *memRecorder) {
	t.Helper()
	rec := &memRecorder{}
	dispatchCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, cfg, _ := newPouredOrderFixture(t)
	return &memoryOrderDispatcher{
		aa:                   []orders.Order{a},
		storeFn:              func(execStoreTarget) (beads.Store, error) { return e.work, nil },
		storageRoutes:        e.routes,
		cfg:                  cfg,
		cityName:             "test-city",
		cityPath:             e.cityPath,
		rec:                  rec,
		ep:                   ep,
		stderr:               io.Discard,
		maxDispatchesPerTick: 1,
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       cancel,
	}, rec
}

func pouredOrderFor(t *testing.T) orders.Order {
	t.Helper()
	_, _, a := newPouredOrderFixture(t)
	return a
}

// moleculeRoots returns the molecule roots in store carrying the order's
// order-run label.
func orderMoleculeRoots(t *testing.T, store beads.Store, scoped string) []beads.Bead {
	t.Helper()
	var out []beads.Bead
	for _, b := range allBeads(t, store) {
		if beads.IsMoleculeType(b.Type) && slicesContain(b.Labels, orders.RunLabel(scoped)) {
			out = append(out, b)
		}
	}
	return out
}

func TestOrderDispatchPouredV1MoleculeLandsInTheWorkStore(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		a := pouredOrderFor(t)
		m, rec := newWorkClassOrderDispatcher(t, e, a, nil)
		dispatchGraphOrder(t, m, e.cityPath)
		if rec.hasType(events.OrderFailed) || !rec.hasType(events.OrderCompleted) {
			t.Fatalf("events = %+v, want completed without failure", rec.events)
		}
		assigned := false
		for _, b := range allBeads(t, e.work) {
			if b.Assignee == "worker" {
				assigned = true
			}
		}
		if !assigned {
			t.Fatalf("no step assigned to `worker` in the work store, where `gc hook` reads it")
		}
		if roots := orderMoleculeRoots(t, e.work, a.ScopedName()); len(roots) != 1 {
			t.Fatalf("order molecule roots in the work store = %d, want 1", len(roots))
		}
		if !e.split {
			return
		}
		for _, b := range allBeads(t, e.class) {
			if coordclass.Classify(b) == coordclass.ClassWork {
				t.Errorf("work-class bead %s (type=%s) in the binding", b.ID, b.Type)
			}
		}
	})
}

// The single-flight gate sees a still-open work-resident root: with the
// tracking bead gone and the cooldown cache cleared, the open root alone holds
// the gate shut.
func TestOrderGateSeesAnOpenWorkResidentPouredRoot(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		a := pouredOrderFor(t)
		a.Interval = "1ms"
		m, _ := newWorkClassOrderDispatcher(t, e, a, nil)
		dispatchGraphOrder(t, m, e.cityPath)
		owner, _ := e.owner()
		dropOrderTracking(t, owner)
		m.cacheMu.Lock()
		m.lastRunCache = nil
		m.cacheMu.Unlock()

		m.dispatch(context.Background(), e.cityPath, time.Now().Add(time.Minute))
		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if !m.drain(drainCtx) {
			t.Fatal("second dispatch did not drain")
		}
		if roots := orderMoleculeRoots(t, e.work, a.ScopedName()); len(roots) != 1 {
			t.Fatalf("order molecule roots = %d, want 1: the gate re-fired while the first run was open", len(roots))
		}
	})
}

// The cooldown clock reads a work-resident root's CreatedAt: with the tracking
// bead gone (retention) and the run finished, the order still waits out its
// interval.
func TestOrderCooldownSeesAWorkResidentPouredRoot(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		a := pouredOrderFor(t) // 15m cooldown
		m, _ := newWorkClassOrderDispatcher(t, e, a, nil)
		dispatchGraphOrder(t, m, e.cityPath)
		closeAll(t, e.work)
		owner, _ := e.owner()
		dropOrderTracking(t, owner)
		m.cacheMu.Lock()
		m.lastRunCache = nil
		m.cacheMu.Unlock()

		m.dispatch(context.Background(), e.cityPath, time.Now().Add(time.Minute))
		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if !m.drain(drainCtx) {
			t.Fatal("second dispatch did not drain")
		}
		if roots := orderMoleculeRoots(t, e.work, a.ScopedName()); len(roots) != 1 {
			t.Fatalf("order molecule roots = %d, want 1: the cooldown did not see the first run", len(roots))
		}
	})
}

// An event order's cursor rides the root it poured. With the run finished and
// no new events, the order must not re-fire on the events it already consumed.
func TestOrderEventCursorSeesAWorkResidentPouredRoot(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		a := pouredOrderFor(t)
		a.Trigger = "event"
		a.Interval = ""
		a.On = events.BeadClosed
		ep := events.NewFake()
		ep.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
		m, _ := newWorkClassOrderDispatcher(t, e, a, ep)
		dispatchGraphOrder(t, m, e.cityPath)
		if roots := orderMoleculeRoots(t, e.work, a.ScopedName()); len(roots) != 1 {
			t.Fatalf("first dispatch roots = %d, want 1", len(roots))
		}
		closeAll(t, e.work)
		m.cacheMu.Lock()
		m.lastRunCache = nil
		m.cacheMu.Unlock()

		m.dispatch(context.Background(), e.cityPath, time.Now().Add(time.Minute))
		drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if !m.drain(drainCtx) {
			t.Fatal("second dispatch did not drain")
		}
		if roots := orderMoleculeRoots(t, e.work, a.ScopedName()); len(roots) != 1 {
			t.Fatalf("order molecule roots = %d, want 1: the event cursor regressed and the order replayed a consumed event", len(roots))
		}
	})
}

func dropOrderTracking(t *testing.T, store beads.Store) {
	t.Helper()
	for _, b := range allBeads(t, store) {
		if orders.IsTrackingBead(b) {
			if err := store.Delete(b.ID); err != nil {
				t.Fatalf("deleting tracking bead %s: %v", b.ID, err)
			}
		}
	}
}

func closeAll(t *testing.T, store beads.Store) {
	t.Helper()
	for _, b := range allBeads(t, store) {
		if b.Status == "closed" {
			continue
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("closing %s: %v", b.ID, err)
		}
	}
}

// Review 8: a single-store city never compiles a formula to build its gate,
// and a compile failure keeps the work leg without memoizing a "no".
func TestOrderGatePoursWorkIsLazyAndFailsSafe(t *testing.T) {
	t.Run("single-store-never-asks", func(t *testing.T) {
		e := newSplitEnv(t, false)
		m, _ := newWorkClassOrderDispatcher(t, e, pouredOrderFor(t), nil)
		m.gateStoresFor(e.cityPath, e.work, "city\x00"+e.cityPath, nil, func() bool {
			t.Fatal("poursWork was asked on a single-store city")
			return false
		})
	})
	t.Run("compile-failure-keeps-the-work-leg", func(t *testing.T) {
		e := newSplitEnv(t, true)
		a := pouredOrderFor(t)
		a.Formula = "does-not-exist"
		m, _ := newWorkClassOrderDispatcher(t, e, a, nil)
		if !m.orderPoursWork(context.Background(), a) {
			t.Fatal("orderPoursWork on an uncompilable formula = false, want true (keep the work leg)")
		}
		m.cacheMu.Lock()
		_, memoized := m.poursWork[a.ScopedName()]
		m.cacheMu.Unlock()
		if memoized {
			t.Fatal("a compile failure was memoized")
		}
		stores, _ := m.gateStoresFor(e.cityPath, e.work, "city\x00"+e.cityPath, nil, func() bool { return m.orderPoursWork(context.Background(), a) })
		if !storeListContains(stores, e.work) {
			t.Fatal("gate legs omit the work store for an order whose formula did not compile")
		}
		m.cacheMu.Lock()
		until, remembered := m.poursWorkFailedUntil[a.ScopedName()]
		m.cacheMu.Unlock()
		if !remembered || !until.After(time.Now()) {
			t.Fatal("a compile failure is not remembered for a while: a broken formula would recompile every tick")
		}
	})
}
