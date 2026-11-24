package orders

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// stampsFn is a CursorFunc returning stamps.
func stampsFn(stamps ...uint64) CursorFunc {
	return func(string) []uint64 { return stamps }
}

// restartedLog is an event log that restarted at seq 1 and holds n matching
// bead.updated events, so its head is n.
func restartedLog(n int) *events.Fake {
	ep := events.NewFake()
	for range n {
		ep.Record(events.Event{Type: "bead.updated"})
	}
	return ep
}

func updatedOrder() Order {
	return Order{Name: "digest", Trigger: "event", On: "bead.updated"}
}

// TestCheckTriggerEventFiresAfterTheLogRestartsBelowARunStamp (#8567, AC-1):
// a run stamped 500 against a log that restarted and holds two matching
// events (head 2) is due on both, reading from 0, and names the discarded
// stamp and the head.
func TestCheckTriggerEventFiresAfterTheLogRestartsBelowARunStamp(t *testing.T) {
	result := CheckTrigger(updatedOrder(), time.Time{}, neverRan, restartedLog(2), stampsFn(500))
	if !result.Due || result.ReadFailed || result.Err != nil {
		t.Fatalf("result = %+v, want due without a read failure", result)
	}
	if !strings.HasPrefix(result.Reason, "event: 2 bead.updated event(s)") {
		t.Fatalf("Reason = %q, want the count of the 2 new events", result.Reason)
	}
	want := CursorReset{Stamp: 500, Head: 2, Cursor: 0}
	if result.CursorReset == nil || *result.CursorReset != want {
		t.Fatalf("CursorReset = %+v, want %+v", result.CursorReset, want)
	}
	if !strings.Contains(result.Reason, "run stamp seq 500 is above the event log head 2; reading after seq 0") {
		t.Fatalf("Reason = %q, want the reset facts", result.Reason)
	}
}

// TestCheckTriggerEventExcludesTrackingBeadsAfterAReset: the reset read still
// skips the dispatcher's own order-tracking bead events.
func TestCheckTriggerEventExcludesTrackingBeadsAfterAReset(t *testing.T) {
	ep := events.NewFake()
	ep.Record(events.Event{Type: "bead.updated", Payload: []byte(`{"labels":["order-tracking"]}`)})
	result := CheckTrigger(updatedOrder(), time.Time{}, neverRan, ep, stampsFn(500))
	if result.Due || result.CursorReset == nil {
		t.Fatalf("result = %+v, want not due on a tracking bead's event, with the reset reported", result)
	}
	if !strings.HasPrefix(result.Reason, "event: no matching events (cursor reset:") {
		t.Fatalf("Reason = %q, want the no-match Reason with the reset facts", result.Reason)
	}
}

func TestBoundCursor(t *testing.T) {
	tests := []struct {
		name          string
		stamps        []uint64
		head          uint64
		wantCursor    uint64
		wantDiscarded uint64
	}{
		{name: "nil", stamps: nil, head: 5},
		{name: "empty", stamps: []uint64{}, head: 5},
		{name: "below head", stamps: []uint64{3, 1}, head: 5, wantCursor: 3},
		{name: "equal to head", stamps: []uint64{5}, head: 5, wantCursor: 5},
		{name: "all above head", stamps: []uint64{500, 7}, head: 5, wantDiscarded: 500},
		{name: "mixed", stamps: []uint64{500, 2, 9, 4}, head: 5, wantCursor: 4, wantDiscarded: 500},
		{name: "duplicates", stamps: []uint64{2, 2, 500, 500}, head: 2, wantCursor: 2, wantDiscarded: 500},
		{name: "empty log", stamps: []uint64{0, 3}, head: 0, wantDiscarded: 3},
		{name: "residual crossing", stamps: []uint64{100, 50}, head: 120, wantCursor: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cursor, discarded := boundCursor(tt.stamps, tt.head)
			if cursor != tt.wantCursor || discarded != tt.wantDiscarded {
				t.Fatalf("boundCursor(%v, %d) = %d, %d; want %d, %d", tt.stamps, tt.head, cursor, discarded, tt.wantCursor, tt.wantDiscarded)
			}
		})
	}
}

func TestSeqsFromLabels(t *testing.T) {
	got := SeqsFromLabels([][]string{
		{"order-run:digest", "seq:500", "seq:abc", "seq:-1", "seq:"},
		nil,
		{"seq:2", "seq:2", "notseq:9"},
	})
	if want := []uint64{500, 2, 2}; !slices.Equal(got, want) {
		t.Fatalf("SeqsFromLabels = %v, want %v", got, want)
	}
	if got := SeqsFromLabels(nil); got != nil {
		t.Fatalf("SeqsFromLabels(nil) = %v, want nil", got)
	}
}
