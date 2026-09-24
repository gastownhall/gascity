package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// fullyCapableBackingStore is a beads.Store test double that implements every
// optional capability route_clear_store.go must forward: beads.GraphApplyStore
// (via ApplyGraphPlan), beads.BatchDeleter, beads.Counter and beads.RowWitness.
// None of the fast/hermetic test backends (FileStore, MemStore) implement
// these -- only BdStore, NativeDoltStore and CachingStore do, and none of
// those are reachable from a cheap, hermetic unit test through the real
// openStoreAtForCity factory (it has no seam to inject a fake backing store,
// and the native/bd providers require a real preflight-gated store). A
// capability-bearing double standing in for that real backend is therefore
// the only way to prove the *forwarding* is correct in isolation from
// whether a given production backend happens to implement the capability.
type fullyCapableBackingStore struct {
	beads.Store

	graphPlanApplied *beads.GraphApplyPlan
	graphApplyErr    error
	deletedIDs       []string
	deleteBatchErr   error
	countCalls       int
	countErr         error
	sawRows          bool
}

func (s *fullyCapableBackingStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	s.graphPlanApplied = plan
	return &beads.GraphApplyResult{}, s.graphApplyErr
}

func (s *fullyCapableBackingStore) DeleteBatch(ids []string) error {
	s.deletedIDs = append(s.deletedIDs, ids...)
	return s.deleteBatchErr
}

func (s *fullyCapableBackingStore) Count(_ context.Context, _ beads.ListQuery, _ ...string) (int, error) {
	s.countCalls++
	return 7, s.countErr
}

func (s *fullyCapableBackingStore) SawRows() bool {
	return s.sawRows
}

