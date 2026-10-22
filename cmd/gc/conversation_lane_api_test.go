package main

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// The API reaches the lane host the controller started: its kinds are the
// ones the host latched while it runs, a config change does not move them,
// and they are gone once the host stops. Pokes ring the host.
func TestControllerStateExposesTheRunningLaneHost(t *testing.T) {
	cs := &controllerState{}
	if got := cs.ConversationLaneKinds(); got != nil {
		t.Fatalf("kinds with no host = %v", got)
	}
	cs.PokeConversationLane("gc-1") // no host: a no-op
	var state api.State = cs
	if _, ok := state.(api.ConversationLaneHost); !ok {
		t.Fatal("controllerState does not implement api.ConversationLaneHost")
	}

	f := newLaneFixture(t)
	f.cfg.Ops.Authoritative = []string{"message"}
	cs = &controllerState{cityBeadStore: f.store, cfg: f.cfg, sp: f.sp, cityPath: f.dir, beadsQuiescent: new(atomic.Bool)}
	ctx, cancel := context.WithCancel(context.Background())
	h := cs.startConversationLaneHost(ctx)
	t.Cleanup(func() { cancel(); h.wg.Wait() })
	if cs.laneHost.Load() != h {
		t.Fatal("startConversationLaneHost installed no lane host for the API")
	}
	if got := cs.ConversationLaneKinds(); !slices.Equal(got, []string{"message"}) {
		t.Fatalf("kinds = %v, want [message]", got)
	}
	f.cfg.Ops.Authoritative = nil // a reload: the host keeps its start-time kinds
	if got := cs.ConversationLaneKinds(); !slices.Equal(got, []string{"message"}) {
		t.Fatalf("kinds after a reload = %v, want [message]", got)
	}
	cancel()
	h.wg.Wait()
	if got := cs.ConversationLaneKinds(); got != nil {
		t.Fatalf("kinds of a stopped host = %v", got)
	}
}

// A host that refused to start (a sessions store without guarded batches)
// or is off executes nothing, so the API refuses its kinds.
func TestControllerStateRefusedLaneHostExecutesNothing(t *testing.T) {
	for _, state := range []string{laneHostStateRefused, laneHostStateOff, laneHostStateUnavailable} {
		cs := &controllerState{}
		h := newConversationLaneHost(cs, []string{"message"})
		h.state = state
		cs.setConversationLaneHost(h)
		if got := cs.ConversationLaneKinds(); got != nil {
			t.Fatalf("kinds of a %s host = %v", state, got)
		}
	}
}
