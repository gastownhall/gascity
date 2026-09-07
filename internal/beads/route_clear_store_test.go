package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin the acceptance criteria for ga-cm2o5t.1.1 (clear
// executor-identity stamps on a genuine gc.routed_to reroute), per the
// design on parent bead ga-cm2o5t.1 secs 2, 3, 6, and 13.1: a Store
// decorator that, on a normalized change to gc.routed_to, clears the three
// executor-identity stamps (gc.session_name, gc.work_dir, legacy work_dir)
// on the rerouted bead and -- when the bead is a molecule step -- on its
// molecule root (sec 6 / Risk R6).

const (
	testOldTarget = "gascity/old-executor"
	testNewTarget = "gascity/new-executor"
)

// identityNormalizer treats every target as already normalized -- sufficient
// for tests that don't exercise pool-slot-suffix collapsing.
var identityNormalizer = beads.RouteNormalizerFunc(func(target string) string { return target })

// collapsingNormalizer simulates agentutil.NormalizePoolRouteTarget's
// pool-slot-suffix collapsing: "worker-<N>" normalizes to "worker", so a
// reroute from "worker" to "worker-2" is a normalized no-op (FR-2) -- the
// exact bug class ga-79uuwq guarded against for raw string comparison.
var collapsingNormalizer = beads.RouteNormalizerFunc(func(target string) string {
	if strings.HasPrefix(target, "worker-") {
		return "worker"
	}
	return target
})

// seedRoutedBead creates a bead carrying gc.routed_to plus the three
// executor-identity stamps a genuine reroute must clear.
func seedRoutedBead(t *testing.T, s beads.Store, routedTo string) beads.Bead {
	t.Helper()
	created, err := s.Create(beads.Bead{
		Title: "stamped bead",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:      routedTo,
			beadmeta.SessionNameMetadataKey:   "gascity--old-executor",
			beadmeta.WorkDirMetadataKey:       "worktrees/old-executor",
			beadmeta.LegacyWorkDirMetadataKey: "worktrees/old-executor-legacy",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

func assertStampsCleared(t *testing.T, s beads.Store, id string) {
	t.Helper()
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, key := range []string{
		beadmeta.SessionNameMetadataKey,
		beadmeta.WorkDirMetadataKey,
		beadmeta.LegacyWorkDirMetadataKey,
	} {
		if got.Metadata[key] != "" {
			t.Errorf("%s: want cleared (empty), got %q", key, got.Metadata[key])
		}
	}
}

func assertStampsIntact(t *testing.T, s beads.Store, id string) {
	t.Helper()
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, key := range []string{
		beadmeta.SessionNameMetadataKey,
		beadmeta.WorkDirMetadataKey,
		beadmeta.LegacyWorkDirMetadataKey,
	} {
		if got.Metadata[key] == "" {
			t.Errorf("%s: want intact (non-empty), got cleared", key)
		}
	}
}

func TestRouteChangeClearingStore_SetMetadata_GenuineReroute_ClearsStamps(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)
	b := seedRoutedBead(t, wrapped, testOldTarget)

	if err := wrapped.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, testNewTarget); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	got, err := wrapped.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != testNewTarget {
		t.Errorf("gc.routed_to: want %q, got %q", testNewTarget, got.Metadata[beadmeta.RoutedToMetadataKey])
	}
	assertStampsCleared(t, wrapped, b.ID)
}

func TestRouteChangeClearingStore_SetMetadata_SameTarget_NoOp(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)
	b := seedRoutedBead(t, wrapped, testOldTarget)

	if err := wrapped.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, testOldTarget); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	assertStampsIntact(t, wrapped, b.ID)
}

func TestRouteChangeClearingStore_SetMetadata_NormalizedEqual_NoOp(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, collapsingNormalizer)
	b := seedRoutedBead(t, wrapped, "worker")

	if err := wrapped.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, "worker-2"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	assertStampsIntact(t, wrapped, b.ID)
}

func TestRouteChangeClearingStore_SetMetadataBatch_GenuineReroute_ClearsStamps(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)
	b := seedRoutedBead(t, wrapped, testOldTarget)

	err := wrapped.SetMetadataBatch(b.ID, map[string]string{
		beadmeta.RoutedToMetadataKey: testNewTarget,
	})
	if err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	assertStampsCleared(t, wrapped, b.ID)
}

func TestRouteChangeClearingStore_Update_GenuineReroute_ClearsStamps(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)
	b := seedRoutedBead(t, wrapped, testOldTarget)

	err := wrapped.Update(b.ID, beads.UpdateOpts{
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: testNewTarget},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertStampsCleared(t, wrapped, b.ID)
}

func TestRouteChangeClearingStore_Update_WithoutRoutedTo_NoOp(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)
	b := seedRoutedBead(t, wrapped, testOldTarget)

	title := "renamed"
	if err := wrapped.Update(b.ID, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertStampsIntact(t, wrapped, b.ID)

	got, err := wrapped.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != title {
		t.Errorf("Title: want %q, got %q", title, got.Title)
	}
}

