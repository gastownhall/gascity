package session

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
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
// holds without maintaining it: the city's file queue and StateFileQueue are
// snapshotters. The beads queue's Snapshot is a whole-city diagnostic (every
// record listed and decoded, failing on any one it cannot decode), not a
// read for every send: before that queue backs a Manager, the startup-turn
// barrier needs a keyed read of the session's own items there.
type deferredSubmitSnapshotter interface {
	Snapshot() (nudgequeue.State, error)
}

// StartupTurnQueued reports whether session id has a startup turn in its
// queue that a send made under ctx would reach the agent ahead of
// (startupTurnAhead). It is that barrier for a send that types into the
// session's runtime without the Manager (worker.RuntimeHandle, which the
// queue's own deliveries, the conversation lane, mail and sling notices take
// to a running session), as the Manager's own sends apply it. It only reads:
// it takes no lock and repairs nothing. A row that does not exist holds
// nothing, as for a handle without one; a row it cannot read holds the send,
// logged, as a queue it cannot read does.
func (m *Manager) StartupTurnQueued(ctx context.Context, id string) bool {
	b, err := m.store.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return false
	}
	if err != nil {
		log.Printf("session: %s: reading the row for a startup turn to send behind: %v; holding the send", id, err)
		return true
	}
	return m.startupTurnAhead(ctx, b, sessionName(id, b))
}

// startupTurnAhead reports whether b's session sessName still has a startup
// turn in the queue that a send made under ctx would reach the agent ahead
// of (nudgequeue.HasLiveStartupTurn): the first turn of a launch that handed
// it back, such as every un-welded exec launch, which the queue delivers once
// the agent takes input. The turn holds every send while it waits and while
// the queue delivers it (in flight), except that delivery itself: the send
// made under the delivery key (runtime.NudgeDeliveryKey) of the batch the
// claim pinned the turn to. A held send queues its text behind the turn, or
// reports it undelivered, instead of typing it. It takes one pure snapshot of
// the queue, never the maintaining listing, and looks under the keys the
// session's items are queued under, as the claim does: its id, its alias and
// the aliases it had. A queue that cannot snapshot reads as no turn, and one
// whose read fails reads as a turn, logged, since a queued send is delivered
// in order either way. It needs no lock: the Manager's sends call it under
// the session's mutation lock only because they hold it already.
func (m *Manager) startupTurnAhead(ctx context.Context, b beads.Bead, sessName string) bool {
	snap, ok := m.deferredQueue().(deferredSubmitSnapshotter)
	if !ok || b.ID == "" {
		return false
	}
	state, err := snap.Snapshot()
	if err != nil {
		log.Printf("session: %q: reading the queue for a startup turn to send behind: %v; holding the send", sessName, err)
		return true
	}
	keys := map[string]bool{b.ID: true}
	for _, alias := range append([]string{b.Metadata["alias"]}, AliasHistory(b.Metadata)...) {
		if alias = strings.TrimSpace(alias); alias != "" {
			keys[alias] = true
		}
	}
	var mine []nudgequeue.Item
	for _, bucket := range [][]nudgequeue.Item{state.Pending, state.InFlight} {
		for _, item := range bucket {
			if keys[item.Agent] {
				mine = append(mine, item)
			}
		}
	}
	return nudgequeue.HasLiveStartupTurn(b.ID, runtime.NudgeDeliveryKey(ctx), mine, time.Now().UTC())
}
