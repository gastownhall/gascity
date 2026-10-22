package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"unicode"

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

// costsFixtureFacts: two formulas, one unattributed, one unpriced, one compute
// fact. The compute fact carries no formula and no tokens, so it appears in the
// default table's WALL_S and nowhere in --by-formula.
func costsFixtureFacts() []usage.Fact {
	return []usage.Fact{
		{RunID: "run-a", FormulaName: "review", Kind: usage.KindModel, InputTokens: 100, OutputTokens: 50, CacheReadTokens: 7, CacheCreationTokens: 3, CostUSDEstimate: 0.01},
		{RunID: "run-a", FormulaName: "build", Kind: usage.KindModel, InputTokens: 40, OutputTokens: 20, CostUSDEstimate: 0.004},
		{RunID: "run-a", FormulaName: "review", Kind: usage.KindModel, InputTokens: 10, Unpriced: true},
		{RunID: "run-b", Kind: usage.KindModel, InputTokens: 20, OutputTokens: 5, CostUSDEstimate: 0.002},
		{RunID: "run-b", Kind: usage.KindCompute, WallSeconds: 12.5},
	}
}

func renderCostsForTest(t *testing.T, facts []usage.Fact, byFormula bool) string {
	t.Helper()
	var out bytes.Buffer
	if rc := renderCosts(&out, facts, byFormula, "usage.jsonl"); rc != 0 {
		t.Fatalf("renderCosts rc = %d", rc)
	}
	return out.String()
}

func tableRow(out, first string) []string {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == first {
			return f
		}
	}
	return nil
}

// The default table is the table on main: no FORMULA column, no formula state
// on runCost, one RUN row per run.
func TestCostsDefaultTableIsUnchangedFromMain(t *testing.T) {
	out := renderCostsForTest(t, costsFixtureFacts(), false)
	header := strings.Fields(strings.SplitN(out, "\n", 2)[0])
	want := []string{"RUN", "INVOCATIONS", "IN", "OUT", "CACHE_R", "CACHE_C", "WALL_S", "EST_USD", "UNPRICED"}
	if strings.Join(header, " ") != strings.Join(want, " ") {
		t.Fatalf("default header = %v, want %v", header, want)
	}
	if got := tableRow(out, "TOTAL"); len(got) != 9 {
		t.Fatalf("default TOTAL row = %v, want 9 fields (label + 8 numbers)", got)
	}
	if got := tableRow(out, "run-a"); len(got) != 9 || got[1] != "3" {
		t.Fatalf("run-a row = %v, want 3 invocations in 9 fields", got)
	}
}

// --by-formula buckets every fact by its OWN formula name: a run whose facts
// carry two names lands in two rows.
func TestCostsByFormulaBucketsPerFact(t *testing.T) {
	rows := aggregateFormulaCosts(costsFixtureFacts())
	got := map[string]formulaCost{}
	for _, r := range rows {
		got[r.FormulaName] = r
	}
	if len(rows) != 3 {
		t.Fatalf("buckets = %+v, want build, review and the empty bucket", rows)
	}
	if r := got["review"]; r.Invocations != 2 || r.InputTokens != 110 || r.OutputTokens != 50 || r.CacheReadTokens != 7 || r.CacheCreationTokens != 3 || r.Unpriced != 1 || r.CostUSDEstimate != 0.01 {
		t.Fatalf("review = %+v", r)
	}
	if r := got["build"]; r.Invocations != 1 || r.InputTokens != 40 {
		t.Fatalf("build = %+v", r)
	}
	if r, ok := got[""]; !ok || r.Invocations != 1 || r.InputTokens != 20 {
		t.Fatalf("unattributed bucket = %+v (present=%v); compute facts must not count", r, ok)
	}
}

