package session

import (
	"strings"

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
