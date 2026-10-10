package main

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

var gatherNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// sleepCountingProvider answers every sleep capability ask and counts them:
// the provider call the sleep policy makes per row.
type sleepCountingProvider struct {
	runtime.Provider
	asks atomic.Int64
}

func (p *sleepCountingProvider) SleepCapability(string) runtime.SessionSleepCapability {
	p.asks.Add(1)
	return runtime.SessionSleepCapabilityFull
}

// gatherFixture is a primed, exact sessions cache (the controller's SQLite
// binding) holding rows, a city on the worker pool, an observation cache with
// one complete inventory pass, and a planner.
type gatherFixture struct {
	env      gatherEnv
	cache    *beads.CachingStore
	backing  *demandBacking
	p        *planner
	inflight *inflightMap
	sp       *sleepCountingProvider
	cur      atomic.Pointer[reconcileEnv]
	rec      atomic.Pointer[externalReadsRecording]
	resolves atomic.Int64
	looks    atomic.Int64
}

func newGatherFixture(t *testing.T, rows ...beads.Bead) *gatherFixture {
	t.Helper()
	f := &gatherFixture{sp: &sleepCountingProvider{}, inflight: newInflightMap()}
	f.cache, f.backing = newDemandCache(t, true, rows...)
	cfg := workerCity(3)
	f.cur.Store(&reconcileEnv{Gen: 1, Cfg: cfg, SP: f.sp})
	clk := &clock.Fake{Time: gatherNow}
	obs := NewObservationCache(clk, time.Minute, "e1")
	obs.PublishInventory(obsPass(clk, 1, 1, completeBackend("")), nil)
	f.p = newPlanner(realPlannerClock{}, func() time.Duration { return time.Minute }, nil, f.inflight, nil, io.Discard)
	f.env = gatherEnv{
		CityPath: t.TempDir(), CityName: "test-city",
		Env:          f.cur.Load,
		Sessions:     func() beads.Store { return f.cache },
		RigStores:    func() map[string]beads.Store { return nil },
		Recording:    f.rec.Load,
		Observations: func() *ObservationCache { return obs },
		Episodes:     func() (map[string]session.StartupHealthEpisode, error) { return readStartupHealthEpisodes(f.cache) },
		ResolveTemplate: func(_ *reconcileEnv, info session.Info) (TemplateParams, error) {
			f.resolves.Add(1)
			return TemplateParams{SessionName: info.SessionNameMetadata}, nil
		},
		LookPath: func(name string) (string, error) { f.looks.Add(1); return "/bin/" + name, nil },
	}
	return f
}

func (f *gatherFixture) gather(t *testing.T) World {
	t.Helper()
	w, err := gather(f.env, f.p, gatherNow)
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return w
}

// Kills v5 R4 violations: a census, demand or episode read that reaches the
// backing store from the pass. Every read is served by the primed cache,
// except Ready's permitted local fallback on the exact (SQLite) leg.
func TestGatherReadsNoBackingStore(t *testing.T) {
	f := newGatherFixture(t,
		poolRow("gc-1", "worker", 1, "active"), poolRow("gc-2", "worker", 2, "asleep"),
		routedDemandBead("gc-r1"), assignedDemandBead("gc-w1", "open"))
	f.backing.armed.Store(true)
	w := f.gather(t)
	if len(w.Census.Rows) != 2 {
		t.Fatalf("census rows = %d, want 2", len(w.Census.Rows))
	}
	for _, op := range f.backing.readLog() {
		if !strings.HasPrefix(op, "Ready ") {
			t.Errorf("gather read the backing store: %s", op)
		}
	}
}

// Kills an empty city on error: a hard failure of the sessions leg fails
// gather instead of returning a World with no rows.
func TestGatherSessionsLegErrorReturnsError(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	failing := liveErrStore{censusErrStore{Store: f.cache, err: errors.New("sessions leg down")}}
	f.env.Sessions = func() beads.Store { return failing }
	w, err := gather(f.env, f.p, gatherNow)
	if err == nil || w.Census != nil {
		t.Fatalf("gather = (census %v, %v), want an error and no census", w.Census, err)
	}
}

// liveErrStore is a failing leg that reports a primed cache.
type liveErrStore struct{ censusErrStore }

func (liveErrStore) IsLive() bool { return true }