// TestRouteChangeClearingStore_GenuineReroute_ClearsMoleculeRootStamps pins
// the sec 6 / Risk R6 molecule-root extension: rerouting a molecule step
// also clears its molecule root's mirrored stamps, since stampRunRootFromStep
// mirrors them onto the root under a different bead ID that the per-id clear
// would not otherwise reach. The root's own gc.routed_to is untouched --
// only its mirrored SN/WD/LWD copies are cleared.
func TestRouteChangeClearingStore_GenuineReroute_ClearsMoleculeRootStamps(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)

	root := seedRoutedBead(t, wrapped, testOldTarget)
	step, err := wrapped.Create(beads.Bead{
		Title: "molecule step",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:      testOldTarget,
			beadmeta.SessionNameMetadataKey:   "gascity--old-executor",
			beadmeta.WorkDirMetadataKey:       "worktrees/old-executor-step",
			beadmeta.LegacyWorkDirMetadataKey: "worktrees/old-executor-step-legacy",
			beadmeta.RootBeadIDMetadataKey:    root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}

	if err := wrapped.SetMetadata(step.ID, beadmeta.RoutedToMetadataKey, testNewTarget); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	assertStampsCleared(t, wrapped, step.ID)
	assertStampsCleared(t, wrapped, root.ID)

	gotRoot, err := wrapped.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if gotRoot.Metadata[beadmeta.RoutedToMetadataKey] != testOldTarget {
		t.Errorf("root gc.routed_to: want untouched %q, got %q", testOldTarget, gotRoot.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// failingSetMetadataStore fails SetMetadata for a chosen key, letting the
// test observe whether the decorator clears stamps before confirming the
// routing write itself succeeded.
type failingSetMetadataStore struct {
	beads.Store
	failKey string
}

func (f *failingSetMetadataStore) SetMetadata(id, key, value string) error {
	if key == f.failKey {
		return errors.New("injected failure")
	}
	return f.Store.SetMetadata(id, key, value)
}

// TestRouteChangeClearingStore_SetMetadata_DelegateFailure_NoClear pins the
// 5-step algorithm's ordering (design sec 2): delegate the routing write
// first, and only clear stamps once that write actually succeeds.
func TestRouteChangeClearingStore_SetMetadata_DelegateFailure_NoClear(t *testing.T) {
	mem := beads.NewMemStore()
	b := seedRoutedBead(t, mem, testOldTarget)

	fs := &failingSetMetadataStore{Store: mem, failKey: beadmeta.RoutedToMetadataKey}
	wrapped := beads.WithRouteChangeClearing(fs, identityNormalizer)

	if err := wrapped.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, testNewTarget); err == nil {
		t.Fatal("SetMetadata: want error from failing backing write, got nil")
	}

	// Read directly from the backing store, bypassing the decorator, to
	// confirm the clear was never attempted when the routing write itself
	// failed.
	assertStampsIntact(t, mem, b.ID)
}

// recursionGuardStore counts SetMetadataBatch calls that reach the backing
// store, so a test can assert that a single reroute produces exactly one
// downstream clear -- not a second, nested clear triggered by the first.
type recursionGuardStore struct {
	beads.Store
	setMetadataBatchCalls int
}

func (r *recursionGuardStore) SetMetadataBatch(id string, kv map[string]string) error {
	r.setMetadataBatchCalls++
	return r.Store.SetMetadataBatch(id, kv)
}

// TestRouteChangeClearingStore_ClearWrite_DoesNotReenterGate pins design sec
// 9's guardrail: the decorator's own clear write (SetMetadataBatch, keyed on
// the three stamp fields only -- it never writes gc.routed_to) must not
// recursively re-trigger a second clear attempt. A single genuine reroute
// must produce exactly two backing SetMetadataBatch calls -- the routing
// delegate itself, plus one clear -- never a third from a nested re-entry.
func TestRouteChangeClearingStore_ClearWrite_DoesNotReenterGate(t *testing.T) {
	mem := beads.NewMemStore()
	guard := &recursionGuardStore{Store: mem}
	wrapped := beads.WithRouteChangeClearing(guard, identityNormalizer)
	b := seedRoutedBead(t, wrapped, testOldTarget)

	before := guard.setMetadataBatchCalls
	err := wrapped.SetMetadataBatch(b.ID, map[string]string{
		beadmeta.RoutedToMetadataKey: testNewTarget,
	})
	if err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	assertStampsCleared(t, wrapped, b.ID)

	if got, want := guard.setMetadataBatchCalls-before, 2; got != want {
		t.Errorf("backing SetMetadataBatch calls: want %d (1 route + 1 clear), got %d -- clear write may have recursively re-triggered the gate", want, got)
	}
}

// TestRouteChangeClearingStore_ImplementsConditionalWritesResolveTargeter
// pins an interoperability requirement documented at
// internal/beads/conditional_writes_resolve.go: any interface-embedding
// Store wrapper must declare its backing store via
// beads.ConditionalWritesResolveTargeter, or beads.ResolveConditionalWriter
// silently collapses to the unset/legacy path for every store this decorator
// wraps -- even under conditional_writes mode=require, where that silent
// collapse is exactly the failure the seam exists to make inexpressible.
// Every other Store-wrapping decorator in this package and cmd/gc
// (WorkStore, GraphStore, SessionStore, MailStore, OrdersStore, NudgesStore,
// the cmd/gc policy store, splittest.StrictStore) declares this the same
// way: return the immediate backing store, unchanged.
func TestRouteChangeClearingStore_ImplementsConditionalWritesResolveTargeter(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)

	targeter, ok := wrapped.(beads.ConditionalWritesResolveTargeter)
	if !ok {
		t.Fatal("Store returned by WithRouteChangeClearing must implement beads.ConditionalWritesResolveTargeter, or ResolveConditionalWriter silently collapses to unset/legacy for every store this decorator wraps")
	}
	if got := targeter.ConditionalWritesResolveTarget(); got != mem {
		t.Errorf("ConditionalWritesResolveTarget: want the immediate backing store, got a different store")
	}
}
