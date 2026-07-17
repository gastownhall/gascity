package timingplan

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestPlanInventoryIsAuthoritative(t *testing.T) {
	result := mustPlan(t, Input{
		Inventory: []InventoryUnit{
			{UnitID: "current/warm"},
			{UnitID: "current/missing"},
			{UnitID: "current/cold"},
		},
		History: []HistoryUnit{
			newHistory("deleted/stale", 20, 500, 500, 500, 0),
			newHistory("current/warm", 20, 7, 8, 9, 1),
			newHistory("current/cold", 2, 1, 2, 3, 0.25),
		},
		Shards:        2,
		Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20, Variance: 0},
		P95CapSeconds: 90,
	})

	assignments := assignmentsByUnitID(t, result)
	wantIDs := []string{"current/cold", "current/missing", "current/warm"}
	if got := sortedKeys(assignments); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("assigned unit IDs = %v, want exactly current inventory %v", got, wantIDs)
	}
	if _, ok := assignments["deleted/stale"]; ok {
		t.Fatal("stale history resurrected deleted/stale")
	}
	if got := assignments["current/warm"].P75Seconds; got != 8 {
		t.Fatalf("warm p75 = %v, want authoritative history 8", got)
	}
	if got := assignments["current/missing"].P75Seconds; got != 10 {
		t.Fatalf("missing p75 = %v, want static default 10", got)
	}
	if got := assignments["current/cold"].P75Seconds; got != 10 {
		t.Fatalf("cold p75 = %v, want static default 10", got)
	}
	if !strings.Contains(assignments["current/missing"].Reason, "missing") {
		t.Fatalf("missing-history reason = %q, want an explicit missing reason", assignments["current/missing"].Reason)
	}
	if !strings.Contains(assignments["current/cold"].Reason, "insufficient") {
		t.Fatalf("cold-history reason = %q, want an explicit insufficient-samples reason", assignments["current/cold"].Reason)
	}
}

func TestPlanTimingAuthorityThresholds(t *testing.T) {
	result := mustPlan(t, Input{
		Inventory: []InventoryUnit{
			{UnitID: "samples-04"},
			{UnitID: "samples-05"},
			{UnitID: "samples-19"},
			{UnitID: "samples-20"},
		},
		History: []HistoryUnit{
			newHistory("samples-04", 4, 2, 3, 4, 0.1),
			newHistory("samples-05", 5, 7, 8, 13, 0.2),
			newHistory("samples-19", 19, 25, 30, 31, 0.3),
			newHistory("samples-20", 20, 25, 30, 31, 0.4),
		},
		Shards:        2,
		Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20, Variance: 0},
		P95CapSeconds: 90,
	})
	assignments := assignmentsByUnitID(t, result)

	assertTiming := func(unitID string, wantP75, wantP95 float64, wantP75Source, wantP95Source string) {
		t.Helper()
		got := assignments[unitID]
		if got.P75Seconds != wantP75 || got.P95Seconds != wantP95 {
			t.Errorf("%s timing = p75 %v, p95 %v; want p75 %v, p95 %v", unitID, got.P75Seconds, got.P95Seconds, wantP75, wantP95)
		}
		if got.P75Source != wantP75Source || got.P95Source != wantP95Source {
			t.Errorf("%s sources = p75 %q, p95 %q; want p75 %q, p95 %q", unitID, got.P75Source, got.P95Source, wantP75Source, wantP95Source)
		}
		if got.Reason == "" {
			t.Errorf("%s has no observable timing-selection reason", unitID)
		}
	}

	assertTiming("samples-04", 10, 20, "static", "estimated")
	assertTiming("samples-05", 8, 20, "empirical", "estimated")
	assertTiming("samples-19", 30, 45, "empirical", "estimated")
	assertTiming("samples-20", 30, 31, "empirical", "empirical")
}

func TestPlanConservativeP95Fallback(t *testing.T) {
	result := mustPlan(t, Input{
		Inventory: []InventoryUnit{{UnitID: "static-dominates"}, {UnitID: "tail-dominates"}},
		History: []HistoryUnit{
			newHistory("static-dominates", 5, 7, 8, 9, 0),
			newHistory("tail-dominates", 19, 20, 30, 31, 0),
		},
		Shards:        1,
		Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20, Variance: 0},
		P95CapSeconds: 90,
	})
	assignments := assignmentsByUnitID(t, result)

	if got := assignments["static-dominates"].P95Seconds; got != 20 {
		t.Errorf("static-dominates p95 = %v, want max(20, 1.5*8) = 20", got)
	}
	if got := assignments["tail-dominates"].P95Seconds; got != 45 {
		t.Errorf("tail-dominates p95 = %v, want max(20, 1.5*30) = 45", got)
	}
	for _, unitID := range []string{"static-dominates", "tail-dominates"} {
		if got := assignments[unitID].P95Source; got != "estimated" {
			t.Errorf("%s p95 source = %q, want estimated before 20 samples", unitID, got)
		}
	}
}

