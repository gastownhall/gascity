package reviewquorum

import "testing"

func TestDefaultLaneConfigsValidate(t *testing.T) {
	lanes := DefaultLaneConfigs()
	if err := ValidateDefaultQuorum(lanes); err != nil {
		t.Fatalf("ValidateDefaultQuorum(default) error = %v", err)
	}
	if len(lanes) != 2 {
		t.Fatalf("default lanes len = %d, want 2", len(lanes))
	}
	if lanes[0].ID != LaneKimi || lanes[0].Provider != ProviderOpenCode || lanes[0].Model != ModelKimi {
		t.Fatalf("first lane = %+v, want kimi opencode lane", lanes[0])
	}
	if lanes[1].ID != LaneDeepSeek || lanes[1].Provider != ProviderOpenCode || lanes[1].Model != ModelDeepSeek {
		t.Fatalf("second lane = %+v, want deepseek opencode lane", lanes[1])
	}
}

func TestValidateDefaultQuorumRejectsContractDrift(t *testing.T) {
	tests := []struct {
		name  string
		lanes []LaneConfig
	}{
		{
			name: "missing lane",
			lanes: []LaneConfig{
				{ID: LaneKimi, Provider: ProviderOpenCode, Model: ModelKimi},
			},
		},
		{
			name: "unexpected lane",
			lanes: []LaneConfig{
				{ID: LaneKimi, Provider: ProviderOpenCode, Model: ModelKimi},
				{ID: "qwen", Provider: ProviderOpenCode, Model: "opencode-go/qwen"},
			},
		},
		{
			name: "wrong provider",
			lanes: []LaneConfig{
				{ID: LaneKimi, Provider: "codex", Model: ModelKimi},
				{ID: LaneDeepSeek, Provider: ProviderOpenCode, Model: ModelDeepSeek},
			},
		},
		{
			name: "wrong model",
			lanes: []LaneConfig{
				{ID: LaneKimi, Provider: ProviderOpenCode, Model: "opencode-go/kimi-k2.5"},
				{ID: LaneDeepSeek, Provider: ProviderOpenCode, Model: ModelDeepSeek},
			},
		},
		{
			name: "uppercase id",
			lanes: []LaneConfig{
				{ID: "Kimi", Provider: ProviderOpenCode, Model: ModelKimi},
				{ID: LaneDeepSeek, Provider: ProviderOpenCode, Model: ModelDeepSeek},
			},
		},
		{
			name: "duplicate id",
			lanes: []LaneConfig{
				{ID: LaneKimi, Provider: ProviderOpenCode, Model: ModelKimi},
				{ID: LaneKimi, Provider: ProviderOpenCode, Model: ModelDeepSeek},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateDefaultQuorum(tt.lanes); err == nil {
				t.Fatal("ValidateDefaultQuorum() error = nil, want error")
			}
		})
	}
}

func TestRateLimitFailuresAreTransient(t *testing.T) {
	for _, reason := range []string{"opencode_rate_limited", "rate_limited", "provider_rate_limited"} {
		if !IsTransientFailure("", reason) {
			t.Fatalf("IsTransientFailure(%q) = false, want true", reason)
		}
		class, gotReason := ClassifyFailure("", reason)
		if class != FailureClassTransient || gotReason != reason {
			t.Fatalf("ClassifyFailure(%q) = %q/%q, want transient/%q", reason, class, gotReason, reason)
		}
	}
}
