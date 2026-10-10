package main

import (
	"bytes"
	"cmp"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// legacyCollectedDemandView is legacy's demand pass (buildDesiredState's
// store block) with its repairs left out: the same collectors, called as
// legacy calls them, with legacy's probe env, its two ready caches and its
// nil reads.
func legacyCollectedDemandView(env demandGatherEnv) demandView {
	cfg := env.Cfg
	targets := buildDemandTargets(env.CityName, env.CityPath, cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths,
		env.OpenSessions, controllerQueryRuntimeEnv, io.Discard)
	var v demandView
	v.AssignedWork, v.AssignedStores, v.AssignedStoreRefs, v.ReadyAssigned, v.StorePartial = collectAssignedWorkBeadsWithStores(
		env.CityPath, cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, env.Sessions, newReadyDemandCache())
	routed, _, routedRefs, routedPartial := collectOpenUnassignedRoutedWork(
		env.CityPath, cfg, env.CityStore, env.RigStores, env.SuspendedRigPaths, io.Discard, nil, nil)
	demandCache := newReadyDemandCache()
	v.Collected = collectedDemand{
		UnassignedRouted:        routed,
		UnassignedRoutedRefs:    routedRefs,
		UnassignedRoutedPartial: routedPartial,
		ColdWakeTemplates:       targets.coldWakeTemplates,
		NamedOnDemandTemplates:  targets.namedOnDemandTemplates,
	}
	for _, pool := range targets.pendingPools {
		v.CustomCheckTemplates = append(v.CustomCheckTemplates, cfg.Agents[pool.agentIdx].QualifiedName())
	}
	if len(targets.defaultScaleTargets) > 0 {
		v.Collected.DefaultProbed = true
		v.Collected.DefaultCounts, v.Collected.DefaultDemand, v.Collected.DefaultPartials, _ = defaultScaleCheckCountsAndDemand(cfg, targets.defaultScaleTargets, demandCache)
	}
	if len(targets.defaultNamedScaleTargets) > 0 {
		v.NamedDefault, v.Collected.NamedPartials, _ = defaultNamedSessionDemand(targets.defaultNamedScaleTargets, cfg, env.CityName, demandCache)
	}
	v.RelocatedClaimRefs = assignedWorkRelocatedClaimRefs(env.CityPath, cfg, env.CityStore)
	v.WakeClaimRefs = assignedWorkClaimRefs(env.CityPath, cfg, env.CityStore)
	return v
}

// sortedDemandView orders v's index-aligned rows by (store ref, ID) and each
// demand's work bead IDs: cached Lists come back in map order.
func sortedDemandView(v demandView) demandView {
	v.AssignedWork, v.AssignedStoreRefs, v.AssignedStores = sortedAlignedRows(v.AssignedWork, v.AssignedStoreRefs, v.AssignedStores)
	v.Collected.UnassignedRouted, v.Collected.UnassignedRoutedRefs, _ = sortedAlignedRows(v.Collected.UnassignedRouted, v.Collected.UnassignedRoutedRefs, nil)
	for template, d := range v.Collected.DefaultDemand {
		d.WorkBeadIDs = slices.Sorted(slices.Values(d.WorkBeadIDs))
		v.Collected.DefaultDemand[template] = d
	}
	return v
}

func sortedAlignedRows(rows []beads.Bead, refs []string, stores []beads.Store) ([]beads.Bead, []string, []beads.Store) {
	type pair struct {
		row   beads.Bead
		ref   string
		store beads.Store
	}
	pairs := make([]pair, len(rows))
	for i := range rows {
		pairs[i] = pair{row: rows[i], ref: refs[i]}
		if stores != nil {
			pairs[i].store = stores[i]
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int { return cmp.Or(cmp.Compare(a.ref, b.ref), cmp.Compare(a.row.ID, b.row.ID)) })
	for i, p := range pairs {
		rows[i], refs[i] = p.row, p.ref
		if stores != nil {
			stores[i] = p.store
		}
	}
	return rows, refs, stores
}

// gatherMatchFixture is a city store and one rig store with demand for every
// view field: a default-probe pool with routed and assigned work, a cold
// pool with a custom scale_check, a pool backing an on_demand named session,
// and a rig pool whose routed work lives in its rig.
func gatherMatchFixture(t *testing.T) demandGatherEnv {
	t.Helper()
	rigPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig-a", Path: rigPath}},
		Agents: []config.Agent{
			poolAgent("worker", "", intPtr(5), 0),
			{Name: "checker", MaxActiveSessions: intPtr(3), ScaleCheck: "exit 0"},
			poolAgent("mayor", "", intPtr(1), 0),
			poolAgent("rigworker", "rig-a", intPtr(2), 0),
		},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "on_demand"}},
	}
	city := beads.NewMemStoreFrom(0, []beads.Bead{
		workBead("gc-r1", "worker", "", "open", 2),
		workBead("gc-r2", "checker", "", "open", 2),
		workBead("gc-r3", "mayor", "", "open", 2),
		workBead("gc-a1", "worker", "worker-1", "in_progress", 2),
		workBead("gc-a2", "worker", "worker-2", "open", 2),
	}, nil)
	rig := beads.NewMemStoreFrom(0, []beads.Bead{workBead("rg-r1", "rig-a/rigworker", "", "open", 2)}, nil)
	return demandGatherEnv{
		CityName: "test-city", CityPath: t.TempDir(), Cfg: cfg,
		CityStore: city, RigStores: map[string]beads.Store{"rig-a": rig},
	}
}

