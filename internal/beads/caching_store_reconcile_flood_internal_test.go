package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
)

// floodLoopHarness wires a cache to the controller's real event topology: every
// reconcile emission is recorded on the bus and then fed back into ApplyEvent on
// the emitting store (cmd/gc/api_state.go startBeadEventWatcher ->
// applyBeadEventToStores). Delivery is deferred to after the pass because the
// bus is asynchronous; applying inline would deadlock on the reconcile lock and
// would not match production ordering anyway.
type floodLoopHarness struct {
	cache   *CachingStore
	pending []cacheFloodEvent
	emitted []string
}

type cacheFloodEvent struct {
	eventType string
	payload   json.RawMessage
}

func newFloodLoopHarness(t *testing.T, backing Store) *floodLoopHarness {
	t.Helper()
	h := newUnprimedFloodLoopHarness(backing)
	if err := h.cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return h
}

// newUnprimedFloodLoopHarness leaves priming to the test, for the boot paths.
func newUnprimedFloodLoopHarness(backing Store) *floodLoopHarness {
	h := &floodLoopHarness{}
	h.cache = NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		h.emitted = append(h.emitted, eventType+":"+beadID)
		h.pending = append(h.pending, cacheFloodEvent{eventType: eventType, payload: payload})
	})
	return h
}

// pass runs one reconcile cycle and then delivers everything it emitted back to
// the cache, the way the controller's watcher does.
func (h *floodLoopHarness) pass() {
	h.cache.runReconciliation()
	h.deliver()
}

// deliver feeds everything the cache emitted since the last delivery, write
// echoes included, back through the snapshot entry point. The controller routes
// cache-actored events (cache-reconcile, cache-local) there.
func (h *floodLoopHarness) deliver() {
	pending := h.pending
	h.pending = nil
	for _, evt := range pending {
		h.cache.ApplyEventSnapshot(evt.eventType, evt.payload)
	}
}

