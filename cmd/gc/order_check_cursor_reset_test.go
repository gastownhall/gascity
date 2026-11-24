package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestOrderCheckReportsEventOrderDueAfterTheLogRestarts: an event order whose
// newest run is stamped seq 500, checked against a log that restarted and
// holds two matching events, is due in gc order check's text and JSON output,
// and the reason states the reset: the stamp, the log head and the cursor the
// check read after (ga-ak8pv3).
func TestOrderCheckReportsEventOrderDueAfterTheLogRestarts(t *testing.T) {
	const reset = "cursor reset: run stamp seq 500 is above the event log head 2; reading after seq 0"
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%v", jsonOutput), func(t *testing.T) {
			ep := events.NewFake()
			ep.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
			ep.Record(events.Event{Type: events.BeadClosed, Actor: "test"})
			aa := []orders.Order{{Name: "watch", Trigger: "event", On: events.BeadClosed, Formula: "mol-watch"}}
			store := beads.NewMemStore()
			if _, err := store.Create(beads.Bead{
				Title:  "order:watch",
				Labels: []string{"order-run:watch", "order-tracking", "order:watch", "seq:500"},
			}); err != nil {
				t.Fatalf("create run: %v", err)
			}
			resolver := func(orders.Order) ([]beads.OrdersStore, error) {
				return []beads.OrdersStore{{Store: store}}, nil
			}

			var stdout, stderr bytes.Buffer
			code := doOrderCheckWithStoresResolverScopedJSON("", nil, aa, time.Now(), ep, resolver, jsonOutput, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("code %d, want 0 (due); stdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
			}
			reason := stdout.String()
			if jsonOutput {
				var out orderCheckJSON
				if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
					t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
				}
				if len(out.Orders) != 1 || !out.Orders[0].Due || !out.AnyDue {
					t.Fatalf("JSON %+v, want the one order due", out)
				}
				reason = out.Orders[0].Reason
			}
			for _, want := range []string{"2 bead.closed event(s)", reset} {
				if !strings.Contains(reason, want) {
					t.Fatalf("reason %q, want it to contain %q", reason, want)
				}
			}
		})
	}
}