// Kills counting a settled effect twice, or not at all: a settlement posted
// before gather clears its entry from the World's in-flight view, and a
// stale settlement of an earlier submit leaves the row's next effect.
func TestGatherDrainsSettlementsFirst(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "asleep"))
	k := rowKey{Leg: "city:test-city", ID: "gc-1"}
	seq := f.inflight.add(inflightEntry{Kind: "start", Key: k})
	f.p.settlements.post(settlement{Key: k, Kind: "start", Seq: seq})
	if w := f.gather(t); len(w.InFlight.Entries) != 0 {
		t.Fatalf("in flight after its settlement = %+v, want none", w.InFlight.Entries)
	}
	next := f.inflight.add(inflightEntry{Kind: "start", Key: k})
	f.p.settlements.post(settlement{Key: k, Kind: "start", Seq: seq})
	if w := f.gather(t); len(w.InFlight.Entries) != 1 || w.InFlight.Entries[0].Seq != next {
		t.Fatalf("in flight after a stale settlement = %+v, want the next submit %d", w.InFlight.Entries, next)
	}
}

// Kills per-row provider calls and a stale memo after a reload: the sleep
// policy asks the provider once per (template, session name) and the
// transport check and template resolution run once per generation, however
// many passes run; a new env generation asks again.
func TestGatherMemoizesPerGeneration(t *testing.T) {
	f := newGatherFixture(t,
		poolRow("gc-1", "worker", 1, "active"), poolRow("gc-2", "worker", 2, "active"), poolRow("gc-3", "worker", 3, "asleep"))
	cfg := f.cur.Load().Cfg
	cfg.Agents[0].StartCommand, cfg.Agents[0].Provider = "", "claude"
	cfg.Providers = map[string]config.ProviderSpec{"claude": {Command: "claude"}}
	counts := func() [3]int64 { return [3]int64{f.sp.asks.Load(), f.looks.Load(), f.resolves.Load()} }
	w := f.gather(t)
	first := counts()
	if first[0] != 3 || first[2] != 3 || first[1] == 0 {
		t.Fatalf("first pass (sleep asks, lookups, resolutions) = %v, want 3 asks, some lookups, 3 resolutions", first)
	}
	if len(w.SleepPolicies) != 3 || w.Templates.Gen != 1 {
		t.Fatalf("first pass: %d sleep policies, memo gen %d; want 3 and gen 1", len(w.SleepPolicies), w.Templates.Gen)
	}
	for range 3 {
		f.gather(t)
	}
	if got := counts(); got != first {
		t.Fatalf("three more passes in one generation: counts %v, want unchanged %v", got, first)
	}
	f.cur.Store(&reconcileEnv{Gen: 2, Cfg: cfg, SP: f.sp})
	w = f.gather(t)
	if got := counts(); got[0] != 2*first[0] || got[1] != 2*first[1] || got[2] != 2*first[2] {
		t.Fatalf("after a reload: counts %v, want each doubled from %v", got, first)
	}
	if w.Templates.Gen != 2 {
		t.Fatalf("after a reload: memo gen %d, want 2", w.Templates.Gen)
	}
}

