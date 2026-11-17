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
	if len(ops) != 1 || ops[0].Kind != beadstest.OpList || ops[0].Query.Limit != 1 || ops[0].Query.Label != LabelOp {
		t.Fatalf("probe ops = %+v, want one list of op rows (label %s) of limit 1: the probe runs on every prompt, and a metadata key alone has no index", ops, LabelOp)
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

// TestOpenOpReadsCostTheSameWhateverHistoryTheStoreHolds is the scale rule for
// the reads a message pays: the hook's probe on every prompt and the lane's
// read on every pass examine the op rows, not the ledger. Ten times the closed
// order-run history costs them nothing more.
func TestOpenOpReadsCostTheSameWhateverHistoryTheStoreHolds(t *testing.T) {
	cost := func(history int) int {
		t.Helper()
		h := newHarness(t)
		if err := beadstest.ClosedHistory(h.mem, history); err != nil {
			t.Fatal(err)
		}
		h.propose(t, msgReq("open", "x"))
		h.rec.Reset()
		if due, err := HasOpenLaneOps(beads.SessionStore{Store: h.rec}, "gc-1"); err != nil || !due {
			t.Fatalf("HasOpenLaneOps = %v, %v; want the open op seen", due, err)
		}
		if ops, _, err := h.store.ListOpenTolerant("gc-1"); err != nil || len(ops) != 1 {
			t.Fatalf("ListOpenTolerant = %d ops, %v; want the one open op", len(ops), err)
		}
		rows, err := beadstest.LedgerRows(h.mem)
		if err != nil {
			t.Fatal(err)
		}
		examined := 0
		for _, op := range h.rec.Ops() {
			if op.Kind == beadstest.OpList {
				examined += beadstest.IndexedRows(rows, op.Query)
			}
		}
		return examined
	}
	small, large := cost(30), cost(300)
	if small != large {
		t.Fatalf("the open-op reads examine %d rows over 30 closed order runs and %d over 300; a message's reads must not grow with the store's history", small, large)
	}
	if small == 0 {
		t.Fatal("the open-op reads examined no row; the model priced nothing")
	}
}
