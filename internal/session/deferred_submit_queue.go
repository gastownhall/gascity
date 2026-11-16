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

// deferredSubmitLister is a DeferredSubmitQueue that also lists what it
// holds for a queue key: pending, in flight and dead, as of now. The city's
// nudge queues (file and beads) and StateFileQueue are listers.
type deferredSubmitLister interface {
	ListForAgent(agent string, now time.Time) (pending, inFlight, dead []nudgequeue.Item, err error)
}

// startupTurnQueuedLocked reports whether b's session sessName still has a
// startup turn waiting in the queue (nudgequeue.HasLiveStartupTurn): the
// first turn of a launch that handed it back, such as every un-welded exec
// launch, which is delivered from the queue once the agent takes input. A
// send typed into the running session now would reach the agent ahead of
// it, so the send queues its text behind it instead. It reads the keys the
// session's items are queued under (its alias, else its id); a queue that
// cannot list reads as no turn, and one whose read fails reads as a turn,
// since a queued send is delivered in order either way.
func (m *Manager) startupTurnQueuedLocked(b beads.Bead, sessName string) bool {
	lister, ok := m.deferredQueue().(deferredSubmitLister)
	if !ok || b.ID == "" {
		return false
	}
	now := time.Now().UTC()
	keys := []string{b.ID}
	if alias := strings.TrimSpace(b.Metadata["alias"]); alias != "" && alias != b.ID {
		keys = append(keys, alias)
	}
	for _, key := range keys {
		pending, inFlight, _, err := lister.ListForAgent(key, now)
		if err != nil {
			log.Printf("session: %q: reading the queue for a startup turn to send behind: %v; queueing the send", sessName, err)
			return true
		}
		if nudgequeue.HasLiveStartupTurn(b.ID, pending, now) || nudgequeue.HasLiveStartupTurn(b.ID, inFlight, now) {
			return true
		}
	}
	return false
}
