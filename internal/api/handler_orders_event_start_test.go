package api

import (
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// afterSeqSpy is an event log that records the AfterSeq of every List.
type afterSeqSpy struct {
	*events.Fake
	after []uint64
}

func (p *afterSeqSpy) List(filter events.Filter) ([]events.Event, error) {
	p.after = append(p.after, filter.AfterSeq)
	return p.Fake.List(filter)
}

// TestCheckOrderTriggerForAPINeverRunEventOrderReadsOnlyPastTheHead: the
// orders/check evaluation of an event order that has never run reads the log
// from its head, not from seq 0, so a poller's rebuild opens none of the
// history and does not report the order due on history the controller never
// replays (ga-1clac5). Both the fresh and the cached-history cursor paths.
// Once the order has run, the check reads past its run cursor.
func TestCheckOrderTriggerForAPINeverRunEventOrderReadsOnlyPastTheHead(t *testing.T) {
	a := orders.Order{Name: "watch", Trigger: "event", On: events.BeadClosed, Formula: "mol-watch"}
	for _, fresh := range []bool{false, true} {
		spy := &afterSeqSpy{Fake: events.NewFake()}
		spy.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
		spy.Record(events.Event{Type: events.BeadClosed, Actor: "test"})

		if got := checkOrderTriggerForAPI(a, time.Now(), nil, nil, spy, fresh); got.Due || got.ReadFailed {
			t.Fatalf("fresh=%v history only: result %+v, want not due", fresh, got)
		}
		spy.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
		if got := checkOrderTriggerForAPI(a, time.Now(), nil, nil, spy, fresh); got.Due || got.ReadFailed {
			t.Fatalf("fresh=%v never run, new event: result %+v, want not due", fresh, got)
		}
		if !slices.Equal(spy.after, []uint64{2, 3}) {
			t.Fatalf("fresh=%v never run: reads after %v, want each read after the head [2 3]", fresh, spy.after)
		}
	}

	spy := &afterSeqSpy{Fake: events.NewFake()}
	for range 3 {
		spy.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
	}
	history := []orderHistoryStoreBead{{bead: beads.Bead{
		ID:     "run-1",
		Status: "closed",
		Labels: []string{"order-run:watch", "order-tracking", "order:watch", "seq:2"},
	}}}
	if got := checkOrderTriggerForAPI(a, time.Now(), history, nil, spy, false); !got.Due || got.Reason != "event: 1 bead.closed event(s)" {
		t.Fatalf("has run: result %+v, want due on the one event past its cursor", got)
	}
	if !slices.Equal(spy.after, []uint64{2}) {
		t.Fatalf("has run: reads after %v, want one read after its run cursor [2]", spy.after)
	}

	failed := checkOrderTriggerForAPI(a, time.Now(), nil, nil, events.NewFailFake(), true)
	if failed.Due || !failed.ReadFailed {
		t.Fatalf("unreadable head: result %+v, want not due and ReadFailed", failed)
	}
}
