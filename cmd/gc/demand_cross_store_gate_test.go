package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
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
// v2 pass does no remote I/O, so on a split city it serves a gate leg's Ready
// from the external-reads lane's gated recording, including an exact leg's
// whose store strands nothing, so that no row of it is left to the leg's own
// verdict (the rows a stranding binding's cache judges in the pass are
// TestV2GateLegReadyAgreesWithTheFederatedGate's). A recording the lane did
// not gate is partial, not served. A city with no gate legs reads as before.
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

// gateLegCity is a split city's ready legs over real stores: the class
// binding's SQLite engine under the controller's exact cache, and a work
// ledger that strands an absent target, as the in-memory store does.
type gateLegCity struct {
	t          *testing.T
	engine     *readLoggingSQLite
	binding    *beads.CachingStore
	work       *beads.MemStore
	legs       []readywork.Leg
	namespaces []string
	now        time.Time
}

// readLoggingSQLite is the binding's engine, counting the reads the cache
// sends it while armed. Embedding keeps every capability the cache asserts
// for: exact reads, the ready projection, the absent-blocker listing.
type readLoggingSQLite struct {
	*beads.SQLiteStore
	armed atomic.Bool
	reads atomic.Int64
}

func (e *readLoggingSQLite) note() {
	if e.armed.Load() {
		e.reads.Add(1)
	}
}

func (e *readLoggingSQLite) Get(id string) (beads.Bead, error) {
	e.note()
	return e.SQLiteStore.Get(id)
}

func (e *readLoggingSQLite) List(q beads.ListQuery) ([]beads.Bead, error) {
	e.note()
	return e.SQLiteStore.List(q)
}

func (e *readLoggingSQLite) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	e.note()
	return e.SQLiteStore.Ready(q...)
}

func (e *readLoggingSQLite) DepList(id, direction string) ([]beads.Dep, error) {
	e.note()
	return e.SQLiteStore.DepList(id, direction)
}

func (e *readLoggingSQLite) ReadyBlockedByAbsent(q beads.ReadyQuery) ([]beads.Bead, error) {
	e.note()
	return e.SQLiteStore.ReadyBlockedByAbsent(q)
}

func newGateLegCity(t *testing.T) *gateLegCity {
	t.Helper()
	namespaces := splitEnvClassNamespaces(t)
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("no reserved graph prefix")
	}
	opened, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix(prefix), beads.WithSQLiteStoreReservedIDPrefixes(namespaces...))
	if err != nil {
		t.Fatal(err)
	}
	engine := &readLoggingSQLite{SQLiteStore: opened.(*beads.SQLiteStore)}
	t.Cleanup(func() { _ = engine.CloseStore() })
	binding := beads.NewCachingStoreForTest(engine, nil)
	work := beads.NewMemStore()
	return &gateLegCity{
		t: t, engine: engine, binding: binding, work: work, namespaces: namespaces,
		legs: []readywork.Leg{
			{Label: "city", Store: work},
			{Label: "class:infra", Store: binding, ClassBinding: true, ReservedPrefixes: namespaces},
		},
		now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	}
}

func (c *gateLegCity) prime() {
	c.t.Helper()
	if err := c.binding.Prime(context.Background()); err != nil {
		c.t.Fatal(err)
	}
	if _, exact := demandLegCache(c.binding); !exact {
		c.t.Fatal("fixture: the binding cache over SQLite is not an exact leg")
	}
}

// gated is what the federated gate serves from the binding now: the
// binding's own live Ready, judged across the legs by readywork.LegGate, the
// rule `gc ready` and the external-reads lane apply.
func (c *gateLegCity) gated() []beads.Bead {
	c.t.Helper()
	q := beads.ReadyQuery{TierMode: beads.TierBoth}
	own, err := beads.HandlesFor(c.binding).Live.Ready(q)
	if err != nil {
		c.t.Fatal(err)
	}
	rows, err := readywork.NewLegGate(c.legs, q).Gate(context.Background(), 1, own)
	if err != nil {
		c.t.Fatal(err)
	}
	return rows
}

// recording is a fresh external-reads recording whose gated binding Ready is
// rows, as the lane publishes one.
func (c *gateLegCity) recording(rows []beads.Bead) *externalReadsRecording {
	return &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
		keyOf(sourceDemand, c.binding): {EndedAt: c.now, sourcePayload: sourcePayload{Leg: legRecording{ReadyAll: rows, ReadyGated: true}}},
	}}
}

