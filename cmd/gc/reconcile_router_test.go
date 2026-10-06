package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

const routerTestLeg = "city:test"

// routerRecorder is a recording routerSink.
type routerRecorder struct {
	mu       sync.Mutex
	sessions []routedSession
	allocs   []routeReason
	resyncs  []string
}

type routedSession struct {
	key    rowKey
	reason routeReason
}

func (rec *routerRecorder) sink() routerSink {
	return routerSink{
		addSession: func(k rowKey, r routeReason) {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.sessions = append(rec.sessions, routedSession{k, r})
		},
		wakeAllocator: func(r routeReason) {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.allocs = append(rec.allocs, r)
		},
		requestResync: func(reason string) {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.resyncs = append(rec.resyncs, reason)
		},
	}
}

// take returns and clears what was recorded: the session IDs (sorted), the
// allocator reason kinds and the resync reasons.
func (rec *routerRecorder) take() (ids, allocs, resyncs []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, s := range rec.sessions {
		ids = append(ids, s.key.ID)
	}
	sort.Strings(ids)
	for _, a := range rec.allocs {
		allocs = append(allocs, a.Kind)
	}
	resyncs = rec.resyncs
	rec.sessions, rec.allocs, rec.resyncs = nil, nil, nil
	return ids, allocs, resyncs
}

func newTestRouter() (*reconcileRouter, *routerRecorder) {
	rec := &routerRecorder{}
	return newReconcileRouter(routerTestLeg, rec.sink(), nil), rec
}

func routerSessionBead(id string, meta map[string]string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	}
}

func routerWorkBead(id, status, assignee string) beads.Bead {
	return beads.Bead{ID: id, Type: "task", Status: status, Assignee: assignee}
}

func beadEvent(t *testing.T, eventType string, b beads.Bead) events.Event {
	t.Helper()
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatalf("encoding %s: %v", b.ID, err)
	}
	return events.Event{Type: eventType, Subject: b.ID, Payload: payload}
}

// memSessionCensus reads the open session rows of store the way the resync
// lane does: loadSessionBeadSnapshot through the session front door.
func memSessionCensus(store beads.Store) func() ([]session.Info, error) {
	return func() ([]session.Info, error) {
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			return nil, err
		}
		return snap.OpenInfos(), nil
	}
}

func memLegs(stores ...beads.Store) func() ([]classStoreCandidate, error) {
	return func() ([]classStoreCandidate, error) {
		out := make([]classStoreCandidate, len(stores))
		for i, s := range stores {
			out[i] = classStoreCandidate{store: s, ref: fmt.Sprintf("leg%d", i)}
		}
		return out, nil
	}
}

func mustRebuild(t *testing.T, r *reconcileRouter, c routerCensus) {
	t.Helper()
	if _, sessErr, legErr := r.rebuild(c); sessErr != nil || legErr != nil {
		t.Fatalf("rebuild: sessions %v, legs %v", sessErr, legErr)
	}
}

func wantStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// writeCountingStore counts every Store write, to prove the router never
// writes (INC-006).
type writeCountingStore struct {
	beads.Store
	mu     sync.Mutex
	writes int
}

func (s *writeCountingStore) count() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
}

func (s *writeCountingStore) Create(b beads.Bead) (beads.Bead, error) {
	s.count()
	return s.Store.Create(b)
}

func (s *writeCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.count()
	return s.Store.Update(id, opts)
}

func (s *writeCountingStore) Close(id string) error { s.count(); return s.Store.Close(id) }

func (s *writeCountingStore) Reopen(id string) error { s.count(); return s.Store.Reopen(id) }

func (s *writeCountingStore) Delete(id string) error { s.count(); return s.Store.Delete(id) }

func (s *writeCountingStore) CloseAll(ids []string, md map[string]string) (int, error) {
	s.count()
	return s.Store.CloseAll(ids, md)
}

func (s *writeCountingStore) SetMetadata(id, key, value string) error {
	s.count()
	return s.Store.SetMetadata(id, key, value)
}

func (s *writeCountingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.count()
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *writeCountingStore) SetLocalString(id, key, value string) error {
	s.count()
	return s.Store.SetLocalString(id, key, value)
}

func (s *writeCountingStore) Tx(msg string, fn func(tx beads.Tx) error) error {
	s.count()
	return s.Store.Tx(msg, fn)
}

func (s *writeCountingStore) DepAdd(issueID, dependsOnID, depType string) error {
	s.count()
	return s.Store.DepAdd(issueID, dependsOnID, depType)
}

func (s *writeCountingStore) DepRemove(issueID, dependsOnID string) error {
	s.count()
	return s.Store.DepRemove(issueID, dependsOnID)
}

// Kills: allocator not woken on a census change. Every bead event type, live
// or replayed, routes a session row in the sessions store to its own key plus
// the allocator, and keeps the identity index in step.
func TestRouterSessionBeadEventEnqueuesItsRowAndAllocator(t *testing.T) {
	for _, evtType := range []string{events.BeadCreated, events.BeadUpdated, events.BeadClosed, events.BeadDeleted} {
		for _, snapshot := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/snapshot=%v", evtType, snapshot), func(t *testing.T) {
				r, rec := newTestRouter()
				sb := routerSessionBead("s-1", map[string]string{"session_name": "worker-1"})
				if evtType == events.BeadClosed {
					sb.Status = "closed"
				}
				r.OnBeadEvent(beadEvent(t, evtType, sb), snapshot, true)

				if got := rec.sessionsLeg(); got != routerTestLeg {
					t.Fatalf("session key leg = %q, want %q", got, routerTestLeg)
				}
				ids, allocs, resyncs := rec.take()
				kind := routeReasonEvent
				if snapshot {
					kind = routeReasonReplay
				}
				wantStrings(t, "session keys", ids, []string{"s-1"})
				wantStrings(t, "allocator wakes", allocs, []string{kind})
				wantStrings(t, "resyncs", resyncs, nil)

				// The name resolves while the row is open, never after it closed.
				r.Enqueue("socket", reconcilekey.SessionNamed("worker-1"))
				ids, _, _ = rec.take()
				gone := evtType == events.BeadClosed || evtType == events.BeadDeleted
				if resolved := len(ids) == 1; resolved == gone {
					t.Fatalf("name resolved to %q after %s, want resolved=%v", ids, evtType, !gone)
				}
			})
		}
	}
}

func (rec *routerRecorder) sessionsLeg() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.sessions) == 0 {
		return ""
	}
	return rec.sessions[len(rec.sessions)-1].key.Leg
}

