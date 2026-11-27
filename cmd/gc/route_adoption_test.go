package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const adoptionTestCityPool = "ccc-researcher"

// adoptionTestConfig is a city whose HQ store (prefix "ct") adopts unrouted
// work into the city-scoped pool adoptionTestCityPool.
func adoptionTestConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "city", Prefix: "ct", UnroutedWorkTarget: adoptionTestCityPool},
		Agents: []config.Agent{
			{Name: adoptionTestCityPool, MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)},
		},
	}
}

// plainCreatedBead is what a plain `bd create` leaves behind: open,
// unassigned, a task, and no metadata at all.
func plainCreatedBead(id string) beads.Bead {
	return beads.Bead{ID: id, Title: "hand-made work", Type: "task", Status: "open"}
}

// adoptionRuntime builds a runtime over one counting city store, configured
// with cfg.
func adoptionRuntime(t *testing.T, cfg *config.City, seed ...beads.Bead) (*CityRuntime, *countingRouteStore) {
	t.Helper()
	store := &countingRouteStore{Store: beads.NewMemStoreFrom(0, seed, nil)}
	cr := &CityRuntime{
		cityName:            "city",
		cityPath:            t.TempDir(),
		cfg:                 cfg,
		standaloneCityStore: store,
		stderr:              io.Discard,
	}
	return cr, store
}

// TestRouteAdoptionDeltaPassAdoptsPlainCreatedCityBead is AC-1: a bead made
// with plain `bd create` in a city whose workspace names an
// unrouted_work_target is routed by the delta pass to exactly the value
// built-in `gc sling <target>` writes, and it is reported as adopted, not
// restored.
func TestRouteAdoptionDeltaPassAdoptsPlainCreatedCityBead(t *testing.T) {
	cfg := adoptionTestConfig()
	bead := plainCreatedBead("ct-1")
	cr, store := adoptionRuntime(t, cfg, bead)
	lane := cr.routeRecoveryLaneOf()

	lane.observe(beadCreatedEvent(t, bead))
	report := cr.recoverUnroutedWorkRoutesDelta()

	want := agentutil.NormalizePoolRouteTarget(cfg, adoptionTestCityPool)
	if got := mustRoutedTo(t, store, "ct-1"); got != want {
		t.Fatalf("ct-1 gc.routed_to = %q, want %q", got, want)
	}
	if report.adopted != 1 {
		t.Fatalf("delta pass adopted %d, want 1", report.adopted)
	}
	if report.restored != 0 {
		t.Fatalf("delta pass restored %d, want 0 (adoption is not a restore)", report.restored)
	}
}

// TestRouteAdoptionCarriedRouteWins pins that a carried gc.run_target is
// restored by the existing recovery path and never overwritten by the target.
func TestRouteAdoptionCarriedRouteWins(t *testing.T) {
	cfg := adoptionTestConfig()
	bead := plainCreatedBead("ct-2")
	bead.Metadata = map[string]string{beadmeta.RunTargetMetadataKey: "other-pool"}
	cr, store := adoptionRuntime(t, cfg, bead)

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)

	if got := mustRoutedTo(t, store, "ct-2"); got != "other-pool" {
		t.Fatalf("ct-2 gc.routed_to = %q, want the carried route %q", got, "other-pool")
	}
	if report.restored != 1 || report.adopted != 0 {
		t.Fatalf("backstop restored=%d adopted=%d, want restored=1 adopted=0", report.restored, report.adopted)
	}
}

// adoptionRigRuntime builds a runtime over a city store and one rig store
// ("hello-world", prefix "hw"), configured with cfg.
func adoptionRigRuntime(t *testing.T, cfg *config.City, citySeed, rigSeed []beads.Bead) (*CityRuntime, *countingRouteStore, *countingRouteStore) {
	t.Helper()
	cityStore := &countingRouteStore{Store: beads.NewMemStoreFrom(0, citySeed, nil)}
	rigStore := &countingRouteStore{Store: beads.NewMemStoreFrom(0, rigSeed, nil)}
	cr := &CityRuntime{
		cityName:            "city",
		cityPath:            t.TempDir(),
		cfg:                 cfg,
		standaloneCityStore: cityStore,
		standaloneRigStores: map[string]beads.Store{"hello-world": rigStore},
		stderr:              io.Discard,
	}
	return cr, cityStore, rigStore
}

// adoptionRigConfig is a city with a "hello-world" rig (prefix "hw") and a
// rig-scoped researcher pool. rigTarget is the rig's unrouted_work_target.
func adoptionRigConfig(t *testing.T, rigTarget string) *config.City {
	t.Helper()
	return &config.City{
		Workspace: config.Workspace{Name: "city", Prefix: "ct"},
		Rigs: []config.Rig{
			{Name: "hello-world", Path: t.TempDir(), Prefix: "hw", UnroutedWorkTarget: rigTarget},
		},
		Agents: []config.Agent{
			{Name: "researcher", Dir: "hello-world", MinActiveSessions: ptr(0), MaxActiveSessions: ptr(3)},
		},
	}
}

