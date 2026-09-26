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

func TestAggregateRunCostsFormulaName(t *testing.T) {
	facts := []usage.Fact{
		{RunID: "run-a", FormulaName: "review", Kind: usage.KindModel, InputTokens: 100, CostUSDEstimate: 0.01},
		{RunID: "run-a", Kind: usage.KindModel, InputTokens: 50, CostUSDEstimate: 0.005}, // no FormulaName on second fact
		{RunID: "run-b", Kind: usage.KindModel, InputTokens: 20, CostUSDEstimate: 0.002}, // no formula
	}
	rows := aggregateRunCosts(facts)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].FormulaName != "review" {
		t.Errorf("run-a FormulaName = %q, want %q", rows[0].FormulaName, "review")
	}
	if rows[1].FormulaName != "" {
		t.Errorf("run-b FormulaName = %q, want empty", rows[1].FormulaName)
	}
}

func TestAggregateByFormula(t *testing.T) {
	rows := []runCost{
		{RunID: "run-a", FormulaName: "review", Invocations: 2, InputTokens: 100, OutputTokens: 50, CostUSDEstimate: 0.01},
		{RunID: "run-b", FormulaName: "review", Invocations: 1, InputTokens: 40, OutputTokens: 20, CostUSDEstimate: 0.005},
		{RunID: "run-c", FormulaName: "build", Invocations: 3, InputTokens: 200, CostUSDEstimate: 0.02},
		{RunID: "run-d", FormulaName: "", Invocations: 1, WallSeconds: 5},
	}
	grouped := aggregateByFormula(rows)
	// Expect 3 groups: "-", "build", "review" (sorted)
	if len(grouped) != 3 {
		t.Fatalf("want 3 groups, got %d", len(grouped))
	}
	if grouped[0].FormulaName != "-" {
		t.Errorf("group[0] FormulaName = %q, want %q", grouped[0].FormulaName, "-")
	}
	if grouped[1].FormulaName != "build" {
		t.Errorf("group[1] FormulaName = %q, want %q", grouped[1].FormulaName, "build")
	}
	if grouped[2].FormulaName != "review" {
		t.Errorf("group[2] FormulaName = %q, want %q", grouped[2].FormulaName, "review")
	}
	// "review" group should aggregate both run-a and run-b
	review := grouped[2]
	if review.Invocations != 3 {
		t.Errorf("review Invocations = %d, want 3", review.Invocations)
	}
	if review.InputTokens != 140 {
		t.Errorf("review InputTokens = %d, want 140", review.InputTokens)
	}
	if review.OutputTokens != 70 {
		t.Errorf("review OutputTokens = %d, want 70", review.OutputTokens)
	}
	// "build" group: run-c only
	if grouped[1].Invocations != 3 {
		t.Errorf("build Invocations = %d, want 3", grouped[1].Invocations)
	}
	// "-" group: run-d only
	if grouped[0].WallSeconds != 5 {
		t.Errorf("- WallSeconds = %v, want 5", grouped[0].WallSeconds)
	}
}