// Kills: replays ignored (the F1 regression). A cache-reconcile replay routes
// exactly like the live event, under reason kind "replay", is counted apart,
// and nothing the router does ever writes a store (INC-006, API-002).
func TestRouterReplayRoutesEnqueueOnlyAndNeverWrites(t *testing.T) {
	sessions := &writeCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		routerSessionBead("s-a", map[string]string{"session_name": "worker-a"}),
		routerSessionBead("s-b", map[string]string{"session_name": "worker-b"}),
	}, nil)}
	work := &writeCountingStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		routerWorkBead("w-1", "in_progress", "worker-a"),
	}, nil)}
	r, rec := newTestRouter()
	mustRebuild(t, r, routerCensus{sessions: memSessionCensus(sessions), legs: memLegs(work)})
	rec.take()

	r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerWorkBead("w-1", "in_progress", "worker-b")), true, false)
	r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})), true, true)

	ids, allocs, _ := rec.take()
	wantStrings(t, "session keys", ids, []string{"s-a", "s-a", "s-b"})
	wantStrings(t, "allocator wakes", allocs, []string{routeReasonReplay, routeReasonReplay})
	rec.mu.Lock()
	for _, s := range rec.sessions {
		if s.reason.Kind != routeReasonReplay || s.reason.Urgent {
			t.Errorf("session %s reason = %+v, want non-urgent replay", s.key.ID, s.reason)
		}
	}
	rec.mu.Unlock()
	if st := r.stats(); st.Replays != 2 || st.Events != 0 {
		t.Fatalf("stats = %+v, want 2 replays and 0 events", st)
	}
	mustRebuild(t, r, routerCensus{sessions: memSessionCensus(sessions), legs: memLegs(work)})
	if sessions.writes != 0 || work.writes != 0 {
		t.Fatalf("store writes = sessions %d, work %d, want 0", sessions.writes, work.writes)
	}
}

// Kills: only the new assignee enqueued; only session_name resolved. Moving
// work between sessions wakes both, through every identity form a session
// answers to.
func TestRouterWorkReassignmentEnqueuesOldAndNewAssignee(t *testing.T) {
	sa := routerSessionBead("s-a", map[string]string{
		"session_name":              "worker-a",
		"configured_named_identity": "named-a",
		"alias":                     "alias-a",
		"alias_history":             "old-a",
	})
	sb := routerSessionBead("s-b", map[string]string{
		"session_name":              "worker-b",
		"configured_named_identity": "named-b",
		"alias":                     "alias-b",
		"alias_history":             "old-b",
	})
	forms := []struct{ name, a, b string }{
		{"bead ID", "s-a", "s-b"},
		{"session_name", "worker-a", "worker-b"},
		{"configured_named_identity", "named-a", "named-b"},
		{"alias", "alias-a", "alias-b"},
		{"alias_history", "old-a", "old-b"},
	}
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			sessions := beads.NewMemStoreFrom(0, []beads.Bead{sa, sb}, nil)
			work := beads.NewMemStoreFrom(0, []beads.Bead{routerWorkBead("w-1", "in_progress", f.a)}, nil)
			r, rec := newTestRouter()
			mustRebuild(t, r, routerCensus{sessions: memSessionCensus(sessions), legs: memLegs(work)})

			r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerWorkBead("w-1", "in_progress", f.b)), false, false)
			ids, allocs, _ := rec.take()
			wantStrings(t, "session keys", ids, []string{"s-a", "s-b"})
			wantStrings(t, "allocator wakes", allocs, []string{routeReasonEvent})

			// The index followed the move: the next change wakes only the new owner.
			r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerWorkBead("w-1", "in_progress", f.b)), false, false)
			ids, _, _ = rec.take()
			wantStrings(t, "session keys after move", ids, []string{"s-b"})
		})
	}
}

// Kills: assignees enqueued only on an assignee change. A status change on
// assigned work wakes its assignee; closing it wakes the assignee once and
// drops the entry.
func TestRouterAssignedWorkStatusChangeEnqueuesAssignee(t *testing.T) {
	r, rec := newTestRouter()
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})), false, true)
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerWorkBead("w-1", "open", "worker-a")), false, false)
	ids, _, _ := rec.take()
	wantStrings(t, "keys on create", ids, []string{"s-a", "s-a"})

	r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerWorkBead("w-1", "in_progress", "worker-a")), false, false)
	ids, _, _ = rec.take()
	wantStrings(t, "keys on claim", ids, []string{"s-a"})

	r.OnBeadEvent(beadEvent(t, events.BeadClosed, routerWorkBead("w-1", "closed", "worker-a")), false, false)
	ids, allocs, _ := rec.take()
	wantStrings(t, "keys on close", ids, []string{"s-a"})
	wantStrings(t, "allocator wakes on close", allocs, []string{routeReasonEvent})
	if _, ok := r.idx.assignee["w-1"]; ok {
		t.Fatal("closed work still in the assignee index")
	}

	// Unassigned, unrouted work wakes nothing (C9; TestRouterAllocatorWakePolicy).
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerWorkBead("w-2", "open", "")), false, false)
	ids, allocs, _ = rec.take()
	wantStrings(t, "keys for unassigned work", ids, nil)
	wantStrings(t, "allocator wakes for unassigned work", allocs, nil)
}

// Kills: a session key enqueued for an allocator key (API-001, API-011).
func TestRouterKeylessAndAllocatorKeysWakeAllocatorOnly(t *testing.T) {
	r, rec := newTestRouter()
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})), false, true)
	rec.take()
	for _, keys := range [][]reconcilekey.Key{nil, {reconcilekey.Allocator()}, {{}}, {reconcilekey.Session("")}} {
		r.Enqueue("api", keys...)
		ids, allocs, resyncs := rec.take()
		wantStrings(t, fmt.Sprintf("session keys for %v", keys), ids, nil)
		wantStrings(t, fmt.Sprintf("allocator wakes for %v", keys), allocs, []string{"api"})
		wantStrings(t, fmt.Sprintf("resyncs for %v", keys), resyncs, nil)
	}
}

// Kills: control-dispatch dropped (API-014, MAINT-056).
func TestRouterControlDispatchKeyMapsToAllocator(t *testing.T) {
	r, rec := newTestRouter()
	r.Enqueue("socket", reconcilekey.ControlDispatch())
	ids, allocs, _ := rec.take()
	wantStrings(t, "session keys", ids, nil)
	wantStrings(t, "allocator wakes", allocs, []string{routeReasonControlDispatch})
	if got := rec.allocsSeen(); len(got) != 0 {
		t.Fatalf("recorder not cleared: %v", got)
	}
}

func (rec *routerRecorder) allocsSeen() []routeReason {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.allocs
}