// TestRouteClearForwardsOptionalCapabilitiesThroughProductionComposition
// composes a store exactly the way openStoreAtForCityWithConfig does at
// cmd/gc/main.go:1698-1701 -- wrapStoreWithBeadPolicies, then
// beads.WithRouteChangeClearing on top -- and asserts that
// beads.GraphApplyFor, beads.HandlesFor (tier expansion), beads.Counter,
// beads.BatchDeleter and beads.RowWitness all still resolve through the
// outermost (route-clear) wrapper.
//
// Per ga-8q8z2w: routeChangeClearingStore embeds the Store INTERFACE, so it
// only promotes the capabilities it re-declares -- every one of these is
// invisible through it today even though the backing store (and the policy
// layer wrapping it) fully supports them.
//
// If a future change moves openStoreAtForCityWithConfig's composition order
// off wrapStoreWithBeadPolicies-then-WithRouteChangeClearing, update this
// test's setup to match; TestOpenStoreAtForCityForwardsReadyContextThroughRouteClear
// below exercises the real factory end to end as a cross-check.
func TestRouteClearForwardsOptionalCapabilitiesThroughProductionComposition(t *testing.T) {
	backing := &fullyCapableBackingStore{Store: beads.NewMemStore(), sawRows: true}
	policyWrapped := wrapStoreWithBeadPolicies(backing, &config.City{})
	store := beads.WithRouteChangeClearing(policyWrapped, func(target string) string {
		return target
	})

	t.Run("GraphApplyFor", func(t *testing.T) {
		applier, ok := beads.GraphApplyFor(store)
		if !ok {
			t.Fatal("GraphApplyFor(store) ok = false, want true: route-clear-wrapped store does not resolve GraphApplyStore")
		}
		plan := &beads.GraphApplyPlan{
			Nodes: []beads.GraphApplyNode{
				{Key: "root", Title: "Root", Metadata: map[string]string{"gc.kind": "wisp"}},
			},
		}
		if _, err := applier.ApplyGraphPlan(context.Background(), plan); err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		if backing.graphPlanApplied == nil {
			t.Fatal("graph plan was not forwarded to the backing store")
		}
	})

	t.Run("BatchDeleter", func(t *testing.T) {
		deleter, ok := store.(beads.BatchDeleter)
		if !ok {
			t.Fatal("store.(beads.BatchDeleter) ok = false, want true: route-clear-wrapped store does not resolve BatchDeleter")
		}
		if err := deleter.DeleteBatch([]string{"bead-1", "bead-2"}); err != nil {
			t.Fatalf("DeleteBatch: %v", err)
		}
		if len(backing.deletedIDs) != 2 {
			t.Fatalf("backing.deletedIDs = %v, want 2 forwarded IDs", backing.deletedIDs)
		}
	})

	t.Run("Counter", func(t *testing.T) {
		counter, ok := store.(beads.Counter)
		if !ok {
			t.Fatal("store.(beads.Counter) ok = false, want true: route-clear-wrapped store does not resolve Counter")
		}
		n, err := counter.Count(context.Background(), beads.ListQuery{})
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 7 {
			t.Fatalf("Count = %d, want 7 (forwarded from backing store)", n)
		}
		if backing.countCalls != 1 {
			t.Fatalf("backing.countCalls = %d, want 1", backing.countCalls)
		}
	})

	t.Run("RowWitness", func(t *testing.T) {
		witness, ok := store.(beads.RowWitness)
		if !ok {
			t.Fatal("store.(beads.RowWitness) ok = false, want true: route-clear-wrapped store does not resolve RowWitness")
		}
		if !witness.SawRows() {
			t.Fatal("SawRows() = false, want true: backing store has seen rows")
		}
	})

	t.Run("HandlesForTierExpansion", func(t *testing.T) {
		handles := beads.HandlesFor(store)
		if _, ok := handles.Cached.(beadPolicyCachedReader); !ok {
			t.Fatalf("Handles().Cached = %T, want beadPolicyCachedReader: policy read-tier expansion is lost once route-clear wraps it", handles.Cached)
		}
		if _, ok := handles.Live.(beadPolicyLiveReader); !ok {
			t.Fatalf("Handles().Live = %T, want beadPolicyLiveReader: policy read-tier expansion is lost once route-clear wraps it", handles.Live)
		}
		// The outermost wrapper must own Handles().Writer, or a caller using
		// it to perform metadata writes silently bypasses route-clear's own
		// write-interception (the entire point of this decorator).
		if any(handles.Writer) != any(store) {
			t.Fatalf("Handles().Writer = %v, want the route-clear-wrapped store itself", handles.Writer)
		}
	})

	t.Run("ReadyContext", func(t *testing.T) {
		reader, ok := store.(beads.ContextReadyReader)
		if !ok {
			t.Fatal("store.(beads.ContextReadyReader) ok = false, want true: route-clear-wrapped store does not resolve ContextReadyReader")
		}
		if _, err := reader.ReadyContext(context.Background()); err != nil {
			t.Fatalf("ReadyContext: %v", err)
		}
	})
}

// TestOpenStoreAtForCityForwardsReadyContextThroughRouteClear opens a store
// through the real production entrypoint named in ga-8q8z2w's exit criteria
// (openStoreAtForCity, the terminal factory for every CLI/standalone open)
// rather than a hand-composed wrapper stack, and confirms ReadyContext --
// one of the capabilities route_clear_store.go must forward -- still
// resolves through it end to end.
//
// It is narrower than the table above only because FileStore (the fast,
// hermetic test provider every other openStoreAtForCity test in this
// package uses) is the one capability-forwarding target in ga-8q8z2w's list
// that a raw FileStore genuinely implements; FileStore has no ApplyGraphPlan,
// DeleteBatch, Count or SawRows to prove the rest against, real or forwarded.
func TestOpenStoreAtForCityForwardsReadyContextThroughRouteClear(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}

	reader, ok := store.(beads.ContextReadyReader)
	if !ok {
		t.Fatal("store.(beads.ContextReadyReader) ok = false, want true: openStoreAtForCity's composed store does not resolve ContextReadyReader")
	}
	if _, err := reader.ReadyContext(context.Background()); err != nil {
		t.Fatalf("ReadyContext: %v", err)
	}
}