func TestPlanDeterministicLongestFirstPacking(t *testing.T) {
	defaults := StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20, Variance: 0}
	first := mustPlan(t, Input{
		Inventory: []InventoryUnit{{UnitID: "unit-c"}, {UnitID: "unit-a"}, {UnitID: "unit-d"}, {UnitID: "unit-b"}},
		History: []HistoryUnit{
			newHistory("unit-b", 20, 7, 8, 9, 2),
			newHistory("unit-d", 20, 5, 6, 7, 4),
			newHistory("unit-a", 20, 8, 9, 12, 1),
			newHistory("unit-c", 20, 6, 7, 8, 3),
		},
		Shards: 2, Defaults: defaults, P95CapSeconds: 90,
	})
	shuffled := mustPlan(t, Input{
		Inventory: []InventoryUnit{{UnitID: "unit-b"}, {UnitID: "unit-d"}, {UnitID: "unit-a"}, {UnitID: "unit-c"}},
		History: []HistoryUnit{
			newHistory("unit-c", 20, 6, 7, 8, 3),
			newHistory("unit-a", 20, 8, 9, 12, 1),
			newHistory("unit-d", 20, 5, 6, 7, 4),
			newHistory("unit-b", 20, 7, 8, 9, 2),
		},
		Shards: 2, Defaults: defaults, P95CapSeconds: 90,
	})

	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first plan: %v", err)
	}
	shuffledJSON, err := json.Marshal(shuffled)
	if err != nil {
		t.Fatalf("marshal shuffled plan: %v", err)
	}
	if string(firstJSON) != string(shuffledJSON) {
		t.Fatalf("shuffled inputs changed canonical output\nfirst:    %s\nshuffled: %s", firstJSON, shuffledJSON)
	}

	if len(first.Shards) != 2 {
		t.Fatalf("shards = %d, want 2", len(first.Shards))
	}
	assertShard(t, first.Shards[0], 0, []string{"unit-a", "unit-d"}, 15, 19)
	assertShard(t, first.Shards[1], 1, []string{"unit-b", "unit-c"}, 15, 17)

	t.Run("numeric ties use stable unit ID", func(t *testing.T) {
		tied := mustPlan(t, Input{
			Inventory: []InventoryUnit{{UnitID: "tie-z"}, {UnitID: "tie-a"}},
			History: []HistoryUnit{
				newHistory("tie-z", 20, 8, 10, 12, 2),
				newHistory("tie-a", 20, 8, 10, 12, 2),
			},
			Shards: 1, Defaults: defaults, P95CapSeconds: 90,
		})
		assertShard(t, tied.Shards[0], 0, []string{"tie-a", "tie-z"}, 20, 24)
	})
}

func TestPlanDeterministicSecondaryOrdering(t *testing.T) {
	result := mustPlan(t, Input{
		Inventory: []InventoryUnit{
			{UnitID: "id-z"},
			{UnitID: "p50-low"},
			{UnitID: "variance-high"},
			{UnitID: "p95-high"},
			{UnitID: "id-a"},
			{UnitID: "p50-high"},
		},
		History: []HistoryUnit{
			newHistory("id-z", 20, 5, 10, 13, 1),
			newHistory("p50-low", 20, 6, 10, 14, 2),
			newHistory("variance-high", 20, 5, 10, 14, 3),
			newHistory("p95-high", 20, 5, 10, 15, 0),
			newHistory("id-a", 20, 5, 10, 13, 1),
			newHistory("p50-high", 20, 7, 10, 14, 2),
		},
		Shards:        1,
		Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20},
		P95CapSeconds: 90,
	})

	assertShard(t, result.Shards[0], 0, []string{
		"p95-high",
		"variance-high",
		"p50-high",
		"p50-low",
		"id-a",
		"id-z",
	}, 60, 83)
}

