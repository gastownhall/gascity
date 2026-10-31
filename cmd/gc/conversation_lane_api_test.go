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

// awaitOp wakes at the end of a pass over its conversation, not only at its
// re-read: with a re-read timer that never fires, an op a pass closed is
// returned once that pass ends.
func TestConversationLaneHostAwaitOpWakesAtPassEnd(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message", "interrupt")
	st, _, err := hh.h.store()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	op := f.propose(t, st, f.msg("now-1", opjournal.RolePrompt, "hello"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	got := make(chan opjournal.Op, 1)
	go func() {
		o, err := hh.h.awaitOp(ctx, f.id, op.ID)
		if err != nil {
			t.Errorf("awaitOp: %v", err)
		}
		got <- o
	}()
	// The await read the op open and waits on its re-read timer, which this
	// test never fires.
	if tm := hh.nextTimer(t); tm.d != laneAwaitReread {
		t.Fatalf("await timer = %v, want the re-read's %v", tm.d, laneAwaitReread)
	}
	if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("closing op: %v", err)
	}
	hh.h.notePass(f.id, opjournal.PassResult{}, nil)
	select {
	case o := <-got:
		if o.Status != opjournal.StatusClosed {
			t.Fatalf("await = %+v, want the op closed", o)
		}
	case <-time.After(laneWaitBudget):
		t.Fatal("the await did not wake at the pass's end")
	}
}

// An await whose context ends reads the op once more before it answers, so
// an op that closed while it waited, with no pass end to wake it, is
// returned closed rather than as the open op it read before the wait.
func TestConversationLaneHostAwaitOpRereadsWhenItsContextEnds(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message", "interrupt")
	st, _, err := hh.h.store()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	op := f.propose(t, st, f.msg("now-1", opjournal.RolePrompt, "hello"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	got := make(chan opjournal.Op, 1)
	go func() {
		o, err := hh.h.awaitOp(ctx, f.id, op.ID)
		if err != nil {
			t.Errorf("awaitOp: %v", err)
		}
		got <- o
	}()
	// The await read the op open and waits on its re-read timer, which this
	// test never fires.
	hh.nextTimer(t)
	if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("closing op: %v", err)
	}
	cancel()
	select {
	case o := <-got:
		if o.Status != opjournal.StatusClosed {
			t.Fatalf("await = %+v, want the op closed", o)
		}
	case <-time.After(laneWaitBudget):
		t.Fatal("the await did not return when its context ended")
	}
}

// An await that returns with no pass over its conversation leaves no
// pass-end waiter behind: one that found its op closed, and one whose
// context ended. One leaving does not take the channel from another await
// still waiting on it, which the pass's end then wakes.
func TestConversationLaneHostAwaitOpLeavesNoWaiter(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message", "interrupt")
	st, _, err := hh.h.store()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	closed := f.propose(t, st, f.msg("closed-1", opjournal.RolePrompt, "hello"))
	if _, err := st.Close(closed.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("closing op: %v", err)
	}
	if got, err := hh.h.awaitOp(context.Background(), f.id, closed.ID); err != nil || got.Status != opjournal.StatusClosed {
		t.Fatalf("await of a closed op = %+v, %v; want it closed", got, err)
	}
	if n := passEndsHeld(hh.h); n != 0 {
		t.Fatalf("an await of a closed op left %d pass-end channels", n)
	}

	// awaitUntil awaits op id in the background until ctx ends; it returns
	// once the await waits on its re-read timer, which this test never
	// fires, so only the end of a pass or ctx wakes it.
	awaitUntil := func(ctx context.Context, id string) <-chan opjournal.Op {
		t.Helper()
		got := make(chan opjournal.Op, 1)
		go func() {
			o, err := hh.h.awaitOp(ctx, f.id, id)
			if err != nil {
				t.Errorf("awaitOp(%s): %v", id, err)
			}
			got <- o
		}()
		hh.nextTimer(t)
		return got
	}
	returned := func(what string, got <-chan opjournal.Op) opjournal.Op {
		t.Helper()
		select {
		case o := <-got:
			return o
		case <-time.After(laneWaitBudget):
			t.Fatalf("%s did not return", what)
			return opjournal.Op{}
		}
	}
	leaving := f.propose(t, st, f.msg("open-1", opjournal.RolePrompt, "hello"))
	ctx, cancel := context.WithCancel(context.Background())
	sole := awaitUntil(ctx, leaving.ID)
	cancel()
	if o := returned("an await whose context ended", sole); o.Status == opjournal.StatusClosed {
		t.Fatalf("await = %+v, want the op open when its context ended", o)
	}
	if n := passEndsHeld(hh.h); n != 0 {
		t.Fatalf("an await whose context ended left %d pass-end channels", n)
	}

	staying := f.propose(t, st, f.msg("open-2", opjournal.RolePrompt, "hello"))
	ctx, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	left := awaitUntil(ctx, leaving.ID)
	stays := awaitUntil(context.Background(), staying.ID)
	cancel()
	returned("the await that left", left)
	if _, err := st.Close(staying.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:test"); err != nil {
		t.Fatalf("closing op: %v", err)
	}
	hh.h.notePass(f.id, opjournal.PassResult{}, nil)
	if o := returned("the await still waiting at the pass's end", stays); o.Status != opjournal.StatusClosed {
		t.Fatalf("await = %+v, want the op closed by the pass", o)
	}
	if n := passEndsHeld(hh.h); n != 0 {
		t.Fatalf("finished awaits left %d pass-end channels", n)
	}
}

// passEndsHeld is how many conversations h holds a pass-end channel for.
func passEndsHeld(h *conversationLaneHost) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.passEnds)
}
