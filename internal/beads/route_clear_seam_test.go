package beads_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// These tests pin the capability-seam gate (ga-l69b9m, architect ruling C-prime
// on ga-cn94949q): a gc.routed_to write that reaches the engine through a
// conditional capability -- the single-key metadata CAS, the revision-fenced
// UpdateIfMatch, or the assignment-guarded UpdateIfAssignment -- clears the
// executor-identity stamps exactly as the decorator's own SetMetadata /
// SetMetadataBatch / Update do, and only when the write landed.
//
// The resolvers peel the decorator (it declares a ConditionalWritesResolveTarget),
// so before the gate every one of these carriers wrote straight to the engine
// and left a stale gc.session_name / gc.work_dir behind a re-route.

// guardedMem is a MemStore that also offers the assignment-guarded update, the
// way BdStore does. MemStore has no UpdateIfAssignment of its own.
type guardedMem struct{ *beads.MemStore }

func (g guardedMem) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	cur, err := g.Get(id)
	if err != nil {
		return false, err
	}
	if cur.Status != expectedStatus || cur.Assignee != expectedAssignee {
		return false, nil
	}
	if err := g.Update(id, opts); err != nil {
		return false, err
	}
	return true, nil
}

// targetingStore stands in for the cmd/gc policy store and the typed class
// wrappers: it embeds the Store interface (so optional capabilities are not
// promoted) and declares the store a resolver should look through to.
type targetingStore struct {
	beads.Store
	target beads.Store
}

func (s targetingStore) ConditionalWritesResolveTarget() beads.Store { return s.target }

// seamShape is one way the decorator is stacked in production.
type seamShape struct {
	name string
	// open stacks the shape over an already-seeded engine and returns what a
	// caller holds. A cache in the stack is primed after the seed.
	open func(t *testing.T, e guardedMem) beads.Store
}

func newCache(t *testing.T, backing beads.Store) *beads.CachingStore {
	t.Helper()
	c := beads.NewCachingStoreForTest(backing, nil)
	if err := c.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return c
}

func seamShapes() []seamShape {
	return []seamShape{
		{"decorator over engine", func(_ *testing.T, e guardedMem) beads.Store {
			return beads.WithRouteChangeClearing(e, identityNormalizer)
		}},
		// Production shape 1: the CLI work store, decorator over the policy store.
		{"decorator over targeter over engine", func(_ *testing.T, e guardedMem) beads.Store {
			return beads.WithRouteChangeClearing(targetingStore{Store: e, target: e}, identityNormalizer)
		}},
		// Production shape 3: the controller binding, cache over decorator.
		{"cache over decorator over engine", func(t *testing.T, e guardedMem) beads.Store {
			return newCache(t, beads.WithRouteChangeClearing(e, identityNormalizer))
		}},
		// Production shape 2: the controller work ledger, policy over cache over decorator.
		{"targeter over cache over decorator over engine", func(t *testing.T, e guardedMem) beads.Store {
			c := newCache(t, beads.WithRouteChangeClearing(e, identityNormalizer))
			return targetingStore{Store: c, target: c}
		}},
		// The decorator outermost over a cache (the shape the lane test also runs).
		{"decorator over cache over engine", func(t *testing.T, e guardedMem) beads.Store {
			return beads.WithRouteChangeClearing(newCache(t, e), identityNormalizer)
		}},
	}
}

// seamCarrier is one capability entry point that can land a write on the engine.
type seamCarrier struct {
	name string
	// write attempts to change key from `from` to `to` on bead id through the
	// carrier. stale makes the carrier's own precondition fail. landed reports
	// whether the write took effect; an error is a transport failure.
	write func(t *testing.T, store beads.Store, engine guardedMem, id, key, from, to string, stale bool) (landed bool, err error)
}

