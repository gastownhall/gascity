package reviewquorum

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestValidateLaneConfigsAcceptsConfiguredLanes(t *testing.T) {
	lanes := []LaneConfig{
		{ID: "primary", Provider: "provider-a", Model: "model-a"},
		{ID: "secondary", Provider: "provider-b", Model: "model-b"},
	}
	if err := ValidateLaneConfigs(lanes); err != nil {
		t.Fatalf("ValidateLaneConfigs() error = %v", err)
	}
}

func TestValidateLaneConfigsRejectsContractDrift(t *testing.T) {
	tests := []struct {
		name  string
		lanes []LaneConfig
	}{
		{
			name: "empty lane id",
			lanes: []LaneConfig{
				{ID: "", Provider: "provider-a", Model: "model-a"},
			},
		},
		{
			name: "missing provider",
			lanes: []LaneConfig{
				{ID: "primary", Provider: "", Model: "model-a"},
			},
		},
		{
			name: "missing model",
			lanes: []LaneConfig{
				{ID: "primary", Provider: "provider-a", Model: ""},
			},
		},
		{
			name: "uppercase id",
			lanes: []LaneConfig{
				{ID: "Primary", Provider: "provider-a", Model: "model-a"},
			},
		},
		{
			name: "duplicate id",
			lanes: []LaneConfig{
				{ID: "primary", Provider: "provider-a", Model: "model-a"},
				{ID: "primary", Provider: "provider-b", Model: "model-b"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateLaneConfigs(tt.lanes); err == nil {
				t.Fatal("ValidateLaneConfigs() error = nil, want error")
			}
		})
	}
}

func TestRateLimitFailuresAreTransient(t *testing.T) {
	for reason := range transientFailureReasons {
		if !IsTransientFailure("", reason) {
			t.Fatalf("IsTransientFailure(%q) = false, want true", reason)
		}
		class, gotReason := ClassifyFailure("", reason)
		if class != FailureClassTransient || gotReason != reason {
			t.Fatalf("ClassifyFailure(%q) = %q/%q, want transient/%q", reason, class, gotReason, reason)
		}
	}
}

func TestClassifyFailureNoneNoFailure(t *testing.T) {
	class, reason := ClassifyFailure(FailureClassNone, "")
	if class != FailureClassNone || reason != "" {
		t.Fatalf("ClassifyFailure(none, empty) = %q/%q, want none/empty", class, reason)
	}
}

func TestClassifyFailureNoneWithReasonIsHardContractFailure(t *testing.T) {
	class, reason := ClassifyFailure(FailureClassNone, "stale_reason")
	if class != FailureClassHard || reason != "invalid_failure_class_stale_reason" {
		t.Fatalf("ClassifyFailure(none, stale_reason) = %q/%q, want hard/invalid_failure_class_stale_reason", class, reason)
	}
}

func TestClassifyFailureInvalidClassPreservesReason(t *testing.T) {
	class, reason := ClassifyFailure("retry_later", "provider exploded")
	if class != FailureClassHard || reason != "invalid_failure_class_provider_exploded" {
		t.Fatalf("ClassifyFailure(retry_later, provider exploded) = %q/%q, want hard/invalid_failure_class_provider_exploded", class, reason)
	}
}

func TestClassifyFailureInvalidClassWithTransientReasonIsHard(t *testing.T) {
	if IsTransientFailure("retry_later", "provider_timeout") {
		t.Fatal("IsTransientFailure(retry_later, provider_timeout) = true, want false")
	}
	class, reason := ClassifyFailure("retry_later", "provider_timeout")
	if class != FailureClassHard || reason != "invalid_failure_class_provider_timeout" {
		t.Fatalf("ClassifyFailure(retry_later, provider_timeout) = %q/%q, want hard/invalid_failure_class_provider_timeout", class, reason)
	}
}

func TestExplicitHardFailureWithTransientReasonStaysHard(t *testing.T) {
	if IsTransientFailure(FailureClassHard, "provider_timeout") {
		t.Fatal("IsTransientFailure(hard, provider_timeout) = true, want false")
	}
	class, reason := ClassifyFailure(FailureClassHard, "provider_timeout")
	if class != FailureClassHard || reason != "provider_timeout" {
		t.Fatalf("ClassifyFailure(hard, provider_timeout) = %q/%q, want hard/provider_timeout", class, reason)
	}
}

func TestLaneOutputJSONMatchesFormulaRequiredKeys(t *testing.T) {
	out := LaneOutput{
		LaneID:        lanePrimary,
		Provider:      "codex",
		Model:         "gpt-5.5",
		Verdict:       VerdictPass,
		Summary:       "clean",
		FindingsCount: 0,
		Findings:      []Finding{},
		Evidence:      []Evidence{},
		Usage:         nil,
		ReadOnlyEnforcement: ReadOnlyEnforcement{
			Observed:        true,
			Enabled:         true,
			Passed:          true,
			BaselineCommand: "git status --porcelain=v1 -z",
			AfterCommand:    "git status --porcelain=v1 -z",
		},
		MutationsDelta: MutationsDelta{},
		FailureClass:   FailureClassNone,
		FailureReason:  "",
	}

	got, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"lane_id":"primary","provider":"codex","model":"gpt-5.5","verdict":"pass","summary":"clean","findings_count":0,"findings":[],"evidence":[],"usage":null,"read_only_enforcement":{"observed":true,"enabled":true,"passed":true,"baseline_command":"git status --porcelain=v1 -z","after_command":"git status --porcelain=v1 -z"},"mutations_delta":{},"failure_class":"none","failure_reason":""}`
	if string(got) != want {
		t.Fatalf("LaneOutput JSON = %s, want %s", got, want)
	}

	var roundTripped LaneOutput
	if err := json.Unmarshal(got, &roundTripped); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(roundTripped, out) {
		t.Fatalf("round trip = %+v, want %+v", roundTripped, out)
	}
}

func TestSummaryJSONMatchesFormulaRequiredKeys(t *testing.T) {
	out := Summary{
		Subject:       "gh:gastownhall/gascity#1694",
		BaseRef:       "origin/main",
		Verdict:       VerdictPass,
		Summary:       "clean",
		FindingsCount: 0,
		Findings:      []Finding{},
		Evidence:      []Evidence{},
		Usage:         nil,
		ReadOnlyEnforcement: ReadOnlyEnforcement{
			Observed:        true,
			Enabled:         true,
			Passed:          true,
			BaselineCommand: "git status --porcelain=v1 -z",
			AfterCommand:    "git status --porcelain=v1 -z",
		},
		MutationsDelta: MutationsDelta{},
		FailureClass:   FailureClassNone,
		FailureReason:  "",
		Lanes:          []LaneOutput{},
	}

	got, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	want := `{"subject":"gh:gastownhall/gascity#1694","base_ref":"origin/main","verdict":"pass","summary":"clean","findings_count":0,"findings":[],"evidence":[],"usage":null,"read_only_enforcement":{"observed":true,"enabled":true,"passed":true,"baseline_command":"git status --porcelain=v1 -z","after_command":"git status --porcelain=v1 -z"},"mutations_delta":{},"failure_class":"none","failure_reason":"","lanes":[]}`
	if string(got) != want {
		t.Fatalf("Summary JSON = %s, want %s", got, want)
	}

	var roundTripped Summary
	if err := json.Unmarshal(got, &roundTripped); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(roundTripped, out) {
		t.Fatalf("round trip = %+v, want %+v", roundTripped, out)
	}
}
