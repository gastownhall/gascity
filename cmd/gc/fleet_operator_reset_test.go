package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// A close by `gc breaker reset` reads differently from a close by a probe
// session staying up, in both the stderr line and the event message, so the log
// of an incident says who ended it.
func TestFleetGuard_OperatorResetIsDistinguishableFromANaturalClose(t *testing.T) {
	t.Run("operator reset of an open breaker", func(t *testing.T) {
		g, clk := newFleetTestGuard(t)
		tripFleet(t, g, clk)
		g.Flush(nil, nil) // the open is not under test
		rec := events.NewFake()
		var stderr strings.Builder

		if !g.Reset() {
			t.Fatal("Reset() = false, want true for an open breaker")
		}
		g.Flush(rec, &stderr)

		if !strings.Contains(stderr.String(), "closed by operator reset") {
			t.Errorf("stderr = %q, want it to say the close was an operator reset", stderr.String())
		}
		if len(rec.Events) != 1 {
			t.Fatalf("events = %d, want 1 close event", len(rec.Events))
		}
		if msg := rec.Events[0].Message; !strings.Contains(msg, "(operator reset)") {
			t.Errorf("event message = %q, want it to carry (operator reset)", msg)
		}
	})

	t.Run("natural close says nothing about an operator", func(t *testing.T) {
		g, clk := newFleetTestGuard(t)
		tripFleet(t, g, clk)
		g.Flush(nil, nil)
		rec := events.NewFake()
		var stderr strings.Builder

		ticket := admitFleetProbe(t, g, clk, "probe")
		ticket.Resolve(fleetStartOK)
		clk.Advance(stabilityThreshold)
		g.ObserveLiveness("probe", true)
		g.Flush(rec, &stderr)

		if fleetState(g).String() != "closed" {
			t.Fatalf("premise: breaker = %v, want closed after the probe stayed up", fleetState(g))
		}
		if strings.Contains(stderr.String(), "operator") || len(rec.Events) == 0 || strings.Contains(rec.Events[len(rec.Events)-1].Message, "operator") {
			t.Errorf("a probe-confirmed close mentions an operator: stderr = %q, events = %+v", stderr.String(), rec.Events)
		}
		if !strings.Contains(stderr.String(), "fleet start breaker closed: automatic session starts resumed") {
			t.Errorf("stderr = %q, want the natural close line", stderr.String())
		}
	})

	t.Run("operator reset of a closed breaker leaves a log trail and no event", func(t *testing.T) {
		g, _ := newFleetTestGuard(t)
		rec := events.NewFake()
		var stderr strings.Builder

		if g.Reset() {
			t.Fatal("Reset() = true, want false: the breaker was not open")
		}
		g.Flush(rec, &stderr)

		if !strings.Contains(stderr.String(), "reset by operator") || !strings.Contains(stderr.String(), "already closed") {
			t.Errorf("stderr = %q, want a line saying an operator reset an already-closed breaker", stderr.String())
		}
		if len(rec.Events) != 0 {
			t.Errorf("events = %+v, want none: no breaker state changed", rec.Events)
		}
	})
}