func casLanded(w beads.MetadataCASWriter, ok bool, t *testing.T, id, key, from, to string, stale bool) (bool, error) {
	t.Helper()
	if !ok {
		return false, errUnresolved
	}
	if stale {
		from = "stale-" + from
	}
	return w.CompareAndSetMetadataKey(id, key, from, to)
}

func fencedLanded(w beads.ConditionalWriter, ok bool, t *testing.T, engine guardedMem, id, key, to string, stale bool) (bool, error) {
	t.Helper()
	if !ok {
		return false, errUnresolved
	}
	cur, err := engine.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rev := cur.Revision
	if stale {
		rev += 1000
	}
	if err := w.UpdateIfMatch(id, rev, beads.UpdateOpts{Metadata: map[string]string{key: to}}); err != nil {
		var pf *beads.PreconditionFailedError
		if errors.As(err, &pf) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func seamCarriers() []seamCarrier {
	return []seamCarrier{
		{"MetadataCASWriterFor", func(t *testing.T, s beads.Store, _ guardedMem, id, key, from, to string, stale bool) (bool, error) {
			w, ok := beads.MetadataCASWriterFor(s)
			return casLanded(w, ok, t, id, key, from, to, stale)
		}},
		{"As[MetadataCASWriter]", func(t *testing.T, s beads.Store, _ guardedMem, id, key, from, to string, stale bool) (bool, error) {
			w, ok := beads.As[beads.MetadataCASWriter](s)
			return casLanded(w, ok, t, id, key, from, to, stale)
		}},
		{"ApplyMetadataCAS", func(_ *testing.T, s beads.Store, _ guardedMem, id, key, from, to string, stale bool) (bool, error) {
			if stale {
				from = "stale-" + from
			}
			out, err := beads.ApplyMetadataCAS(s, id, key, from, to)
			return out == beads.MetadataCASSwapped, err
		}},
		{"ConditionalWriterForTarget.CAS", func(t *testing.T, s beads.Store, _ guardedMem, id, key, from, to string, stale bool) (bool, error) {
			w, ok := beads.ConditionalWriterForTarget(s)
			return casLanded(w, ok, t, id, key, from, to, stale)
		}},
		{"ConditionalWriterForTarget.UpdateIfMatch", func(t *testing.T, s beads.Store, e guardedMem, id, key, _, to string, stale bool) (bool, error) {
			w, ok := beads.ConditionalWriterForTarget(s)
			return fencedLanded(w, ok, t, e, id, key, to, stale)
		}},
		{"As[ConditionalWriter].CAS", func(t *testing.T, s beads.Store, _ guardedMem, id, key, from, to string, stale bool) (bool, error) {
			w, ok := beads.As[beads.ConditionalWriter](s)
			return casLanded(w, ok, t, id, key, from, to, stale)
		}},
		{"As[ConditionalWriter].UpdateIfMatch", func(t *testing.T, s beads.Store, e guardedMem, id, key, _, to string, stale bool) (bool, error) {
			w, ok := beads.As[beads.ConditionalWriter](s)
			return fencedLanded(w, ok, t, e, id, key, to, stale)
		}},
		{"ResolveConditionalWriter.CAS", func(t *testing.T, s beads.Store, _ guardedMem, id, key, from, to string, stale bool) (bool, error) {
			w, _, err := beads.ResolveConditionalWriter(s)
			if err != nil {
				t.Fatalf("ResolveConditionalWriter: %v", err)
			}
			return casLanded(w, w != nil, t, id, key, from, to, stale)
		}},
		{"ResolveConditionalWriter.UpdateIfMatch", func(t *testing.T, s beads.Store, e guardedMem, id, key, _, to string, stale bool) (bool, error) {
			w, _, err := beads.ResolveConditionalWriter(s)
			if err != nil {
				t.Fatalf("ResolveConditionalWriter: %v", err)
			}
			return fencedLanded(w, w != nil, t, e, id, key, to, stale)
		}},
		{"AssignmentGuardedUpdaterFor", func(t *testing.T, s beads.Store, e guardedMem, id, key, _, to string, stale bool) (bool, error) {
			w, ok := beads.AssignmentGuardedUpdaterFor(s)
			if !ok {
				return false, errUnresolved
			}
			cur, err := e.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			assignee := cur.Assignee
			if stale {
				assignee = "someone-else"
			}
			return w.UpdateIfAssignment(id, cur.Status, assignee, beads.UpdateOpts{Metadata: map[string]string{key: to}})
		}},
		{"As[AssignmentGuardedUpdater]", func(t *testing.T, s beads.Store, e guardedMem, id, key, _, to string, stale bool) (bool, error) {
			w, ok := beads.As[beads.AssignmentGuardedUpdater](s)
			if !ok {
				return false, errUnresolved
			}
			cur, err := e.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			assignee := cur.Assignee
			if stale {
				assignee = "someone-else"
			}
			return w.UpdateIfAssignment(id, cur.Status, assignee, beads.UpdateOpts{Metadata: map[string]string{key: to}})
		}},
	}
}

const seamOtherKey = "gc.seam_note"

// errUnresolved reports that a carrier's capability lookup found no writer.
var errUnresolved = errors.New("capability did not resolve")

// newSeamEngine is the shared engine double: a MemStore with the
// assignment-guarded update, stamped conditional_writes=require so
// ResolveConditionalWriter resolves it (a MemStore carries no mode until
// stamped).
func newSeamEngine(t *testing.T) guardedMem {
	t.Helper()
	e := guardedMem{beads.NewMemStore()}
	if err := beads.StampOpenedStore(e, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatalf("StampOpenedStore: %v", err)
	}
	return e
}

// seamOpaque reports the (shape, carrier) pairs whose lookup is non-peeling
// by design: As[ConditionalWriter] resolves through ConditionalWriterFor,
// which never looks through a wrapper that merely declares a resolve target,
// so a targeter hides the writer. The tests assert that, so the gate cannot
// change it unnoticed.
func seamOpaque(shape seamShape, carrier seamCarrier) bool {
	return strings.Contains(shape.name, "targeter") && strings.HasPrefix(carrier.name, "As[ConditionalWriter]")
}

// seamAttempt lands one write through the carrier under test and reports
// whether it landed.
type seamAttempt func(key, from, to string, stale bool) bool

// forEachSeam runs run once per (shape, carrier) over a freshly seeded,
// routed bead. Pairs whose lookup is opaque by design (seamOpaque) are
// asserted unresolved and skipped.
func forEachSeam(t *testing.T, run func(t *testing.T, engine guardedMem, id string, attempt seamAttempt)) {
	t.Helper()
	for _, shape := range seamShapes() {
		for _, carrier := range seamCarriers() {
			t.Run(shape.name+"/"+carrier.name, func(t *testing.T) {
				engine := newSeamEngine(t)
				b := seedRoutedBead(t, engine, testOldTarget)
				top := shape.open(t, engine)
				opaque := seamOpaque(shape, carrier)
				attempt := func(key, from, to string, stale bool) bool {
					landed, err := carrier.write(t, top, engine, b.ID, key, from, to, stale)
					switch {
					case errors.Is(err, errUnresolved) && opaque:
						t.Skipf("%s is non-peeling and %s hides it, as designed", carrier.name, shape.name)
					case errors.Is(err, errUnresolved):
						t.Fatalf("%s did not resolve through %s", carrier.name, shape.name)
					case opaque:
						t.Fatalf("%s resolved through %s, but the lookup is non-peeling by design", carrier.name, shape.name)
					case err != nil:
						t.Fatalf("write: %v", err)
					}
					return landed
				}
				run(t, engine, b.ID, attempt)
			})
		}
	}
}

func routedTo(t *testing.T, s beads.Store, id string) string {
	t.Helper()
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got.Metadata[beadmeta.RoutedToMetadataKey]
}

// A genuine re-route that lands through any capability clears the stamps.
func TestRouteClearSeam_GenuineRerouteThroughCapabilityClearsStamps(t *testing.T) {
	forEachSeam(t, func(t *testing.T, engine guardedMem, id string, attempt seamAttempt) {
		if !attempt(beadmeta.RoutedToMetadataKey, testOldTarget, testNewTarget, false) {
			t.Fatalf("the re-route did not land")
		}
		if got := routedTo(t, engine, id); got != testNewTarget {
			t.Errorf("gc.routed_to = %q, want %q", got, testNewTarget)
		}
		assertStampsCleared(t, engine, id)
	})
}

// A write that lost its race changed nothing, so it must clear nothing.
func TestRouteClearSeam_LostWriteLeavesStampsIntact(t *testing.T) {
	forEachSeam(t, func(t *testing.T, engine guardedMem, id string, attempt seamAttempt) {
		if attempt(beadmeta.RoutedToMetadataKey, testOldTarget, testNewTarget, true) {
			t.Fatalf("the stale write landed")
		}
		if got := routedTo(t, engine, id); got != testOldTarget {
			t.Errorf("gc.routed_to = %q, want it untouched (%q)", got, testOldTarget)
		}
		assertStampsIntact(t, engine, id)
	})
}

// A capability write to any other key is a straight pass-through.
func TestRouteClearSeam_NonRoutedToWriteLeavesStampsIntact(t *testing.T) {
	forEachSeam(t, func(t *testing.T, engine guardedMem, id string, attempt seamAttempt) {
		if !attempt(seamOtherKey, "", "x", false) {
			t.Fatalf("the write to %s did not land", seamOtherKey)
		}
		assertStampsIntact(t, engine, id)
		if got := routedTo(t, engine, id); got != testOldTarget {
			t.Errorf("gc.routed_to = %q, want %q", got, testOldTarget)
		}
	})
}

// The same target written back is not a re-route. A CAS onto the value
// already present reports not-swapped; the fenced and guarded updates land.
// Either way nothing is cleared.
func TestRouteClearSeam_SameTargetThroughCapabilityIsNoOp(t *testing.T) {
	forEachSeam(t, func(t *testing.T, engine guardedMem, id string, attempt seamAttempt) {
		attempt(beadmeta.RoutedToMetadataKey, testOldTarget, testOldTarget, false)
		assertStampsIntact(t, engine, id)
	})
}

// getFailsOnceStore fails its first Get, standing in for a pre-read that
// cannot be served.
type getFailsOnceStore struct {
	guardedMem
	failed bool
}

func (s *getFailsOnceStore) Get(id string) (beads.Bead, error) {
	if !s.failed {
		s.failed = true
		return beads.Bead{}, errors.New("injected pre-read failure")
	}
	return s.guardedMem.Get(id)
}

// A pre-read that fails lets the write proceed with no clear, and says so.
func TestRouteClearSeam_PreReadFailureWritesWithoutClearAndLogs(t *testing.T) {
	logs := captureLog(t)
	mem := newSeamEngine(t)
	b := seedRoutedBead(t, mem, testOldTarget)
	engine := &getFailsOnceStore{guardedMem: mem}
	store := beads.WithRouteChangeClearing(engine, identityNormalizer)

	w, ok := beads.MetadataCASWriterFor(store)
	if !ok {
		t.Fatal("metadata CAS capability did not resolve")
	}
	swapped, err := w.CompareAndSetMetadataKey(b.ID, beadmeta.RoutedToMetadataKey, testOldTarget, testNewTarget)
	if err != nil || !swapped {
		t.Fatalf("CAS = (%v, %v), want the write to proceed", swapped, err)
	}
	if got := routedTo(t, mem, b.ID); got != testNewTarget {
		t.Errorf("gc.routed_to = %q, want %q", got, testNewTarget)
	}
	assertStampsIntact(t, mem, b.ID)
	if !strings.Contains(logs.String(), b.ID) {
		t.Errorf("want one log line naming %s for the skipped clear; log:\n%s", b.ID, logs.String())
	}
}

// failingClearStore fails the batch write the decorator's clear uses.
type failingClearStore struct{ guardedMem }

func (s failingClearStore) SetMetadataBatch(string, map[string]string) error {
	return errors.New("injected clear failure")
}

// A failed clear never turns a landed re-route into an error.
func TestRouteClearSeam_FailedClearDoesNotFailLandedWrite(t *testing.T) {
	logs := captureLog(t)
	mem := newSeamEngine(t)
	b := seedRoutedBead(t, mem, testOldTarget)
	store := beads.WithRouteChangeClearing(failingClearStore{mem}, identityNormalizer)

	w, ok := beads.MetadataCASWriterFor(store)
	if !ok {
		t.Fatal("metadata CAS capability did not resolve")
	}
	swapped, err := w.CompareAndSetMetadataKey(b.ID, beadmeta.RoutedToMetadataKey, testOldTarget, testNewTarget)
	if err != nil || !swapped {
		t.Fatalf("CAS = (%v, %v), want a landed write with no error", swapped, err)
	}
	if got := routedTo(t, mem, b.ID); got != testNewTarget {
		t.Errorf("gc.routed_to = %q, want %q", got, testNewTarget)
	}
	if !strings.Contains(logs.String(), "injected clear failure") {
		t.Errorf("want the failed clear logged; log:\n%s", logs.String())
	}
}

// The gate must not leak into the decorator's structure: it stays a
// non-conditional wrapper that declares its target.
func TestRouteClearSeam_DecoratorIsNotAStructuralWriter(t *testing.T) {
	store := beads.WithRouteChangeClearing(guardedMem{beads.NewMemStore()}, identityNormalizer)
	if _, ok := store.(beads.ConditionalWriter); ok {
		t.Error("the decorator must not satisfy ConditionalWriter structurally")
	}
	if _, ok := store.(beads.MetadataCASWriter); ok {
		t.Error("the decorator must not satisfy MetadataCASWriter structurally")
	}
	if _, ok := store.(beads.AssignmentGuardedUpdater); ok {
		t.Error("the decorator must not satisfy AssignmentGuardedUpdater structurally")
	}
	if _, ok := store.(beads.ConditionalWritesResolveTargeter); !ok {
		t.Error("the decorator must keep declaring its conditional-writes resolve target")
	}
}

// The CachingStore's own writer methods reach the engine through the gate.
func TestRouteClearSeam_CachingStoreWriterMethodsClearStamps(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, c *beads.CachingStore, e guardedMem, id string) (bool, error)
	}{
		{"CompareAndSetMetadataKey", func(_ *testing.T, c *beads.CachingStore, _ guardedMem, id string) (bool, error) {
			return c.CompareAndSetMetadataKey(id, beadmeta.RoutedToMetadataKey, testOldTarget, testNewTarget)
		}},
		{"UpdateIfMatch", func(t *testing.T, c *beads.CachingStore, e guardedMem, id string) (bool, error) {
			return fencedLanded(c, true, t, e, id, beadmeta.RoutedToMetadataKey, testNewTarget, false)
		}},
		{"UpdateIfAssignment", func(t *testing.T, c *beads.CachingStore, e guardedMem, id string) (bool, error) {
			cur, err := e.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			return c.UpdateIfAssignment(id, cur.Status, cur.Assignee, beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: testNewTarget}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newSeamEngine(t)
			b := seedRoutedBead(t, engine, testOldTarget)
			cache := newCache(t, beads.WithRouteChangeClearing(engine, identityNormalizer))
			landed, err := tc.write(t, cache, engine, b.ID)
			if err != nil || !landed {
				t.Fatalf("write = (%v, %v), want a landed write", landed, err)
			}
			assertStampsCleared(t, engine, b.ID)
		})
	}
}
