package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

func cursorTestOrders(names ...string) []orders.Order {
	aa := make([]orders.Order, 0, len(names))
	for _, n := range names {
		aa = append(aa, orders.Order{Name: n})
	}
	return aa
}

// A reload rebuilds the dispatcher; the rotation must resume at the order it
// was about to start with, even when the order set was reordered or resized.
func TestCarryDispatchCursorFromFollowsOrderName(t *testing.T) {
	prev := &memoryOrderDispatcher{aa: cursorTestOrders("a", "b", "c", "d", "e"), nextDispatchStart: 3}

	next := &memoryOrderDispatcher{aa: cursorTestOrders("z", "a", "d", "e")}
	next.carryDispatchCursorFrom(prev)
	if next.nextDispatchStart != 2 {
		t.Fatalf("nextDispatchStart = %d, want 2 (order d)", next.nextDispatchStart)
	}

	// The next-up order was removed: restart at the head, as before.
	gone := &memoryOrderDispatcher{aa: cursorTestOrders("a", "b", "c", "e")}
	gone.carryDispatchCursorFrom(prev)
	if gone.nextDispatchStart != 0 {
		t.Fatalf("nextDispatchStart = %d, want 0 when the order is gone", gone.nextDispatchStart)
	}

	// Nil and empty sides are no-ops.
	var nilM *memoryOrderDispatcher
	nilM.carryDispatchCursorFrom(prev)
	empty := &memoryOrderDispatcher{}
	empty.carryDispatchCursorFrom(prev)
	next.carryDispatchCursorFrom(&memoryOrderDispatcher{})
}

// A slow pass admits proportionally more clock-driven orders, bounded.
func TestPassBudgetScalesWithElapsedPass(t *testing.T) {
	cfg := &config.City{}
	m := &memoryOrderDispatcher{cfg: cfg, maxDispatchesPerTick: 4}
	iv := cfg.Daemon.PatrolIntervalDuration()
	t0 := time.Unix(1000, 0)
	if got := m.passBudget(t0); got != 4 {
		t.Fatalf("first pass budget = %d, want 4", got)
	}
	if got := m.passBudget(t0.Add(iv)); got != 4 {
		t.Fatalf("on-time pass budget = %d, want 4", got)
	}
	if got := m.passBudget(t0.Add(iv + 5*iv)); got != 20 {
		t.Fatalf("5-interval pass budget = %d, want 20", got)
	}
	if got := m.passBudget(t0.Add(iv + 5*iv + 1000*iv)); got != 4*maxOrderBudgetScale {
		t.Fatalf("stalled pass budget = %d, want cap %d", got, 4*maxOrderBudgetScale)
	}
}

// Only late or budget-spent passes are logged; on-time passes with budget to
// spare stay silent.
func TestLogLatePassReportsOnlySlowOrSpentPasses(t *testing.T) {
	cfg := &config.City{}
	iv := cfg.Daemon.PatrolIntervalDuration()
	var buf bytes.Buffer
	m := &memoryOrderDispatcher{cfg: cfg, maxDispatchesPerTick: 4, stderr: &buf}

	m.logLatePass(iv, 4, 3, 67, time.Second)
	if buf.Len() != 0 {
		t.Fatalf("on-time pass logged: %q", buf.String())
	}
	m.logLatePass(5*iv, 20, 7, 67, 2*time.Second)
	if !strings.Contains(buf.String(), "budget=20 admitted=7 orders=67") {
		t.Fatalf("late pass not logged: %q", buf.String())
	}
	buf.Reset()
	m.logLatePass(iv, 4, 4, 67, time.Second)
	if !strings.Contains(buf.String(), "admitted=4") {
		t.Fatalf("budget-spent pass not logged: %q", buf.String())
	}
}
