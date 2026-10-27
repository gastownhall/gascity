package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// laneAwaitReread bounds how long AwaitConversationOp waits between reads of
// the op when no pass over its conversation ends.
const laneAwaitReread = time.Second

// errNoLaneHost is AwaitConversationOp's answer while no lane host runs.
var errNoLaneHost = errors.New("no conversation lane host runs")

// controllerState is the State the API's producer routes reach the
// conversation lane host through.
var _ api.ConversationLaneHost = (*controllerState)(nil)

// setConversationLaneHost installs the lane host the API reaches; nil
// removes it.
func (cs *controllerState) setConversationLaneHost(h *conversationLaneHost) {
	cs.laneHost.Store(h)
}

// ConversationLaneKinds is the op kinds the controller's lane host executes:
// the kinds [ops] authoritative listed when it started, while it runs, and
// nil while no host runs (none started, refused, off, or stopped). A config
// reload does not change them: the host latched them at its start.
func (cs *controllerState) ConversationLaneKinds() []string {
	if cs == nil {
		return nil
	}
	if h := cs.laneHost.Load(); h != nil {
		return h.runningKinds()
	}
	return nil
}

// PokeConversationLane asks the controller's lane host for a pass over
// conversation conv now, so an op the API just proposed or canceled does not
// wait for the host's anti-entropy scan. It does nothing while no host runs.
func (cs *controllerState) PokeConversationLane(conv string) {
	if cs == nil {
		return
	}
	if h := cs.laneHost.Load(); h != nil {
		h.Poke(conv)
	}
}

// runningKinds is the kinds h latched at its start while it runs, nil
// otherwise.
func (h *conversationLaneHost) runningKinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != laneHostStateRunning || (h.ctx != nil && h.ctx.Err() != nil) {
		return nil
	}
	return slices.Clone(h.kinds)
}

// AwaitConversationOp waits until op id of conversation conv closes, or ctx
// ends, and returns the op as last read. It wakes at the end of each pass
// the lane host runs over conv, and re-reads the op at least every
// laneAwaitReread. It answers errNoLaneHost while no host runs.
func (cs *controllerState) AwaitConversationOp(ctx context.Context, conv, id string) (opjournal.Op, error) {
	if cs == nil {
		return opjournal.Op{}, errNoLaneHost
	}
	h := cs.laneHost.Load()
	if h == nil {
		return opjournal.Op{}, errNoLaneHost
	}
	return h.awaitOp(ctx, conv, id)
}

// awaitOp is AwaitConversationOp over h.
func (h *conversationLaneHost) awaitOp(ctx context.Context, conv, id string) (opjournal.Op, error) {
	st, _, err := h.store()
	if err != nil {
		return opjournal.Op{}, err
	}
	for {
		op, done, err := h.awaitOpOnce(ctx, st, conv, id)
		if done {
			return op, err
		}
	}
}

// awaitOpOnce reads op id of conv and, while it is open, waits for the end
// of the next pass over conv, laneAwaitReread, or ctx. done reports that the
// await is over: the op closed, ctx ended, or the read failed (err).
func (h *conversationLaneHost) awaitOpOnce(ctx context.Context, st *opjournal.Store, conv, id string) (op opjournal.Op, done bool, err error) {
	// Taken before the read, so a pass that ends between the read and the
	// wait still wakes it, and released on every return, so an await that
	// ends with no pass over conv leaves no waiter behind.
	ended, release := h.passEnd(conv)
	defer release()
	op, err = st.Get(id)
	switch {
	case err != nil:
		return op, true, fmt.Errorf("reading op %s: %w", id, err)
	case op.Conversation != conv:
		return op, true, fmt.Errorf("op %s is not an op of conversation %s", id, conv)
	case op.Status == opjournal.StatusClosed:
		return op, true, nil
	}
	fire, stop := h.newTimer(laneAwaitReread)
	defer stop()
	select {
	case <-ctx.Done():
		return op, true, nil
	case <-ended:
	case <-fire:
	}
	return op, false, nil
}

// passEndWait is the channel the end of the next pass over a conversation
// closes, and how many awaits hold it.
type passEndWait struct {
	ch      chan struct{}
	waiters int
}

// passEnd is the channel the end of the next pass over conv closes, and the
// release its caller runs once it no longer waits on it. The pass's end
// drops the channel (passEndedLocked); so does the last release before it.
func (h *conversationLaneHost) passEnd(conv string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.passEnds == nil {
		h.passEnds = map[string]*passEndWait{}
	}
	w, ok := h.passEnds[conv]
	if !ok {
		w = &passEndWait{ch: make(chan struct{})}
		h.passEnds[conv] = w
	}
	w.waiters++
	return w.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.waiters--
		if w.waiters == 0 && h.passEnds[conv] == w {
			delete(h.passEnds, conv)
		}
	}
}

// passEndedLocked wakes the waiters on the end of a pass over conv
// (passEnd). h.mu is held.
func (h *conversationLaneHost) passEndedLocked(conv string) {
	if w, ok := h.passEnds[conv]; ok {
		close(w.ch)
		delete(h.passEnds, conv)
	}
}
