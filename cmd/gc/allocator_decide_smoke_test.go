package main

import (
	"io"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// A smoke-level comparison with legacy's desired state (the full
// differential suite is P3-5c). One city, two copies of its store: legacy's
// demand pass runs on one and creates and binds rows; the decide runs on
// the other, fed by the same collectors the gather phase will call. They
// must agree on the existing row's selection and binding, the pool count,
// and the slots of the rows to create.
func TestDecideSmokeMatchesLegacyDesiredState(t *testing.T) {
	cityPath := t.TempDir()
	now := time.Now().UTC()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "sleep 1000", MaxActiveSessions: intPtr(3)}},
	}
	rows := func() []beads.Bead {
		created := now.Add(-time.Hour)
		return []beads.Bead{
			{
				ID: "gc-1", Title: "worker-1", Type: session.BeadType, Labels: []string{session.LabelSession}, Status: "open", CreatedAt: created,
				Metadata: map[string]string{
					"template": "worker", "state": "active", "pool_managed": "true",
					"session_name": "s-gc-1", "agent_name": "worker-1", "pool_slot": "1", "generation": "1",
				},
			},
			{ID: "w-1", Title: "w-1", Type: "task", Status: "open", CreatedAt: created, Metadata: map[string]string{"gc.routed_to": "worker"}},
			{ID: "w-2", Title: "w-2", Type: "task", Status: "open", CreatedAt: created, Metadata: map[string]string{"gc.routed_to": "worker"}},
		}
	}

	legacyStore := beads.NewMemStoreFrom(0, rows(), nil)
	legacySnap, err := loadSessionBeadSnapshot(legacyStore)
	if err != nil {
		t.Fatal(err)
	}
	legacy := buildDesiredStateWithSessionBeadsAt("city", cityPath, now, now, cfg, runtime.NewFake(), legacyStore, nil, legacySnap, nil, io.Discard)
	after, err := sessionFrontDoor(legacyStore).ListAll(session.ListAllOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var legacySlots []int
	legacyDesired := map[string]bool{}
	var legacyTrigger string
	for _, info := range after {
		if _, ok := legacy.State[info.SessionNameMetadata]; ok {
			legacyDesired[info.ID] = true
		}
		if info.ID == "gc-1" {
			legacyTrigger = info.TriggerBeadID
			continue
		}
		slot, _ := strconv.Atoi(info.PoolSlot)
		legacySlots = append(legacySlots, slot)
	}
	slices.Sort(legacySlots)

	store := beads.NewMemStoreFrom(0, rows(), nil)
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
			UnassignedRouted: routed, UnassignedRoutedRefs: routedRefs, UnassignedRoutedPartial: routedPartial,
		},
	}
	d := f.decide()

	var slots []int
	for _, p := range d.Plans {
		slots = append(slots, p.Plan.poolSlot)
	}
	slices.Sort(slots)
	if !slices.Equal(slots, legacySlots) {
		t.Fatalf("create slots: decide %v, legacy %v (trace %v)", slots, legacySlots, d.Trace)
	}
	if !legacyDesired["gc-1"] || legacyTrigger == "" {
		t.Fatalf("fixture: legacy must desire and rebind the existing row (desired %v, trigger %q)", legacyDesired, legacyTrigger)
	}
	// The row is not alive, so the decide publishes legacy's trigger write
	// as its binding (AM2: applied at start instead).
	e := entryOf(t, d, "gc-1")
	if !e.InDesired || e.Binding == nil || e.Binding.WorkBeadID != legacyTrigger {
		t.Fatalf("existing row: indesired=%v binding=%+v, legacy bound %q", e.InDesired, e.Binding, legacyTrigger)
	}
	if got, want := d.Snapshot.PoolDesired["worker"], len(legacy.State); got != want {
		t.Fatalf("PoolDesired = %d, legacy desires %d pool sessions", got, want)
	}
}
