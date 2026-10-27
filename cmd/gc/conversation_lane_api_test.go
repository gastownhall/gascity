package main

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/session"
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
	h := cs.startConversationLaneHost(ctx, reconcilerLegacy)
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
		h := newConversationLaneHost(cs, []string{"message"}, session.StopRequestsLive)
		h.state = state
		cs.setConversationLaneHost(h)
		if got := cs.ConversationLaneKinds(); got != nil {
			t.Fatalf("kinds of a %s host = %v", state, got)
		}
	}
}

// AwaitConversationOp returns once the lane host's pass closes the op, waking
// at the pass's end, and the op as read, still open, when its context ends
// first; with no host it answers errNoLaneHost.
func TestControllerAwaitConversationOpReturnsAtClose(t *testing.T) {
	if _, err := (&controllerState{}).AwaitConversationOp(context.Background(), "gc-1", "op-1"); !errors.Is(err, errNoLaneHost) {
		t.Fatalf("await with no host = %v, want errNoLaneHost", err)
	}
	f := newLaneFixture(t)
	f.cfg.Ops.Authoritative = []string{"message", "interrupt"}
	cs := &controllerState{cityBeadStore: f.store, cfg: f.cfg, sp: f.sp, cityPath: f.dir, beadsQuiescent: new(atomic.Bool)}
	ctx, cancel := context.WithCancel(context.Background())
	h := cs.startConversationLaneHost(ctx, reconcilerLegacy)
	t.Cleanup(func() { cancel(); h.wg.Wait() })
	// The host runs on the real clock, so the ops are proposed on it too.
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: laneLiveSessions{Store: f.store}}, clock.Real{})
	if err != nil {
		t.Fatalf("NewLaneStore: %v", err)
	}
	held := f.propose(t, st, f.msg("held-1", opjournal.RolePrompt, "hello", func(r *opjournal.Request) {
		r.NotBefore = time.Now().Add(time.Hour)
	}))
	short, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	got, err := cs.AwaitConversationOp(short, f.id, held.ID)
	if err != nil || got.Status == opjournal.StatusClosed {
		t.Fatalf("await of an op that cannot run = %+v, %v; want it open when the context ends", got, err)
	}
	op := f.propose(t, st, f.msg("now-1", opjournal.RolePrompt, "hello"))
	cs.PokeConversationLane(f.id)
	long, stopLong := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopLong()
	got, err = cs.AwaitConversationOp(long, f.id, op.ID)
	if err != nil || got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("await = %+v, %v; want it closed succeeded by the host's pass", got.Outcome, err)
	}
}