// Kills: a field mapped wrong or dropped, a collector called with other
// arguments than legacy's, and the shared ready cache reading other than
// legacy's two. Over the same stores, gather through legacyDemandReads gives
// legacy's view, field for field; the fixture must give every field a value.
func TestGatherDemandMatchesLegacyCollectors(t *testing.T) {
	for name, build := range map[string]func(*testing.T) demandGatherEnv{
		"city and rig":   gatherMatchFixture,
		"repair fixture": func(t *testing.T) demandGatherEnv { return newRepairGoldenFixture(t).gatherEnv() },
	} {
		t.Run(name, func(t *testing.T) {
			env := build(t)
			want := sortedDemandView(legacyCollectedDemandView(env))
			got, err := gatherDemand(env, legacyDemandReads{})
			if err != nil {
				t.Fatalf("gatherDemand: %v", err)
			}
			got = sortedDemandView(got)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("gathered view differs from legacy's collectors\n got  %+v\n want %+v", got, want)
			}
			if len(want.AssignedWork) == 0 || len(want.Collected.UnassignedRouted) == 0 || !want.Collected.DefaultProbed || len(want.WakeClaimRefs) == 0 {
				t.Fatalf("fixture: legacy view lacks assigned work, routed work, a default probe or claim refs: %+v", want)
			}
		})
	}
	env := gatherMatchFixture(t)
	want := legacyCollectedDemandView(env)
	if len(want.ReadyAssigned) == 0 || len(want.CustomCheckTemplates) == 0 || len(want.Collected.ColdWakeTemplates) == 0 ||
		len(want.Collected.NamedOnDemandTemplates) == 0 || want.NamedDefault == nil || len(want.Collected.DefaultCounts) == 0 {
		t.Fatalf("fixture: legacy view leaves a field empty: %+v", want)
	}
}

func (f repairGoldenFixture) gatherEnv() demandGatherEnv {
	return demandGatherEnv{
		CityName: f.env.CityName, CityPath: f.env.CityPath, Cfg: f.env.Cfg,
		CityStore: f.env.CityStore, RigStores: f.env.RigStores, SuspendedRigPaths: f.env.SuspendedRigPaths,
		Sessions: f.env.Sessions,
	}
}

