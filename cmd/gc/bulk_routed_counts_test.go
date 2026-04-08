package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// failingStore wraps a MemStore and forces Ready/List to fail. Used to
// exercise the per-rig fallback semantics in precomputeBulkRoutedCounts.
type failingStore struct {
	beads.Store
}

func (failingStore) Ready() ([]beads.Bead, error) {
	return nil, errors.New("simulated dolt failure")
}

func (failingStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("simulated dolt failure")
}

func mustSeed(t *testing.T, store beads.Store, in beads.Bead) {
	t.Helper()
	out, err := store.Create(in)
	if err != nil {
		t.Fatalf("seed bead: %v", err)
	}
	if in.Status != "" && in.Status != out.Status {
		status := in.Status
		if err := store.Update(out.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("seed bead status: %v", err)
		}
	}
}

func TestPrecomputeBulkRoutedCounts_GroupsByRoutedTo(t *testing.T) {
	store := beads.NewMemStore()
	// Routed to alpha: one ready unassigned, one ready assigned (excluded),
	// one in-progress unassigned, one in-progress assigned (excluded).
	mustSeed(t, store, beads.Bead{Title: "r1", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	mustSeed(t, store, beads.Bead{Title: "r2", Assignee: "worker-1", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	mustSeed(t, store, beads.Bead{Title: "p1", Status: "in_progress", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	mustSeed(t, store, beads.Bead{Title: "p2", Status: "in_progress", Assignee: "worker-2", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	// Routed to beta: two ready unassigned.
	mustSeed(t, store, beads.Bead{Title: "b1", Metadata: map[string]string{"gc.routed_to": "beta"}})
	mustSeed(t, store, beads.Bead{Title: "b2", Metadata: map[string]string{"gc.routed_to": "beta"}})
	// Unrouted: excluded from all counts.
	mustSeed(t, store, beads.Bead{Title: "u1"})

	cfg := &config.City{}
	bulk := precomputeBulkRoutedCounts(map[string]beads.Store{"rig1": store}, cfg)
	if bulk == nil {
		t.Fatal("bulk is nil")
	}
	if !bulk.Covers("rig1") {
		t.Error("rig1 should be covered")
	}
	if got := bulk.Ready["alpha"]; got != 1 {
		t.Errorf("Ready[alpha] = %d, want 1", got)
	}
	if got := bulk.InProgress["alpha"]; got != 1 {
		t.Errorf("InProgress[alpha] = %d, want 1", got)
	}
	if got := bulk.Total("alpha"); got != 2 {
		t.Errorf("Total(alpha) = %d, want 2", got)
	}
	if got := bulk.Total("beta"); got != 2 {
		t.Errorf("Total(beta) = %d, want 2", got)
	}
	if !bulk.Has("alpha") || !bulk.Has("beta") {
		t.Error("Has should be true for both templates")
	}
	if bulk.Has("gamma") {
		t.Error("Has(gamma) should be false")
	}
}

func TestPrecomputeBulkRoutedCounts_SkipsFileProvider(t *testing.T) {
	// When cfg.Beads.Provider == "file", buildStores assigns a single
	// shared FileStore to every rig, so iterating rigStores would read
	// HQ data once per rig and miss the rig dolt backends entirely.
	// precomputeBulkRoutedCounts must short-circuit in that mode so
	// callers fall back to the per-pool subprocess path.
	store := beads.NewMemStore()
	mustSeed(t, store, beads.Bead{Title: "r", Metadata: map[string]string{"gc.routed_to": "alpha"}})
	stores := map[string]beads.Store{"rig1": store, "rig2": store}
	cfg := &config.City{Beads: config.BeadsConfig{Provider: "file"}}
	if got := precomputeBulkRoutedCounts(stores, cfg); got != nil {
		t.Errorf("file provider must return nil so fallback kicks in, got %+v", got)
	}
}

func TestPrecomputeBulkRoutedCounts_PartialRigFailure(t *testing.T) {
	ok := beads.NewMemStore()
	mustSeed(t, ok, beads.Bead{Title: "x", Metadata: map[string]string{"gc.routed_to": "alpha"}})

	stores := map[string]beads.Store{
		"healthy": ok,
		"broken":  failingStore{Store: beads.NewMemStore()},
	}
	bulk := precomputeBulkRoutedCounts(stores, &config.City{})
	if bulk == nil {
		t.Fatal("bulk is nil")
	}
	if !bulk.Covers("healthy") {
		t.Error("healthy rig should be covered")
	}
	if bulk.Covers("broken") {
		t.Error("broken rig must NOT be covered — caller should fall back per-rig")
	}
	if got := bulk.Ready["alpha"]; got != 1 {
		t.Errorf("Ready[alpha] = %d, want 1 (broken rig must not erase healthy counts)", got)
	}
}

func TestBulkRoutedCounts_NilSafe(t *testing.T) {
	var b *BulkRoutedCounts
	if b.Covers("anything") {
		t.Error("nil Covers must return false")
	}
	if b.Has("anything") {
		t.Error("nil Has must return false")
	}
	if b.Total("anything") != 0 {
		t.Error("nil Total must return 0")
	}
}

// failingRunner returns an error so any test that hits it will fail loudly
// — used to assert the bulk fast path skips the subprocess runner.
func failingRunner(t *testing.T) ScaleCheckRunner {
	return func(cmd, dir string) (string, error) {
		t.Helper()
		t.Errorf("runner called with cmd=%q dir=%q — bulk fast path should skip it", cmd, dir)
		return "", nil
	}
}

func TestComputeWorkSet_BulkFastPathHit(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	bulk := &BulkRoutedCounts{
		Ready:  map[string]int{"worker": 1},
		OKRigs: map[string]bool{"": true}, // empty rig name matches default
	}
	work := computeWorkSet(cfg, failingRunner(t), "test-city", "/tmp", nil, bulk, nil)
	if !work["worker"] {
		t.Error("expected worker to be in workSet from bulk hit")
	}
}

func TestComputeWorkSet_BulkFastPathMiss(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	bulk := &BulkRoutedCounts{
		Ready:  map[string]int{}, // no work for worker
		OKRigs: map[string]bool{"": true},
	}
	work := computeWorkSet(cfg, failingRunner(t), "test-city", "/tmp", nil, bulk, nil)
	if work["worker"] {
		t.Error("expected no work; bulk hit must short-circuit subprocess")
	}
}

func TestComputeWorkSet_BulkFastPathPoolNameRouting(t *testing.T) {
	// Pool instances route by PoolName, not QualifiedName — see
	// config.Agent.EffectiveWorkQuery / EffectiveOnBoot. Verify the
	// bulk fast path keys lookups by PoolName when set.
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker-1", PoolName: "worker"},
		},
	}
	bulk := &BulkRoutedCounts{
		Ready:  map[string]int{"worker": 1}, // routed by template, not instance
		OKRigs: map[string]bool{"": true},
	}
	work := computeWorkSet(cfg, failingRunner(t), "test-city", "/tmp", nil, bulk, nil)
	if !work["worker-1"] {
		t.Error("expected worker-1 (PoolName=worker) to be in workSet via PoolName routing")
	}
}

func TestComputeWorkSet_BulkRigNotCoveredFallsBack(t *testing.T) {
	// When the bulk path didn't cover this agent's rig, computeWorkSet
	// must fall back to the per-pool subprocess runner instead of
	// silently treating the agent as having no work.
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	bulk := &BulkRoutedCounts{
		Ready:  map[string]int{},
		OKRigs: map[string]bool{}, // no rigs covered
	}
	called := false
	runner := func(_, _ string) (string, error) {
		called = true
		return `[{"id":"BL-1"}]`, nil
	}
	work := computeWorkSet(cfg, runner, "test-city", "/tmp", nil, bulk, nil)
	if !called {
		t.Error("runner was not called — fallback path skipped")
	}
	if !work["worker"] {
		t.Error("expected worker to be in workSet from fallback runner")
	}
}

func TestPrecomputeBulkRoutedCounts_EmptyInputs(t *testing.T) {
	if got := precomputeBulkRoutedCounts(nil, &config.City{}); got != nil {
		t.Error("nil stores should return nil")
	}
	if got := precomputeBulkRoutedCounts(map[string]beads.Store{}, &config.City{}); got != nil {
		t.Error("empty stores should return nil")
	}
	if got := precomputeBulkRoutedCounts(map[string]beads.Store{"r": beads.NewMemStore()}, nil); got != nil {
		t.Error("nil cfg should return nil")
	}
}