// Kills a gate opened on unprimed inputs: each boot input reads false until
// its input is ready, with no exception for a suspended city or for an
// unconfirmed (partial) server-absent inventory pass.
func TestGatherBootStateComputed(t *testing.T) {
	yes := true
	suspended := func() suspensionstate.State {
		return suspensionstate.State{City: suspensionstate.Override{Suspended: &yes}}
	}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, f *gatherFixture)
		want    bootState
		err     bool
	}{
		{name: "all inputs ready", want: bootState{CachePrimed: true, InventoryComplete: true, RecordingSeen: true}},
		{name: "cache unprimed", err: true, want: bootState{InventoryComplete: true}, arrange: func(_ *testing.T, f *gatherFixture) {
			unprimed := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
			f.env.Sessions = func() beads.Store { return unprimed }
		}},
		{name: "inventory partial, server absent unconfirmed", want: bootState{CachePrimed: true, RecordingSeen: true}, arrange: func(_ *testing.T, f *gatherFixture) {
			clk := &clock.Fake{Time: gatherNow}
			obs := NewObservationCache(clk, time.Minute, "e1")
			obs.PublishInventory(obsPass(clk, 1, 1, partialSingle()), nil)
			f.env.Observations = func() *ObservationCache { return obs }
		}},
		{name: "inventory partial, server confirmed dead", want: bootState{CachePrimed: true, InventoryComplete: true, RecordingSeen: true}, arrange: func(_ *testing.T, f *gatherFixture) {
			clk := &clock.Fake{Time: gatherNow}
			obs := NewObservationCache(clk, time.Minute, "e1")
			dead := partialSingle()
			dead.ServerAbsent, dead.ConfirmedDead = true, true
			obs.PublishInventory(obsPass(clk, 1, 1, dead), nil)
			f.env.Observations = func() *ObservationCache { return obs }
		}},
		{name: "no inventory pass", want: bootState{CachePrimed: true, RecordingSeen: true}, arrange: func(_ *testing.T, f *gatherFixture) {
			f.env.Observations = nil
		}},
		{name: "lane-fed leg, no recording", want: bootState{CachePrimed: true, InventoryComplete: true}, arrange: laneFedRig},
		{name: "lane-fed leg, recording", want: bootState{CachePrimed: true, InventoryComplete: true, RecordingSeen: true}, arrange: func(t *testing.T, f *gatherFixture) {
			laneFedRig(t, f)
			f.rec.Store(&externalReadsRecording{Seq: 1})
		}},
		{name: "suspended city, lane-fed leg, no recording", want: bootState{CachePrimed: true, InventoryComplete: true}, arrange: func(t *testing.T, f *gatherFixture) {
			laneFedRig(t, f)
			f.env.Suspension = suspended
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
			if tc.arrange != nil {
				tc.arrange(t, f)
			}
			w, err := gather(f.env, f.p, gatherNow)
			if (err != nil) != tc.err {
				t.Fatalf("gather err = %v, want error %t", err, tc.err)
			}
			if f.env.Suspension != nil && !w.CitySuspended {
				t.Error("the suspension reader's state did not suspend the city")
			}
			if w.Boot != tc.want || f.p.boot != tc.want {
				t.Errorf("boot = %+v (planner %+v), want %+v", w.Boot, f.p.boot, tc.want)
			}
		})
	}
}

// laneFedRig adds a rig whose store has no cache: a lane-fed demand leg.
func laneFedRig(t *testing.T, f *gatherFixture) {
	t.Helper()
	cfg := *f.cur.Load().Cfg
	cfg.Rigs = []config.Rig{{Name: "rig-a", Path: t.TempDir()}}
	f.cur.Store(&reconcileEnv{Gen: 1, Cfg: &cfg, SP: f.sp})
	rig := beads.NewMemStore()
	f.env.RigStores = func() map[string]beads.Store { return map[string]beads.Store{"rig-a": rig} }
}

// Kills a census-only row given a writer (AL1): with a rig leg in the census,
// gather builds the sessions leg's store alone, so an effect on any other
// leg's row refuses no-writer.
func TestGatherBuildsOnlyTheSessionsLegsWriter(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	laneFedRig(t, f)
	w := f.gather(t)
	if len(w.Census.Legs) != 2 || len(w.LegStores) != 1 || w.LegStores[w.Census.sessionsLeg()] != beads.Store(f.cache) {
		t.Fatalf("legs %v, leg stores %v, want two census legs and the sessions leg's store alone", w.Census.Legs, w.LegStores)
	}
}

// Kills a K1 env that loses the sessions snapshot (the stamp step would run
// inert) or the default scale_check target stores (their legs would go
// unrecorded): the lane's env closure carries both, from the cached census.
func TestGatherK1EnvFromCachedCensus(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	f.backing.armed.Store(true)
	env, err := f.env.externalReadsEnv()
	if err != nil {
		t.Fatalf("k1 env: %v", err)
	}
	if env.Cfg != f.cur.Load().Cfg || env.CityStore != f.cache || len(env.ProbeStores) != 1 || env.ProbeStores[0] != f.cache {
		t.Errorf("k1 env = cfg %p store %v probes %v, want the published config, the sessions store and its probe", env.Cfg, env.CityStore, env.ProbeStores)
	}
	if _, ok := env.Sessions.FindInfoByID("gc-1"); !ok {
		t.Error("k1 env's sessions snapshot lacks gc-1")
	}
	if ops := f.backing.readLog(); len(ops) > 0 {
		t.Errorf("k1 env read the backing store: %q", ops)
	}
}