// TestRouteAdoptionRigTargetAdoptsOnlyItsOwnScope is AC-2's first half: a
// rig's unrouted_work_target adopts the rig's plain-created bead and leaves
// the city's unrouted, since the workspace sets no target.
func TestRouteAdoptionRigTargetAdoptsOnlyItsOwnScope(t *testing.T) {
	cfg := adoptionRigConfig(t, "hello-world/researcher")
	cityBead, rigBead := plainCreatedBead("ct-9"), plainCreatedBead("hw-9")
	cr, cityStore, rigStore := adoptionRigRuntime(t, cfg, []beads.Bead{cityBead}, []beads.Bead{rigBead})

	report := cr.runRouteRecoveryBackstop(backstopReasonCadence)

	if got := mustRoutedTo(t, rigStore, "hw-9"); got != "hello-world/researcher" {
		t.Fatalf("hw-9 gc.routed_to = %q, want %q", got, "hello-world/researcher")
	}
	if got := mustRoutedTo(t, cityStore, "ct-9"); got != "" {
		t.Fatalf("ct-9 gc.routed_to = %q, want empty (the workspace sets no target)", got)
	}
	if report.adopted != 1 {
		t.Fatalf("backstop adopted %d, want 1", report.adopted)
	}
}

// TestRouteAdoptionNoTargetAnywhereWritesNothing is AC-2's second half: with
// no unrouted_work_target configured, plain-created beads add no candidates
// and draw no writes from either pass.
func TestRouteAdoptionNoTargetAnywhereWritesNothing(t *testing.T) {
	cfg := adoptionRigConfig(t, "")
	cityBead, rigBead := plainCreatedBead("ct-9"), plainCreatedBead("hw-9")
	cr, cityStore, rigStore := adoptionRigRuntime(t, cfg, []beads.Bead{cityBead}, []beads.Bead{rigBead})
	lane := cr.routeRecoveryLaneOf()

	lane.observe(beadCreatedEvent(t, cityBead))
	lane.observe(beadCreatedEvent(t, rigBead))
	delta := cr.recoverUnroutedWorkRoutesDelta()
	if delta.candidates != 0 || delta.legReads != 0 {
		t.Fatalf("delta candidates=%d reads=%d, want 0/0 with no target configured", delta.candidates, delta.legReads)
	}
	backstop := cr.runRouteRecoveryBackstop(backstopReasonCadence)
	if backstop.candidates != 0 || backstop.adopted != 0 {
		t.Fatalf("backstop candidates=%d adopted=%d, want 0/0 with no target configured", backstop.candidates, backstop.adopted)
	}
	if cityStore.writes != 0 || rigStore.writes != 0 {
		t.Fatalf("writes city=%d rig=%d, want 0", cityStore.writes, rigStore.writes)
	}
}