// Kills: ID keys resolved through the index, which loses a row created after
// the last rebuild (API-006..010). Operator keys are urgent.
func TestRouterSessionIDKeyNeedsNoIndex(t *testing.T) {
	r, rec := newTestRouter()
	r.Enqueue("api", reconcilekey.Session("s-new"))
	rec.mu.Lock()
	got := rec.sessions
	rec.mu.Unlock()
	want := []routedSession{{rowKey{Leg: routerTestLeg, ID: "s-new"}, routeReason{Kind: "api", Urgent: true}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sessions = %+v, want %+v", got, want)
	}
	_, allocs, resyncs := rec.take()
	wantStrings(t, "allocator wakes", allocs, nil)
	wantStrings(t, "resyncs", resyncs, nil)
}

// Kills: resync request dropped for a name the index cannot resolve.
func TestRouterUnresolvedNameWakesAllocatorAndRequestsResync(t *testing.T) {
	r, rec := newTestRouter()
	r.Enqueue("provider-event", reconcilekey.SessionNamed("ghost"))
	ids, allocs, resyncs := rec.take()
	wantStrings(t, "session keys", ids, nil)
	wantStrings(t, "allocator wakes", allocs, []string{"provider-event"})
	wantStrings(t, "resyncs", resyncs, []string{"unresolved-name"})
	if st := r.stats(); st.Unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1", st.Unresolved)
	}
}

// Kills: resync missing on a supervisor reload (API-004).
func TestRouterSupervisorReloadWakesAllocatorAndResync(t *testing.T) {
	r, rec := newTestRouter()
	r.Enqueue(routeReasonSupervisor, reconcilekey.Allocator())
	ids, allocs, resyncs := rec.take()
	wantStrings(t, "session keys", ids, nil)
	wantStrings(t, "allocator wakes", allocs, []string{routeReasonSupervisor})
	wantStrings(t, "resyncs", resyncs, []string{routeReasonSupervisor})
}

// Kills: name resolved before owner (a reused name mis-routed). Each changed
// runtime wakes its attributed owner, else the rows its name resolves to, and
// the allocator (an observation flip is a selection input); an unchanged one
// and a partial pass's unlisted one wake no session.
func TestRouterObservationFlipPrefersOwnerThenNameThenAllocator(t *testing.T) {
	cases := []struct {
		name      string
		attrs     InventoryAttrs
		gone      BackendPass // the second pass
		wantIDs   []string
		wantAlloc bool
	}{
		{"owner wins over name", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "s-owner"}, completeBackend(""), []string{"s-owner"}, true},
		{"name when ownerless", InventoryAttrs{OwnerState: OwnerNone}, completeBackend(""), []string{"s-indexed"}, true},
		{"name when attribution unknown", InventoryAttrs{}, completeBackend(""), []string{"s-indexed"}, true},
		// The backend's health degrades, which is allocator work; the
		// unlisted runtime is not concluded absent, so no session wakes.
		{"partial pass concludes no absence", InventoryAttrs{OwnerState: OwnerSession, OwnerID: "s-owner"}, partialSingle(), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, rec := newTestRouter()
			r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-indexed", map[string]string{"session_name": "rt-1"})), false, true)
			cache, clk := newTestObservationCache()
			cache.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "rt-1")), map[string]InventoryAttrs{"rt-1": tc.attrs})
			prev := cache.Snapshot()
			rec.take()

			cache.PublishInventory(obsPass(clk, 2, 1, tc.gone), nil)
			r.OnInventoryPass(prev, cache.Snapshot())
			ids, allocs, _ := rec.take()
			wantStrings(t, "session keys", ids, tc.wantIDs)
			if tc.wantAlloc {
				wantStrings(t, "allocator wakes", allocs, []string{routeReasonInventory})
			} else {
				wantStrings(t, "allocator wakes", allocs, nil)
			}
		})
	}

	t.Run("allocator when nothing resolves", func(t *testing.T) {
		r, rec := newTestRouter()
		cache, clk := newTestObservationCache()
		cache.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "stray")), map[string]InventoryAttrs{"stray": {OwnerState: OwnerNone}})
		prev := cache.Snapshot()
		cache.PublishInventory(obsPass(clk, 2, 1, completeBackend("")), nil)
		r.OnInventoryPass(prev, cache.Snapshot())
		ids, allocs, _ := rec.take()
		wantStrings(t, "session keys", ids, nil)
		wantStrings(t, "allocator wakes", allocs, []string{routeReasonInventory})
	})

	t.Run("reused name wakes old and new owner", func(t *testing.T) {
		r, rec := newTestRouter()
		cache, clk := newTestObservationCache()
		old := liveAttrs("inc-1")
		old.OwnerState, old.OwnerID = OwnerSession, "s-old"
		cache.PublishInventory(obsPass(clk, 1, 1, completeBackend("", "rt-1")), map[string]InventoryAttrs{"rt-1": old})
		prev := cache.Snapshot()
		reused := liveAttrs("inc-2")
		reused.OwnerState, reused.OwnerID = OwnerSession, "s-new"
		cache.PublishInventory(obsPass(clk, 2, 1, completeBackend("", "rt-1")), map[string]InventoryAttrs{"rt-1": reused})
		r.OnInventoryPass(prev, cache.Snapshot())
		ids, _, _ := rec.take()
		wantStrings(t, "session keys", ids, []string{"s-new", "s-old"})
	})

	t.Run("primed change wakes allocator", func(t *testing.T) {
		r, rec := newTestRouter()
		cache, clk := newTestObservationCache()
		prev := cache.Snapshot()
		cache.PublishInventory(obsPass(clk, 1, 1, completeBackend("")), nil)
		r.OnInventoryPass(prev, cache.Snapshot())
		_, allocs, _ := rec.take()
		wantStrings(t, "allocator wakes", allocs, []string{routeReasonInventory})

		prev = cache.Snapshot()
		cache.PublishInventory(obsPass(clk, 2, 1, completeBackend("")), nil)
		r.OnInventoryPass(prev, cache.Snapshot())
		_, allocs, _ = rec.take()
		wantStrings(t, "allocator wakes on a quiet pass", allocs, nil)
	})
}

// Kills: a single-valued identity map. Two rows answering to one identity
// (a duplicate, or an alias reused while the old holder still lists it in
// alias_history) are both woken.
func TestRouterDuplicateIdentityEnqueuesEveryHolder(t *testing.T) {
	r, rec := newTestRouter()
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-a", map[string]string{"alias": "nux"})), false, true)
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-b", map[string]string{"alias": "rex", "alias_history": "nux"})), false, true)
	rec.take()
	r.Enqueue("socket", reconcilekey.SessionNamed("nux"))
	ids, allocs, _ := rec.take()
	wantStrings(t, "session keys", ids, []string{"s-a", "s-b"})
	wantStrings(t, "allocator wakes", allocs, nil)
}

// Kills: a stale identity keeps routing after its row closed, or after the
// row dropped an alias. A closed row's names resolve only to it, as
// tombstones, and need no resync.
func TestRouterClosedSessionDropsIdentities(t *testing.T) {
	r, rec := newTestRouter()
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-a", map[string]string{"session_name": "worker-a", "alias": "nux"})), false, true)
	r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})), false, true)
	rec.take()
	r.Enqueue("socket", reconcilekey.SessionNamed("nux"))
	ids, _, _ := rec.take()
	wantStrings(t, "keys for a dropped alias", ids, nil)

	closed := routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})
	closed.Status = "closed"
	r.OnBeadEvent(beadEvent(t, events.BeadUpdated, closed), false, true)
	rec.take()
	r.Enqueue("socket", reconcilekey.SessionNamed("worker-a"))
	ids, _, resyncs := rec.take()
	wantStrings(t, "keys for a closed row", ids, []string{"s-a"})
	wantStrings(t, "resyncs", resyncs, nil)
	if len(r.idx.identities) != 0 || len(r.idx.keyIdents) != 0 {
		t.Fatalf("index not empty after close: %v / %v", r.idx.identities, r.idx.keyIdents)
	}
	// Work is never routed through a tombstone: only open rows hold work.
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerWorkBead("w-1", "open", "worker-a")), false, false)
	ids, _, _ = rec.take()
	wantStrings(t, "keys for work assigned to a closed row", ids, nil)
}