// TestReconcileEmissionFedBackDoesNotSustainFloodLoop pins the fix for the
// cache-reconcile event flood (a rig emitting bead.updated for every open bead,
// every pass, forever — 2k+ events/minute against an unchanging backing store).
//
// The loop is closed by the cache re-absorbing its own reconcile emission:
//
//  1. Reconcile absorbs a row with an explicit dependency snapshot and an
//     is_blocked verdict, then emits bead.updated carrying that full snapshot.
//  2. Bead.Dependencies and Bead.Needs are `omitempty`, so a bead with no
//     dependencies marshals with neither key. The payload is now
//     indistinguishable from a bd on_update hook patch, which legitimately omits
//     dependencies after a removal.
//  3. ApplyEvent therefore treats dependency coverage as unknown: it drops the
//     cached dep set, clears the is_blocked verdict, clears depsComplete
//     store-wide, and reports a mutation.
//  4. The mutation stamps a sequence number, which fences the row out of the
//     next pass' absorb, so the cleared verdict is never repaired.
//  5. The pass after that absorbs again, sees cached is_blocked nil against a
//     fresh &false, calls that a change, and emits — returning to step 1.
//
// A dependency-free bead that nothing is writing to must go quiet.
func TestReconcileEmissionFedBackDoesNotSustainFloodLoop(t *testing.T) {
	mem := NewMemStore()
	unblocked := false
	subject, err := mem.Create(Bead{Title: "backlog issue", IsBlocked: &unblocked})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := newFloodLoopHarness(t, mem)
	h.pass()
	h.emitted = nil

	// Seed exactly one real divergence, the way any external writer would. Every
	// emission after the pass that reports it is the cache talking to itself.
	edited := "backlog issue (edited)"
	if err := mem.Update(subject.ID, UpdateOpts{Title: &edited}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	h.pass()
	seeded := len(h.emitted)
	if seeded == 0 {
		t.Fatalf("reconcile did not report the seeded title change; harness is not exercising the emit path")
	}
	h.emitted = nil

	for i := 0; i < 5; i++ {
		h.pass()
	}
	if len(h.emitted) != 0 {
		t.Fatalf("reconcile kept emitting for an unchanging bead after the seeded change settled: %v", h.emitted)
	}
}

// TestReconcileEmissionFedBackPreservesReadyVerdict isolates the state damage
// behind the loop above. Re-absorbing the cache's own snapshot must not nil out
// the is_blocked verdict reconcile just installed, and must not clear the
// store-wide depsComplete latch, which silently downgrades the reconcile
// dependency comparison for every other bead in the rig.
func TestReconcileEmissionFedBackPreservesReadyVerdict(t *testing.T) {
	mem := NewMemStore()
	unblocked := false
	subject, err := mem.Create(Bead{Title: "backlog issue", IsBlocked: &unblocked})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	h := newFloodLoopHarness(t, mem)
	h.pass()

	h.cache.mu.RLock()
	cached, ok := h.cache.beads[subject.ID]
	depsComplete := h.cache.depsComplete
	h.cache.mu.RUnlock()

	if !ok {
		t.Fatalf("bead %s dropped from cache", subject.ID)
	}
	if cached.IsBlocked == nil {
		t.Fatalf("re-absorbing the cache's own reconcile snapshot nilled %s's is_blocked verdict", subject.ID)
	}
	if !depsComplete {
		t.Fatalf("re-absorbing the cache's own reconcile snapshot cleared the store-wide depsComplete latch")
	}
}

// TestCloseAllEchoFedBackKeepsVerdictsAndReconcileQuiet pins the binding-cache
// flood (every reconcile re-announcing ~14.7k rows). The controller closes an
// edge-less row with CloseAll (orders.Store.CloseRuns), which keeps the closed
// row but drops its deps entry; its own bead.closed comes back through the
// watcher with no dependencies key (omitempty). That echo must not clear the
// store-wide depsComplete, wipe every other row's ready verdict, or make the
// next reconcile re-announce rows nothing touched.
func TestCloseAllEchoFedBackKeepsVerdictsAndReconcileQuiet(t *testing.T) {
	mem := NewMemStore()
	unblocked := false
	var ids []string
	for i := 0; i < 20; i++ {
		b, err := mem.Create(Bead{Title: fmt.Sprintf("row %d", i), IsBlocked: &unblocked})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}
	h := newFloodLoopHarness(t, mem)
	h.pass()
	if !h.cache.depsComplete {
		t.Fatalf("precondition: depsComplete=false after an idle pass")
	}

	if _, err := h.cache.CloseAll([]string{ids[0]}, map[string]string{"close_reason": "done"}); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	h.deliver()

	if !h.cache.depsComplete || h.cache.Stats().DepsCoverageLost != 0 {
		t.Fatalf("the cache's own CloseAll echo cleared the store-wide depsComplete")
	}
	for _, id := range ids[1:] {
		if h.cache.beads[id].IsBlocked == nil {
			t.Fatalf("the CloseAll echo wiped the ready verdict of untouched row %s", id)
		}
	}
	h.emitted = nil
	h.pass()
	if len(h.emitted) != 0 {
		t.Fatalf("reconcile re-announced %d rows nothing touched: %v", len(h.emitted), h.emitted)
	}
}

// TestEchoedWritesLeaveIdleReconcileQuiet is the property behind the test
// above: whatever sequence of writes the controller makes through its cache,
// with every echo fed back, the next reconcile over an unchanged backing emits
// nothing. The written rows carry no verdict, so nothing about them is
// legitimately re-stamped; the bystanders carry one, so an emission is a wiped
// verdict.
func TestEchoedWritesLeaveIdleReconcileQuiet(t *testing.T) {
	for seed := int64(1); seed <= 64; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			mem := NewMemStore()
			unblocked := false
			var bystanders, work []string
			for i := 0; i < 8; i++ {
				b, err := mem.Create(Bead{Title: fmt.Sprintf("bystander %d", i), IsBlocked: &unblocked})
				if err != nil {
					t.Fatalf("Create bystander: %v", err)
				}
				bystanders = append(bystanders, b.ID)
			}
			for i := 0; i < 4; i++ {
				b, err := mem.Create(Bead{Title: fmt.Sprintf("work %d", i), Type: "task"})
				if err != nil {
					t.Fatalf("Create work: %v", err)
				}
				work = append(work, b.ID)
			}
			h := newFloodLoopHarness(t, mem)
			h.pass()

			pick := func() string { return work[rng.Intn(len(work))] }
			var trace []string
			for step := 0; step < 12; step++ {
				var err error
				switch op := rng.Intn(6); op {
				case 0:
					var b Bead
					b, err = h.cache.Create(Bead{Title: fmt.Sprintf("created %d", step), Type: "task"})
					work = append(work, b.ID)
					trace = append(trace, "create")
				case 1:
					id, title := pick(), fmt.Sprintf("edited %d", step)
					err = h.cache.Update(id, UpdateOpts{Title: &title})
					trace = append(trace, "update "+id)
				case 2:
					id := pick()
					err = h.cache.Close(id)
					trace = append(trace, "close "+id)
				case 3:
					a, b := pick(), pick()
					_, err = h.cache.CloseAll([]string{a, b}, map[string]string{"close_reason": "done"})
					trace = append(trace, "closeall "+a+","+b)
				case 4:
					a, b := pick(), pick()
					if a != b {
						err = h.cache.DepAdd(a, b, "blocks")
						trace = append(trace, "depadd "+a+"->"+b)
					}
				case 5:
					a, b := pick(), pick()
					err = h.cache.DepRemove(a, b)
					trace = append(trace, "depremove "+a+"->"+b)
				}
				if err != nil {
					t.Fatalf("step %d (%v): %v", step, trace, err)
				}
				if rng.Intn(2) == 0 {
					h.deliver()
				}
			}
			h.deliver()

			if !h.cache.depsComplete {
				t.Fatalf("echoed writes %v cleared depsComplete", trace)
			}
			for _, id := range bystanders {
				if h.cache.beads[id].IsBlocked == nil {
					t.Fatalf("echoed writes %v wiped bystander %s's verdict", trace, id)
				}
			}
			h.emitted = nil
			h.pass()
			if len(h.emitted) != 0 {
				t.Fatalf("idle reconcile after echoed writes %v emitted %v", trace, h.emitted)
			}
		})
	}
}

