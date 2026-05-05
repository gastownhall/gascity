package reviewquorum

import "testing"

func TestFinalizeReturnsAwaitingOnlyWithoutLaneOutputs(t *testing.T) {
	got := Finalize(nil)
	if got.Verdict != VerdictAwaitingReviewers {
		t.Fatalf("Verdict = %q, want %q", got.Verdict, VerdictAwaitingReviewers)
	}
	if len(got.Lanes) != 0 {
		t.Fatalf("Lanes len = %d, want 0", len(got.Lanes))
	}
}

func TestFinalizeSoftFailsTransientLaneWithoutAwaitingFinalize(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:        LaneKimi,
			Provider:      ProviderOpenCode,
			Model:         ModelKimi,
			Verdict:       VerdictPass,
			Summary:       "no issues found",
			FindingsCount: 0,
			Usage:         Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Enabled: true,
				Passed:  true,
			},
		},
		{
			LaneID:        LaneDeepSeek,
			Provider:      ProviderOpenCode,
			Model:         ModelDeepSeek,
			Verdict:       VerdictBlocked,
			FailureClass:  FailureClassTransient,
			FailureReason: "provider_rate_limited",
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Enabled: true,
				Passed:  true,
			},
		},
	})

	if got.Verdict != VerdictBlocked {
		t.Fatalf("Verdict = %q, want %q", got.Verdict, VerdictBlocked)
	}
	if got.FailureClass != FailureClassTransient {
		t.Fatalf("FailureClass = %q, want transient", got.FailureClass)
	}
	if got.FailureReason != "deepseek:provider_rate_limited" {
		t.Fatalf("FailureReason = %q, want deepseek:provider_rate_limited", got.FailureReason)
	}
	if got.Summary == "awaiting_finalize" || got.Verdict == "awaiting_finalize" {
		t.Fatalf("summary must not use ambiguous awaiting_finalize: %+v", got)
	}
	if got.Usage.TotalTokens != 15 {
		t.Fatalf("Usage.TotalTokens = %d, want 15", got.Usage.TotalTokens)
	}
}

func TestFinalizeFindingsRequestChanges(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:  LaneKimi,
			Verdict: VerdictPass,
			Findings: []Finding{
				{Title: "bug", File: "main.go", Start: 12},
			},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Enabled: true, Passed: true},
		},
		{
			LaneID:              LaneDeepSeek,
			Verdict:             VerdictPass,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Enabled: true, Passed: true},
		},
	})
	if got.Verdict != VerdictPassWithFindings {
		t.Fatalf("Verdict = %q, want pass_with_findings", got.Verdict)
	}
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want 1", got.FindingsCount)
	}
}

func TestFinalizeMutationsRequestChanges(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:  LaneKimi,
			Verdict: VerdictPass,
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{
				{Path: "internal/reviewquorum/types.go", Status: "M"},
			}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Enabled: true, Passed: false},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.ReadOnlyEnforcement.Passed {
		t.Fatal("ReadOnlyEnforcement.Passed = true, want false")
	}
}