func TestCostsByFormulaTable(t *testing.T) {
	out := renderCostsForTest(t, costsFixtureFacts(), true)
	header := strings.Fields(strings.SplitN(out, "\n", 2)[0])
	want := []string{"FORMULA", "INVOCATIONS", "IN", "OUT", "CACHE_R", "CACHE_C", "EST_USD", "UNPRICED"}
	if strings.Join(header, " ") != strings.Join(want, " ") {
		t.Fatalf("--by-formula header = %v, want %v (no wall or compute columns)", header, want)
	}
	if got := tableRow(out, "-"); got == nil || got[1] != "1" {
		t.Fatalf("unattributed row = %v, want formula shown as -", got)
	}
	if got := tableRow(out, "TOTAL"); len(got) != 8 {
		t.Fatalf("TOTAL row = %v, want 8 fields", got)
	}
	if !strings.Contains(out, "no pricing") {
		t.Fatalf("missing the no-pricing note:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "compute") {
		t.Fatalf("footnote must say why wall/compute columns are absent:\n%s", out)
	}
}

func TestCostsByFormulaEmptyResultMessage(t *testing.T) {
	// Only a compute fact: the default table has a row, --by-formula has nothing
	// to bucket and says so in its own words.
	facts := []usage.Fact{{RunID: "run-b", Kind: usage.KindCompute, WallSeconds: 4}}
	out := renderCostsForTest(t, facts, true)
	if strings.Contains(out, "No usage facts recorded yet") {
		t.Fatalf("--by-formula reused the default empty message:\n%s", out)
	}
	if !strings.Contains(out, "No model usage") {
		t.Fatalf("--by-formula empty message = %q, want one naming model usage", out)
	}
}

// T12: the per-formula buckets conserve everything the default table's model
// columns report, Unpriced included.
func TestCostsByFormulaConservesDefaultTotals(t *testing.T) {
	facts := costsFixtureFacts()
	var dflt runCost
	for _, r := range aggregateRunCosts(facts) {
		dflt.Invocations += r.Invocations
		dflt.InputTokens += r.InputTokens
		dflt.OutputTokens += r.OutputTokens
		dflt.CacheReadTokens += r.CacheReadTokens
		dflt.CacheCreationTokens += r.CacheCreationTokens
		dflt.CostUSDEstimate += r.CostUSDEstimate
		dflt.Unpriced += r.Unpriced
	}
	var sum formulaCost
	for _, r := range aggregateFormulaCosts(facts) {
		sum.Invocations += r.Invocations
		sum.InputTokens += r.InputTokens
		sum.OutputTokens += r.OutputTokens
		sum.CacheReadTokens += r.CacheReadTokens
		sum.CacheCreationTokens += r.CacheCreationTokens
		sum.CostUSDEstimate += r.CostUSDEstimate
		sum.Unpriced += r.Unpriced
	}
	if sum.Invocations != dflt.Invocations || sum.InputTokens != dflt.InputTokens || sum.OutputTokens != dflt.OutputTokens ||
		sum.CacheReadTokens != dflt.CacheReadTokens || sum.CacheCreationTokens != dflt.CacheCreationTokens || sum.Unpriced != dflt.Unpriced {
		t.Fatalf("buckets %+v do not conserve default totals %+v", sum, dflt)
	}
	if math.Abs(sum.CostUSDEstimate-dflt.CostUSDEstimate) > 1e-9 {
		t.Fatalf("EST_USD buckets = %v, default = %v", sum.CostUSDEstimate, dflt.CostUSDEstimate)
	}
}

// A formula name is pack-authored text; the table must not hand a terminal
// escape sequence through to the operator, and the bucket must still be one
// row per distinct name.
func TestCostsByFormulaNeutralizesControlCharactersInTheName(t *testing.T) {
	hostile := "evil\x1b[31m\x07\nname"
	facts := []usage.Fact{
		{RunID: "run-a", FormulaName: hostile, Kind: usage.KindModel, InputTokens: 10},
		{RunID: "run-a", FormulaName: hostile, Kind: usage.KindModel, InputTokens: 5},
	}
	out := renderCostsForTest(t, facts, true)
	for _, r := range out {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			t.Fatalf("--by-formula output holds control character %U:\n%q", r, out)
		}
	}
	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "evil") {
			row = line
		}
	}
	if !strings.Contains(row, "name") {
		t.Fatalf("--by-formula split or lost the name across lines (row %q):\n%s", row, out)
	}
	if got := strings.Count(out, "evil"); got != 1 {
		t.Fatalf("--by-formula printed the one distinct name %d times, want 1:\n%s", got, out)
	}
}
