package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
)

// hangingByIDStore is a work leg whose reads by id park until release closes:
// a ledger that stops answering mid-judgment. Embedding the Store interface
// hides the leaf's optional capabilities, so it strands nothing and every read
// by id goes through Get.
type hangingByIDStore struct {
	beads.Store
	release chan struct{}
}

func (s *hangingByIDStore) Get(id string) (beads.Bead, error) {
	<-s.release
	return s.Store.Get(id)
}

// TestLegacyCrossStoreGateGivesUpAtItsDeadline (#8360 review): the legacy
// demand pass judged a split city's snapshots with no deadline, so one ledger
// that stopped answering held the pass, and every pass after it. The gate now
// gives up at crossStoreGateDeadline and keeps the snapshot it was handed,
// reported partial: the pass retains and blocks creates instead of waiting, and
// never counts less demand than the store served.
func TestLegacyCrossStoreGateGivesUpAtItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mem := beads.NewMemStore()
		row, err := mem.Create(beads.Bead{Title: "held by a graph step", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.DepAdd(row.ID, "external:gcg-1", "blocks"); err != nil {
			t.Fatal(err)
		}
		work := &hangingByIDStore{Store: mem, release: make(chan struct{})}
		legs := []readywork.Leg{
			{Label: "city", Store: work},
			{Label: "class:x", Store: beads.NewMemStore(), ClassBinding: true, ReservedPrefixes: []string{"gcg"}},
		}
		cache := newReadyDemandCache().withGateLegs(legs)

		start := time.Now()
		got, err := cache.gateSnapshot(work, []beads.Bead{row}, nil)
		if waited := time.Since(start); waited != crossStoreGateDeadline {
			t.Errorf("the gate returned after %s, want its deadline %s", waited, crossStoreGateDeadline)
		}
		if !beads.IsPartialResult(err) || !errors.Is(err, errCrossStoreGateDeadline) {
			t.Errorf("gate error = %v, want a partial result naming the deadline", err)
		}
		if len(got) != 1 || got[0].ID != row.ID {
			t.Errorf("gated rows = %v, want the snapshot as handed: demand is never under-counted from a stalled read", beadIDsOf(got))
		}
		close(work.release)
	})
}

// TestV2DemandServesAGateLegsReadyOnlyFromAGatedRecording (#8360 review): the
// v2 pass does no remote I/O, so on a split city it serves every gate leg's
// Ready from the external-reads lane's gated recording, the exact leg's too,
// whose rows it otherwise reads from its cache. A recording the lane did not
// gate is partial, not served. A city with no gate legs reads as before.
func TestV2DemandServesAGateLegsReadyOnlyFromAGatedRecording(t *testing.T) {
	ledger, ledgerBacking := newDemandCache(t, false, routedDemandBead("gc-r1"))
	binding, bindingBacking := newDemandCache(t, true, routedDemandBead("gcg-r2"))
	legs := []readywork.Leg{
		{Label: "city", Store: ledger},
		{Label: "class:x", Store: binding, ClassBinding: true, ReservedPrefixes: []string{"gcg"}},
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	recording := func(gated bool) *externalReadsRecording {
		leg := func(id string) sourceResult {
			return sourceResult{EndedAt: now, sourcePayload: sourcePayload{Leg: legRecording{ReadyAll: []beads.Bead{routedDemandBead(id)}, ReadyGated: gated}}}
		}
		return &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
			keyOf(sourceDemand, ledger):  leg("gc-r1"),
			keyOf(sourceDemand, binding): leg("gcg-r2-gated"),
		}}
	}
	ledgerBacking.armed.Store(true)
	bindingBacking.armed.Store(true)

	split := func(rec *externalReadsRecording) *v2DemandReads {
		reads := newV2DemandReads(now, rec)
		reads.gateLegs = legs
		return reads
	}
	gated := split(recording(true))
	if rows, err := gated.ReadyAll(ledger); err != nil || !slices.Equal(beadIDsOf(rows), []string{"gc-r1"}) {
		t.Errorf("gated ledger Ready = %v, %v; want the recorded gc-r1", beadIDsOf(rows), err)
	}
	if rows, err := gated.ReadyAll(binding); err != nil || !slices.Equal(beadIDsOf(rows), []string{"gcg-r2-gated"}) {
		t.Errorf("gated exact binding Ready = %v, %v; want the recorded gcg-r2-gated, not the binding's cache", beadIDsOf(rows), err)
	}
	ungated := split(recording(false))
	for _, store := range []beads.Store{ledger, binding} {
		if rows, err := ungated.ReadyAll(store); !beads.IsPartialResult(err) || !errors.Is(err, errDemandRecordingUngated) || len(rows) != 0 {
			t.Errorf("ungated recording served %v, %v; want a partial result with no rows", beadIDsOf(rows), err)
		}
	}
	if ops := append(ledgerBacking.readLog(), bindingBacking.readLog()...); len(ops) > 0 {
		t.Errorf("a split city's v2 Ready read a leg's backing: %q", ops)
	}

	// Control: with no gate legs the exact leg is read from its cache.
	if rows, err := newV2DemandReads(now, recording(true)).ReadyAll(binding); err != nil || !slices.Equal(beadIDsOf(rows), []string{"gcg-r2"}) {
		t.Errorf("unsplit exact binding Ready = %v, %v; want gcg-r2 from its cache", beadIDsOf(rows), err)
	}
}

