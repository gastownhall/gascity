package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/usage"
)

func TestAggregateRunCosts(t *testing.T) {
	facts := []usage.Fact{
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 100, OutputTokens: 50, CacheReadTokens: 5, CostUSDEstimate: 0.01},
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 10, Unpriced: true}, // excluded from cost
		{RunID: "run-a", Kind: usage.KindCompute, WallSeconds: 12.5},
		{RunID: "run-b", Kind: usage.KindCompute, WallSeconds: 3},
	}
	rows := aggregateRunCosts(facts)
	if len(rows) != 2 {
		t.Fatalf("want 2 runs, got %d", len(rows))
	}
	// Sorted by run id: run-a, run-b.
	a, b := rows[0], rows[1]
	if a.RunID != "run-a" || b.RunID != "run-b" {
		t.Fatalf("run order wrong: %q, %q", a.RunID, b.RunID)
	}
	if a.Invocations != 2 {
		t.Fatalf("run-a invocations = %d, want 2", a.Invocations)
	}
	if a.InputTokens != 110 || a.OutputTokens != 50 || a.CacheReadTokens != 5 {
		t.Fatalf("run-a tokens wrong: %+v", a)
	}
	if a.WallSeconds != 12.5 || a.ComputeFacts != 1 {
		t.Fatalf("run-a compute wrong: %+v", a)
	}
	if a.Unpriced != 1 {
		t.Fatalf("run-a unpriced = %d, want 1", a.Unpriced)
	}
	if a.CostUSDEstimate != 0.01 {
		t.Fatalf("run-a cost = %v, want 0.01 (unpriced excluded)", a.CostUSDEstimate)
	}
	if b.WallSeconds != 3 || b.ComputeFacts != 1 || b.Invocations != 0 {
		t.Fatalf("run-b wrong: %+v", b)
	}
}

func TestAggregateRunCostsEmpty(t *testing.T) {
	if rows := aggregateRunCosts(nil); len(rows) != 0 {
		t.Fatalf("nil facts must yield no rows, got %d", len(rows))
	}
}
