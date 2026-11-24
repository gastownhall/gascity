package opjournal

import (
	"errors"
	"testing"
)

// RecentClosed lists a conversation's closed lane ops of one kind, newest
// first, within its capped window, and leaves out open ops, other kinds,
// legacy ops and other conversations; ClosedAt is the close transition's
// time.
func TestRecentClosed(t *testing.T) {
	h := newLaneHarness(t)
	propose := func(conv, key string, exec ExecutorKind) Op {
		r := laneMsg(key, RoleNotice, "note")
		r.Conversation, r.Executor = conv, exec
		return h.propose(t, r)
	}
	closeOp := func(op Op, arm Arm) Op {
		closed, err := h.store.Close(op.ID, Outcome{Arm: arm, Reason: "expired"}, "lane:ctl")
		if err != nil {
			t.Fatal(err)
		}
		return closed
	}
	first := closeOp(propose("gc-1", "k1", ExecutorLane), ArmFailed)
	propose("gc-1", "k2", ExecutorLane)
	legacy := propose("gc-1", "k3", ExecutorLegacyCLI)
	if _, err := h.store.Close(legacy.ID, Outcome{Arm: ArmSucceeded}, "legacy"); err != nil {
		t.Fatal(err)
	}
	closeOp(propose("gc-2", "k4", ExecutorLane), ArmFailed)
	last := closeOp(propose("gc-1", "k5", ExecutorLane), ArmSkipped)
	// An interrupt and a legacy message created and closed after the newest
	// message do not take its place in the window: the cap counts only the
	// kind's lane ops.
	stop := closeOp(h.propose(t, laneInterrupt("stop-1", "")), ArmFailed)
	later := propose("gc-1", "k6", ExecutorLegacyCLI)
	if _, err := h.store.Close(later.ID, Outcome{Arm: ArmFailed, Reason: "expired"}, "legacy"); err != nil {
		t.Fatal(err)
	}

	got, quarantined, err := h.store.RecentClosed("gc-1", KindMessage, 10)
	if err != nil || len(quarantined) != 0 {
		t.Fatalf("RecentClosed = %v, quarantined %+v", err, quarantined)
	}
	if len(got) != 2 || got[0].ID != last.ID || got[1].ID != first.ID {
		t.Fatalf("closed = %+v, want %s then %s", got, last.ID, first.ID)
	}
	if at := got[0].ClosedAt(); at.IsZero() || !at.Equal(last.ClosedAt()) {
		t.Fatalf("ClosedAt = %v, want the close transition's time %v", at, last.ClosedAt())
	}
	window, _, err := h.store.RecentClosed("gc-1", KindMessage, 1)
	if err != nil || len(window) != 1 || window[0].ID != last.ID {
		t.Fatalf("capped window = %+v, %v; want only the newest message", window, err)
	}
	if stops, _, err := h.store.RecentClosed("gc-1", KindInterrupt, 10); err != nil || len(stops) != 1 || stops[0].ID != stop.ID {
		t.Fatalf("closed interrupts = %+v, %v; want only %s", stops, err, stop.ID)
	}
	if open := propose("gc-3", "k7", ExecutorLane); !open.ClosedAt().IsZero() {
		t.Fatal("an open op has a close time")
	}
	if _, _, err := h.store.RecentClosed("gc-1", KindMessage, 0); err == nil {
		t.Fatal("RecentClosed with no limit = nil error")
	}
	if _, _, err := h.store.RecentClosed("gc-1", "bogus", 10); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("RecentClosed of an unknown kind = %v, want ErrUnknownKind", err)
	}
}

// RecentClosed returns the closed rows it cannot decode beside the ops it
// serves, newest first, as ListOpenTolerant returns open ones: a row of a
// newer schema, and a row labeled for the conversation but keyed to
// another, which lacks that conversation's label.
func TestRecentClosedReturnsRowsItCannotServe(t *testing.T) {
	h := newLaneHarness(t)
	closeMsg := func(key string) Op {
		op := h.propose(t, laneMsg(key, RoleNotice, "note"))
		closed, err := h.store.Close(op.ID, Outcome{Arm: ArmFailed, Reason: "expired"}, "lane:ctl")
		if err != nil {
			t.Fatal(err)
		}
		return closed
	}
	readable := closeMsg("k1")
	future := closeMsg("k2")
	moved := closeMsg("k3")
	if err := h.mem.SetMetadata(future.ID, keySchema, "99"); err != nil {
		t.Fatal(err)
	}
	if err := h.mem.SetMetadata(moved.ID, keyConversation, "gc-9"); err != nil {
		t.Fatal(err)
	}

	got, quarantined, err := h.store.RecentClosed("gc-1", KindMessage, 10)
	if err != nil {
		t.Fatalf("RecentClosed: %v", err)
	}
	if len(got) != 1 || got[0].ID != readable.ID {
		t.Fatalf("closed = %+v, want only the readable op %s", got, readable.ID)
	}
	if len(quarantined) != 2 ||
		quarantined[0].ID != moved.ID || !errors.Is(quarantined[0].Err, ErrCorruptOp) ||
		quarantined[1].ID != future.ID || !errors.Is(quarantined[1].Err, ErrUnsupportedSchema) {
		t.Fatalf("quarantined = %+v, want %s (another conversation's) then %s (a newer schema)", quarantined, moved.ID, future.ID)
	}
}
