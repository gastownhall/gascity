package main

import "github.com/gastownhall/gascity/internal/api"

// controllerState is the State the API's producer routes ring the
// conversation lane host through.
var _ api.ConversationLanePoker = (*controllerState)(nil)

// setConversationLanePoke installs the running lane host's Poke; nil removes
// it.
func (cs *controllerState) setConversationLanePoke(poke func(conv string) bool) {
	if poke == nil {
		cs.lanePoke.Store(nil)
		return
	}
	cs.lanePoke.Store(&poke)
}

// PokeConversationLane asks the controller's lane host for a pass over
// conversation conv now, so an op the API just proposed or canceled does not
// wait for the host's anti-entropy scan. It does nothing while no host runs.
func (cs *controllerState) PokeConversationLane(conv string) {
	if cs == nil {
		return
	}
	if poke := cs.lanePoke.Load(); poke != nil {
		(*poke)(conv)
	}
}