// Kills: a swap that does not replay the ops logged during the census read.
// Events that land between the census read and the swap survive the rebuild.
func TestRouterRebuildDuringEventsLosesNothing(t *testing.T) {
	sessions := beads.NewMemStoreFrom(0, []beads.Bead{
		routerSessionBead("s-a", map[string]string{"session_name": "worker-a"}),
	}, nil)
	work := beads.NewMemStore()
	r, rec := newTestRouter()
	census := memSessionCensus(sessions)
	mustRebuild(t, r, routerCensus{
		sessions: func() ([]session.Info, error) {
			infos, err := census()
			// The census is read; these land before the swap.
			r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-late", map[string]string{"session_name": "worker-late"})), false, true)
			r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerWorkBead("w-late", "open", "worker-late")), false, false)
			return infos, err
		},
		legs: memLegs(work),
	})
	rec.take()

	r.Enqueue("socket", reconcilekey.SessionNamed("worker-late"), reconcilekey.SessionNamed("worker-a"))
	ids, _, resyncs := rec.take()
	wantStrings(t, "session keys", ids, []string{"s-a", "s-late"})
	wantStrings(t, "resyncs", resyncs, nil)
	if got := r.idx.assignee["w-late"]; got != "worker-late" {
		t.Fatalf("assignee[w-late] = %q, want worker-late", got)
	}
}

// Kills: the index wiped on a census error (P-3). A failed sessions census
// keeps the session index whole; a failed assignee leg keeps the entries the
// readable legs do not supersede. Both ask for a resync.
func TestRouterPartialCensusKeepsPreviousEntries(t *testing.T) {
	sessions := beads.NewMemStoreFrom(0, []beads.Bead{
		routerSessionBead("s-a", map[string]string{"session_name": "worker-a"}),
	}, nil)
	legA := beads.NewMemStoreFrom(0, []beads.Bead{routerWorkBead("w-a", "in_progress", "worker-a")}, nil)
	legB := beads.NewMemStoreFrom(0, []beads.Bead{routerWorkBead("w-b", "in_progress", "worker-a")}, nil)
	r, rec := newTestRouter()
	mustRebuild(t, r, routerCensus{sessions: memSessionCensus(sessions), legs: memLegs(legA, legB)})

	if rows, sessErr, legErr := r.rebuild(routerCensus{
		sessions: func() ([]session.Info, error) { return nil, errors.New("sessions store down") },
		legs:     memLegs(legA, failingListStore{legB}),
	}); sessErr == nil || legErr == nil || rows != nil {
		t.Fatalf("rebuild over a failed census = rows %v, sessions %v, legs %v; want both errors and no rows", rows, sessErr, legErr)
	}
	_, _, resyncs := rec.take()
	wantStrings(t, "resyncs", resyncs, []string{"census-error"})
	r.Enqueue("socket", reconcilekey.SessionNamed("worker-a"))
	ids, _, _ := rec.take()
	wantStrings(t, "session keys after a failed sessions census", ids, []string{"s-a"})
	want := map[string]string{"w-a": "worker-a", "w-b": "worker-a"}
	if !reflect.DeepEqual(r.idx.assignee, want) {
		t.Fatalf("assignee after a failed leg = %v, want %v", r.idx.assignee, want)
	}

	// A refused city (no legs at all) reads as partial too, and does not fail
	// the session index: boot readiness gates on sessErr alone.
	rows, sessErr, legErr := r.rebuild(routerCensus{
		sessions: memSessionCensus(sessions),
		legs:     func() ([]classStoreCandidate, error) { return nil, errors.New("refused") },
	})
	if sessErr != nil || legErr == nil {
		t.Fatalf("rebuild with refused legs = sessions %v, legs %v; want only a leg error", sessErr, legErr)
	}
	if want := []rowKey{{Leg: routerTestLeg, ID: "s-a"}}; !reflect.DeepEqual(rows, want) {
		t.Fatalf("indexed rows = %v, want %v", rows, want)
	}
	if !reflect.DeepEqual(r.idx.assignee, want) {
		t.Fatalf("assignee after refused legs = %v, want %v", r.idx.assignee, want)
	}

	// A complete census does read absence: w-b is gone from its leg.
	mustRebuild(t, r, routerCensus{sessions: memSessionCensus(sessions), legs: memLegs(legA, beads.NewMemStore())})
	if want := map[string]string{"w-a": "worker-a"}; !reflect.DeepEqual(r.idx.assignee, want) {
		t.Fatalf("assignee after a complete census = %v, want %v", r.idx.assignee, want)
	}
}

// Kills: a bead event tail gap ignored.
func TestRouterEventGapRequestsResync(t *testing.T) {
	r, rec := newTestRouter()
	r.OnEventGap()
	_, _, resyncs := rec.take()
	wantStrings(t, "resyncs", resyncs, []string{"bead-event-gap"})
}

// Kills: an undecodable event silently dropped.
func TestRouterUndecodablePayloadWakesAllocator(t *testing.T) {
	r, rec := newTestRouter()
	for _, payload := range []json.RawMessage{nil, json.RawMessage(`{not json`), json.RawMessage(`{"id":""}`)} {
		r.OnBeadEvent(events.Event{Type: events.BeadUpdated, Subject: "x", Payload: payload}, false, true)
	}
	ids, allocs, _ := rec.take()
	wantStrings(t, "session keys", ids, nil)
	wantStrings(t, "allocator wakes", allocs, []string{routeReasonUndecodable, routeReasonUndecodable, routeReasonUndecodable})
	if st := r.stats(); st.Undecodable != 3 {
		t.Fatalf("undecodable = %d, want 3", st.Undecodable)
	}
}

// Kills: a wrong-leg session key enqueued. A session bead applied only to
// another leg (a migrated relic, C2.11) is a census input and nothing more.
func TestRouterRelicSessionRowOnOtherLegIsCensusOnly(t *testing.T) {
	r, rec := newTestRouter()
	r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerSessionBead("s-relic", map[string]string{"session_name": "worker-r"})), false, false)
	ids, allocs, _ := rec.take()
	wantStrings(t, "session keys", ids, nil)
	wantStrings(t, "allocator wakes", allocs, []string{routeReasonEvent})
	if len(r.idx.keyIdents) != 0 {
		t.Fatalf("relic indexed: %v", r.idx.keyIdents)
	}
}

