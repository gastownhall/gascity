package reviewquorum

import "testing"

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
	if class != FailureClassHard || reason != "invalid_failure_class" {
		t.Fatalf("ClassifyFailure(none, stale_reason) = %q/%q, want hard/invalid_failure_class", class, reason)
	}
}
