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

// TestCheckTriggerEventRunAfterALogRestartConsumesItsEvents (AC-2): once the
// order runs on the restarted log (stamped at its head, 2), the retained old
// stamp 500 no longer replays the consumed events, and the next matching event
// fires it again.
func TestCheckTriggerEventRunAfterALogRestartConsumesItsEvents(t *testing.T) {
	ep := restartedLog(2)
	a := updatedOrder()
	quiet := CheckTrigger(a, time.Time{}, neverRan, ep, stampsFn(500, 2))
	if quiet.Due || quiet.ReadFailed {
		t.Fatalf("after the post-restart run: result = %+v, want not due", quiet)
	}
	if want := (CursorReset{Stamp: 500, Head: 2, Cursor: 2}); quiet.CursorReset == nil || *quiet.CursorReset != want {
		t.Fatalf("after the post-restart run: CursorReset = %+v, want %+v", quiet.CursorReset, want)
	}

	ep.Record(events.Event{Type: "bead.updated"}) // seq 3
	next := CheckTrigger(a, time.Time{}, neverRan, ep, stampsFn(500, 2))
	if !next.Due || !strings.HasPrefix(next.Reason, "event: 1 bead.updated event(s)") {
		t.Fatalf("after one new matching event: result = %+v, want due with count 1", next)
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

// TestCheckTriggerEventStampsAtOrBelowHeadMatchExistingBehavior (#8567, AC-3):
// stamps at or below the event log head keep the pre-fix Due/count/Reason
// contract exactly — the reset machinery must be invisible when no stamp is
// stale. Equality is admissible: a stamp equal to the head has consumed
// through the head and the order is not due.
func TestCheckTriggerEventStampsAtOrBelowHeadMatchExistingBehavior(t *testing.T) {
	cases := []struct {
		name       string
		stamps     []uint64
		events     int
		wantDue    bool
		wantReason string
	}{
		{
			name:       "stamp below the head fires only events after it",
			stamps:     []uint64{3},
			events:     5,
			wantDue:    true,
			wantReason: "event: 2 bead.updated event(s)",
		},
		{
			name:       "stamp equal to the head consumes through the head",
			stamps:     []uint64{5},
			events:     5,
			wantDue:    false,
			wantReason: "event: no matching events",
		},
		{
			name:       "mixed stamps below the head reduce to the max",
			stamps:     []uint64{3, 2},
			events:     5,
			wantDue:    true,
			wantReason: "event: 2 bead.updated event(s)",
		},
		{
			name:       "zero stamps are inert",
			stamps:     []uint64{0},
			events:     2,
			wantDue:    true,
			wantReason: "event: 2 bead.updated event(s)",
		},
		{
			name:       "head itself is admissible when it is the max stamp",
			stamps:     []uint64{2, 7},
			events:     7,
			wantDue:    false,
			wantReason: "event: no matching events",
		},
		{
			name:       "no stamps read the whole log",
			stamps:     nil,
			events:     3,
			wantDue:    true,
			wantReason: "event: 3 bead.updated event(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := CheckTrigger(updatedOrder(), time.Now(), neverRan, restartedLog(tc.events), stampsFn(tc.stamps...))
			if res.Due != tc.wantDue {
				t.Fatalf("Due = %v, want %v (%+v)", res.Due, tc.wantDue, res)
			}
			if res.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want exactly %q (no reset clause)", res.Reason, tc.wantReason)
			}
			if res.CursorReset != nil {
				t.Errorf("CursorReset = %+v, want none for stamps at or below the head", res.CursorReset)
			}
			if res.Err != nil {
				t.Errorf("Err = %v, want none", res.Err)
			}
			if res.ReadFailed {
				t.Errorf("ReadFailed = true, want a clean read")
			}
		})
	}
}