// Kills: recover removed (a panic would kill the bead event watcher). The
// panic is counted, asks for a resync, and leaves the router usable.
func TestRouterMappingPanicIsRecovered(t *testing.T) {
	rec := &routerRecorder{}
	sink := rec.sink()
	explode := true
	add := sink.addSession
	sink.addSession = func(k rowKey, reason routeReason) {
		if explode {
			panic("sink exploded")
		}
		add(k, reason)
	}
	var stderr strings.Builder
	r := newReconcileRouter(routerTestLeg, sink, &stderr)
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})), false, true)
	if st := r.stats(); st.Panics != 1 {
		t.Fatalf("panics = %d, want 1", st.Panics)
	}
	if !strings.Contains(stderr.String(), "sink exploded") {
		t.Fatalf("stderr = %q, want the panic logged", stderr.String())
	}
	_, _, resyncs := rec.take()
	wantStrings(t, "resyncs", resyncs, []string{"router-panic"})

	explode = false
	r.Enqueue("socket", reconcilekey.SessionNamed("worker-a"))
	ids, _, _ := rec.take()
	wantStrings(t, "session keys after the panic", ids, []string{"s-a"})
}

// Kills: the tombstone fallback removed (every routine retirement forces a
// resync). CloseDetailed stops the runtime, then closes the bead; the
// runtime's gone report for the retired name arrives after the close is
// routed and wakes the closed row, with no resync. A complete rebuild drops
// the tombstone.
func TestRouterRetiredRowGoneNameNeedsNoResync(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := session.NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Title: "chat", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	r, rec := newTestRouter()
	census := routerCensus{sessions: memSessionCensus(store), legs: memLegs()}
	mustRebuild(t, r, census)
	created, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	r.OnBeadEvent(beadEvent(t, events.BeadUpdated, created), false, true)

	if _, err := mgr.CloseDetailed(info.ID); err != nil {
		t.Fatalf("CloseDetailed: %v", err)
	}
	if sp.IsRunning(info.SessionName) {
		t.Fatal("CloseDetailed left the runtime running")
	}
	closed, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	r.OnBeadEvent(beadEvent(t, events.BeadClosed, closed), false, true)
	rec.take()

	for _, reason := range []string{"lane-gone", "on-death", "provider-event"} {
		r.Enqueue(reason, reconcilekey.SessionNamed(info.SessionName))
		ids, allocs, resyncs := rec.take()
		wantStrings(t, reason+" session keys", ids, []string{info.ID})
		wantStrings(t, reason+" allocator wakes", allocs, nil)
		wantStrings(t, reason+" resyncs", resyncs, nil)
	}
	if st := r.stats(); st.Unresolved != 0 {
		t.Fatalf("unresolved = %d, want 0", st.Unresolved)
	}

	mustRebuild(t, r, census)
	r.Enqueue("lane-gone", reconcilekey.SessionNamed(info.SessionName))
	ids, _, resyncs := rec.take()
	wantStrings(t, "session keys after a complete rebuild", ids, nil)
	wantStrings(t, "resyncs after a complete rebuild", resyncs, []string{"unresolved-name"})
}

// Kills: unbounded tombstones; tombstones dropped by a failed rebuild.
// Closing more rows than the cap holds asks for a resync; a failed sessions
// census keeps the tombstones and a complete rebuild clears them.
func TestRouterTombstonesAreBounded(t *testing.T) {
	r, rec := newTestRouter()
	// Each row answers to two identities: its ID and its session_name.
	rows := routerMaxTombstones/2 + 1
	for i := 0; i < rows; i++ {
		sb := routerSessionBead(fmt.Sprintf("s-%d", i), map[string]string{"session_name": fmt.Sprintf("worker-%d", i)})
		r.OnBeadEvent(beadEvent(t, events.BeadCreated, sb), false, true)
		sb.Status = "closed"
		r.OnBeadEvent(beadEvent(t, events.BeadClosed, sb), false, true)
		_, _, resyncs := rec.take()
		if i < rows-1 {
			wantStrings(t, fmt.Sprintf("resyncs at row %d", i), resyncs, nil)
		} else {
			wantStrings(t, "resyncs past the cap", resyncs, []string{"tombstone-overflow"})
		}
	}
	if r.idx.tombCount != routerMaxTombstones {
		t.Fatalf("tombstones = %d, want %d", r.idx.tombCount, routerMaxTombstones)
	}

	if _, sessErr, _ := r.rebuild(routerCensus{
		sessions: func() ([]session.Info, error) { return nil, errors.New("sessions store down") },
		legs:     memLegs(),
	}); sessErr == nil {
		t.Fatal("rebuild over a failed sessions census returned nil")
	}
	r.Enqueue("lane-gone", reconcilekey.SessionNamed("worker-0"))
	ids, _, _ := rec.take()
	wantStrings(t, "keys after a failed rebuild", ids, []string{"s-0"})

	mustRebuild(t, r, routerCensus{sessions: memSessionCensus(beads.NewMemStore()), legs: memLegs()})
	if r.idx.tombCount != 0 || len(r.idx.tombstones) != 0 || r.idx.tombFull {
		t.Fatalf("tombstones after a complete rebuild = %d (%d names, full %v), want none", r.idx.tombCount, len(r.idx.tombstones), r.idx.tombFull)
	}
}

// Kills: a minimal close or delete payload (ID only) read as work. An ID
// indexed as a session row of the sessions store is that row: it wakes and
// its names become tombstones.
func TestRouterMinimalCloseOfIndexedSessionIsThatSession(t *testing.T) {
	for _, evtType := range []string{events.BeadClosed, events.BeadDeleted} {
		t.Run(evtType, func(t *testing.T) {
			r, rec := newTestRouter()
			r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})), false, true)
			rec.take()

			minimal := events.Event{Type: evtType, Subject: "s-a", Payload: json.RawMessage(`{"id":"s-a"}`)}
			r.OnBeadEvent(minimal, false, true)
			ids, allocs, resyncs := rec.take()
			wantStrings(t, "session keys", ids, []string{"s-a"})
			wantStrings(t, "allocator wakes", allocs, []string{routeReasonEvent})
			wantStrings(t, "resyncs", resyncs, nil)
			if len(r.idx.keyIdents) != 0 {
				t.Fatalf("closed row still indexed: %v", r.idx.keyIdents)
			}
			r.Enqueue("on-death", reconcilekey.SessionNamed("worker-a"))
			ids, _, resyncs = rec.take()
			wantStrings(t, "keys for the closed row's name", ids, []string{"s-a"})
			wantStrings(t, "resyncs for the closed row's name", resyncs, nil)

			// An ID that was never a session row stays work.
			r.OnBeadEvent(events.Event{Type: evtType, Subject: "w-1", Payload: json.RawMessage(`{"id":"w-1"}`)}, false, true)
			ids, _, _ = rec.take()
			wantStrings(t, "keys for a minimal work close", ids, nil)
		})
	}
}