// TestSplitCityV2DemandIsGatedOffThePass is the v2 half of
// TestSplitCityDemandIsGatedWhenHandedTheSessionsBinding (#8360 review): once
// the cross-store gate armed inside the v2 gather, the pass read the ledger
// live (per-row DepList, the blockers by id) with no deadline, against CONTRACT
// v5 R4. The external-reads lane now gates every leg's ready snapshot off the
// pass, the exact SQLite binding's included, and the gather counts the same
// demand from the recording without one read of the ledger's backing.
func TestSplitCityV2DemandIsGatedOffThePass(t *testing.T) {
	cityPath := t.TempDir()
	maxSessions, minSessions := 5, 0
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc", Prefix: "gc"},
		Storage:   splitEnvStorageConfig(),
		Agents:    []config.Agent{{Name: "dog", StartCommand: "true", MinActiveSessions: &minSessions, MaxActiveSessions: &maxSessions}},
	}
	workEngine := splittest.NewNativeWorkStore(t, "gc")
	ledgerBacking := &demandBacking{Store: workEngine}
	ledger := beads.NewCachingStoreForTest(ledgerBacking, nil)
	work := wrapStoreWithBeadPolicies(ledger, cfg)
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("no reserved graph prefix")
	}
	leaf, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix(prefix), beads.WithSQLiteStoreReservedIDPrefixes(splitEnvClassNamespaces(t)...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leaf.(interface{ CloseStore() error }).CloseStore() })
	binding := beads.NewCachingStoreForTest(leaf, nil)
	if _, exact := demandLegCache(binding); !exact {
		t.Fatal("fixture: the controller's binding cache over SQLite is not an exact leg")
	}
	routes := splitEnvRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	routed := map[string]string{"gc.routed_to": "dog"}
	create := func(store beads.Store, title string, meta map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Title: title, Type: "task", Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	step := create(binding, "graph blocker", nil)
	held := create(workEngine, "held by an open graph step", routed)
	if err := workEngine.DepAdd(held.ID, "external:"+step.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	clean := create(workEngine, "clean", routed)
	workBlocker := create(workEngine, "work blocker", nil)
	stranded := create(binding, "graph row behind a work blocker", routed)
	if err := binding.DepAdd(stranded.ID, workBlocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	for _, cache := range []*beads.CachingStore{ledger, binding} {
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	lane, _ := newTestBackstopLane(externalReadsEnv{CityName: "gc", CityPath: cityPath, Cfg: cfg, CityStore: binding})
	lane.steps = nil
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	demand := func(phase string) []string {
		t.Helper()
		now = now.Add(time.Minute)
		passAndJoin(context.Background(), at(lane, now))
		rec := lane.recording()
		for _, store := range []beads.Store{work, binding} {
			if leg, ok := recordedLeg(rec, store); !ok || !leg.ReadyGated {
				t.Fatalf("%s: the lane's recording of %T (recorded %t) is not gated", phase, store, ok)
			}
		}
		before := len(ledgerBacking.readLog())
		ledgerBacking.armed.Store(true)
		view, err := gatherDemand(demandGatherEnv{CityName: "gc", CityPath: cityPath, Cfg: cfg, CityStore: binding}, newV2DemandReads(now, rec))
		ledgerBacking.armed.Store(false)
		if err != nil {
			t.Fatalf("%s: gatherDemand: %v", phase, err)
		}
		if ops := ledgerBacking.readLog()[before:]; len(ops) > 0 {
			t.Errorf("%s: the v2 gather read the ledger's backing: %q", phase, ops)
		}
		ids := slices.Clone(view.Collected.DefaultDemand["dog"].WorkBeadIDs)
		slices.Sort(ids)
		return ids
	}
	want := func(ids ...string) []string {
		slices.Sort(ids)
		return ids
	}

	if got := demand("blockers open"); !slices.Equal(got, want(clean.ID)) {
		t.Fatalf("blockers open: v2 demand counts %v, want only %s: %s waits on open graph step %s and %s on open work bead %s", got, clean.ID, held.ID, step.ID, stranded.ID, workBlocker.ID)
	}
	if err := workEngine.Close(workBlocker.ID); err != nil {
		t.Fatal(err)
	}
	if got := demand("work blocker closed"); !slices.Equal(got, want(clean.ID, stranded.ID)) {
		t.Fatalf("work blocker closed: v2 demand counts %v, want %s and the released binding row %s", got, clean.ID, stranded.ID)
	}
	if err := binding.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	if got := demand("graph blocker closed"); !slices.Equal(got, want(clean.ID, stranded.ID, held.ID)) {
		t.Fatalf("graph blocker closed: v2 demand counts %v, want %s, %s and %s", got, clean.ID, stranded.ID, held.ID)
	}
}
