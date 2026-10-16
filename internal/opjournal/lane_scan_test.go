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
// that is submitted, accepted or executing, each conversation once, and
// ignores legacy ops and closed lane ops.
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

	convs, truncated, err := h.store.OpenLaneConversations(0)
	if err != nil {
		t.Fatalf("OpenLaneConversations: %v", err)
	}
	if truncated {
		t.Fatal("truncated with no limit")
	}
	if want := []string{"gc-1", "gc-2", "gc-3"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("conversations = %v, want %v", convs, want)
	}
}

// TestOpenLaneConversationsReportsTruncation proves a list that fills every
// page up to the cap reports truncation, and one that does not, none.
func TestOpenLaneConversationsReportsTruncation(t *testing.T) {
	h := newLaneHarness(t)
	h.propose(t, laneMsgOn("gc-1", "a"))
	h.propose(t, laneMsgOn("gc-2", "b"))
	// With a limit of one, more rows than laneScanPages pages hold truncate.
	for i := range laneScanPages {
		h.propose(t, laneMsgOn("gc-1", "extra-"+string(rune('a'+i))))
	}
	convs, truncated, err := h.store.OpenLaneConversations(1)
	if err != nil || !truncated {
		t.Fatalf("OpenLaneConversations(1) = %v, truncated %v, %v; want truncated", convs, truncated, err)
	}
	convs, truncated, err = h.store.OpenLaneConversations(laneScanPages + 3)
	if err != nil || truncated || len(convs) != 2 {
		t.Fatalf("OpenLaneConversations(pages+3) = %v, truncated %v, %v; want both, untruncated", convs, truncated, err)
	}
}

// TestOpenLaneConversationsEmpty proves a store with no lane op returns none.
func TestOpenLaneConversationsEmpty(t *testing.T) {
	h := newLaneHarness(t)
	convs, truncated, err := h.store.OpenLaneConversations(10)
	if err != nil || truncated || len(convs) != 0 {
		t.Fatalf("OpenLaneConversations = %v, truncated %v, %v; want none", convs, truncated, err)
	}
}

// TestOpenLaneConversationsReadError proves a failed list is returned, not
// read as no conversations.
func TestOpenLaneConversationsReadError(t *testing.T) {
	// The scan needs only reads, so the store is built bare: a failing
	// list wrapper hides the conditional writes NewStore asks for.
	s := &Store{store: beads.SessionStore{Store: failingListStore{&beads.MemStore{}}}, clk: &clock.Fake{Time: t0}, prefix: "gc"}
	if convs, _, err := s.OpenLaneConversations(10); err == nil || convs != nil {
		t.Fatalf("OpenLaneConversations = %v, %v; want the list error", convs, err)
	}
}

// TestOpenLaneConversationsPagesPastAHotConversation proves one
// conversation's backlog does not hide the conversations behind it: each
// keyed list reads on in pages of limit rows, and reports truncation only
// when it stops at the page cap after a full page.
func TestOpenLaneConversationsPagesPastAHotConversation(t *testing.T) {
	h := newLaneHarness(t)
	for _, key := range []string{"a1", "a2", "a3"} {
		h.propose(t, laneMsgOn("gc-hot", key))
	}
	h.propose(t, laneMsgOn("gc-cold", "b1"))
	convs, truncated, err := h.store.OpenLaneConversations(2)
	if err != nil || truncated {
		t.Fatalf("OpenLaneConversations(2) = %v, truncated %v, %v; want every page read", convs, truncated, err)
	}
	if want := []string{"gc-hot", "gc-cold"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("conversations = %v, want %v", convs, want)
	}
	for i := range laneScanPages {
		h.propose(t, laneMsgOn("gc-hot", "more-"+string(rune('a'+i))))
	}
	convs, truncated, err = h.store.OpenLaneConversations(1)
	if err != nil || !truncated {
		t.Fatalf("OpenLaneConversations(1) = %v, truncated %v, %v; want truncated at the page cap", convs, truncated, err)
	}
	if want := []string{"gc-hot", "gc-cold"}; !reflect.DeepEqual(convs, want) {
		t.Fatalf("capped conversations = %v, want %v from the pages read", convs, want)
	}
}