// TestRouteAdoptionRefusesIneligibleBeads is AC-3: in a scope with a target,
// neither the delta nor the backstop pass adopts an open, unassigned,
// unrouted bead that the target's Tier 3 query would refuse, that is held,
// ephemeral, workflow or control topology, or that gc ever claimed.
func TestRouteAdoptionRefusesIneligibleBeads(t *testing.T) {
	withMeta := func(key string) func(*beads.Bead) {
		return func(b *beads.Bead) { b.Metadata = map[string]string{key: "x"} }
	}
	withLabel := func(label string) func(*beads.Bead) {
		return func(b *beads.Bead) { b.Labels = []string{label} }
	}
	cases := []struct {
		name  string
		shape func(*beads.Bead)
	}{
		{"epic", func(b *beads.Bead) { b.Type = "epic" }},
		{"ready-excluded type", func(b *beads.Bead) { b.Type = "molecule" }},
		{"ready-excluded label", withLabel("gc:session")},
		{"dispatch hold", withLabel(beadmeta.DispatchHoldLabels[0])},
		{"human label", withLabel(beadmeta.HumanLabel)},
		{"ephemeral", func(b *beads.Bead) { b.Ephemeral = true }},
		{"gc.kind", withMeta(beadmeta.KindMetadataKey)},
		{"gc.execution_routed_to", withMeta(beadmeta.ExecutionRoutedToMetadataKey)},
		{"gc.deferred_routed_to", withMeta(beadmeta.DeferredRoutedToMetadataKey)},
		{"gc.deferred_execution_routed_to", withMeta(beadmeta.DeferredExecutionRoutedToMetadataKey)},
		{"gc.instantiating", withMeta(beadmeta.InstantiatingMetadataKey)},
		{"gc.claimed_at", withMeta(beadmeta.ClaimedAtMetadataKey)},
		{"route-recovery quarantined", func(b *beads.Bead) {
			b.Metadata = map[string]string{beadmeta.RouteQuarantineMetadataKey: "true"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := plainCreatedBead("ct-3")
			tc.shape(&bead)
			if adoptableUnroutedWork(bead) {
				t.Fatalf("adoptableUnroutedWork(%s) = true, want false", tc.name)
			}
			cr, store := adoptionRuntime(t, adoptionTestConfig(), bead)
			cr.routeRecoveryLaneOf().observe(beadCreatedEvent(t, bead))

			delta := cr.recoverUnroutedWorkRoutesDelta()
			backstop := cr.runRouteRecoveryBackstop(backstopReasonCadence)

			if got := mustRoutedTo(t, store, "ct-3"); got != "" {
				t.Fatalf("gc.routed_to = %q, want empty", got)
			}
			if delta.adopted != 0 || backstop.adopted != 0 {
				t.Fatalf("adopted delta=%d backstop=%d, want 0/0", delta.adopted, backstop.adopted)
			}
		})
	}
	t.Run("gc.run_target is restored, not adopted", func(t *testing.T) {
		bead := plainCreatedBead("ct-4")
		bead.Metadata = map[string]string{beadmeta.RunTargetMetadataKey: "carried-pool"}
		if adoptableUnroutedWork(bead) {
			t.Fatal("adoptableUnroutedWork(gc.run_target) = true, want false")
		}
		cr, store := adoptionRuntime(t, adoptionTestConfig(), bead)
		cr.routeRecoveryLaneOf().observe(beadCreatedEvent(t, bead))
		delta := cr.recoverUnroutedWorkRoutesDelta()
		if got := mustRoutedTo(t, store, "ct-4"); got != "carried-pool" {
			t.Fatalf("gc.routed_to = %q, want the carried %q", got, "carried-pool")
		}
		if delta.adopted != 0 || delta.restored != 1 {
			t.Fatalf("delta adopted=%d restored=%d, want 0/1", delta.adopted, delta.restored)
		}
	})
}

// claimConcurrently is a session claiming the bead after the lane's live
// read: assigned and in progress, with gc.routed_to still empty.
func claimConcurrently(id string) func(*beads.MemStore) {
	return func(store *beads.MemStore) {
		assignee, status := "researcher-session", "in_progress"
		if err := store.Update(id, beads.UpdateOpts{Assignee: &assignee, Status: &status}); err != nil {
			panic(err)
		}
	}
}

// TestRouteAdoptionLosesToAWriteAfterItsLiveRead is AC-4: when another writer
// routes or a session claims an adoption candidate after the lane's live read,
// that route or claim stands, and the lost write is neither an adoption nor a
// restore charged to the flap limit.
func TestRouteAdoptionLosesToAWriteAfterItsLiveRead(t *testing.T) {
	const id = "ct-race"
	races := map[string]func(string) func(*beads.MemStore){
		"another writer routes": routeConcurrently,
		"a session claims":      claimConcurrently,
	}
	passes := map[string]func(cr *CityRuntime) routeRecoveryReport{
		"backstop": func(cr *CityRuntime) routeRecoveryReport {
			return cr.runRouteRecoveryBackstop(backstopReasonCadence)
		},
		"delta": func(cr *CityRuntime) routeRecoveryReport {
			cr.routeRecoveryLaneOf().observe(beadCreatedEvent(t, plainCreatedBead(id)))
			return cr.recoverUnroutedWorkRoutesDelta()
		},
	}
	for raceName, race := range races {
		for passName, pass := range passes {
			t.Run(raceName+"/"+passName, func(t *testing.T) {
				before := beads.NewMemStoreFrom(0, []beads.Bead{plainCreatedBead(id)}, nil)
				race(id)(before)
				want, err := before.Get(id)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}

				store := &raceAfterLiveReadStore{
					MemStore: beads.NewMemStoreFrom(0, []beads.Bead{plainCreatedBead(id)}, nil),
					target:   id,
					race:     race(id),
				}
				cr := &CityRuntime{cityName: "city", cfg: adoptionTestConfig(), standaloneCityStore: store, stderr: io.Discard}

				report := pass(cr)
				if !store.fired {
					t.Fatal("the competing write never ran; the pass did not re-read the bead")
				}
				if report.err != nil {
					t.Fatalf("pass error = %v, want nil: losing the race is not an error", report.err)
				}
				if report.adopted != 0 || report.restored != 0 {
					t.Fatalf("pass adopted=%d restored=%d, want 0/0", report.adopted, report.restored)
				}
				got, err := store.MemStore.Get(id)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Assignee != want.Assignee || got.Status != want.Status ||
					got.Metadata[beadmeta.RoutedToMetadataKey] != want.Metadata[beadmeta.RoutedToMetadataKey] {
					t.Fatalf("after the pass assignee=%q status=%q routed_to=%q, want the other writer's %q/%q/%q",
						got.Assignee, got.Status, got.Metadata[beadmeta.RoutedToMetadataKey],
						want.Assignee, want.Status, want.Metadata[beadmeta.RoutedToMetadataKey])
				}
				lane := cr.routeRecoveryLaneOf()
				lane.mu.Lock()
				defer lane.mu.Unlock()
				if n := lane.restores[id]; n != 0 {
					t.Fatalf("flap tally for a lost race = %d, want 0", n)
				}
			})
		}
	}
}
