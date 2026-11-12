package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func listTestSessionBead(id, status string, meta map[string]string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Title:    "worker",
		Type:     BeadType,
		Status:   status,
		Labels:   []string{LabelSession, "agent:rig/worker"},
		Metadata: meta,
	}
}

// listTestStore is the session store the fold confirms against: the rows it
// serves, by id, and how many times the fold read one.
type listTestStore struct {
	rows  map[string]beads.Bead
	err   error
	reads int
}

func newListTestStore() *listTestStore {
	return &listTestStore{rows: map[string]beads.Bead{}}
}

func (s *listTestStore) served(id string) (beads.Bead, bool, error) {
	s.reads++
	if s.err != nil {
		return beads.Bead{}, false, s.err
	}
	b, ok := s.rows[id]
	return b, ok, nil
}

func (s *listTestStore) fold(capacity int) *ListTransitions {
	return NewListTransitions(capacity, s.served)
}

// apply folds a notification of the store's own current row: the store holds
// b (or, for a delete, no longer holds it), as it does once the controller's
// caches applied the event.
func (s *listTestStore) apply(t *testing.T, lt *ListTransitions, change BeadChange, b beads.Bead) ListChange {
	t.Helper()
	if change == BeadChangeDeleted {
		delete(s.rows, b.ID)
	} else {
		s.rows[b.ID] = b
	}
	return observeListChange(t, lt, change, b)
}

// observeListChange folds a notification without touching the store, as a
// stale or repeated notification arrives.
func observeListChange(t *testing.T, lt *ListTransitions, change BeadChange, b beads.Bead) ListChange {
	t.Helper()
	transition, err := lt.Observe(change, b)
	if err != nil {
		t.Fatalf("Observe(%v, %s): %v", change, b.ID, err)
	}
	return transition.Change
}

func TestListTransitionsCreateAnnouncesOnce(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	b := listTestSessionBead("gc-1", "open", map[string]string{"template": "rig/worker", "alias": "w1", "agent_name": "rig/worker-1"})
	store.rows[b.ID] = b

	transition, err := lt.Observe(BeadChangeCreated, b)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if transition.Change != ListChangeCreated {
		t.Fatalf("first create: change = %v, want ListChangeCreated", transition.Change)
	}
	if info := transition.Session; info.ID != "gc-1" || info.Template != "rig/worker" || info.Alias != "w1" || info.AgentName != "rig/worker-1" {
		t.Fatalf("first create: session = %+v, want the bead's id, template, alias and agent name", info)
	}
	// The same create announced again (a refresh that installed the row, then
	// the create's own notification) is not a second transition.
	if change := observeListChange(t, lt, BeadChangeCreated, b); change != ListChangeNone {
		t.Fatalf("repeated create: change = %v, want ListChangeNone", change)
	}
	// A metadata update of an open session is not a list change.
	if change := observeListChange(t, lt, BeadChangeUpdated, b); change != ListChangeNone {
		t.Fatalf("update of an open session: change = %v, want ListChangeNone", change)
	}
	// A notification that changes nothing reads no row: only a transition is
	// confirmed against the store.
	if store.reads != 1 {
		t.Fatalf("store reads = %d, want 1 (the create's confirmation)", store.reads)
	}
}

func TestListTransitionsCloseAnnouncesOnce(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	closed := listTestSessionBead("gc-1", "closed", map[string]string{"template": "rig/worker", "close_reason": "drained"})
	store.rows[closed.ID] = closed

	transition, err := lt.Observe(BeadChangeClosed, closed)
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	if transition.Change != ListChangeClosed {
		t.Fatalf("first close: change = %v, want ListChangeClosed", transition.Change)
	}
	if transition.Session.ID != "gc-1" || transition.Session.Template != "rig/worker" {
		t.Fatalf("first close: session = %+v, want the bead's id and template", transition.Session)
	}
	if transition.CloseReason != "drained" {
		t.Fatalf("first close: close reason = %q, want drained", transition.CloseReason)
	}
	// A second close (the writer's own event and the cache's inferred one, or
	// a double close) is not a second transition.
	if change := observeListChange(t, lt, BeadChangeClosed, closed); change != ListChangeNone {
		t.Fatalf("double close: change = %v, want ListChangeNone", change)
	}
	// Metadata settled on the closed row later (the usage lane's clears) is
	// not a list change either.
	if change := observeListChange(t, lt, BeadChangeUpdated, closed); change != ListChangeNone {
		t.Fatalf("update of a closed session: change = %v, want ListChangeNone", change)
	}
}