// Kills: a demand repair run inside the pass. The repair golden fixture gives
// every repair legacy interleaves with these reads a row to write
// (TestBackstopDemandRepairsWriteLogGoldenInLegacyOrder); gather writes none,
// through either reads.
func TestGatherDemandPerformsNoWrites(t *testing.T) {
	for name, reads := range map[string]demandReads{
		"legacy reads": legacyDemandReads{},
		"v2 reads":     newV2DemandReads(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), nil),
	} {
		f := newRepairGoldenFixture(t)
		if _, err := gatherDemand(f.gatherEnv(), reads); err != nil {
			t.Fatalf("%s: gatherDemand: %v", name, err)
		}
		if len(f.log.ops) > 0 {
			t.Errorf("%s: gather wrote %q", name, f.log.ops)
		}
	}
}

// gatherLaneTestConfig has a default-probe pool backing an on_demand named
// session, so a leg's failure reaches every partial the view carries, and a
// custom scale_check pool, whose probe env gather never builds.
func gatherLaneTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			poolAgent("worker", "", intPtr(5), 0),
			{Name: "checker", MaxActiveSessions: intPtr(3), ScaleCheck: "exit 0"},
		},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
}

// Kills: a live read inside the pass (C0.4): gather reading a lane-fed leg's
// backing, or reading demand other than through reads. The leg's cache holds
// a blocked row folded to "open" that the recording excluded; gather sees
// only the recording and never touches the backing.
func TestGatherDemandNoLiveReadOnLaneLegs(t *testing.T) {
	cache, backing := newDemandCache(t, false, routedDemandBead("gc-blocked"), routedDemandBead("gc-open"), assignedDemandBead("gc-a1", "in_progress"))
	backing.armed.Store(true)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	open := routedDemandBead("gc-open")
	rec := &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
		keyOf(sourceDemand, cache): {EndedAt: now, sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{open}, ReadyAll: []beads.Bead{open}}}},
	}}

	v, err := gatherDemand(demandGatherEnv{Cfg: gatherLaneTestConfig(), CityStore: cache}, newV2DemandReads(now, rec))
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	if ops := backing.readLog(); len(ops) > 0 {
		t.Errorf("gather read a lane-fed leg's backing: %q", ops)
	}
	if got := ids(v.Collected.UnassignedRouted); !slices.Equal(got, []string{"gc-open"}) {
		t.Errorf("unassigned routed work = %v, want the recorded gc-open only", got)
	}
	if got := v.Collected.DefaultCounts["worker"]; got != 1 {
		t.Errorf("worker default demand = %d, want 1 from the recorded Ready", got)
	}
	if got := ids(v.AssignedWork); !slices.Equal(got, []string{"gc-a1"}) || v.StorePartial {
		t.Errorf("assigned work = %v (partial %t), want gc-a1 from the strict cached List", got, v.StorePartial)
	}
	if !slices.Equal(v.CustomCheckTemplates, []string{"checker"}) {
		t.Errorf("custom scale_check pools = %v, want [checker]", v.CustomCheckTemplates)
	}
}

// Kills: a partial flag dropped between the collectors and the view, which
// would let the decide shrink a template it cannot see (P-3). With no
// recording, the lane-fed leg reads nothing, and every partial the view
// carries must say so; a fresh recording is the control.
func TestGatherDemandPartialPropagates(t *testing.T) {
	cfg := gatherLaneTestConfig()
	cache, _ := newDemandCache(t, false, routedDemandBead("gc-r1"))
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r1 := routedDemandBead("gc-r1")
	fresh := &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
		keyOf(sourceDemand, cache): {EndedAt: now, sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{r1}, ReadyAll: []beads.Bead{r1}}}},
	}}
	for _, tc := range []struct {
		name    string
		rec     *externalReadsRecording
		partial bool
	}{
		{name: "fresh recording", rec: fresh},
		{name: "no recording", partial: true},
	} {
		v, err := gatherDemand(demandGatherEnv{Cfg: cfg, CityStore: cache}, newV2DemandReads(now, tc.rec))
		if err != nil {
			t.Fatalf("%s: gatherDemand: %v", tc.name, err)
		}
		c := v.Collected
		if v.StorePartial != tc.partial || c.UnassignedRoutedPartial != tc.partial || c.DefaultPartials["worker"] != tc.partial || c.NamedPartials["worker"] != tc.partial {
			t.Errorf("%s: partial assigned=%t routed=%t default=%t named=%t, want all %t",
				tc.name, v.StorePartial, c.UnassignedRoutedPartial, c.DefaultPartials["worker"], c.NamedPartials["worker"], tc.partial)
		}
		if merged := mergeCollectedDemand(cfg, c); merged.PoolScaleCheckPartial["worker"] != tc.partial {
			t.Errorf("%s: merged pool partial for worker = %t, want %t", tc.name, merged.PoolScaleCheckPartial["worker"], tc.partial)
		}
	}
}