// Kills: overflow not flagged (N1); overflow ignored at the swap (N2). A build
// that logs more than routerMaxPending ops is abandoned with its own error and
// resync reason; the live index stays current, and a retry rebuilds.
func TestRouterRebuildOverflowAbandonsAndRetries(t *testing.T) {
	sessions := beads.NewMemStoreFrom(0, []beads.Bead{
		routerSessionBead("s-census", map[string]string{"session_name": "worker-census"}),
	}, nil)
	r, rec := newTestRouter()
	census := memSessionCensus(sessions)
	flood := func() ([]session.Info, error) {
		infos, err := census()
		// One session op plus routerMaxPending work ops: one past the log.
		late, cerr := sessions.Create(routerSessionBead("", map[string]string{"session_name": "worker-late"}))
		if cerr != nil {
			t.Fatalf("create: %v", cerr)
		}
		r.OnBeadEvent(beadEvent(t, events.BeadCreated, late), false, true)
		for i := 0; i < routerMaxPending; i++ {
			r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerWorkBead(fmt.Sprintf("w-%d", i), "open", "worker-late")), false, false)
		}
		return infos, err
	}
	rows, sessErr, legErr := r.rebuild(routerCensus{sessions: flood, legs: memLegs()})
	if !errors.Is(sessErr, errRouterPendingOverflow) || legErr != nil || rows != nil {
		t.Fatalf("overflowed rebuild = rows %v, sessions %v, legs %v; want the overflow error alone", rows, sessErr, legErr)
	}
	_, _, resyncs := rec.take()
	wantStrings(t, "resyncs", resyncs, []string{"rebuild-overflow"})
	if r.building || r.pending != nil {
		t.Fatalf("build left open: building %v, %d pending", r.building, len(r.pending))
	}

	// The abandoned build swapped nothing: every event is live, the census
	// row the build read is not.
	if len(r.idx.assignee) != routerMaxPending || r.idx.assignee[fmt.Sprintf("w-%d", routerMaxPending-1)] != "worker-late" {
		t.Fatalf("live assignee index has %d entries, want %d ending in worker-late", len(r.idx.assignee), routerMaxPending)
	}
	r.Enqueue("socket", reconcilekey.SessionNamed("worker-late"), reconcilekey.SessionNamed("worker-census"))
	ids, _, resyncs := rec.take()
	if len(ids) != 1 || ids[0] == "s-census" {
		t.Fatalf("session keys = %q, want only the late row", ids)
	}
	wantStrings(t, "resyncs for the unbuilt census row", resyncs, []string{"unresolved-name"})

	// The retry rebuilds from the census.
	mustRebuild(t, r, routerCensus{sessions: census, legs: memLegs()})
	r.Enqueue("socket", reconcilekey.SessionNamed("worker-late"), reconcilekey.SessionNamed("worker-census"))
	ids, _, resyncs = rec.take()
	if len(ids) != 2 {
		t.Fatalf("session keys after the retry = %q, want both rows", ids)
	}
	wantStrings(t, "resyncs after the retry", resyncs, nil)
}

// Kills: the log replayed out of order (C3). Two updates to one row during
// the census read land in order: the last alias wins.
func TestRouterRebuildReplaysLogInOrder(t *testing.T) {
	sessions := beads.NewMemStoreFrom(0, []beads.Bead{
		routerSessionBead("s-a", map[string]string{"alias": "a0"}),
	}, nil)
	r, rec := newTestRouter()
	census := memSessionCensus(sessions)
	mustRebuild(t, r, routerCensus{
		sessions: func() ([]session.Info, error) {
			infos, err := census()
			for _, alias := range []string{"a1", "a2"} {
				r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerSessionBead("s-a", map[string]string{"alias": alias})), false, true)
			}
			return infos, err
		},
		legs: memLegs(),
	})
	rec.take()
	r.Enqueue("socket", reconcilekey.SessionNamed("a2"))
	ids, _, _ := rec.take()
	wantStrings(t, "keys for the last alias", ids, []string{"s-a"})
	for _, stale := range []string{"a0", "a1"} {
		r.Enqueue("socket", reconcilekey.SessionNamed(stale))
		ids, _, _ = rec.take()
		wantStrings(t, "keys for stale alias "+stale, ids, nil)
	}
}

// Kills: closing one holder drops a shared identity for all (N26).
func TestRouterClosingOneHolderKeepsSharedIdentity(t *testing.T) {
	r, rec := newTestRouter()
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-a", map[string]string{"alias": "nux"})), false, true)
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routerSessionBead("s-b", map[string]string{"alias": "rex", "alias_history": "nux"})), false, true)
	closed := routerSessionBead("s-a", map[string]string{"alias": "nux"})
	closed.Status = "closed"
	r.OnBeadEvent(beadEvent(t, events.BeadClosed, closed), false, true)
	rec.take()
	r.Enqueue("socket", reconcilekey.SessionNamed("nux"))
	ids, _, _ := rec.take()
	wantStrings(t, "session keys", ids, []string{"s-b"})
}

// Kills: urgency dropped on the allocator path (N5) or the control-dispatch
// path (N6). Operator intents stay urgent; lane reasons do not.
func TestRouterAllocatorWakesCarryUrgency(t *testing.T) {
	r, rec := newTestRouter()
	cases := []struct {
		reason string
		key    reconcilekey.Key
		want   routeReason
	}{
		{"api", reconcilekey.Allocator(), routeReason{Kind: "api", Urgent: true}},
		{"socket", reconcilekey.ControlDispatch(), routeReason{Kind: routeReasonControlDispatch, Detail: "socket", Urgent: true}},
		{"provider-event", reconcilekey.Allocator(), routeReason{Kind: "provider-event"}},
		{"provider-event", reconcilekey.ControlDispatch(), routeReason{Kind: routeReasonControlDispatch, Detail: "provider-event"}},
	}
	for _, tc := range cases {
		r.Enqueue(tc.reason, tc.key)
		got := rec.allocsSeen()
		rec.take()
		if want := []routeReason{tc.want}; !reflect.DeepEqual(got, want) {
			t.Errorf("Enqueue(%q, %v) allocator wakes = %+v, want %+v", tc.reason, tc.key, got, want)
		}
	}
}

// Kills: live events counted as replays (N24).
func TestRouterCountsLiveAndReplayApart(t *testing.T) {
	r, _ := newTestRouter()
	for i := 0; i < 5; i++ {
		r.OnBeadEvent(beadEvent(t, events.BeadUpdated, routerWorkBead("w-1", "open", "")), i >= 3, false)
	}
	if st := r.stats(); st.Events != 3 || st.Replays != 2 {
		t.Fatalf("stats = %+v, want 3 events and 2 replays", st)
	}
}

