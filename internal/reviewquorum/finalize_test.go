package reviewquorum

import "testing"

const (
	lanePrimary   = "primary"
	laneSecondary = "secondary"
)

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
			LaneID:        lanePrimary,
			Provider:      "local",
			Model:         "model-a",
			Verdict:       VerdictPass,
			Summary:       "no issues found",
			FindingsCount: 0,
			Usage:         Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed: true,
				Enabled:  true,
				Passed:   true,
			},
		},
		{
			LaneID:        laneSecondary,
			Provider:      "local",
			Model:         "model-b",
			Verdict:       VerdictBlocked,
			FailureClass:  FailureClassTransient,
			FailureReason: "provider_rate_limited",
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed: true,
				Enabled:  true,
				Passed:   true,
			},
		},
	})

	if got.Verdict != VerdictBlocked {
		t.Fatalf("Verdict = %q, want %q", got.Verdict, VerdictBlocked)
	}
	if got.FailureClass != FailureClassTransient {
		t.Fatalf("FailureClass = %q, want transient", got.FailureClass)
	}
	if got.FailureReason != "lane=secondary reason=provider_rate_limited" {
		t.Fatalf("FailureReason = %q, want lane=secondary reason=provider_rate_limited", got.FailureReason)
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
			LaneID:  lanePrimary,
			Verdict: VerdictPass,
			Findings: []Finding{
				{Title: "bug", File: "main.go", Start: 12},
			},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
		{
			LaneID:              laneSecondary,
			Verdict:             VerdictPass,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if got.Verdict != VerdictPassWithFindings {
		t.Fatalf("Verdict = %q, want pass_with_findings", got.Verdict)
	}
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want 1", got.FindingsCount)
	}
}

func TestFinalizeIgnoresFailureClassNoneOnPassingLane(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             VerdictPass,
			FailureClass:        FailureClassNone,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
		{
			LaneID:              laneSecondary,
			Verdict:             VerdictPass,
			FailureClass:        " none ",
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %q, want pass", got.Verdict)
	}
	if got.FailureClass != "" || got.FailureReason != "" {
		t.Fatalf("failure = %q/%q, want empty", got.FailureClass, got.FailureReason)
	}
}

func TestFinalizeFailureClassNoneStillHonorsLaneVerdict(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             VerdictFail,
			FailureClass:        FailureClassNone,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureReason != "lane=primary reason=lane_failed" {
		t.Fatalf("FailureReason = %q, want lane=primary reason=lane_failed", got.FailureReason)
	}
}

func TestFinalizeMutationsRequestChanges(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:  lanePrimary,
			Verdict: VerdictPass,
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{
				{Path: "internal/reviewquorum/types.go", Status: "M"},
			}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: false},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.ReadOnlyEnforcement.Passed {
		t.Fatal("ReadOnlyEnforcement.Passed = true, want false")
	}
}

func TestFinalizeReadOnlyViolationOverridesFindings(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:  lanePrimary,
			Verdict: VerdictPass,
			Findings: []Finding{
				{Title: "bug", File: "main.go", Start: 12},
			},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
		{
			LaneID:  laneSecondary,
			Verdict: VerdictPass,
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{
				{Path: "main.go", Status: "M"},
			}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: false},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureClass != FailureClassHard {
		t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
	}
	if got.FailureReason != "read_only_mutation_detected" {
		t.Fatalf("FailureReason = %q, want read_only_mutation_detected", got.FailureReason)
	}
}

func TestFinalizeReadOnlyViolationOverridesTransientFailure(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:        lanePrimary,
			Verdict:       VerdictBlocked,
			FailureClass:  FailureClassTransient,
			FailureReason: "provider_timeout",
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed: true,
				Enabled:  true,
				Passed:   true,
			},
		},
		{
			LaneID:  laneSecondary,
			Verdict: VerdictPass,
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{
				{Path: "main.go", Status: "M"},
			}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: false},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureClass != FailureClassHard {
		t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
	}
	if got.FailureReason != "read_only_mutation_detected" {
		t.Fatalf("FailureReason = %q, want read_only_mutation_detected", got.FailureReason)
	}
}

func TestFinalizeUnknownVerdictBlocksWithContractFailure(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             "approve",
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if got.Verdict != VerdictBlocked {
		t.Fatalf("Verdict = %q, want blocked", got.Verdict)
	}
	if got.FailureClass != FailureClassTransient {
		t.Fatalf("FailureClass = %q, want transient", got.FailureClass)
	}
	if got.FailureReason != "lane=primary reason=unknown_verdict_value" {
		t.Fatalf("FailureReason = %q, want lane=primary reason=unknown_verdict_value", got.FailureReason)
	}
}

func TestFinalizeTransientFailureOutranksFindings(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:  lanePrimary,
			Verdict: VerdictPassWithFindings,
			Findings: []Finding{
				{Title: "bug", File: "main.go", Start: 12},
			},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
		{
			LaneID:        laneSecondary,
			Verdict:       VerdictBlocked,
			FailureClass:  FailureClassTransient,
			FailureReason: "provider_timeout",
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed: true,
				Enabled:  true,
				Passed:   true,
			},
		},
	})
	if got.Verdict != VerdictBlocked {
		t.Fatalf("Verdict = %q, want blocked", got.Verdict)
	}
	if got.FailureClass != FailureClassTransient {
		t.Fatalf("FailureClass = %q, want transient", got.FailureClass)
	}
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want 1", got.FindingsCount)
	}
}

func TestFinalizeMissingReadOnlyEnforcementHardFails(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:  lanePrimary,
			Verdict: VerdictPass,
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureClass != FailureClassHard {
		t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
	}
	if got.FailureReason != "lane=primary reason=read_only_enforcement_missing" {
		t.Fatalf("FailureReason = %q, want read_only_enforcement_missing", got.FailureReason)
	}
	if got.ReadOnlyEnforcement.Observed {
		t.Fatal("ReadOnlyEnforcement.Observed = true, want false")
	}
}

func TestFinalizeDisabledReadOnlyEnforcementHardFails(t *testing.T) {
	got := Finalize([]LaneOutput{
		{
			LaneID:  lanePrimary,
			Verdict: VerdictPass,
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed: true,
				Enabled:  false,
				Passed:   true,
			},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureClass != FailureClassHard {
		t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
	}
	if got.FailureReason != "lane=primary reason=read_only_enforcement_disabled" {
		t.Fatalf("FailureReason = %q, want read_only_enforcement_disabled", got.FailureReason)
	}
}
