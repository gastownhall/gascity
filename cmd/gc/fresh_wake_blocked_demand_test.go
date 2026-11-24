package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// freshWakeBlockedCfg is a wake_mode="fresh" pool with no minimum and no
// custom scale_check, so the assigned step is the only possible demand.
func freshWakeBlockedCfg() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{{
			Name: "worker", StartCommand: "sleep 1000", WakeMode: "fresh",
			MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
		}},
	}
}

// freshWakeBlockedStore seeds an asleep pool session holding an open routed
// step; when blocked, the step waits on an open review bead through a blocks
// dependency, and the dependency-cleared control leaves it ready.
func freshWakeBlockedStore(t *testing.T, now time.Time, blocked bool) (beads.Store, string) {
	t.Helper()
	store := beads.NewMemStore()
	asleep, err := store.Create(beads.Bead{
		Title: "worker-1", Type: session.BeadType, Labels: []string{session.LabelSession}, CreatedAt: now.Add(-2 * time.Hour),
		Metadata: map[string]string{
			"template": "worker", "agent_name": "worker-1", "pool_slot": "1", "pool_managed": "true",
			"state": "asleep", "sleep_reason": "drained", "slept_at": now.Add(-time.Hour).Format(time.RFC3339),
			"generation": "2", "instance_token": "tok-2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(asleep.ID, "session_name", PoolSessionName("worker", asleep.ID)); err != nil {
		t.Fatal(err)
	}
	review, err := store.Create(beads.Bead{Title: "review", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	step, err := store.Create(beads.Bead{
		Title: "finalize", Type: "task", Status: "open", Assignee: asleep.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		if err := store.DepAdd(step.ID, review.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	return store, asleep.ID
}

// TestDecideAndLegacyFreshWakeSkipBlockedAssignedStep drives the v2 allocator
// and legacy realization from their own collected snapshots
// (gastownhall/gascity#8172). An open step blocked behind another pool's
// review, assigned to a fresh-wake agent's asleep session, contributes no pool
// demand and creates no clean session in either path, and the stale asleep row
// is not resumed; the dependency-cleared control still plans exactly one clean
// create. Both paths agree on the count.
func TestDecideAndLegacyFreshWakeSkipBlockedAssignedStep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		blocked bool
		want    int
	}{
		{name: "blocked step", blocked: true, want: 0},
		{name: "dependency-cleared control", blocked: false, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			now := time.Now().UTC()
			cfg := freshWakeBlockedCfg()

			legacyStore, asleepID := freshWakeBlockedStore(t, now, tc.blocked)
			legacySnap, err := loadSessionBeadSnapshot(legacyStore)
			if err != nil {
				t.Fatal(err)
			}
			legacy := buildDesiredStateWithSessionBeadsAt("city", cityPath, now, now, cfg, runtime.NewFake(), legacyStore, nil, legacySnap, nil, io.Discard)
			after, err := sessionFrontDoor(legacyStore).ListAll(session.ListAllOptions{})
			if err != nil {
				t.Fatal(err)
			}
			created := 0
			for _, info := range after {
				if info.ID != asleepID {
					created++
				}
			}
			if _, resumed := legacy.State[PoolSessionName("worker", asleepID)]; resumed {
				t.Errorf("legacy desires the stale asleep session %s, want it left asleep", asleepID)
			}
			if got := len(legacy.State); got != tc.want {
				t.Errorf("legacy desired %v, want %d pool session(s)", desiredNames(legacy), tc.want)
			}
			if created != tc.want {
				t.Errorf("legacy created %d clean session bead(s), want %d", created, tc.want)
			}

			store, _ := freshWakeBlockedStore(t, now, tc.blocked)
			snap, err := loadSessionBeadSnapshot(store)
			if err != nil {
				t.Fatal(err)
			}
			f := newAllocFixture(t, cfg)
			f.in.Now, f.in.CityPath = now, cityPath
			f.legs = []classStoreCandidate{{ref: "city:city", store: store}}
			assigned, _, refs, ready, partial := collectAssignedWorkBeadsWithStores(cityPath, cfg, store, nil, nil, snap, newReadyDemandCache())
			routed, _, routedRefs, routedPartial := collectOpenUnassignedRoutedWork(cityPath, cfg, store, nil, nil, io.Discard, nil, nil)
			targets := buildDemandTargets("city", cityPath, cfg, store, nil, nil, snap.OpenInfos(), controllerQueryRuntimeEnv, io.Discard)
			counts, demand, partials, _ := defaultScaleCheckCountsAndDemand(cfg, targets.defaultScaleTargets, newReadyDemandCache())
			f.in.Demand = demandView{
				AssignedWork: assigned, AssignedStoreRefs: refs, ReadyAssigned: ready, StorePartial: partial,
				Collected: collectedDemand{
					DefaultProbed: len(targets.defaultScaleTargets) > 0, DefaultCounts: counts, DefaultDemand: demand, DefaultPartials: partials,
					ColdWakeTemplates: targets.coldWakeTemplates, NamedOnDemandTemplates: targets.namedOnDemandTemplates,
					NameOnlyTemplates: targets.nameOnlyTemplates,
					UnassignedRouted:  routed, UnassignedRoutedRefs: routedRefs, UnassignedRoutedPartial: routedPartial,
				},
			}
			d := f.decide()

			if got := d.Snapshot.PoolDesired["worker"]; got != tc.want {
				t.Errorf("v2 PoolDesired[worker] = %d, want %d (trace %v)", got, tc.want, d.Trace)
			}
			if got := len(d.Plans); got != tc.want {
				t.Errorf("v2 planned %d create(s), want %d (trace %v)", got, tc.want, d.Trace)
			}
			if e := entryOf(t, d, asleepID); e.InDesired {
				t.Errorf("v2 desires the stale asleep session %s, want it left asleep", asleepID)
			}
			if got, legacyCount := d.Snapshot.PoolDesired["worker"], len(legacy.State); got != legacyCount {
				t.Errorf("v2 PoolDesired[worker] = %d, legacy desires %d", got, legacyCount)
			}
		})
	}
}

// TestCityRuntimeDemandSnapshotFreshWakeIgnoresBlockedAssignedStep pins the
// runtime awake-count arm of the fresh-wake readiness gate
// (gastownhall/gascity#8172): a wake_mode="fresh" pool's asleep session
// holding an open step blocked behind a review bead must contribute zero
// awake demand, while the dependency-cleared control contributes one. The
// custom scale_check pins independent demand at zero so the wake path is the
// only demand source.
func TestCityRuntimeDemandSnapshotFreshWakeIgnoresBlockedAssignedStep(t *testing.T) {
	const template = "worker"
	for _, tc := range []struct {
		name    string
		blocked bool
		want    int
	}{
		{name: "blocked assigned step is zero awake demand", blocked: true, want: 0},
		{name: "dependency-cleared control wakes one session", blocked: false, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			store := beads.NewMemStore()
			now := time.Now().UTC()
			asleep, err := store.Create(beads.Bead{
				Title: "worker-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
				Metadata: map[string]string{
					"template": template, "agent_name": "worker-1", "pool_slot": "1", "pool_managed": "true",
					"state": "asleep", "sleep_reason": "drained", "slept_at": now.Add(-time.Hour).Format(time.RFC3339),
					"generation": "2", "instance_token": "tok-2",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadata(asleep.ID, "session_name", PoolSessionName(template, asleep.ID)); err != nil {
				t.Fatal(err)
			}
			blocker, err := store.Create(beads.Bead{Title: "review", Type: "task", Status: "open"})
			if err != nil {
				t.Fatal(err)
			}
			step, err := store.Create(beads.Bead{
				Title: "finalize", Type: "task", Status: "open", Assignee: asleep.ID,
				Metadata: map[string]string{"gc.routed_to": template},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.blocked {
				if err := store.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents:    []config.Agent{{Name: template, StartCommand: "true", MaxActiveSessions: intPtr(1), WakeMode: "fresh", ScaleCheck: "printf 0"}},
			}
			cr := &CityRuntime{
				cityName: "test-city",
				cityPath: cityPath,
				cfg:      cfg,
				sp:       runtime.NewFake(),
				cs: &controllerState{
					cityName:      "test-city",
					cityPath:      cityPath,
					cityBeadStore: store,
					eventProv:     events.NewFake(),
				},
				stderr: io.Discard,
			}
			cr.buildFnWithSessionBeads = func(cfg *config.City, sp runtime.Provider, store beads.Store, rigStores map[string]beads.Store, sessionBeads *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle) DesiredStateResult {
				return buildDesiredStateWithSessionBeads("test-city", cityPath, time.Now(), cfg, sp, store, rigStores, sessionBeads, trace, io.Discard)
			}
			sessionBeads, err := loadSessionBeadSnapshot(store)
			if err != nil {
				t.Fatal(err)
			}

			snap := cr.loadDemandSnapshot(sessionBeads, nil, "patrol", false)

			if got := snap.result.PoolDesiredCounts[template]; got != tc.want {
				t.Fatalf("PoolDesiredCounts[%s] = %d, want %d", template, got, tc.want)
			}
		})
	}
}

// TestCityRuntimeDemandSnapshotFreshWakeReadinessIsStoreScoped pins the
// reconciler's awake count (the demand snapshot's pool compute): an open step
// assigned to a fresh-wake agent's asleep session counts toward the pool only
// when the build's store-scoped readiness proves it ready, keyed by its own
// store ref.
func TestCityRuntimeDemandSnapshotFreshWakeReadinessIsStoreScoped(t *testing.T) {
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "session-worker",
		Status: "open",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":         "worker-session-worker",
			"template":             "worker",
			"agent_name":           "worker-1",
			"pool_slot":            "1",
			poolManagedMetadataKey: boolMetadata(true),
			"state":                "asleep",
		},
	}})
	step := beads.Bead{
		ID: "step-1", Title: "finalize", Type: "task", Status: "open", Assignee: "session-worker",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	}
	for _, tc := range []struct {
		name  string
		ready map[storeScopedBeadKey]bool
		want  int
	}{
		{name: "no readiness proof", ready: nil, want: 0},
		{name: "ready in another store only", ready: map[storeScopedBeadKey]bool{{StoreRef: "riga", ID: "step-1"}: true}, want: 0},
		{name: "ready in its own store", ready: map[storeScopedBeadKey]bool{{StoreRef: "", ID: "step-1"}: true}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := &CityRuntime{
				cityName: "city",
				cityPath: t.TempDir(),
				cfg:      freshWakeBlockedCfg(),
				cs:       &controllerState{eventProv: events.NewFake()},
				stderr:   io.Discard,
			}
			cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
				return DesiredStateResult{
					State:                 map[string]TemplateParams{},
					ScaleCheckCounts:      map[string]int{"worker": 0},
					AssignedWorkBeads:     []beads.Bead{step},
					AssignedWorkStoreRefs: []string{""},
					ReadyAssigned:         tc.ready,
				}
			}

			snapshot := cr.loadDemandSnapshot(sessionBeads, nil, "poke", false)

			if got := snapshot.result.PoolDesiredCounts["worker"]; got != tc.want {
				t.Fatalf("PoolDesiredCounts[worker] = %d, want %d", got, tc.want)
			}
		})
	}
}
