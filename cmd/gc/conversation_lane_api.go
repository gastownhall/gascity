package main

import (
	"slices"

	"github.com/gastownhall/gascity/internal/api"
)

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
