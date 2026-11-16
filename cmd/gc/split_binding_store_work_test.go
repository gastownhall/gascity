package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// bindingStoreWorkGate holds one actual SQLite binding read. Store embedding
// preserves the read boundary while leaving all data and errors with the engine.
type bindingStoreWorkGate struct {
	beads.Store
	lists   atomic.Int64
	gets    atomic.Int64
	armed   atomic.Bool
	fail    atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	open    sync.Once
}

func (g *bindingStoreWorkGate) unblock() { g.open.Do(func() { close(g.release) }) }

func (g *bindingStoreWorkGate) Get(id string) (beads.Bead, error) {
	g.gets.Add(1)
	return g.Store.Get(id)
}

func (g *bindingStoreWorkGate) List(q beads.ListQuery) ([]beads.Bead, error) {
	g.lists.Add(1)
	if g.armed.Load() {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	if g.fail.Load() {
		return nil, errors.New("binding read unavailable")
	}
	return g.Store.List(q)
}

func TestSplitBindingCacheCountsReadsThroughItsLateBoundOwner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		populated bool
		fail      bool
	}{
		{name: "empty"},
		{name: "populated", populated: true},
		{name: "failed_read", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate := &bindingStoreWorkGate{
				Store: newSplitEnvClassLeaf(t), entered: make(chan struct{}), release: make(chan struct{}),
			}
			cfg := &config.City{Workspace: config.Workspace{Name: "counted-city", Prefix: "gc"}}
			work := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)
			stubControllerCityStore(t, work)
			// A non-cancelable context keeps priming synchronous, with no loop.
			// withControllerCache's PrimeActive reads before owner.Store(cs);
			// reaching the end of construction proves that nil-owner read is safe.
			// Its counted/uncounted policy is deliberately not asserted here.
			cs := newControllerStateWithRoutes(context.Background(), splitEnvRoutes(gate), cfg, runtime.NewFake(), nil, "counted-city", t.TempDir())
			if gate.lists.Load() == 0 {
				t.Fatal("constructor did not exercise the pre-owner binding read")
			}
			cache, ok := cs.Stores().Graph().Store.(*beads.CachingStore)
			if !ok || cache == nil {
				t.Fatal("controller did not expose its binding CachingStore")
			}
			if cs.Stores().Graph().Store == cs.CityBeadStore() || cache.Backing() != gate {
				t.Fatal("fixture: graph cache is not over the distinct split binding")
			}
			if cs.storeWorkInFlight() {
				t.Fatal("constructor retained store work after its reads finished")
			}
			var seeded beads.Bead
			if tc.populated {
				var err error
				seeded, err = gate.Create(beads.Bead{Title: "out-of-band binding row", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
			}
			gate.armed.Store(true)
			gate.fail.Store(tc.fail)
			cache.RequestReconcile()
			done := make(chan struct{})
			go func() {
				defer close(done)
				cache.ReconcileIfDueForTest()
			}()
			t.Cleanup(func() {
				gate.unblock()
				awaitClose(t, done, "split binding reconcile cleanup")
				cache.StopReconciler()
			})
			awaitClose(t, gate.entered, "post-construction binding read")
			if !cs.storeWorkInFlight() {
				t.Error("controller reports no store work while its binding cache is reading")
			}
			gate.unblock()
			awaitClose(t, done, "post-construction binding read completion")
			if cs.storeWorkInFlight() {
				t.Error("controller retained store work after its binding read finished")
			}
			if tc.fail {
				if problem := cache.Stats().LastProblem; !strings.Contains(problem, "binding read unavailable") {
					t.Errorf("reconcile problem = %q, want the failed binding read", problem)
				}
				return
			}
			if tc.populated {
				before := gate.gets.Load() + gate.lists.Load()
				if row, err := cache.Get(seeded.ID); err != nil || row.ID != seeded.ID {
					t.Errorf("reconcile did not cache the binding row: %v, %v", row, err)
				}
				if gate.gets.Load()+gate.lists.Load() != before {
					t.Error("reading the reconciled row unexpectedly reached the binding")
				}
			}
			cache.RequestReconcile()
			cache.ReconcileIfDueForTest()
			if cs.storeWorkInFlight() {
				t.Error("repeated binding reconcile retained store work")
			}
		})
	}
}
