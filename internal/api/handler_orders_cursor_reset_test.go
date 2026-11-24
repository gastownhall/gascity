package api

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestCheckOrderTriggerForAPIReportsEventOrderDueAfterTheLogRestarts: the
// orders/check evaluation of an event order whose newest run is stamped seq
// 500, against a log that restarted and holds two matching events, is due on
// both the cached-history and the fresh cursor paths, and its reason states
// the reset: the stamp, the log head and the cursor it read after (ga-ak8pv3).
func TestCheckOrderTriggerForAPIReportsEventOrderDueAfterTheLogRestarts(t *testing.T) {
	const reset = "cursor reset: run stamp seq 500 is above the event log head 2; reading after seq 0"
	a := orders.Order{Name: "watch", Trigger: "event", On: events.BeadClosed, Formula: "mol-watch"}
	run := beads.Bead{
		ID:     "run-1",
		Title:  "order:watch",
		Status: "closed",
		Labels: []string{"order-run:watch", "order-tracking", "order:watch", "seq:500"},
	}
	for _, fresh := range []bool{false, true} {
		ep := events.NewFake()
		ep.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
		ep.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
		var history []orderHistoryStoreBead
		var infos []workflowStoreInfo
		if fresh {
			store := beads.NewMemStore()
			if _, err := store.Create(run); err != nil {
				t.Fatalf("create run: %v", err)
			}
			infos = []workflowStoreInfo{{store: store}}
		} else {
			history = []orderHistoryStoreBead{{bead: run}}
		}

		got := checkOrderTriggerForAPI(a, time.Now(), history, infos, ep, fresh)
		if !got.Due || got.ReadFailed {
			t.Fatalf("fresh=%v: result %+v, want due", fresh, got)
		}
		for _, want := range []string{"2 bead.closed event(s)", reset} {
			if !strings.Contains(got.Reason, want) {
				t.Fatalf("fresh=%v: reason %q, want it to contain %q", fresh, got.Reason, want)
			}
		}
	}
}
