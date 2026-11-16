package session

import (
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// DeferredSubmitQueue is where a Manager queues a submit the session cannot
// take yet. The city's nudge queue satisfies it.
type DeferredSubmitQueue interface {
	EnqueueDeferred(nudgequeue.Item) error
}

// WithDeferredSubmitQueue routes deferred submits into q. Without it, a
// Manager with a city path appends them to the city's state.json queue. A nil
// q is ignored. The city path still decides whether the Manager defers at all.
func WithDeferredSubmitQueue(q DeferredSubmitQueue) ManagerOption {
	return func(m *Manager) {
		if q != nil {
			m.deferredSubmits = q
		}
	}
}

// errNoDeferredQueue refuses a deferred submit from a Manager with no city
// path.
var errNoDeferredQueue = errors.New("deferred submit is unavailable without a city path")

// deferredQueue returns where the Manager queues deferred submits, or nil when
// it has no city path and so defers none.
func (m *Manager) deferredQueue() DeferredSubmitQueue {
	if strings.TrimSpace(m.cityPath) == "" {
		return nil
	}
	if m.deferredSubmits != nil {
		return m.deferredSubmits
	}
	return nudgequeue.StateFileQueue{CityPath: m.cityPath}
}

// deferredSubmitSnapshotter is a DeferredSubmitQueue that can read what it
// holds without maintaining it: the city's nudge queues (file and beads) and
// StateFileQueue are snapshotters.
type deferredSubmitSnapshotter interface {
	Snapshot() (nudgequeue.State, error)
}

// startupTurnQueuedLocked reports whether b's session sessName still has a
// startup turn waiting in the queue (nudgequeue.HasLiveStartupTurn): the
// first turn of a launch that handed it back, such as every un-welded exec
// launch, which is delivered from the queue once the agent takes input. A
// send typed into the running session now would reach the agent ahead of
// it, so the send queues its text behind it, or reports it undelivered,
// instead. inFlightToo counts a turn the queue is delivering now as well; a
// live-only send leaves it out, since the queue's own delivery of the turn,
// and of the items it holds behind the turn, runs with the turn in flight.
// It takes one pure snapshot of the queue, never the maintaining listing,
// and looks under the keys the session's items are queued under (its alias
// and its id). A queue that cannot snapshot reads as no turn, and one whose
// read fails reads as a turn, logged, since a queued send is delivered in
// order either way.
func (m *Manager) startupTurnQueuedLocked(b beads.Bead, sessName string, inFlightToo bool) bool {
	snap, ok := m.deferredQueue().(deferredSubmitSnapshotter)
	if !ok || b.ID == "" {
		return false
	}
	state, err := snap.Snapshot()
	if err != nil {
		log.Printf("session: %q: reading the queue for a startup turn to send behind: %v; holding the send", sessName, err)
		return true
	}
	alias := strings.TrimSpace(b.Metadata["alias"])
	mine := func(items []nudgequeue.Item) []nudgequeue.Item {
		var out []nudgequeue.Item
		for _, item := range items {
			if item.Agent == b.ID || (alias != "" && item.Agent == alias) {
				out = append(out, item)
			}
		}
		return out
	}
	now := time.Now().UTC()
	if nudgequeue.HasLiveStartupTurn(b.ID, mine(state.Pending), now) {
		return true
	}
	return inFlightToo && nudgequeue.HasLiveStartupTurn(b.ID, mine(state.InFlight), now)
}
