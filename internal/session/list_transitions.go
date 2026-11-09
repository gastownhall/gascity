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
	// ListChangeCreated: a session bead was created open, or a closed session
	// bead was reopened.
	ListChangeCreated
	// ListChangeClosed: an open session bead closed.
	ListChangeClosed
)

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
//   - a create of an open row is a creation unless the row is known open;
//   - a close is a close unless the row is known closed, an id it never saw
//     included, since the notification itself is the transition;
//   - an update carrying an open row is a creation (a reopen) only when the
//     row is known closed, and one carrying a closed row is a close only when
//     the row is known open: an update of an id it never saw says nothing
//     about a transition, so it is remembered, not reported;
//   - a delete forgets the id.
//
// Notifications for beads that are not session beads change nothing. It is
// safe for concurrent use.
type ListTransitions struct {
	mu       sync.Mutex
	closed   map[string]bool
	capacity int
}

// NewListTransitions returns a ListTransitions that remembers at most
// capacity session ids (DefaultListTransitionsCapacity when capacity < 1).
func NewListTransitions(capacity int) *ListTransitions {
	if capacity < 1 {
		capacity = DefaultListTransitionsCapacity
	}
	return &ListTransitions{closed: map[string]bool{}, capacity: capacity}
}

// Observe folds one notification, b being the bead's post-image (for a
// delete, only its ID need be set), and returns the list change it makes
// with the session's persisted projection. The projection is meaningful only
// when the change is not ListChangeNone.
func (t *ListTransitions) Observe(change BeadChange, b beads.Bead) (ListChange, Info) {
	id := strings.TrimSpace(b.ID)
	if id == "" {
		return ListChangeNone, Info{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if change == BeadChangeDeleted {
		delete(t.closed, id)
		return ListChangeNone, Info{}
	}
	if b.Type != BeadType {
		return ListChangeNone, Info{}
	}
	isClosed := change == BeadChangeClosed || b.Status == "closed"
	wasClosed, known := t.closed[id]
	t.remember(id, isClosed)
	var result ListChange
	switch {
	case change == BeadChangeCreated && !isClosed && (!known || wasClosed):
		result = ListChangeCreated
	case change == BeadChangeClosed && (!known || !wasClosed):
		// A row created closed was never open: its create remembered it
		// closed, so this close is not reported.
		result = ListChangeClosed
	case change == BeadChangeUpdated && known && wasClosed && !isClosed:
		result = ListChangeCreated
	case change == BeadChangeUpdated && known && !wasClosed && isClosed:
		result = ListChangeClosed
	default:
		return ListChangeNone, Info{}
	}
	return result, infoFromPersistedBead(b)
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

// CloseReasonOf returns the canonical close reason a closed session bead
// records (ClosePatch's close_reason), "" when it records none.
func CloseReasonOf(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata["close_reason"])
}