// served is the v2 pass's Ready of the binding under rec, and the reads it
// sent the binding's engine.
func (c *gateLegCity) served(rec *externalReadsRecording) ([]beads.Bead, int64) {
	c.t.Helper()
	reads := newV2DemandReads(c.now, rec)
	reads.gateLegs = c.legs
	before := c.engine.reads.Load()
	c.engine.armed.Store(true)
	rows, err := reads.ReadyAll(c.binding)
	c.engine.armed.Store(false)
	if err != nil {
		c.t.Fatalf("v2 ReadyAll of the binding: %v", err)
	}
	return rows, c.engine.reads.Load() - before
}

// ownVerdict reports whether every blocking edge of the binding row id, as its
// engine holds them, names a plain target in the binding's reserved namespace
// that the binding's cache holds: the rows whose readiness needs no other
// store.
func (c *gateLegCity) ownVerdict(id string) bool {
	c.t.Helper()
	deps, err := c.engine.SQLiteStore.DepList(id, "down")
	if err != nil {
		c.t.Fatal(err)
	}
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) || dep.DependsOnID == id {
			continue
		}
		if strings.HasPrefix(dep.DependsOnID, "external:") || !slices.ContainsFunc(c.namespaces, func(ns string) bool {
			return strings.HasPrefix(dep.DependsOnID, ns+"-")
		}) {
			return false
		}
		if _, err := beads.HandlesFor(c.binding).Cached.Get(dep.DependsOnID); err != nil {
			return false
		}
	}
	return true
}

// closeOutside closes id in the binding's engine, as another process does,
// and applies the bead.closed the bead-event watcher delivers to the binding
// cache for it.
func (c *gateLegCity) closeOutside(id string) {
	c.t.Helper()
	if err := c.engine.Close(id); err != nil {
		c.t.Fatal(err)
	}
	row, err := c.engine.SQLiteStore.Get(id)
	if err != nil {
		c.t.Fatal(err)
	}
	payload, err := beads.EncodeBeadEventPayload(row)
	if err != nil {
		c.t.Fatal(err)
	}
	c.binding.ApplyEventSnapshot(events.BeadClosed, payload)
}

