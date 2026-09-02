package core

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/orders"
)

func TestStalledBeadAlarmOrderContract(t *testing.T) {
	t.Parallel()

	order := readOrder(t, "stalled-bead-alarm.toml")
	if err := orders.Validate(order); err != nil {
		t.Fatalf("stalled-bead-alarm.toml failed validation: %v", err)
	}
	if order.Trigger != "cooldown" {
		t.Errorf("trigger = %q, want cooldown", order.Trigger)
	}
	if order.Exec != "gc beads stall-check" {
		t.Errorf("exec = %q, want exact gc beads stall-check", order.Exec)
	}
	if order.Formula != "" || order.Pool != "" {
		t.Errorf("direct exec order has formula=%q pool=%q, want both empty", order.Formula, order.Pool)
	}
	if !order.NoWorkGate {
		t.Error("no_work_gate = false, want true so the store probe is never gated by open work")
	}
	if !order.Idempotent {
		t.Error("idempotent = false, want true for overlapping cooldown sweeps")
	}
	interval, err := time.ParseDuration(order.Interval)
	if err != nil || interval <= 0 {
		t.Errorf("interval = %q, want a positive Go duration: %v", order.Interval, err)
	}
}
