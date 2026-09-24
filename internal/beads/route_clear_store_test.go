package beads_test

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// captureLog redirects the default logger's output to a buffer for the
// duration of the test, mirroring internal/beads' own
// caching_store_cadence_internal_test.go:captureLog. Duplicated rather than
// imported: that helper is unexported in package beads, and this file is
// package beads_test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	})
	return buf
}

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

// testStampedExecutor is the pool-route identity gc.session_name encodes in
// the empty-oldTarget tests below (ga-34b0ps, implementing the ga-u5okvo
// ruling, verdict (b) refined -- MPR send-back round 2 on #6217).
const testStampedExecutor = "gascity/builder"

// seedUnroutedBeadWithSession creates a bead carrying no gc.routed_to (the
// empty-oldTarget shape both Lane 1 and Lane 2 restore into) but already
// stamped with the three executor-identity fields, as a bead mid-flight
// through a real session would be. sessionName is stored as-is (already in
// session-name/tmux-safe shape); callers encode via
// agent.SanitizeQualifiedNameForSession when they need it to decode back to a
// specific qualified identity.
func seedUnroutedBeadWithSession(t *testing.T, s beads.Store, sessionName string) beads.Bead {
	t.Helper()
	created, err := s.Create(beads.Bead{
		Title: "unrouted, session-stamped bead",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:      "",
			beadmeta.SessionNameMetadataKey:   sessionName,
			beadmeta.WorkDirMetadataKey:       "worktrees/stamped-executor",
			beadmeta.LegacyWorkDirMetadataKey: "worktrees/stamped-executor-legacy",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// TestRouteChangeClearingStore_SetMetadata_EmptyOldTarget_RestoresSameExecutor_NoOp
// pins the Lane 1 shape (cmd/gc/detached_orphan_lane.go:restoreDetachedOrphanRoute /
// sweepDetachedHandoffOrphans): a failed done sequence cleared gc.routed_to
// (and assignee) together, leaving the bead's gc.session_name stamp as the
// only trace of who was working it. Recovery re-derives the SAME pool route
// from that bead's own session bead and writes it back to gc.routed_to. Before
// this fix, clearIfGenuine treated any empty oldTarget as automatically
// "genuine" (normalizer("") can never equal normalizer(non-empty)), so this
// restore wiped the very stamps it was trying to preserve. The fix bridges
// the stamped session-name-shape identity back to pool-route shape via
// agent.UnsanitizeQualifiedNameFromSession (existing helper, per the
// identity-separator-contract-v1 doc) before comparing against newTarget.
func TestRouteChangeClearingStore_SetMetadata_EmptyOldTarget_RestoresSameExecutor_NoOp(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)

	sessionName := agent.SanitizeQualifiedNameForSession(testStampedExecutor)
	b := seedUnroutedBeadWithSession(t, wrapped, sessionName)

	if err := wrapped.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, testStampedExecutor); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	got, err := wrapped.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != testStampedExecutor {
		t.Errorf("gc.routed_to: want %q, got %q", testStampedExecutor, got.Metadata[beadmeta.RoutedToMetadataKey])
	}
	assertStampsIntact(t, wrapped, b.ID)
}

// TestRouteChangeClearingStore_SetMetadata_EmptyOldTarget_GenuineMismatch_ClearsStamps
// pins the Lane 2 shape (cmd/gc/route_recovery.go:carriedPoolRoute /
// recoverUnroutedWorkRoutes): gc.routed_to is restored from the bead's own
// carried gc.run_target, a legacy/declared field independent of whichever
// specific session last touched it. When that recovered route genuinely
// differs from the executor the bead's gc.session_name stamp implies, the
// stamps must still clear -- this is a real handoff, not a same-executor
// restore, and is the pre-existing, correct behavior this fix must not
// regress while fixing Lane 1 above.
func TestRouteChangeClearingStore_SetMetadata_EmptyOldTarget_GenuineMismatch_ClearsStamps(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)

	sessionName := agent.SanitizeQualifiedNameForSession(testStampedExecutor)
	b := seedUnroutedBeadWithSession(t, wrapped, sessionName)

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

// TestRouteChangeClearingStore_SetMetadata_EmptyOldTarget_NoSessionNameStamped_ClearsStamps
// guards against an overly broad fix: an empty oldTarget must NOT become a
// blanket "never clear" carve-out. When the bead carries no gc.session_name
// at all -- the ordinary first-ever routing of a fresh bead, still the
// dominant real-world empty-oldTarget case -- there is no stamped identity to
// bridge against, so clearIfGenuine has nothing to compare and must fall
// through to its pre-existing unconditional clear.
func TestRouteChangeClearingStore_SetMetadata_EmptyOldTarget_NoSessionNameStamped_ClearsStamps(t *testing.T) {
	mem := beads.NewMemStore()
	wrapped := beads.WithRouteChangeClearing(mem, identityNormalizer)

	b := seedUnroutedBeadWithSession(t, wrapped, "")

	if err := wrapped.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, testNewTarget); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	assertStampsCleared(t, wrapped, b.ID)
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
// failingClearBatchStore fails SetMetadataBatch for one chosen bead id,
// letting a test observe that a backing-store rejection of the internal
// stamp-clearing write (as opposed to the routing write itself, which never
// goes through SetMetadataBatch when the reroute is triggered via
// SetMetadata) is logged rather than silently discarded.
type failingClearBatchStore struct {
	beads.Store
	failID string
}

func (f *failingClearBatchStore) SetMetadataBatch(id string, kv map[string]string) error {
	if id == f.failID {
		return errors.New("injected clear-batch failure")
	}
	return f.Store.SetMetadataBatch(id, kv)
}

// TestRouteChangeClearingStore_ClearWriteFailure_IsLogged pins design NFR-5
// ("clear failures are logged, never block the triggering write" -- Sec 1
// requirements table; "Clear failures are logged and swallowed" verbatim in
// Sec 6 prose and Sec 13.1): a backing-store rejection of the bead's own
// stamp-clearing write must produce a log line, and must never surface as an
// error from the triggering routing write.
func TestRouteChangeClearingStore_ClearWriteFailure_IsLogged(t *testing.T) {
	mem := beads.NewMemStore()
	b := seedRoutedBead(t, mem, testOldTarget)

	fs := &failingClearBatchStore{Store: mem, failID: b.ID}
	wrapped := beads.WithRouteChangeClearing(fs, identityNormalizer)

	buf := captureLog(t)

	if err := wrapped.SetMetadata(b.ID, beadmeta.RoutedToMetadataKey, testNewTarget); err != nil {
		t.Fatalf("SetMetadata: want nil -- a failed clear write must never fail the triggering routing write, got %v", err)
	}

	if got := buf.String(); !strings.Contains(got, b.ID) || !strings.Contains(got, "injected clear-batch failure") {
		t.Errorf("want a log line naming bead %s and the swallowed clear-write failure, got log output: %q", b.ID, got)
	}
}

// TestRouteChangeClearingStore_MoleculeRootClearWriteFailure_IsLogged pins
// the same NFR-5 guarantee for the second of the two swallowed
// SetMetadataBatch calls clearIfGenuine makes: the molecule-root mirror
// clear. The step's own clear is left to succeed so this test isolates the
// root-clear call site specifically.
func TestRouteChangeClearingStore_MoleculeRootClearWriteFailure_IsLogged(t *testing.T) {
	mem := beads.NewMemStore()
	seed := beads.WithRouteChangeClearing(mem, identityNormalizer)

	root := seedRoutedBead(t, seed, testOldTarget)
	step, err := seed.Create(beads.Bead{
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

	fs := &failingClearBatchStore{Store: mem, failID: root.ID}
	wrapped := beads.WithRouteChangeClearing(fs, identityNormalizer)

	buf := captureLog(t)

	if err := wrapped.SetMetadata(step.ID, beadmeta.RoutedToMetadataKey, testNewTarget); err != nil {
		t.Fatalf("SetMetadata: want nil -- a failed root-mirror clear must never fail the triggering routing write, got %v", err)
	}
	assertStampsCleared(t, wrapped, step.ID)

	if got := buf.String(); !strings.Contains(got, root.ID) || !strings.Contains(got, "injected clear-batch failure") {
		t.Errorf("want a log line naming molecule root %s and the swallowed clear-write failure, got log output: %q", root.ID, got)
	}
}

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
