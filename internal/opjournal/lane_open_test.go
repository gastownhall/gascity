package opjournal

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestHasOpenLaneOps proves the probe sees an open lane op of the
// conversation and nothing else: no closed op, no legacy op, no other
// conversation's op; and that it reads with one keyed list of limit 1.
func TestHasOpenLaneOps(t *testing.T) {
	h := newHarness(t)
	probe := func(conv string) bool {
		t.Helper()
		got, err := HasOpenLaneOps(beads.SessionStore{Store: h.rec}, conv)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if probe("gc-1") {
		t.Fatal("an empty store reports an open lane op")
	}
	closedOp := h.propose(t, msgReq("closed", "x"))
	if _, err := h.store.Close(closedOp.ID, Outcome{Arm: ArmSucceeded}, "lane:test"); err != nil {
		t.Fatal(err)
	}
	h.propose(t, legacyReq(ExecutorLegacyAPI, "legacy"))
	if probe("gc-1") {
		t.Fatal("a closed or legacy op reads as an open lane op")
	}
	other := msgReq("other", "x")
	other.Conversation = "gc-2"
	h.propose(t, other)
	if probe("gc-1") {
		t.Fatal("another conversation's op reads as this one's")
	}
	h.propose(t, msgReq("open", "x"))
	h.rec.Reset()
	if !probe("gc-1") {
		t.Fatal("an open lane op was not seen")
	}
	ops := h.rec.Ops()
	if len(ops) != 1 || ops[0].Kind != beadstest.OpList || ops[0].Query.Limit != 1 {
		t.Fatalf("probe ops = %+v, want one keyed list of limit 1", ops)
	}
}

// TestHasOpenLaneOpsErrors proves a failed read, a nil store and an invalid
// conversation are errors, never "no ops".
func TestHasOpenLaneOpsErrors(t *testing.T) {
	if _, err := HasOpenLaneOps(beads.SessionStore{Store: failingListStore{beads.NewMemStore()}}, "gc-1"); err == nil {
		t.Fatal("a failed read reported no error")
	}
	if _, err := HasOpenLaneOps(beads.SessionStore{}, "gc-1"); err == nil {
		t.Fatal("a nil store reported no error")
	}
	if _, err := HasOpenLaneOps(beads.SessionStore{Store: beads.NewMemStore()}, ""); err == nil {
		t.Fatal("an empty conversation reported no error")
	}
}