// Kills: names the cache stopped tracking ignored (N4). A name gone from the
// snapshot wakes its owner and the allocator.
func TestRouterUntrackedNameWakesOwner(t *testing.T) {
	r, rec := newTestRouter()
	prev := &ObservationSnapshot{ByName: map[string]RuntimeObservation{
		"rt-1": {SessionName: "rt-1", OwnerState: OwnerSession, Owner: reconcilekey.Session("s-a")},
	}}
	r.OnInventoryPass(prev, &ObservationSnapshot{PassSeq: 2})
	ids, allocs, _ := rec.take()
	wantStrings(t, "session keys", ids, []string{"s-a"})
	wantStrings(t, "allocator wakes", allocs, []string{routeReasonInventory})
}

// Kills: a readable leg's rows not superseding kept entries when another leg
// fails (N9). Work reassigned on a readable leg takes its new assignee; the
// failed leg's entries are kept.
func TestRouterFailedLegKeepsEntriesReadableLegsSupersede(t *testing.T) {
	legA := beads.NewMemStoreFrom(0, []beads.Bead{routerWorkBead("w-a", "in_progress", "worker-a")}, nil)
	legB := beads.NewMemStoreFrom(0, []beads.Bead{routerWorkBead("w-b", "in_progress", "worker-a")}, nil)
	r, _ := newTestRouter()
	sessions := memSessionCensus(beads.NewMemStore())
	mustRebuild(t, r, routerCensus{sessions: sessions, legs: memLegs(legA, legB)})

	reassigned := "worker-b"
	if err := legA.Update("w-a", beads.UpdateOpts{Assignee: &reassigned}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, _, legErr := r.rebuild(routerCensus{sessions: sessions, legs: memLegs(legA, failingListStore{legB})}); legErr == nil {
		t.Fatal("rebuild over a failed leg returned no leg error")
	}
	if want := map[string]string{"w-a": "worker-b", "w-b": "worker-a"}; !reflect.DeepEqual(r.idx.assignee, want) {
		t.Fatalf("assignee = %v, want %v", r.idx.assignee, want)
	}
}

// Kills: the build flag left set by a panicking census read, which would log
// every later event forever; a resync requested under rebuildMu.
func TestRouterRebuildEndsCleanlyAndResyncsOutsideItsLock(t *testing.T) {
	rec := &routerRecorder{}
	sink := rec.sink()
	var r *reconcileRouter
	record := sink.requestResync
	sink.requestResync = func(reason string) {
		if !r.rebuildMu.TryLock() {
			t.Errorf("resync %q requested under rebuildMu", reason)
		} else {
			r.rebuildMu.Unlock()
		}
		record(reason)
	}
	r = newReconcileRouter(routerTestLeg, sink, nil)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("census panic not propagated")
			}
		}()
		_, _, _ = r.rebuild(routerCensus{sessions: func() ([]session.Info, error) { panic("census exploded") }, legs: memLegs()})
	}()
	if r.building || r.pending != nil {
		t.Fatalf("build left open after a census panic: building %v, %d pending", r.building, len(r.pending))
	}

	_, _, _ = r.rebuild(routerCensus{sessions: func() ([]session.Info, error) { return nil, errors.New("down") }, legs: memLegs()})
	_, _, resyncs := rec.take()
	wantStrings(t, "resyncs", resyncs, []string{"census-error"})
}

// The indexes stay consistent under concurrent events and rebuilds: once the
// writers finish, the live index equals a fresh rebuild of the final census,
// and the two halves of the identity index agree. Each writer updates the
// store before routing the event, as the caches apply before the router.
func TestRouterIndexConsistentUnderConcurrentEventsAndRebuilds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sessions := beads.NewMemStore()
		work := beads.NewMemStore()
		r, _ := newTestRouter()
		census := routerCensus{sessions: memSessionCensus(sessions), legs: memLegs(work)}

		const writers, steps = 8, 20
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sb, err := sessions.Create(routerSessionBead("", map[string]string{"session_name": fmt.Sprintf("worker-%d", w)}))
				if err != nil {
					t.Errorf("create session: %v", err)
					return
				}
				r.OnBeadEvent(beadEvent(t, events.BeadCreated, sb), false, true)
				wb, err := work.Create(routerWorkBead("", "open", ""))
				if err != nil {
					t.Errorf("create work: %v", err)
					return
				}
				for i := 0; i < steps; i++ {
					alias := fmt.Sprintf("alias-%d-%d", w, i)
					if err := sessions.SetMetadata(sb.ID, "alias", alias); err != nil {
						t.Errorf("set alias: %v", err)
						return
					}
					sb.Metadata["alias"] = alias
					r.OnBeadEvent(beadEvent(t, events.BeadUpdated, sb), i%2 == 0, true)
					wb.Assignee = alias
					if err := work.Update(wb.ID, beads.UpdateOpts{Assignee: &alias}); err != nil {
						t.Errorf("assign work: %v", err)
						return
					}
					r.OnBeadEvent(beadEvent(t, events.BeadUpdated, wb), false, false)
				}
				if w%2 == 0 {
					if err := sessions.Close(sb.ID); err != nil {
						t.Errorf("close session: %v", err)
						return
					}
					sb.Status = "closed"
					r.OnBeadEvent(beadEvent(t, events.BeadClosed, sb), false, true)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < steps; i++ {
				if _, sessErr, legErr := r.rebuild(census); sessErr != nil || legErr != nil {
					t.Errorf("rebuild: sessions %v, legs %v", sessErr, legErr)
				}
			}
		}()
		wg.Wait()

		fresh, _ := newTestRouter()
		mustRebuild(t, fresh, census)
		// Tombstones are history, not census: compare the live maps.
		live := routerIndex{identities: r.idx.identities, keyIdents: r.idx.keyIdents, assignee: r.idx.assignee}
		want := routerIndex{identities: fresh.idx.identities, keyIdents: fresh.idx.keyIdents, assignee: fresh.idx.assignee}
		if !reflect.DeepEqual(live, want) {
			t.Fatalf("live index diverged from the census:\nlive  %+v\nfresh %+v", live, want)
		}
		for k, ids := range r.idx.keyIdents {
			for _, id := range ids {
				if _, ok := r.idx.identities[id][k]; !ok {
					t.Fatalf("identity %q missing row %v", id, k)
				}
			}
		}
		for id, rows := range r.idx.identities {
			for k := range rows {
				if !containsString(r.idx.keyIdents[k], id) {
					t.Fatalf("row %v missing identity %q", k, id)
				}
			}
		}
	})
}

func routedWorkBead(id, status, assignee, route string) beads.Bead {
	b := routerWorkBead(id, status, assignee)
	if route != "" {
		b.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: route}
	}
	return b
}

