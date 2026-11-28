package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestSplitCityV2DemandWithholdsInputGatedWorkOffThePass pins the v2 half of
// #8832 on the topology maintainer-city runs (session_reconciler = "v2", a
// split city). In the class binding: a routed graph step, and an assigned
// workflow root, both of a workflow whose input convoy and held input live in
// the work ledger. While the input is held the step is no pool demand and no
// v2 Ready row — though its own edges would settle it on the binding's cache
// — and the root is no wake demand; once the hold is released both count.
//
// The gather reads no byte of the ledger's backing (CONTRACT v5 R4): the
// external-reads lane judged both rows off the pass, and a subject its
// recording does not cover leaves the leg partial, unjudged and uncounted,
// rather than being judged live. Kills: the own-verdict merge restoring the
// step, a live input-admission read in the pass, and an unjudged root counted
// as wake demand.
func TestSplitCityV2DemandWithholdsInputGatedWorkOffThePass(t *testing.T) {
	cityPath := t.TempDir()
	maxSessions, minSessions := 5, 0
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc", Prefix: "gc"},
		Storage:   splitEnvStorageConfig(),
		Agents:    []config.Agent{{Name: "dog", StartCommand: "true", MinActiveSessions: &minSessions, MaxActiveSessions: &maxSessions}},
	}
	// The work ledger is lane-fed, as a bd ledger is: the embedded Store
	// hides the engine's exact reads, so the v2 pass reads it only from the
	// lane's recording.
	workEngine := beads.NewMemStore()
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

	create := func(store beads.Store, b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	input := create(workEngine, beads.Bead{Title: "input", Type: "task", Labels: []string{"hold:external"}})
	conv := create(workEngine, beads.Bead{Title: "input convoy", Type: "convoy"})
	if err := workEngine.DepAdd(conv.ID, input.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	gatedRoot := func(assignee string) beads.Bead {
		return beads.Bead{Title: "workflow root", Type: "molecule", Assignee: assignee, Metadata: map[string]string{
			beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
			beadmeta.InputConvoyIDMetadataKey: conv.ID,
		}}
	}
	root := create(binding, gatedRoot(""))
	step := create(binding, beads.Bead{Title: "graph-step", Type: "task", Metadata: map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
		beadmeta.RoutedToMetadataKey:   "dog",
	}})
	assigned := create(binding, gatedRoot("seat-1"))
	for _, cache := range []*beads.CachingStore{ledger, binding} {
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	lane, _ := newTestBackstopLane(externalReadsEnv{CityName: "gc", CityPath: cityPath, Cfg: cfg, CityStore: binding})
	lane = awaitEveryRead(lane)
	lane.steps = nil
	env := demandGatherEnv{CityName: "gc", CityPath: cityPath, Cfg: cfg, CityStore: binding}
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	type observed struct {
		pool     []string
		readyAll []string
		ready    map[string]bool
		partial  bool
	}
	// gather runs the v2 pass over rec, failing on any read of the ledger's
	// backing.
	gather := func(phase string, rec *externalReadsRecording) observed {
		t.Helper()
		before := len(ledgerBacking.readLog())
		ledgerBacking.armed.Store(true)
		view, err := gatherDemand(env, newV2DemandReads(now, rec))
		reads := newV2DemandReads(now, rec)
		reads.gateLegs = crossStoreGateLegs(cityPath, cfg, binding, nil, nil)
		readyAll, readyErr := reads.ReadyAll(binding)
		ledgerBacking.armed.Store(false)
		if err != nil {
			t.Fatalf("%s: gatherDemand: %v", phase, err)
		}
		if readyErr != nil {
			t.Fatalf("%s: v2 ReadyAll of the binding: %v", phase, readyErr)
		}
		if ops := ledgerBacking.readLog()[before:]; len(ops) > 0 {
			t.Errorf("%s: the v2 pass read the ledger's backing: %q", phase, ops)
		}
		o := observed{pool: slices.Clone(view.Collected.DefaultDemand["dog"].WorkBeadIDs), readyAll: beadIDsOf(readyAll), ready: map[string]bool{}, partial: view.StorePartial}
		for key, ok := range view.ReadyAssigned {
			if ok {
				o.ready[key.ID] = true
			}
		}
		return o
	}
	record := func() *externalReadsRecording {
		t.Helper()
		now = now.Add(time.Minute)
		passAndJoin(context.Background(), at(lane, now))
		return lane.recording()
	}

	held := gather("input held", record())
	if slices.Contains(held.pool, step.ID) || slices.Contains(held.readyAll, step.ID) {
		t.Fatalf("input held: pool demand %v, v2 Ready %v count step %s, whose input %s is held", held.pool, held.readyAll, step.ID, input.ID)
	}
	if held.ready[assigned.ID] {
		t.Fatalf("input held: assigned root %s, whose input %s is held, is wake demand", assigned.ID, input.ID)
	}
	if held.partial {
		t.Fatal("input held: the pass is partial, want every subject judged by the lane's recording")
	}

	// A subject the recording never judged: the pass neither reads it live
	// nor counts it, and the leg is partial until the lane judges it.
	rec := lane.recording()
	late := create(binding, gatedRoot("seat-2"))
	unjudged := gather("unrecorded subject", rec)
	if unjudged.ready[late.ID] || !unjudged.partial {
		t.Fatalf("unrecorded subject: assigned root %s wake demand=%t, partial=%t; want no wake demand and a partial pass", late.ID, unjudged.ready[late.ID], unjudged.partial)
	}

	if err := workEngine.Update(input.ID, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatal(err)
	}
	released := gather("input released", record())
	if !slices.Contains(released.pool, step.ID) || !slices.Contains(released.readyAll, step.ID) {
		t.Fatalf("input released: pool demand %v, v2 Ready %v lack step %s", released.pool, released.readyAll, step.ID)
	}
	if !released.ready[assigned.ID] || !released.ready[late.ID] {
		t.Fatalf("input released: wake demand %v lacks assigned roots %s and %s", released.ready, assigned.ID, late.ID)
	}
	if released.partial {
		t.Fatal("input released: the pass is partial, want every subject judged by the lane's recording")
	}
}
