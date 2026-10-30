package opjournal

import (
	"testing"
)

// RecentClosed lists a conversation's closed lane ops, newest first, within
// its capped window, and leaves out open ops, legacy ops and other
// conversations; ClosedAt is the close transition's time.
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

	got, err := h.store.RecentClosed("gc-1", 10)
	if err != nil {
		t.Fatalf("RecentClosed: %v", err)
	}
	if len(got) != 2 || got[0].ID != last.ID || got[1].ID != first.ID {
		t.Fatalf("closed = %+v, want %s then %s", got, last.ID, first.ID)
	}
	if at := got[0].ClosedAt(); at.IsZero() || !at.Equal(last.ClosedAt()) {
		t.Fatalf("ClosedAt = %v, want the close transition's time %v", at, last.ClosedAt())
	}
	window, err := h.store.RecentClosed("gc-1", 1)
	if err != nil || len(window) != 1 || window[0].ID != last.ID {
		t.Fatalf("capped window = %+v, %v; want only the newest row", window, err)
	}
	if open := propose("gc-3", "k6", ExecutorLane); !open.ClosedAt().IsZero() {
		t.Fatal("an open op has a close time")
	}
	if _, err := h.store.RecentClosed("gc-1", 0); err == nil {
		t.Fatal("RecentClosed with no limit = nil error")
	}
}
