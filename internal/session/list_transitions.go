package session

import (
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// BeadChange names the ledger mutation a bead notification reports: the
// bead.created, bead.updated, bead.closed and bead.deleted events.
type BeadChange int

// The ledger mutations ListTransitions folds.
const (
	BeadChangeCreated BeadChange = iota + 1
	BeadChangeUpdated
	BeadChangeClosed
	BeadChangeDeleted
)

// ListChange is a change to the set of open sessions: a session joined it or
// left it.
type ListChange int

// The list changes Observe reports.
const (
	// ListChangeNone: the notification does not change the open-session set.
	ListChangeNone ListChange = iota
	// ListChangeCreated: a session joined the open set. Its bead was created
	// open or reopened, or the fold saw it open for the first time.
	ListChangeCreated
	// ListChangeClosed: an open session bead closed.
	ListChangeClosed
)

// ListTransition is what Observe reports for one notification.
type ListTransition struct {
	// Change is the change the notification makes to the open-session set.
	Change ListChange
	// Session is the session's persisted projection, read from the row that
	// confirmed the change. It is meaningful only when Change is not
	// ListChangeNone.
	Session Info
	// CloseReason is the close reason that row records (ClosePatch's
	// close_reason), "" when it records none.
	CloseReason string
}

// ServedRow reads a session bead as its store serves it now, after the
// notification being folded was applied to the controller's caches: the row
// a client reads when it lists the sessions. found is false when the store
// holds no such row.
type ServedRow func(id string) (b beads.Bead, found bool, err error)

// DefaultListTransitionsCapacity bounds the session ids a ListTransitions
// remembers. Overflow forgets them all, which costs at most one repeated
// announcement per id.
const DefaultListTransitionsCapacity = 1 << 16

// ListTransitions folds session-bead ledger notifications into the changes
// they make to the set of open sessions, each reported once. The ledger's
// notification stream can carry one mutation more than once (a writer's own
// event and the controller cache's inferred one, a refresh's announcement and
// the create's), so it remembers, per session id, whether it last reported
// the row open or closed:
//
//   - a notification carrying an open row is a creation unless the row is
//     known open: a create, a reopen, or the first sight of a session that
//     opened before the fold started or that overflow forgot, which a client
//     that listed the sessions already shows;
//   - a close is a close unless the row is known closed, an id it never saw
//     included, since the notification itself is the transition;
//   - an update carrying a closed row is a close only when the row is known
//     open. One of an id it never saw is neither reported nor remembered, so
//     the bead.closed that may follow it still reports the close;
//   - a create of a closed row is remembered, never reported: the row was
//     never open;
//   - a delete forgets the id.
//
// The stream can also carry a notification that is no longer true: a
// writer's snapshot that lost a race with a close, an inferred close the
// backing never made, an echo delivered after a later change. So a
// notification that would report a transition is first confirmed against the
// row its store serves (ServedRow). If that row is not in the state the
// notification reports, the notification reports and remembers nothing. A
// row the store does not hold confirms only a close.
//
// Notifications for beads that are not session beads change nothing. It is
// safe for concurrent use.
type ListTransitions struct {
	served   ServedRow
	mu       sync.Mutex
	closed   map[string]bool
	capacity int
}

// NewListTransitions returns a ListTransitions that confirms each transition
// against served and remembers at most capacity session ids
// (DefaultListTransitionsCapacity when capacity < 1).
func NewListTransitions(capacity int, served ServedRow) *ListTransitions {
	if capacity < 1 {
		capacity = DefaultListTransitionsCapacity
	}
	return &ListTransitions{served: served, closed: map[string]bool{}, capacity: capacity}
}

// Observe folds one notification, b being the bead's post-image (for a
// delete, only its ID need be set), and returns the list change it makes. A
// non-nil error says the row that would confirm the change could not be
// read; the notification then reports and remembers nothing.
func (t *ListTransitions) Observe(change BeadChange, b beads.Bead) (ListTransition, error) {
	id := strings.TrimSpace(b.ID)
	if id == "" {
		return ListTransition{}, nil
	}
	t.mu.Lock()
	if change == BeadChangeDeleted {
		delete(t.closed, id)
		t.mu.Unlock()
		return ListTransition{}, nil
	}
	if !IsSessionBeadOrRepairable(b) {
		t.mu.Unlock()
		return ListTransition{}, nil
	}
	isClosed := change == BeadChangeClosed || b.Status == "closed"
	if t.changeLocked(id, change, isClosed) == ListChangeNone {
		t.recordLocked(id, change, isClosed)
		t.mu.Unlock()
		return ListTransition{}, nil
	}
	t.mu.Unlock()

	// The read is made outside the lock: it can reach the backing store.
	row, found, err := t.served(id)
	if err != nil {
		return ListTransition{}, err
	}
	if found {
		if servedOpen := IsSessionBeadOrRepairable(row) && row.Status != "closed"; servedOpen == isClosed {
			return ListTransition{}, nil
		}
		b = row
	} else if !isClosed {
		return ListTransition{}, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	result := t.changeLocked(id, change, isClosed)
	t.recordLocked(id, change, isClosed)
	if result == ListChangeNone {
		return ListTransition{}, nil
	}
	return ListTransition{
		Change:      result,
		Session:     infoFromPersistedBead(b),
		CloseReason: strings.TrimSpace(b.Metadata["close_reason"]),
	}, nil
}

// changeLocked is the change a notification of id in state isClosed makes,
// given the state the fold remembers for id. Caller must hold t.mu.
func (t *ListTransitions) changeLocked(id string, change BeadChange, isClosed bool) ListChange {
	wasClosed, known := t.closed[id]
	switch {
	case !isClosed && (!known || wasClosed):
		return ListChangeCreated
	case isClosed && known && !wasClosed:
		return ListChangeClosed
	case isClosed && !known && change == BeadChangeClosed:
		// The close of a row the fold never saw: the notification itself is
		// the transition. A row created closed is known closed, so its close
		// is not reported.
		return ListChangeClosed
	}
	return ListChangeNone
}

// recordLocked remembers id's state from a notification, except an update
// carrying a closed row of an id the fold never saw: remembered closed, it
// would swallow the bead.closed that may follow it. Caller must hold t.mu.
func (t *ListTransitions) recordLocked(id string, change BeadChange, isClosed bool) {
	if _, known := t.closed[id]; !known && isClosed && change == BeadChangeUpdated {
		return
	}
	t.remember(id, isClosed)
}

// remember records id's last observed state, forgetting every id first when
// a new one would exceed the capacity.
func (t *ListTransitions) remember(id string, closed bool) {
	if _, ok := t.closed[id]; !ok && len(t.closed) >= t.capacity {
		t.closed = map[string]bool{}
	}
	t.closed[id] = closed
}

func (t *ListTransitions) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.closed)
}
