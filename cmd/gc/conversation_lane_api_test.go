package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// A proposal or cancel through the API rings the running lane host through
// the controller state.
func TestControllerStatePokesTheConversationLane(t *testing.T) {
	cs := &controllerState{}
	cs.PokeConversationLane("gc-1") // no host: a no-op
	var got []string
	cs.setConversationLanePoke(func(conv string) bool { got = append(got, conv); return true })
	var state api.State = cs
	poker, ok := state.(api.ConversationLanePoker)
	if !ok {
		t.Fatal("controllerState does not implement api.ConversationLanePoker")
	}
	poker.PokeConversationLane("gc-1")
	cs.setConversationLanePoke(nil)
	poker.PokeConversationLane("gc-2")
	if len(got) != 1 || got[0] != "gc-1" {
		t.Fatalf("pokes = %q, want [gc-1]", got)
	}
}
