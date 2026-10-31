package opjournal

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
)

// laneMsgOn is a lane message on conversation conv.
func laneMsgOn(conv, key string) Request {
	r := laneMsg(key, RoleNotice, "hello")
	r.Conversation = conv
	return r
}

// TestOpenLaneConversationsFindsOpenLaneOps proves the scan finds a lane op
// that is submitted, accepted, executing or settling, each conversation once,
// and ignores legacy ops, closed lane ops and lifecycle-kind ops another lane
// executes.
func TestOpenLaneConversationsFindsOpenLaneOps(t *testing.T) {
	h := newLaneHarness(t)
	h.propose(t, laneMsgOn("gc-1", "submitted"))
	h.propose(t, laneMsgOn("gc-1", "submitted-again"))
	accepted := h.propose(t, laneMsgOn("gc-2", "accepted"))
	if _, err := h.store.Transition(accepted.ID, StatusSubmitted, StatusAccepted, "test"); err != nil {
		t.Fatalf("Transition accepted: %v", err)
	}
	executing := h.propose(t, laneMsgOn("gc-3", "executing"))
	if _, err := h.store.Transition(executing.ID, StatusSubmitted, StatusExecuting, "test"); err != nil {
		t.Fatalf("Transition executing: %v", err)
	}
	closed := h.propose(t, laneMsgOn("gc-4", "closed"))
	if res, _, err := h.store.Cancel(closed.ID, "test", "done"); err != nil || res != CancelClosed {
		t.Fatalf("Cancel = %v, %v", res, err)
	}
	legacy := laneMsgOn("gc-5", "legacy")
	legacy.Executor = ExecutorLegacy
	h.propose(t, legacy)
	settling := h.propose(t, laneMsgOn("gc-6", "settling"))
	if _, err := h.store.Transition(settling.ID, StatusSubmitted, StatusAccepted, "test"); err != nil {
		t.Fatalf("Transition accepted: %v", err)
	}
	if _, err := h.store.Transition(settling.ID, StatusAccepted, StatusSettling, "test"); err != nil {
		t.Fatalf("Transition settling: %v", err)
	}
	h.propose(t, Request{
		Conversation: "gc-7", Kind: KindWake, Executor: ExecutorLane, ClientKey: "wake",
		Producer: Producer{Principal: "operator", Actor: ActorOperator},
	})

	convs, next, err := h.store.OpenLaneConversations(0, LaneScanCursor{})
	if err != nil {
		t.Fatalf("OpenLaneConversations: %v", err)
	}
	if !next.Done() {
		t.Fatal("cursor not done with no limit")
	}
	if want := []string{"gc-1", "gc-2", "gc-3", "gc-6"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("conversations = %v, want %v", convs, want)
	}
}

// TestOpenLaneConversationsReportsTruncation proves a list that fills every
// page up to the cap returns a cursor that is not done, and one that does
// not, a done cursor.
func TestOpenLaneConversationsReportsTruncation(t *testing.T) {
	h := newLaneHarness(t)
	h.propose(t, laneMsgOn("gc-1", "a"))
	h.propose(t, laneMsgOn("gc-2", "b"))
	// With a limit of one, more rows than laneScanPages pages hold truncate.
	for i := range laneScanPages {
		h.propose(t, laneMsgOn("gc-1", "extra-"+string(rune('a'+i))))
	}
	convs, next, err := h.store.OpenLaneConversations(1, LaneScanCursor{})
	if err != nil || next.Done() {
		t.Fatalf("OpenLaneConversations(1) = %v, done %v, %v; want truncated", convs, next.Done(), err)
	}
	convs, next, err = h.store.OpenLaneConversations(laneScanPages+3, LaneScanCursor{})
	if err != nil || !next.Done() || len(convs) != 2 {
		t.Fatalf("OpenLaneConversations(pages+3) = %v, done %v, %v; want both, untruncated", convs, next.Done(), err)
	}
}

