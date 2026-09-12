package api

import (
	"context"

	"github.com/gastownhall/gascity/internal/events"
)

// eventStreamPrefetch bounds how far the reader goroutine behind an event SSE
// stream may run ahead of the sender. The buffer's depth doubles as the
// stream's backlog signal: while it holds anything, the subscriber is behind
// the city and per-event enrichment must not deepen the lag.
const eventStreamPrefetch = 64

// streamResult is one watcher result handed to a stream's send loop. A
// non-nil err is terminal and is the last value on the channel.
type streamResult[T any] struct {
	event T
	err   error
}

// readEventsAhead drains next on its own goroutine into a buffered channel.
//
// Reading ahead keeps the watcher moving while the sender is busy encoding,
// flushing to a slow client, or projecting, and it makes len(ch) a live
// measure of how far behind the subscriber is. The goroutine forwards a
// terminal error and then exits; it also exits when ctx is canceled, which
// happens no later than the HTTP handler returning.
func readEventsAhead[T any](ctx context.Context, next func() (T, error)) <-chan streamResult[T] {
	ch := make(chan streamResult[T], eventStreamPrefetch)
	go func() {
		defer close(ch)
		for {
			event, err := next()
			select {
			case ch <- streamResult[T]{event: event, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// projectWorkflowEventWithSlack computes an event's optional workflow
// projection only while the subscriber is keeping up (backlog == 0).
//
// Resolving a projection reads the bead stores: when the event payload does not
// identify the subject as a workflow bead, every store is probed, and a store
// Get that misses the cache falls through to the backing store — for a
// bd-backed rig, a subprocess. Computing that inline for every
// bead.created/updated/closed event made delivery hostage to bead-store
// latency: one slow store stalled the whole stream head-of-line, and a
// subscriber received a small fraction of the log wherever its cursor sat.
//
// Delivery is the stream's contract; the projection is enrichment. It is
// already absent whenever it cannot be resolved, and it carries requires_resync
// so a consumer re-reads the workflow regardless. Dropping it while the stream
// is behind costs a consumer nothing and lets the stream catch up, after which
// projections resume on their own.
func projectWorkflowEventWithSlack(state State, event events.Event, backlog int) *workflowEventProjection {
	if backlog > 0 {
		return nil
	}
	return projectWorkflowEvent(state, event)
}
