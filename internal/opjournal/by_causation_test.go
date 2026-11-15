package opjournal

import (
	"testing"
)

// OpenByCausation finds the open lane ops of a cause across conversations,
// and leaves out closed ops, other causes and legacy ops.
func TestOpenByCausation(t *testing.T) {
	h := newLaneHarness(t)
	caused := func(conv, key, cause string, exec ExecutorKind) Op {
		r := laneMsg(key, RoleNotice, "you have mail")
		r.Conversation, r.Causation, r.Executor = conv, cause, exec
		return h.propose(t, r)
	}
	a := caused("gc-1", "mail:m1", "m1", ExecutorLane)
	b := caused("gc-2", "mail:m1", "m1", ExecutorLane)
	caused("gc-1", "mail:m2", "m2", ExecutorLane)
	caused("gc-3", "mail:m1-legacy", "m1", ExecutorLegacyCLI)
	closed := caused("gc-4", "mail:m1-closed", "m1", ExecutorLane)
	if _, err := h.store.Close(closed.ID, Outcome{Arm: ArmSucceeded}, "lane:ctl"); err != nil {
		t.Fatal(err)
	}
	got, err := h.store.OpenByCausation("m1")
	if err != nil {
		t.Fatalf("OpenByCausation: %v", err)
	}
	if len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("ops = %+v, want %s and %s", got, a.ID, b.ID)
	}
	if _, err := h.store.OpenByCausation(""); err == nil {
		t.Fatal("OpenByCausation(\"\") = nil error, want a refusal")
	}
}