func TestPlanTailAwareAggregateCapPlacement(t *testing.T) {
	result := mustPlan(t, Input{
		Inventory: []InventoryUnit{
			{UnitID: "longer"},
			{UnitID: "tail-heavy"},
			{UnitID: "candidate"},
		},
		History: []HistoryUnit{
			newHistory("longer", 20, 15, 20, 20, 0),
			newHistory("tail-heavy", 20, 5, 10, 85, 0),
			newHistory("candidate", 20, 3, 5, 10, 0),
		},
		Shards:        2,
		Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20},
		P95CapSeconds: 90,
	})

	assertShard(t, result.Shards[0], 0, []string{"longer", "candidate"}, 25, 30)
	assertShard(t, result.Shards[1], 1, []string{"tail-heavy"}, 10, 85)

	t.Run("no fitting shard retains and flags the unit", func(t *testing.T) {
		noFit := mustPlan(t, Input{
			Inventory: []InventoryUnit{{UnitID: "first"}, {UnitID: "retained"}},
			History: []HistoryUnit{
				newHistory("first", 20, 8, 10, 60, 0),
				newHistory("retained", 20, 4, 5, 60, 0),
			},
			Shards:        1,
			Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20},
			P95CapSeconds: 90,
		})

		assignments := assignmentsByUnitID(t, noFit)
		if !slices.Contains(assignments["retained"].Hazards, "shard-p95-cap-exceeded") {
			t.Fatalf("retained hazards = %v, want aggregate shard-p95-cap-exceeded", assignments["retained"].Hazards)
		}
		if slices.Contains(assignments["retained"].Hazards, "p95-cap-exceeded") {
			t.Fatalf("retained hazards = %v, aggregate overflow must not mark the unit individually oversized", assignments["retained"].Hazards)
		}
		assertShard(t, noFit.Shards[0], 0, []string{"first", "retained"}, 15, 120)
	})
}