// Kills each clause of the allocator wake policy (CONTRACT §1.1, C9) dropped
// or widened. On any leg, replays included, the allocator wakes for a close
// or delete, a session row (relics too) and work whose current or previously
// indexed version carries gc.routed_to or an assignee; any other work event
// wakes nothing and is counted. "Before" comes from an earlier event, a
// rebuild, or a partial rebuild that keeps it.
func TestRouterAllocatorWakePolicy(t *testing.T) {
	work := func(route, assignee string) beads.Bead { return routedWorkBead("w-1", "open", assignee, route) }
	seedEvent := func(b beads.Bead) func(*testing.T, *reconcileRouter) {
		return func(t *testing.T, r *reconcileRouter) {
			r.OnBeadEvent(beadEvent(t, events.BeadUpdated, b), false, false)
		}
	}
	seedRebuild := func(failLeg bool) func(*testing.T, *reconcileRouter) {
		return func(t *testing.T, r *reconcileRouter) {
			census := routerCensus{sessions: memSessionCensus(beads.NewMemStore()), legs: memLegs(beads.NewMemStoreFrom(0, []beads.Bead{work("pool", "")}, nil))}
			mustRebuild(t, r, census)
			if failLeg {
				census.legs = memLegs(failingListStore{beads.NewMemStore()})
				if _, _, legErr := r.rebuild(census); legErr == nil {
					t.Fatal("rebuild over a failing leg reported no error")
				}
			}
		}
	}
	session := routerSessionBead("s-a", map[string]string{"session_name": "worker-a"})
	cases := []struct {
		name      string
		seed      func(*testing.T, *reconcileRouter)
		eventType string
		b         beads.Bead
		snapshot  bool
		sessions  bool // applied to the sessions store
		want      bool
	}{
		{name: "work with neither", eventType: events.BeadUpdated, b: work("", "")},
		{name: "work with neither, replayed", eventType: events.BeadUpdated, b: work("", ""), snapshot: true},
		{name: "work with neither after a closed route", seed: seedEvent(routedWorkBead("w-1", "closed", "", "pool")), eventType: events.BeadUpdated, b: work("", "")},
		{name: "routed work", eventType: events.BeadCreated, b: work("pool", ""), want: true},
		{name: "routed work, replayed", eventType: events.BeadUpdated, b: work("pool", ""), snapshot: true, want: true},
		{name: "assigned work", eventType: events.BeadUpdated, b: work("", "worker-a"), want: true},
		{name: "assigned blocked work", eventType: events.BeadUpdated, b: routedWorkBead("w-1", "blocked", "worker-a", ""), want: true},
		{name: "route removed", seed: seedEvent(work("pool", "")), eventType: events.BeadUpdated, b: work("", ""), want: true},
		{name: "route removed after a rebuild", seed: seedRebuild(false), eventType: events.BeadUpdated, b: work("", ""), want: true},
		{name: "route removed after a partial rebuild", seed: seedRebuild(true), eventType: events.BeadUpdated, b: work("", ""), want: true},
		{name: "assignee removed", seed: seedEvent(work("", "worker-a")), eventType: events.BeadUpdated, b: work("", ""), want: true},
		{name: "close of work with neither", eventType: events.BeadClosed, b: routedWorkBead("w-1", "closed", "", ""), want: true},
		{name: "delete of work with neither", eventType: events.BeadDeleted, b: work("", ""), snapshot: true, want: true},
		{name: "session row, replayed", eventType: events.BeadUpdated, b: session, snapshot: true, sessions: true, want: true},
		{name: "relic session row, replayed", eventType: events.BeadUpdated, b: session, snapshot: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, rec := newTestRouter()
			if tc.seed != nil {
				tc.seed(t, r)
			}
			rec.take()
			suppressed := r.stats().WakesSuppressed
			r.OnBeadEvent(beadEvent(t, tc.eventType, tc.b), tc.snapshot, tc.sessions)
			_, allocs, _ := rec.take()
			if got := len(allocs) > 0; got != tc.want {
				t.Fatalf("allocator woken = %v (%q), want %v", got, allocs, tc.want)
			}
			if got, want := r.stats().WakesSuppressed-suppressed, map[bool]uint64{false: 1}[tc.want]; got != want {
				t.Fatalf("wakes suppressed = %d, want %d", got, want)
			}
		})
	}
}

// Kills the routed set growing without bound, or a full set suppressing
// wakes it can no longer vouch for. Past routerMaxRouted every work event
// wakes the allocator; a complete rebuild that fits clears the overflow.
func TestRouterRoutedSetIsBounded(t *testing.T) {
	r, rec := newTestRouter()
	for i := 0; i < routerMaxRouted; i++ {
		r.idx.routed[fmt.Sprintf("w-fill-%d", i)] = struct{}{}
	}
	r.OnBeadEvent(beadEvent(t, events.BeadCreated, routedWorkBead("w-1", "open", "", "pool")), false, false)
	if len(r.idx.routed) != routerMaxRouted || !r.idx.routedFull {
		t.Fatalf("routed set = %d entries, full %v; want %d, full", len(r.idx.routed), r.idx.routedFull, routerMaxRouted)
	}
	rec.take()
	unrouted := beadEvent(t, events.BeadUpdated, routerWorkBead("w-2", "open", ""))
	r.OnBeadEvent(unrouted, false, false)
	if _, allocs, _ := rec.take(); len(allocs) != 1 {
		t.Fatalf("allocator wakes with a full routed set = %q, want one", allocs)
	}

	mustRebuild(t, r, routerCensus{sessions: memSessionCensus(beads.NewMemStore()), legs: memLegs(beads.NewMemStore())})
	r.OnBeadEvent(unrouted, false, false)
	if _, allocs, _ := rec.take(); len(allocs) != 0 || r.idx.routedFull {
		t.Fatalf("after a fitting rebuild: allocator wakes %q, full %v; want none", allocs, r.idx.routedFull)
	}
}

// BenchmarkRouterWakePolicy routes an hour of maintainer-city bead events
// (51k, P3 §6.4): mostly unrouted work churn and session-row replays, some
// routed and assigned work. cpu-fraction is one core's busy share at that rate.
func BenchmarkRouterWakePolicy(b *testing.B) {
	r := newReconcileRouter(routerTestLeg, routerSink{
		addSession: func(rowKey, routeReason) {}, wakeAllocator: func(routeReason) {}, requestResync: func(string) {},
	}, nil)
	var evts []events.Event
	for i := 0; i < 1000; i++ {
		var bead beads.Bead
		switch id := fmt.Sprintf("b-%d", i%200); i % 10 {
		case 0, 1, 2:
			bead = routerSessionBead("s-"+id, map[string]string{"session_name": "worker-" + id})
		case 3:
			bead = routedWorkBead(id, "open", "", "pool")
		case 4:
			bead = routedWorkBead(id, "in_progress", "worker-"+id, "pool")
		default:
			bead = routerWorkBead("u-"+id, "open", "")
		}
		payload, err := beads.EncodeBeadEventPayload(bead)
		if err != nil {
			b.Fatal(err)
		}
		evts = append(evts, events.Event{Type: events.BeadUpdated, Subject: bead.ID, Payload: payload})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.OnBeadEvent(evts[i%len(evts)], i%2 == 0, true)
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)*51000/3.6e12, "cpu-fraction")
}