// A close of a session this observer never saw open (created before it
// started) is still a transition: the event says the row just closed.
func TestListTransitionsCloseOfUnseenSessionAnnounces(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	if change := store.apply(t, lt, BeadChangeClosed, listTestSessionBead("gc-9", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("close of an unseen session: change = %v, want ListChangeClosed", change)
	}
}

// An update of a closed row the observer never saw open says nothing about a
// transition: it must not announce a close for a session long gone.
func TestListTransitionsUpdateOfUnseenClosedSessionIsSilent(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	if change := store.apply(t, lt, BeadChangeUpdated, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeNone {
		t.Fatalf("update of an unseen closed session: change = %v, want ListChangeNone", change)
	}
}

// A writer that reports a close as a bead.updated carrying the closed row,
// for a session the observer never saw (open before it started), is followed
// by the bead.closed the cache re-announces. The update must not swallow that
// close.
func TestListTransitionsClosingUpdateOfUnseenSessionKeepsTheClose(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	closed := listTestSessionBead("gc-1", "closed", nil)
	if change := store.apply(t, lt, BeadChangeUpdated, closed); change != ListChangeNone {
		t.Fatalf("closing update of an unseen session: change = %v, want ListChangeNone", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, closed); change != ListChangeClosed {
		t.Fatalf("close after the closing update: change = %v, want ListChangeClosed", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, closed); change != ListChangeNone {
		t.Fatalf("repeated close: change = %v, want ListChangeNone", change)
	}
}

// The same holds for a session the observer saw open and then forgot when its
// memory overflowed.
func TestListTransitionsClosingUpdateAfterOverflowKeepsTheClose(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(2)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-2", "open", nil))
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-3", "open", nil)) // forgets gc-1
	closed := listTestSessionBead("gc-1", "closed", nil)
	if change := store.apply(t, lt, BeadChangeUpdated, closed); change != ListChangeNone {
		t.Fatalf("closing update of a forgotten session: change = %v, want ListChangeNone", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, closed); change != ListChangeClosed {
		t.Fatalf("close after the closing update: change = %v, want ListChangeClosed", change)
	}
}

// An update carrying an open row the observer never saw is a session a
// client may not list yet: a session closed before the observer started and
// reopened after (a configured named session is reopened in place, by a
// bead.updated), or one already open, which a client that read the list shows
// already. Either way it is announced as created, once.
func TestListTransitionsUpdateOfUnseenOpenSessionAnnouncesCreated(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	// Metadata settled on the row while it was closed says nothing.
	if change := store.apply(t, lt, BeadChangeUpdated, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeNone {
		t.Fatalf("update of the closed row: change = %v, want ListChangeNone", change)
	}
	reopened := listTestSessionBead("gc-1", "open", map[string]string{"template": "rig/worker"})
	if change := store.apply(t, lt, BeadChangeUpdated, reopened); change != ListChangeCreated {
		t.Fatalf("reopen of a session closed before the observer started: change = %v, want ListChangeCreated", change)
	}
	if change := store.apply(t, lt, BeadChangeUpdated, reopened); change != ListChangeNone {
		t.Fatalf("next update of the reopened session: change = %v, want ListChangeNone", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("close after the reopen: change = %v, want ListChangeClosed", change)
	}
}

// A closed session the observer forgot when its memory overflowed is
// announced when it reopens.
func TestListTransitionsReopenAfterOverflowAnnouncesCreated(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(2)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	store.apply(t, lt, BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil))
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-2", "open", nil))
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-3", "open", nil)) // forgets gc-1
	if change := store.apply(t, lt, BeadChangeUpdated, listTestSessionBead("gc-1", "open", nil)); change != ListChangeCreated {
		t.Fatalf("reopen of a forgotten closed session: change = %v, want ListChangeCreated", change)
	}
}

// A reopened session (a configured named session's closed row reopened) joins
// the open set again, so it is announced as created; its next close is
// announced too.
func TestListTransitionsReopenAnnouncesCreatedAgain(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	store.apply(t, lt, BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil))

	if change := store.apply(t, lt, BeadChangeUpdated, listTestSessionBead("gc-1", "open", nil)); change != ListChangeCreated {
		t.Fatalf("reopen: change = %v, want ListChangeCreated", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("close after reopen: change = %v, want ListChangeClosed", change)
	}
}

// A bead.updated that carries the close (status closed) is the close when the
// observer knew the row open; the bead.closed that follows is not a second one.
func TestListTransitionsCloseByUpdateOfKnownOpenSession(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	if change := store.apply(t, lt, BeadChangeUpdated, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("closing update: change = %v, want ListChangeClosed", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeNone {
		t.Fatalf("close after closing update: change = %v, want ListChangeNone", change)
	}
}

// A notification older than the row the store serves (a writer's snapshot or
// a create echo delivered after the close) is not a reopen: the store shows
// the session closed.
func TestListTransitionsStaleOpenNotificationAfterCloseIsSilent(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	open := listTestSessionBead("gc-1", "open", nil)
	store.apply(t, lt, BeadChangeCreated, open)
	store.apply(t, lt, BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil))

	if change := observeListChange(t, lt, BeadChangeCreated, open); change != ListChangeNone {
		t.Fatalf("late create echo: change = %v, want ListChangeNone", change)
	}
	if change := observeListChange(t, lt, BeadChangeUpdated, open); change != ListChangeNone {
		t.Fatalf("late open snapshot: change = %v, want ListChangeNone", change)
	}
	// The stale notifications changed nothing: a real reopen still announces.
	if change := store.apply(t, lt, BeadChangeUpdated, open); change != ListChangeCreated {
		t.Fatalf("reopen after the stale notifications: change = %v, want ListChangeCreated", change)
	}
}

// A close the store does not show (a late close delivered after a reopen, or
// a close a stale read inferred) is not a close.
func TestListTransitionsStaleCloseAfterReopenIsSilent(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	closed := listTestSessionBead("gc-1", "closed", nil)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	store.apply(t, lt, BeadChangeClosed, closed)
	store.apply(t, lt, BeadChangeUpdated, listTestSessionBead("gc-1", "open", nil))

	if change := observeListChange(t, lt, BeadChangeClosed, closed); change != ListChangeNone {
		t.Fatalf("late close after the reopen: change = %v, want ListChangeNone", change)
	}
	if change := observeListChange(t, lt, BeadChangeUpdated, closed); change != ListChangeNone {
		t.Fatalf("late closed snapshot after the reopen: change = %v, want ListChangeNone", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, closed); change != ListChangeClosed {
		t.Fatalf("close after the stale notifications: change = %v, want ListChangeClosed", change)
	}
}

// A row the store no longer holds confirms a close (the session is not open)
// and refutes a creation.
func TestListTransitionsRowTheStoreDoesNotHold(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	delete(store.rows, "gc-1")
	if change := observeListChange(t, lt, BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("close of a row the store does not hold: change = %v, want ListChangeClosed", change)
	}
	if change := observeListChange(t, lt, BeadChangeCreated, listTestSessionBead("gc-2", "open", nil)); change != ListChangeNone {
		t.Fatalf("create of a row the store does not hold: change = %v, want ListChangeNone", change)
	}
}

// The transition reports the session as the store serves it, close reason
// included, not as the notification carried it.
func TestListTransitionsReportsTheServedRow(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	store.rows["gc-1"] = listTestSessionBead("gc-1", "closed", map[string]string{"template": "rig/worker", "close_reason": "orphaned"})

	transition, err := lt.Observe(BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil))
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if transition.Change != ListChangeClosed || transition.Session.Template != "rig/worker" || transition.CloseReason != "orphaned" {
		t.Fatalf("close = %+v, want ListChangeClosed with the served row's template and close reason", transition)
	}
}

// A transition the store cannot confirm is not announced, and nothing is
// remembered, so the notification counts once the row can be read.
func TestListTransitionsUnreadableRowReportsNothing(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	b := listTestSessionBead("gc-1", "open", nil)
	store.rows[b.ID] = b
	store.err = errors.New("backing unavailable")
	transition, err := lt.Observe(BeadChangeCreated, b)
	if err == nil || transition.Change != ListChangeNone {
		t.Fatalf("create with the row unreadable = (%+v, %v), want ListChangeNone and the read error", transition, err)
	}
	store.err = nil
	if change := observeListChange(t, lt, BeadChangeCreated, b); change != ListChangeCreated {
		t.Fatalf("create once the row reads: change = %v, want ListChangeCreated", change)
	}
}

// Non-session beads, a failed create (no row, so no notification carries a
// session bead) and an undecodable notification change nothing.
func TestListTransitionsIgnoreNonSessionBeads(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	for _, b := range []beads.Bead{
		{ID: "gc-2", Type: "task", Status: "open"},
		{ID: "gc-3", Type: "message", Status: "closed"},
		{ID: "gc-4", Type: "", Status: "open"},
		{ID: "", Type: BeadType, Status: "open"},
	} {
		for _, c := range []BeadChange{BeadChangeCreated, BeadChangeUpdated, BeadChangeClosed} {
			if change := store.apply(t, lt, c, b); change != ListChangeNone {
				t.Fatalf("Observe(%v, %+v) = %v, want ListChangeNone", c, b, change)
			}
		}
	}
	if store.reads != 0 {
		t.Fatalf("store reads = %d, want 0 for beads that are not sessions", store.reads)
	}
}

// A repairable session row (an empty type with the gc:session label, left by
// a crash or a migration) is listed with the sessions, so it is announced
// like one.
func TestListTransitionsRepairableSessionRow(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	open := listTestSessionBead("gc-1", "open", nil)
	open.Type = ""
	closed := open
	closed.Status = "closed"
	if change := store.apply(t, lt, BeadChangeCreated, open); change != ListChangeCreated {
		t.Fatalf("create of a repairable row: change = %v, want ListChangeCreated", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, closed); change != ListChangeClosed {
		t.Fatalf("close of a repairable row: change = %v, want ListChangeClosed", change)
	}
}

// A session row created already closed never joined the open set.
func TestListTransitionsCreatedClosedIsSilent(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	b := listTestSessionBead("gc-1", "closed", nil)
	if change := store.apply(t, lt, BeadChangeCreated, b); change != ListChangeNone {
		t.Fatalf("create of a closed row: change = %v, want ListChangeNone", change)
	}
	if change := store.apply(t, lt, BeadChangeClosed, b); change != ListChangeNone {
		t.Fatalf("close of a row created closed: change = %v, want ListChangeNone", change)
	}
}

// A delete forgets the row: an id the store reuses afterwards is a new session.
func TestListTransitionsDeleteForgetsTheRow(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(16)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	store.apply(t, lt, BeadChangeDeleted, beads.Bead{ID: "gc-1"})
	if change := store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil)); change != ListChangeCreated {
		t.Fatalf("create after delete: change = %v, want ListChangeCreated", change)
	}
}

// The remembered set is bounded: overflow forgets everything, which costs at
// most a repeated announcement per id. A forgotten open session's close is
// still announced.
func TestListTransitionsBoundedMemory(t *testing.T) {
	store := newListTestStore()
	lt := store.fold(2)
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-1", "open", nil))
	store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-2", "open", nil))
	if change := store.apply(t, lt, BeadChangeCreated, listTestSessionBead("gc-3", "open", nil)); change != ListChangeCreated {
		t.Fatalf("create past capacity: change = %v, want ListChangeCreated", change)
	}
	if got := lt.size(); got > 2 {
		t.Fatalf("remembered %d ids, want at most 2", got)
	}
	if change := store.apply(t, lt, BeadChangeClosed, listTestSessionBead("gc-1", "closed", nil)); change != ListChangeClosed {
		t.Fatalf("close of a forgotten id: change = %v, want ListChangeClosed", change)
	}
}