// TestOpenLaneConversationsEmpty proves a store with no lane op returns none.
func TestOpenLaneConversationsEmpty(t *testing.T) {
	h := newLaneHarness(t)
	convs, next, err := h.store.OpenLaneConversations(10, LaneScanCursor{})
	if err != nil || !next.Done() || len(convs) != 0 {
		t.Fatalf("OpenLaneConversations = %v, done %v, %v; want none", convs, next.Done(), err)
	}
}

// TestOpenLaneConversationsReadError proves a failed list is returned, not
// read as no conversations.
func TestOpenLaneConversationsReadError(t *testing.T) {
	// The scan needs only reads, so the store is built bare: a failing
	// list wrapper hides the conditional writes NewStore asks for.
	s := &Store{store: beads.SessionStore{Store: failingListStore{&beads.MemStore{}}}, clk: &clock.Fake{Time: t0}, prefix: "gc"}
	if convs, _, err := s.OpenLaneConversations(10, LaneScanCursor{}); err == nil || convs != nil {
		t.Fatalf("OpenLaneConversations = %v, %v; want the list error", convs, err)
	}
}

// TestOpenLaneConversationsPagesPastAHotConversation proves one
// conversation's backlog does not hide the conversations behind it: each
// keyed list reads on in pages of limit rows, and records a cursor only
// when it stops at the page cap after a full page.
func TestOpenLaneConversationsPagesPastAHotConversation(t *testing.T) {
	h := newLaneHarness(t)
	for _, key := range []string{"a1", "a2", "a3"} {
		h.propose(t, laneMsgOn("gc-hot", key))
	}
	h.propose(t, laneMsgOn("gc-cold", "b1"))
	convs, next, err := h.store.OpenLaneConversations(2, LaneScanCursor{})
	if err != nil || !next.Done() {
		t.Fatalf("OpenLaneConversations(2) = %v, done %v, %v; want every page read", convs, next.Done(), err)
	}
	if want := []string{"gc-hot", "gc-cold"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("conversations = %v, want %v", convs, want)
	}
	for i := range laneScanPages {
		h.propose(t, laneMsgOn("gc-hot", "more-"+string(rune('a'+i))))
	}
	convs, next, err = h.store.OpenLaneConversations(1, LaneScanCursor{})
	if err != nil || next.Done() {
		t.Fatalf("OpenLaneConversations(1) = %v, done %v, %v; want truncated at the page cap", convs, next.Done(), err)
	}
	if want := []string{"gc-hot", "gc-cold"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("capped conversations = %v, want %v from the pages read", convs, want)
	}
}

// TestOpenLaneConversationsResumesFromCursor proves a conversation behind the
// page cap is reached by the next scan from the returned cursor, and that the
// scan after a list is read to its end starts over at its oldest row.
func TestOpenLaneConversationsResumesFromCursor(t *testing.T) {
	h := newLaneHarness(t)
	for i := range laneScanPages + 1 {
		h.propose(t, laneMsgOn("gc-hot", "hot-"+string(rune('a'+i))))
	}
	h.propose(t, laneMsgOn("gc-cold", "cold"))

	convs, next, err := h.store.OpenLaneConversations(1, LaneScanCursor{})
	if err != nil || next.Done() {
		t.Fatalf("first scan = %v, done %v, %v; want stopped at the page cap", convs, next.Done(), err)
	}
	if want := []string{"gc-hot"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("first scan conversations = %v, want %v", convs, want)
	}
	convs, next, err = h.store.OpenLaneConversations(1, next)
	if err != nil || !next.Done() {
		t.Fatalf("second scan = %v, done %v, %v; want read to the end", convs, next.Done(), err)
	}
	if want := []string{"gc-hot", "gc-cold"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("second scan conversations = %v, want %v", convs, want)
	}
	convs, _, err = h.store.OpenLaneConversations(1, next)
	if err != nil {
		t.Fatalf("third scan: %v", err)
	}
	if want := []string{"gc-hot"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("third scan conversations = %v, want %v from the oldest row again", convs, want)
	}
}