func TestPlanRejectsMalformedTiming(t *testing.T) {
	validInput := func() Input {
		return Input{
			Inventory:     []InventoryUnit{{UnitID: "unit"}},
			History:       []HistoryUnit{newHistory("unit", 20, 5, 10, 15, 1)},
			Shards:        1,
			Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20},
			P95CapSeconds: 90,
		}
	}

	tests := []struct {
		name      string
		input     Input
		wantError string
	}{
		{
			name: "omitted static defaults",
			input: Input{
				Inventory:     []InventoryUnit{{UnitID: "unit"}},
				Shards:        1,
				P95CapSeconds: 90,
			},
			wantError: "defaults.duration_seconds_p75 must be positive",
		},
		{
			name: "static p50 exceeds p75",
			input: func() Input {
				input := validInput()
				input.Defaults.P50Seconds = 11
				return input
			}(),
			wantError: "defaults",
		},
		{
			name: "static p75 exceeds p95",
			input: func() Input {
				input := validInput()
				input.Defaults.P95Seconds = 9
				return input
			}(),
			wantError: "defaults",
		},
		{
			name: "history p50 exceeds p75",
			input: func() Input {
				input := validInput()
				input.History[0].P50Seconds = 11
				return input
			}(),
			wantError: "history[0]",
		},
		{
			name: "history p75 exceeds p95",
			input: func() Input {
				input := validInput()
				input.History[0].P95Seconds = 9
				return input
			}(),
			wantError: "history[0]",
		},
		{
			name: "estimated p95 overflows",
			input: Input{
				Inventory:     []InventoryUnit{{UnitID: "unit"}},
				Shards:        1,
				Defaults:      StaticTiming{P50Seconds: 1, P75Seconds: math.MaxFloat64, P95Seconds: math.MaxFloat64},
				P95CapSeconds: math.MaxFloat64,
			},
			wantError: "estimated p95",
		},
		{
			name: "aggregate p75 overflows",
			input: Input{
				Inventory: []InventoryUnit{{UnitID: "first"}, {UnitID: "second"}},
				History: []HistoryUnit{
					newHistory("first", 20, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, 0),
					newHistory("second", 20, math.MaxFloat64, math.MaxFloat64, math.MaxFloat64, 0),
				},
				Shards:        1,
				Defaults:      StaticTiming{P50Seconds: 1, P75Seconds: 2, P95Seconds: 3},
				P95CapSeconds: math.MaxFloat64,
			},
			wantError: "aggregate p75",
		},
		{
			name: "aggregate p95 overflows",
			input: Input{
				Inventory: []InventoryUnit{{UnitID: "first"}, {UnitID: "second"}},
				History: []HistoryUnit{
					newHistory("first", 20, 1, 1, math.MaxFloat64, 0),
					newHistory("second", 20, 1, 1, math.MaxFloat64, 0),
				},
				Shards:        1,
				Defaults:      StaticTiming{P50Seconds: 1, P75Seconds: 2, P95Seconds: 3},
				P95CapSeconds: math.MaxFloat64,
			},
			wantError: "aggregate p95",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Plan(test.input)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Plan error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestPlanShardCountBounds(t *testing.T) {
	input := Input{
		Shards:        256,
		Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20},
		P95CapSeconds: 90,
	}
	result := mustPlan(t, input)
	if len(result.Shards) != 256 {
		t.Fatalf("shards = %d, want accepted maximum 256", len(result.Shards))
	}

	for name, shards := range map[string]int{
		"above maximum": 257,
		"huge integer":  int(^uint(0) >> 1),
	} {
		t.Run(name, func(t *testing.T) {
			input.Shards = shards
			_, err := Plan(input)
			if err == nil || !strings.Contains(err.Error(), "must not exceed 256") {
				t.Fatalf("Plan error = %v, want shard-limit error", err)
			}
		})
	}
}

func TestPlanNeverSkipsColdOrOversizedUnits(t *testing.T) {
	result := mustPlan(t, Input{
		Inventory: []InventoryUnit{{UnitID: "missing"}, {UnitID: "cold"}, {UnitID: "oversized"}},
		History: []HistoryUnit{
			newHistory("cold", 1, 1, 2, 3, 0),
			newHistory("oversized", 20, 60, 80, 120, 900),
		},
		Shards:        2,
		Defaults:      StaticTiming{P50Seconds: 4, P75Seconds: 10, P95Seconds: 20, Variance: 0},
		P95CapSeconds: 90,
	})
	assignments := assignmentsByUnitID(t, result)

	if len(assignments) != 3 {
		t.Fatalf("assigned units = %d, want all 3 current units", len(assignments))
	}
	for _, unitID := range []string{"missing", "cold", "oversized"} {
		if _, ok := assignments[unitID]; !ok {
			t.Errorf("current unit %q was skipped", unitID)
		}
	}
	if got := assignments["cold"].P75Source; got != "static" {
		t.Errorf("cold p75 source = %q, want static", got)
	}
	if got := assignments["missing"].P75Source; got != "static" {
		t.Errorf("missing p75 source = %q, want static", got)
	}
	oversized := assignments["oversized"]
	if oversized.P95Seconds != 120 {
		t.Errorf("oversized p95 = %v, want authoritative 120", oversized.P95Seconds)
	}
	if !slices.Contains(oversized.Hazards, "p95-cap-exceeded") {
		t.Errorf("oversized hazards = %v, want unit p95-cap-exceeded", oversized.Hazards)
	}
	if slices.Contains(oversized.Hazards, "shard-p95-cap-exceeded") {
		t.Errorf("oversized hazards = %v, individual oversize must not be mislabeled as aggregate shard overflow", oversized.Hazards)
	}
}

func mustPlan(t *testing.T, input Input) Result {
	t.Helper()
	result, err := Plan(input)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return result
}

func newHistory(unitID string, samples int, p50, p75, p95, variance float64) HistoryUnit {
	return HistoryUnit{
		UnitID: unitID, SuccessfulSamples: samples,
		P50Seconds: p50, P75Seconds: p75, P95Seconds: p95, Variance: variance,
	}
}

func assignmentsByUnitID(t *testing.T, result Result) map[string]Assignment {
	t.Helper()
	assignments := make(map[string]Assignment)
	for _, shard := range result.Shards {
		for _, unit := range shard.Units {
			if _, duplicate := assignments[unit.UnitID]; duplicate {
				t.Fatalf("unit %q assigned more than once", unit.UnitID)
			}
			assignments[unit.UnitID] = unit
		}
	}
	return assignments
}

func assertShard(t *testing.T, got Shard, wantIndex int, wantUnitIDs []string, wantP75, wantP95 float64) {
	t.Helper()
	if got.Index != wantIndex {
		t.Errorf("shard index = %d, want %d", got.Index, wantIndex)
	}
	gotIDs := make([]string, len(got.Units))
	var unitP75, unitP95 float64
	for index, unit := range got.Units {
		gotIDs[index] = unit.UnitID
		unitP75 += unit.P75Seconds
		unitP95 += unit.P95Seconds
		if unit.P75Source == "" || unit.P95Source == "" || unit.Reason == "" {
			t.Errorf("unit %q lacks observable source/reason: %+v", unit.UnitID, unit)
		}
	}
	if !reflect.DeepEqual(gotIDs, wantUnitIDs) {
		t.Errorf("shard %d unit order = %v, want %v", got.Index, gotIDs, wantUnitIDs)
	}
	if got.P75Seconds != wantP75 || got.P95Seconds != wantP95 {
		t.Errorf("shard %d totals = p75 %v, p95 %v; want p75 %v, p95 %v", got.Index, got.P75Seconds, got.P95Seconds, wantP75, wantP95)
	}
	if got.P75Seconds != unitP75 || got.P95Seconds != unitP95 {
		t.Errorf("shard %d totals = p75 %v, p95 %v; summed units = p75 %v, p95 %v", got.Index, got.P75Seconds, got.P95Seconds, unitP75, unitP95)
	}
}

func sortedKeys(values map[string]Assignment) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