// Kills an effect reading a memo the planner is rewriting (S-17): readers
// on other goroutines walk the published memo while passes publish new
// rows and generations. Run under -race.
func TestTemplateMemoImmutableUnderConcurrentReaders(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"))
	f.gather(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m := f.p.memo.templates.Load()
				for k, r := range m.entries {
					_ = k.SessionName + r.TP.SessionName
				}
			}
		}()
	}
	cfg := f.cur.Load().Cfg
	for i := range 20 {
		id := "gc-" + string(rune('a'+i))
		if _, err := f.cache.Create(poolRow(id, "worker", 2, "asleep")); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if i%5 == 0 {
			f.cur.Store(&reconcileEnv{Gen: uint64(2 + i), Cfg: cfg, SP: f.sp})
		}
		f.gather(t)
	}
	close(stop)
	wg.Wait()
	if got := len(f.p.memo.templates.Load().entries); got != 21 {
		t.Fatalf("memo holds %d entries, want one per row (21)", got)
	}
}

// Kills a lease input creeping back (SC A5): no type of this package that
// gather projects carries a lease, the census included.
func TestGatherProjectsNoLease(t *testing.T) {
	world := reflect.TypeFor[World]()
	seen := map[reflect.Type]bool{}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			if typ.Kind() == reflect.Map {
				walk(path+"[key]", typ.Key())
			}
			if seen[typ] {
				return
			}
			seen[typ] = true
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] || typ.PkgPath() != world.PkgPath() {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			field := typ.Field(i)
			if strings.Contains(field.Name, "Lease") {
				t.Errorf("World projects a lease: %s.%s", path, field.Name)
			}
			walk(path+"."+field.Name, field.Type)
		}
	}
	walk("World", world)
}

// wispSession is an open session row on the wisp (ephemeral) tier.
func wispSession(id string, meta ...string) beads.Bead {
	b := sessionRow(id, meta...)
	b.Ephemeral = true
	return b
}

// Kills a wisp-tier session row invisible to the planner (v5.1 AL1): a
// sessions binding that answers at the tier asked (no policy layer) serves
// its ephemeral rows only to a FederatedReadTier census.
func TestCensusReadsWispTier(t *testing.T) {
	f := newGatherFixture(t, poolRow("gc-1", "worker", 1, "active"), wispSession("gc-w", "template", "worker", "state", "active", "session_name", "wisp-1"))
	w := f.gather(t)
	if _, ok := w.Census.Rows[rowKey{Leg: "city:test-city", ID: "gc-w"}]; !ok || len(w.Census.Rows) != 2 {
		t.Fatalf("census rows = %v, want gc-1 and the wisp-tier gc-w", w.Census.Rows)
	}
}

// Kills a wisp-tier session row invisible to C11 (v5.1 AL1): the boot census
// counts an ephemeral row in a state main does not know.
func TestBootCensusReadsWispTier(t *testing.T) {
	cfg := workerCity(3)
	store := censusStore(wispSession("gc-w", "template", "worker", "state", "archived", "session_name", "wisp-1"))
	m, err := readV2SessionMigration(t.TempDir(), "test-city", cfg, store, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if m.UnknownStates["archived"] != 1 {
		t.Fatalf("unknown states = %v, want the wisp-tier archived row", m.UnknownStates)
	}
}

// Kills a mislabelled row (a gc:session bead with no template and no session
// name) reaching the arms, and a real row flagged for lacking only one of
// the two.
func TestMislabelledRowFlagged(t *testing.T) {
	f := newGatherFixture(t,
		sessionRow("gc-task", "state", "active"),
		sessionRow("gc-tpl", "template", "worker", "state", "active"),
		sessionRow("gc-name", "session_name", "adhoc", "state", "active"))
	w := f.gather(t)
	want := map[rowKey]bool{{Leg: "city:test-city", ID: "gc-task"}: true}
	if !reflect.DeepEqual(w.Mislabelled, want) {
		t.Fatalf("mislabelled = %v, want %v", w.Mislabelled, want)
	}
	if _, ok := w.Templates.lookup(w.Census.Rows[rowKey{Leg: "city:test-city", ID: "gc-task"}].Info); ok {
		t.Error("a mislabelled row's template was resolved")
	}
}
