package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// These tests pin the guarded-batch gate (ga-l69b9m, architect Amendment 1) at
// the cmd/gc seams that reach a batch: the conversation lane host, the emitting
// class store, the bead-policy store, and the controller's demand-leg
// classification of a cache over the decorated binding.

func rerouteBatch(id string, revision int64) beads.GuardedBatch {
	return beads.GuardedBatch{Items: []beads.GuardedItem{{Update: &beads.GuardedUpdate{
		ID:               id,
		ExpectedRevision: &revision,
		Opts:             beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: carrierNewExecutor}},
	}}}}
}

func requireBatchedRerouteClearsStamps(t *testing.T, engine *beads.SQLiteStore, store beads.Store) {
	t.Helper()
	bead := seedCarrierBead(t, engine)
	batcher, ok := beads.GuardedBatcherFor(store)
	if !ok {
		t.Fatalf("GuardedBatcherFor(%T) found no batcher over a SQLite engine", store)
	}
	if _, err := batcher.ApplyGuarded(context.Background(), rerouteBatch(bead.ID, bead.Revision)); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	requireCarrierStampsCleared(t, engine, bead.ID)
}

// The lane host and its journal store come up over a decorated SQLite engine.
func TestRouteClearGuardedBatchLaneStoreOverDecoratedEngine(t *testing.T) {
	engine := openBindingEngineForTest(t)
	decorated := beads.WithRouteChangeClearing(engine, routeClearIdentity)
	if err := laneStoreCapability(decorated); err != nil {
		t.Fatalf("laneStoreCapability(decorated SQLite) = %v, want the lane host to start", err)
	}
	if _, err := opjournal.NewLaneStore(beads.SessionStore{Store: decorated}, cliOpClock); err != nil {
		t.Fatalf("opjournal.NewLaneStore over a decorated SQLite engine: %v", err)
	}
}

func TestRouteClearGuardedBatchThroughEmittingClassStore(t *testing.T) {
	engine := openBindingEngineForTest(t)
	decorated := beads.WithRouteChangeClearing(engine, routeClearIdentity)
	requireBatchedRerouteClearsStamps(t, engine, &emittingClassStore{Store: decorated, cityPath: t.TempDir()})
}

func TestRouteClearGuardedBatchThroughBeadPolicyStore(t *testing.T) {
	engine := openBindingEngineForTest(t)
	policy := wrapStoreWithBeadPolicies(engine, &config.City{})
	requireBatchedRerouteClearsStamps(t, engine, beads.WithRouteChangeClearing(policy, routeClearIdentity))
}

// A cache over the decorated binding keeps the engine's exact-read declaration,
// so the demand lane serves that leg from its cache as it does over bare SQLite.
func TestRouteClearDemandLegStaysExactOverTheDecoratedBinding(t *testing.T) {
	engine := openBindingEngineForTest(t)
	guarded := beads.WithRouteChangeClearing(engine, routeClearIdentity)
	cs, _ := newCachedSplitControllerState(context.Background(), t, splitClassRoutes(guarded), beads.NewMemStore())
	cache := bindingCacheOf(t, cs)
	got, exact := demandLegCache(cache)
	if got != cache {
		t.Fatalf("demandLegCache resolved %p, want the binding cache %p", got, cache)
	}
	if !exact {
		t.Fatal("the binding cache over the decorated SQLite engine is not exact; the demand lane would read it live")
	}
}