// TestV2GateLegServesAReleasedInNamespaceRowWithoutANewRecording pins the
// split-city react latency (ga-ylpbsy.67). A graph step blocked by another
// graph row in the binding is released when that row closes. Its readiness
// needs no other store, yet the v2 pass served the binding's Ready only from
// the external-reads lane's recording, refreshed every ~20 s, so the step
// waited for the lane before it got a seat (react p50 5.9 s before the gate
// leg, ~17 s after). Kills: an exact gate leg whose own-verdict rows still
// wait for a new recording, and an in-pass read that reaches the engine.
func TestV2GateLegServesAReleasedInNamespaceRowWithoutANewRecording(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(c *gateLegCity, id string) error
	}{
		{"closed through the cache", func(c *gateLegCity, id string) error { return c.binding.Close(id) }},
		{"closed by another process", func(c *gateLegCity, id string) error { c.closeOutside(id); return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGateLegCity(t)
			create := func(title string) beads.Bead {
				t.Helper()
				b, err := c.engine.Create(beads.Bead{Title: title, Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			// A graph step as a formula lays it down: blocked by the row
			// before it, and tracking its workflow root.
			root := create("workflow root")
			blocker := create("graph blocker")
			step := create("graph step")
			if err := c.engine.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if err := c.engine.DepAdd(step.ID, root.ID, "tracks"); err != nil {
				t.Fatal(err)
			}
			c.prime()
			rec := c.recording(c.gated())
			if ids := beadIDsOf(rec.Sources[keyOf(sourceDemand, c.binding)].Leg.ReadyAll); slices.Contains(ids, step.ID) {
				t.Fatalf("fixture: the recording already serves %s, whose blocker is open: %v", step.ID, ids)
			}

			if err := tc.close(c, blocker.ID); err != nil {
				t.Fatal(err)
			}
			rows, reads := c.served(rec)
			if !slices.Contains(beadIDsOf(rows), step.ID) {
				t.Errorf("the pass after %s closed serves %v without %s: an in-namespace release waited for the lane's next recording", blocker.ID, beadIDsOf(rows), step.ID)
			}
			if reads != 0 {
				t.Errorf("the v2 pass sent the binding's engine %d reads; it must read the binding only through its cache", reads)
			}
		})
	}
}

// TestV2GateLegWithdrawsARecordedStepWhoseBlockerReopened (ga-ylpbsy.67
// review): a step the lane recorded as released, whose in-namespace blocker
// reopens after the recording, is the cache's to answer, and the cache now
// holds it blocked. Serving the recorded row anyway (keeping the recording's
// rows beside the cache's instead of letting the cache's verdict replace them)
// hands a seat work its blocker holds back until the lane records again. Both
// ways a reopen reaches the binding cache: its own write, and another
// process's write delivered as an event. Kills: a recorded row kept over the
// cache's newer verdict.
func TestV2GateLegWithdrawsARecordedStepWhoseBlockerReopened(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reopen func(c *gateLegCity, id string)
	}{
		{"reopened through the cache", func(c *gateLegCity, id string) {
			if err := c.binding.Reopen(id); err != nil {
				c.t.Fatal(err)
			}
		}},
		{"reopened by another process", func(c *gateLegCity, id string) {
			if err := c.engine.Reopen(id); err != nil {
				c.t.Fatal(err)
			}
			row, err := c.engine.SQLiteStore.Get(id)
			if err != nil {
				c.t.Fatal(err)
			}
			payload, err := beads.EncodeBeadEventPayload(row)
			if err != nil {
				c.t.Fatal(err)
			}
			c.binding.ApplyEventSnapshot(events.BeadUpdated, payload)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGateLegCity(t)
			blocker, err := c.engine.Create(beads.Bead{Title: "graph blocker", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			step, err := c.engine.Create(beads.Bead{Title: "graph step", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.engine.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if err := c.engine.Close(blocker.ID); err != nil {
				t.Fatal(err)
			}
			c.prime()
			rec := c.recording(c.gated())
			if ids := beadIDsOf(rec.Sources[keyOf(sourceDemand, c.binding)].Leg.ReadyAll); !slices.Contains(ids, step.ID) {
				t.Fatalf("fixture: the recording does not serve released %s: %v", step.ID, ids)
			}
			tc.reopen(c, blocker.ID)
			if gated := beadIDsOf(c.gated()); slices.Contains(gated, step.ID) {
				t.Fatalf("fixture: the gate still serves %s after its blocker reopened: %v", step.ID, gated)
			}
			rows, _ := c.served(rec)
			if slices.Contains(beadIDsOf(rows), step.ID) {
				t.Errorf("served %s from the stale recording after its in-namespace blocker %s reopened: %v", step.ID, blocker.ID, beadIDsOf(rows))
			}
		})
	}
}

// TestV2GateLegReadyAgreesWithTheFederatedGate is the readiness oracle for an
// exact gate leg's Ready (ga-ylpbsy.67). Over random blocker graphs whose edges
// mix in-namespace, foreign, external:, capability and absent targets, rows
// created before and after the cache primed (only the latter can lack a
// projected verdict), some rows already closed when the recording is taken,
// and random closes, close-blocked closes and reopens after it:
//
//   - with nothing changed, the pass serves exactly what the federated gate
//     serves;
//   - after the changes, every served row is one the gate serves now, or a
//     recorded row the cache leaves to the recording, served as recorded,
//     exactly as before: one the gate must judge across stores, or one whose
//     lost verdict the cache cannot re-derive until its next re-scan;
//   - every recorded row the cache leaves to the recording is still served;
//   - every row whose readiness needs no other store and that the gate
//     serves now, and every such row the cache's own Ready serves, is
//     served, wherever the cache vouches for it.
//
// Kills: a row served past an open or absent blocker, a row whose readiness
// needs another store read off the cache, a recorded row kept over the cache's
// newer verdict, and an own-verdict row that waits for the lane.
func TestV2GateLegReadyAgreesWithTheFederatedGate(t *testing.T) {
	for seed := int64(1); seed <= 40; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			c := newGateLegCity(t)
			var work, graph []string
			for i := 0; i < 5; i++ {
				b, err := c.work.Create(beads.Bead{Title: fmt.Sprintf("work %d", i), Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				work = append(work, b.ID)
			}
			newGraph := func(store beads.Store, title string) string {
				b, err := store.Create(beads.Bead{Title: title, Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				graph = append(graph, b.ID)
				return b.ID
			}
			target := func(self string) (string, string) {
				depType := []string{"blocks", "blocks", "waits-for", "conditional-blocks", "tracks", "parent-child"}[rng.Intn(6)]
				switch rng.Intn(7) {
				case 0, 1, 2:
					for {
						if id := graph[rng.Intn(len(graph))]; id != self {
							return id, depType
						}
					}
				case 3:
					return work[rng.Intn(len(work))], depType
				case 4:
					return "external:" + work[rng.Intn(len(work))], depType
				case 5:
					return "external:rig:capability", depType
				default:
					return fmt.Sprintf("%s-absent%d", c.namespaces[0], rng.Intn(3)), depType
				}
			}
			addEdges := func(store beads.Store, id string) {
				for n := rng.Intn(4); n > 0; n-- {
					to, depType := target(id)
					if err := store.DepAdd(id, to, depType); err != nil {
						t.Fatal(err)
					}
				}
			}
			for i := 0; i < 10; i++ {
				newGraph(c.engine.SQLiteStore, fmt.Sprintf("graph %d", i))
			}
			for _, id := range graph {
				addEdges(c.engine.SQLiteStore, id)
			}
			// Some blockers are already closed when the recording is taken, so
			// a recorded row can carry a release that a reopen withdraws.
			for _, id := range graph {
				if rng.Intn(3) == 0 {
					if err := c.engine.Close(id); err != nil {
						t.Fatal(err)
					}
				}
			}
			c.prime()
			for i := 0; i < 4; i++ {
				addEdges(c.binding, newGraph(c.binding, fmt.Sprintf("graph after prime %d", i)))
			}

			gated0 := c.gated()
			rec := c.recording(gated0)
			served0, reads := c.served(rec)
			if reads != 0 {
				t.Fatalf("the pass sent the binding's engine %d reads", reads)
			}
			if got, want := sortedIDs(served0), sortedIDs(gated0); !slices.Equal(got, want) {
				t.Fatalf("unchanged city: the pass serves %v, the federated gate %v", got, want)
			}

			for n := 3 + rng.Intn(5); n > 0; n-- {
				store, id := beads.Store(c.binding), graph[rng.Intn(len(graph))]
				if rng.Intn(3) == 0 {
					store, id = c.work, work[rng.Intn(len(work))]
				}
				row, err := store.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case row.Status == "closed":
					err = store.Reopen(id)
				case rng.Intn(4) == 0:
					if err = store.SetMetadata(id, beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkOutcomeBlocked); err == nil {
						err = store.Close(id)
					}
				case store == beads.Store(c.binding) && rng.Intn(2) == 0:
					c.closeOutside(id)
				default:
					err = store.Close(id)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			judged, err := c.binding.ReadyJudgmentContext(context.Background(), beads.ReadyQuery{TierMode: beads.TierBoth})
			if err != nil {
				t.Fatalf("fixture: the binding cache cannot judge its Ready: %v", err)
			}

			gated1 := idSet(c.gated())
			recorded := idSet(gated0)
			cacheReady := idSet(judged.Rows)
			served1, reads := c.served(rec)
			if reads != 0 {
				t.Fatalf("the pass sent the binding's engine %d reads", reads)
			}
			served := idSet(served1)
			// selfJudged rows are the cache's to answer: their readiness needs no
			// other store, and the cache vouched for them. Every other row is the
			// recording's, as it always was.
			selfJudged := func(id string) bool {
				_, vouched := judged.Deps[id]
				return vouched && c.ownVerdict(id)
			}
			describe := func(id string) string {
				deps, _ := c.engine.SQLiteStore.DepList(id, "down")
				_, vouched := judged.Deps[id]
				return fmt.Sprintf("edges %v, cache vouched %t, cache ready %t, recorded %t, gate serves %t", deps, vouched, cacheReady[id], recorded[id], gated1[id])
			}
			for id := range served {
				if !gated1[id] && (!recorded[id] || selfJudged(id)) {
					t.Errorf("served %s, which the federated gate holds and which is not a recorded row the cache leaves to the recording (%s)", id, describe(id))
				}
			}
			for id := range recorded {
				if !selfJudged(id) && !served[id] {
					t.Errorf("recorded row %s, which the cache leaves to the recording, is no longer served (%s)", id, describe(id))
				}
			}
			for id := range gated1 {
				if selfJudged(id) && !served[id] {
					t.Errorf("own-verdict row %s, which the federated gate serves now, is not served (%s)", id, describe(id))
				}
			}
			for id := range cacheReady {
				if c.ownVerdict(id) && !served[id] {
					t.Errorf("own-verdict row %s, which the cache's Ready serves, is not served (%s)", id, describe(id))
				}
			}
		})
	}
}

func idSet(rows []beads.Bead) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, b := range rows {
		out[b.ID] = true
	}
	return out
}

func sortedIDs(rows []beads.Bead) []string {
	ids := beadIDsOf(rows)
	slices.Sort(ids)
	return ids
}