// Kills: an env with no config or store gathered as a city with no demand.
func TestGatherDemandRefusesEnvWithoutStore(t *testing.T) {
	for name, env := range map[string]demandGatherEnv{
		"no config": {CityStore: beads.NewMemStore()},
		"no store":  {Cfg: gatherLaneTestConfig()},
	} {
		if _, err := gatherDemand(env, legacyDemandReads{}); err == nil {
			t.Errorf("%s: gatherDemand succeeded, want an error", name)
		}
	}
}

// Kills: a collector panic that escapes the gather, and a panicking leg read
// as complete. A panic in a fan-out leg (W-H2's recoverLeg) is that leg's
// partial and the gather goes on with the city leg's rows; one in the
// default probe, which reads on the gather's own goroutine, fails the gather
// with an empty view instead of killing the pass.
func TestGatherDemandSurvivesCollectorPanic(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "repo", Path: "/repo"}},
		Agents:    []config.Agent{poolAgent("worker", "", intPtr(5), 0), poolAgent("rigworker", "repo", intPtr(2), 0)},
	}
	env := func(rig panickingLegStore, stderr io.Writer) demandGatherEnv {
		city := beads.NewMemStoreFrom(0, []beads.Bead{
			workBead("gc-r1", "worker", "", "open", 2),
			workBead("gc-a1", "worker", "worker-1", "in_progress", 2),
		}, nil)
		rig.Store = beads.NewMemStoreFrom(100, nil, nil)
		return demandGatherEnv{Cfg: cfg, CityStore: city, RigStores: map[string]beads.Store{"repo": rig}, Stderr: stderr}
	}

	t.Run("fan-out leg", func(t *testing.T) {
		var stderr bytes.Buffer
		v, err := gatherDemand(env(panickingLegStore{panicOnList: true}, &stderr), legacyDemandReads{})
		if err != nil {
			t.Fatalf("gatherDemand: %v, want the panicking leg read partial", err)
		}
		if !v.StorePartial || !v.Collected.UnassignedRoutedPartial {
			t.Errorf("partial assigned=%t routed=%t, want both: the rig leg panicked", v.StorePartial, v.Collected.UnassignedRoutedPartial)
		}
		if got := ids(v.AssignedWork); !slices.Equal(got, []string{"gc-a1"}) {
			t.Errorf("assigned work = %v, want the city leg's gc-a1", got)
		}
		if got := ids(v.Collected.UnassignedRouted); !slices.Equal(got, []string{"gc-r1"}) {
			t.Errorf("routed work = %v, want the city leg's gc-r1", got)
		}
		if !strings.Contains(stderr.String(), "injected leg panic") {
			t.Errorf("stderr = %q, want the recovered panic", stderr.String())
		}
	})

	t.Run("inline probe", func(t *testing.T) {
		var stderr bytes.Buffer
		v, err := gatherDemand(env(panickingLegStore{panicOnReady: true}, &stderr), legacyDemandReads{})
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("err = %v, want the probe's panic as the gather's error", err)
		}
		if !reflect.DeepEqual(v, demandView{}) {
			t.Errorf("view = %+v, want empty with the error", v)
		}
		if !strings.Contains(stderr.String(), "injected leg panic: Ready") || !strings.Contains(stderr.String(), "goroutine") {
			t.Errorf("stderr = %q, want the panic value and its stack", stderr.String())
		}
	})
}