// listHookSQLiteStore is a SQLite engine that runs afterList once, after the
// next List returns, to land a write inside a scan. Embedding keeps every
// capability the cache asserts for, unexported ones included.
type listHookSQLiteStore struct {
	*SQLiteStore
	afterList func()
}

func (s *listHookSQLiteStore) List(q ListQuery) ([]Bead, error) {
	out, err := s.SQLiteStore.List(q)
	if f := s.afterList; f != nil {
		s.afterList = nil
		f()
	}
	return out, err
}

// newVerdictSQLite opens a SQLite engine, whose rows carry their edges and an
// is_blocked verdict, holding n edge-less rows and a blocker with a step it
// blocks, so a wipe is visible on both verdicts.
func newVerdictSQLite(t *testing.T, n int) (*listHookSQLiteStore, []string) {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	engine := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = engine.CloseStore() })
	var ids []string
	for i := 0; i < n; i++ {
		b, err := engine.Create(Bead{Title: fmt.Sprintf("row %d", i), Type: "task"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		ids = append(ids, b.ID)
	}
	blocker, err := engine.Create(Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	step, err := engine.Create(Bead{Title: "step", Type: "task"})
	if err != nil {
		t.Fatalf("Create step: %v", err)
	}
	if err := engine.DepAdd(step.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	return &listHookSQLiteStore{SQLiteStore: engine}, append(ids, blocker.ID, step.ID)
}

// externalSnapshot is the payload a one-shot CLI's store records for b: the
// snapshot with an explicit empty dependencies key.
func externalSnapshot(t *testing.T, store Store, id string) json.RawMessage {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	payload, err := EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatalf("EncodeBeadEventPayload: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := fields["dependencies"]; !ok {
		fields["dependencies"] = json.RawMessage("[]")
	}
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return out
}

// assertVerdictsKept fails when any of ids lost its ready verdict.
func (h *floodLoopHarness) assertVerdictsKept(t *testing.T, when string, ids []string) {
	t.Helper()
	h.cache.mu.RLock()
	defer h.cache.mu.RUnlock()
	var wiped []string
	for _, id := range ids {
		if h.cache.beads[id].IsBlocked == nil {
			wiped = append(wiped, id)
		}
	}
	if len(wiped) > 0 {
		t.Fatalf("%s wiped the ready verdict of %d of %d untouched rows: %v", when, len(wiped), len(ids), wiped)
	}
}

// TestSQLiteBootPrimeRacedByAWriteKeepsVerdicts covers the restart flood: a
// write landing during the boot Prime's listing sends it down the non-replace
// branch, which leaves depsComplete false. The first status write after that
// must still invalidate only the rows that depend on the written one, because
// a SQLite cache's deps entries came from rows carrying their edges.
func TestSQLiteBootPrimeRacedByAWriteKeepsVerdicts(t *testing.T) {
	engine, ids := newVerdictSQLite(t, 20)
	h := newUnprimedFloodLoopHarness(engine)
	if err := h.cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	engine.afterList = func() {
		if err := h.cache.SetMetadata(ids[0], "touched", "during-prime"); err != nil {
			t.Errorf("SetMetadata: %v", err)
		}
	}
	if err := h.cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if h.cache.depsComplete {
		t.Fatalf("precondition: the raced Prime took the replace branch")
	}
	h.deliver()

	open := "open"
	if err := h.cache.Update(ids[1], UpdateOpts{Status: &open}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.deliver()
	h.assertVerdictsKept(t, "a status write after the raced boot Prime", ids[2:])

	h.emitted = nil
	h.pass()
	if len(h.emitted) != 0 {
		t.Fatalf("first reconcile after the raced boot Prime re-announced %d rows: %v", len(h.emitted), h.emitted)
	}
}

// TestSQLiteMidScanFenceKeepsVerdicts covers the other way a reconcile ends
// with depsComplete false: an event lands mid-scan on an event-created,
// edge-less row, which holds no deps entry, so the pass skips it as fenced and
// counts a coverage hole. The next status write must not wipe every verdict.
func TestSQLiteMidScanFenceKeepsVerdicts(t *testing.T) {
	engine, ids := newVerdictSQLite(t, 20)
	h := newFloodLoopHarness(t, engine)
	h.pass()

	external, err := engine.Create(Bead{Title: "external", Type: "task"})
	if err != nil {
		t.Fatalf("Create external: %v", err)
	}
	h.cache.ApplyEventSnapshot("bead.created", externalSnapshot(t, engine, external.ID))
	engine.afterList = func() {
		title := "external (edited mid-scan)"
		if err := engine.Update(external.ID, UpdateOpts{Title: &title}); err != nil {
			t.Errorf("Update external: %v", err)
			return
		}
		h.cache.ApplyEventSnapshot("bead.updated", externalSnapshot(t, engine, external.ID))
	}
	h.pass()
	if h.cache.depsComplete {
		t.Fatalf("precondition: the fenced entry-less row did not degrade depsComplete")
	}
	if got := h.cache.Stats().DepsCoverageLost; got != 1 {
		t.Fatalf("DepsCoverageLost = %d after one degrading reconcile, want 1", got)
	}

	open := "open"
	if err := h.cache.Update(ids[1], UpdateOpts{Status: &open}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	h.deliver()
	h.assertVerdictsKept(t, "a status write after a mid-scan fence", ids)

	h.emitted = nil
	h.pass()
	for _, e := range h.emitted {
		// The entry-less row's verdict is the one the cache could not vouch
		// for; it alone may be re-stamped.
		if e != "bead.updated:"+external.ID {
			t.Fatalf("reconcile after a mid-scan fence re-announced rows nothing touched: %v", h.emitted)
		}
	}
}

// TestSQLiteCloseEchoOfEventCreatedRowKeepsVerdicts covers a close of a row the
// cache learned from an event: edge-less under complete coverage, it never got
// a deps entry, and the close echo carries no dependencies key.
func TestSQLiteCloseEchoOfEventCreatedRowKeepsVerdicts(t *testing.T) {
	engine, ids := newVerdictSQLite(t, 20)
	h := newFloodLoopHarness(t, engine)
	h.pass()

	external, err := engine.Create(Bead{Title: "external", Type: "message"})
	if err != nil {
		t.Fatalf("Create external: %v", err)
	}
	h.cache.ApplyEventSnapshot("bead.created", externalSnapshot(t, engine, external.ID))
	if err := h.cache.Close(external.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h.deliver()
	if !h.cache.depsComplete {
		t.Fatalf("the close echo of an event-created row cleared depsComplete")
	}
	h.assertVerdictsKept(t, "the close echo of an event-created row", ids)

	h.emitted = nil
	h.pass()
	if len(h.emitted) != 0 {
		t.Fatalf("reconcile after the close echo re-announced %d rows: %v", len(h.emitted), h.emitted)
	}
}

// TestSQLiteIncompleteCoverageStillInvalidatesDependents pins what the bounded
// invalidation keeps: with depsComplete false, closing a blocker still clears
// the verdict of a row whose entry depends on it and of every row with no entry,
// whose edges the cache cannot see. Only rows whose entries prove them
// independent keep theirs.
func TestSQLiteIncompleteCoverageStillInvalidatesDependents(t *testing.T) {
	engine, ids := newVerdictSQLite(t, 2)
	bystander, unknown, blocker, step := ids[0], ids[1], ids[2], ids[3]
	h := newFloodLoopHarness(t, engine)
	h.pass()

	h.cache.mu.Lock()
	delete(h.cache.deps, unknown)
	h.cache.depsComplete = false
	h.cache.mu.Unlock()

	if err := h.cache.Close(blocker); err != nil {
		t.Fatalf("Close blocker: %v", err)
	}
	h.cache.mu.RLock()
	defer h.cache.mu.RUnlock()
	if h.cache.beads[step].IsBlocked != nil {
		t.Fatalf("step kept its verdict after its blocker closed")
	}
	if h.cache.beads[unknown].IsBlocked != nil {
		t.Fatalf("a row with no deps entry kept its verdict under incomplete coverage")
	}
	if h.cache.beads[bystander].IsBlocked == nil {
		t.Fatalf("a row whose entry proves it independent lost its verdict")
	}
}
