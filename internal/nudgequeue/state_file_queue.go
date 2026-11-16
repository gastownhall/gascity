package nudgequeue

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// StateFileQueue is the verbs of the city's state.json nudge queue that
// packages below cmd/gc use: the deferred-submit append, the wait-nudge
// withdrawal and a lock-free per-agent listing. cmd/gc's file queue
// delegates the first two here, so each has exactly one implementation.
type StateFileQueue struct {
	// CityPath is the city whose queue this is.
	CityPath string
	// Shadows is the nudges-class store holding the queue's shadow beads; the
	// zero handle means the withdrawal removes queue entries and touches no
	// shadow.
	Shadows beads.NudgesStore
}

// EnqueueDeferred appends item to the pending bucket. It runs no maintenance,
// writes no shadow and does not check for a duplicate id: a deferred submit
// mints a fresh id for every message.
func (q StateFileQueue) EnqueueDeferred(item Item) error {
	return WithStateFor(q.CityPath, "enqueue_deferred", func(state *State) error {
		state.Pending = append(state.Pending, item)
		SortState(state)
		return nil
	})
}

// WithdrawQueuedWaitNudges withdraws the pending and in-flight wait nudges
// ids (WithdrawWaitNudges).
func (q StateFileQueue) WithdrawQueuedWaitNudges(ids []string) error {
	return WithdrawWaitNudges(q.Shadows, q.CityPath, ids)
}

// ListForAgent reads, without the lock, the items queued for agent: pending
// and in flight as of now, and dead, which includes a live item past its
// deliver-by deadline. It runs no maintenance, so it may be stale.
func (q StateFileQueue) ListForAgent(agent string, now time.Time) (pending, inFlight, dead []Item, err error) {
	state, err := LoadState(q.CityPath)
	if err != nil {
		return nil, nil, nil, err
	}
	live := func(items []Item) []Item {
		var out []Item
		for _, item := range items {
			switch {
			case item.Agent != agent:
			case expiredAt(item, now):
				dead = append(dead, item)
			default:
				out = append(out, item)
			}
		}
		return out
	}
	pending, inFlight = live(state.Pending), live(state.InFlight)
	for _, item := range state.Dead {
		if item.Agent == agent {
			dead = append(dead, item)
		}
	}
	return pending, inFlight, dead, nil
}
