package reviewquorum

import (
	"encoding/json"
	"reflect"
	"testing"
)

const (
	lanePrimary   = "primary"
	laneSecondary = "secondary"
	reviewSubject = "gh:gastownhall/gascity#1694"
	reviewBaseRef = "origin/main"
)

func finalize(outputs []LaneOutput) Summary {
	lanes := append([]LaneOutput(nil), outputs...)
	for i := range lanes {
		if lanes[i].Provider == "" {
			lanes[i].Provider = "test-provider"
		}
		if lanes[i].Model == "" {
			lanes[i].Model = "test-model"
		}
	}
	return Finalize(reviewSubject, reviewBaseRef, lanes)
}

func TestFinalizeReturnsAwaitingOnlyWithoutLaneOutputs(t *testing.T) {
	got := finalize(nil)
	if got.Verdict != VerdictAwaitingReviewers {
		t.Fatalf("Verdict = %q, want %q", got.Verdict, VerdictAwaitingReviewers)
	}
	if got.Subject != reviewSubject || got.BaseRef != reviewBaseRef {
		t.Fatalf("identity = %q/%q, want %q/%q", got.Subject, got.BaseRef, reviewSubject, reviewBaseRef)
	}
	if len(got.Lanes) != 0 {
		t.Fatalf("Lanes len = %d, want 0", len(got.Lanes))
	}
}

func TestFinalizePropagatesReviewIdentityToSummaryJSON(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             VerdictPass,
			FindingsCount:       0,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})

	if got.Subject != reviewSubject || got.BaseRef != reviewBaseRef {
		t.Fatalf("identity = %q/%q, want %q/%q", got.Subject, got.BaseRef, reviewSubject, reviewBaseRef)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if string(fields["subject"]) != `"`+reviewSubject+`"` || string(fields["base_ref"]) != `"`+reviewBaseRef+`"` {
		t.Fatalf("JSON identity = %s/%s, want %q/%q", fields["subject"], fields["base_ref"], reviewSubject, reviewBaseRef)
	}
}

func TestFinalizeRejectsMissingLaneIdentityFields(t *testing.T) {
	tests := []struct {
		name   string
		lane   LaneOutput
		reason string
	}{
		{
			name: "missing provider",
			lane: LaneOutput{
				LaneID:              lanePrimary,
				Model:               "model-a",
				Verdict:             VerdictPass,
				ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
			},
			reason: "lane=primary reason=provider_missing",
		},
		{
			name: "missing model",
			lane: LaneOutput{
				LaneID:              lanePrimary,
				Provider:            "provider-a",
				Verdict:             VerdictPass,
				ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
			},
			reason: "lane=primary reason=model_missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Finalize(reviewSubject, reviewBaseRef, []LaneOutput{tt.lane})
			if got.Verdict != VerdictFail {
				t.Fatalf("Verdict = %q, want fail", got.Verdict)
			}
			if got.FailureClass != FailureClassHard {
				t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
			}
			if got.FailureReason != tt.reason {
				t.Fatalf("FailureReason = %q, want %q", got.FailureReason, tt.reason)
			}
		})
	}
}

func TestFinalizeRejectsFindingsCountMismatch(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:        lanePrimary,
			Verdict:       VerdictPassWithFindings,
			FindingsCount: 2,
			Findings: []Finding{
				{Title: "missing peer", File: "main.go", Start: 12},
			},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})

	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureClass != FailureClassHard {
		t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
	}
	if got.FailureReason != "lane=primary reason=findings_count_mismatch" {
		t.Fatalf("FailureReason = %q, want findings_count_mismatch", got.FailureReason)
	}
}

func TestFinalizeRejectsFindingsBearingVerdictsWithoutFindings(t *testing.T) {
	for _, verdict := range []string{VerdictPassWithFindings, VerdictFail} {
		t.Run(verdict, func(t *testing.T) {
			got := finalize([]LaneOutput{
				{
					LaneID:              lanePrimary,
					Verdict:             verdict,
					FindingsCount:       0,
					ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
				},
			})

			if got.Verdict != VerdictFail {
				t.Fatalf("Verdict = %q, want fail", got.Verdict)
			}
			if got.FailureClass != FailureClassHard {
				t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
			}
			if got.FailureReason != "lane=primary reason=materialized_findings_missing" {
				t.Fatalf("FailureReason = %q, want materialized_findings_missing", got.FailureReason)
			}
		})
	}
}

func TestFinalizeSoftFailsTransientLaneWithoutAwaitingFinalize(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:        lanePrimary,
			Provider:      "local",
			Model:         "model-a",
			Verdict:       VerdictPass,
			Summary:       "no issues found",
			FindingsCount: 0,
			Usage:         &Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
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
	if got.Usage == nil || got.Usage.TotalTokens != 15 {
		t.Fatalf("Usage = %+v, want TotalTokens 15", got.Usage)
	}
}

func TestFinalizeFindingsRequestChanges(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:        lanePrimary,
			Verdict:       VerdictPass,
			FindingsCount: 1,
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

func TestFinalizeDeduplicatesFindingsWithLaneEvidence(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:        lanePrimary,
			Verdict:       VerdictPassWithFindings,
			FindingsCount: 1,
			Findings: []Finding{
				{
					Severity: "major",
					Title:    "double counted finding",
					Body:     "same issue",
					File:     "internal/reviewquorum/finalize.go",
					Start:    32,
					End:      34,
				},
			},
			Evidence:            []Evidence{{Kind: "file", Path: "internal/reviewquorum/finalize.go", Note: "primary"}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
		{
			LaneID:        laneSecondary,
			Verdict:       VerdictPassWithFindings,
			FindingsCount: 1,
			Findings: []Finding{
				{
					Severity: "major",
					Title:    "double counted finding",
					Body:     "same issue",
					File:     "internal/reviewquorum/finalize.go",
					Start:    32,
					End:      34,
				},
			},
			Evidence:            []Evidence{{Kind: "file", Path: "internal/reviewquorum/finalize.go", Note: "secondary"}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if got.FindingsCount != 1 {
		t.Fatalf("FindingsCount = %d, want deduplicated count 1", got.FindingsCount)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("Findings len = %d, want 1", len(got.Findings))
	}
	wantLanes := []string{lanePrimary, laneSecondary}
	if !reflect.DeepEqual(got.Findings[0].Lanes, wantLanes) {
		t.Fatalf("Findings[0].Lanes = %+v, want %+v", got.Findings[0].Lanes, wantLanes)
	}
	if len(got.Findings[0].Evidence) != 1 {
		t.Fatalf("Findings[0].Evidence len = %d, want first lane-level evidence only", len(got.Findings[0].Evidence))
	}
}

func TestFinalizeIgnoresFailureClassNoneOnPassingLane(t *testing.T) {
	got := finalize([]LaneOutput{
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
	if got.FailureClass != FailureClassNone || got.FailureReason != "" {
		t.Fatalf("failure = %q/%q, want none/empty", got.FailureClass, got.FailureReason)
	}
}

func TestFinalizeSuccessUsesDurableNoFailureContract(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             VerdictPass,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if got.FailureClass != FailureClassNone || got.FailureReason != "" {
		t.Fatalf("failure = %q/%q, want none/empty", got.FailureClass, got.FailureReason)
	}
	if got.Findings == nil {
		t.Fatal("Findings = nil, want empty array for durable JSON")
	}
	if got.Evidence == nil {
		t.Fatal("Evidence = nil, want empty array for durable JSON")
	}
	if got.Lanes == nil {
		t.Fatal("Lanes = nil, want empty or populated array for durable JSON")
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if string(fields["failure_class"]) != `"`+FailureClassNone+`"` {
		t.Fatalf("failure_class JSON = %s, want %q", fields["failure_class"], FailureClassNone)
	}
	if string(fields["findings"]) != "[]" {
		t.Fatalf("findings JSON = %s, want []", fields["findings"])
	}
	if string(fields["evidence"]) != "[]" {
		t.Fatalf("evidence JSON = %s, want []", fields["evidence"])
	}
}

func TestFinalizeFailureClassNoneStillHonorsLaneVerdict(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             VerdictFail,
			FindingsCount:       1,
			Findings:            []Finding{{Title: "blocking issue", File: "main.go", Start: 12}},
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
	got := finalize([]LaneOutput{
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
	got := finalize([]LaneOutput{
		{
			LaneID:        lanePrimary,
			Verdict:       VerdictPass,
			FindingsCount: 1,
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
	if got.FailureReason != "lane=secondary reason=read_only_mutation_detected" {
		t.Fatalf("FailureReason = %q, want lane=secondary reason=read_only_mutation_detected", got.FailureReason)
	}
	if got.FindingsCount != 1 || len(got.Findings) != 1 {
		t.Fatalf("Findings = %d/%d, want preserved finding despite read-only failure", got.FindingsCount, len(got.Findings))
	}
}

func TestFinalizeReadOnlyViolationOverridesTransientFailure(t *testing.T) {
	got := finalize([]LaneOutput{
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
	if got.FailureReason != "lane=secondary reason=read_only_mutation_detected" {
		t.Fatalf("FailureReason = %q, want lane=secondary reason=read_only_mutation_detected", got.FailureReason)
	}
}

func TestFinalizeUnknownVerdictFailsWithHardContractFailure(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             "approve",
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureClass != FailureClassHard {
		t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
	}
	if got.FailureReason != "lane=primary reason=unknown_verdict_value" {
		t.Fatalf("FailureReason = %q, want lane=primary reason=unknown_verdict_value", got.FailureReason)
	}
}

func TestFinalizeTransientFailureOutranksFindings(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:        lanePrimary,
			Verdict:       VerdictPassWithFindings,
			FindingsCount: 1,
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
	got := finalize([]LaneOutput{
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
	got := finalize([]LaneOutput{
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

func TestFinalizeReadOnlyMutationFailureIdentifiesAllMutatingLanes(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:  lanePrimary,
			Verdict: VerdictPass,
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{
				{Path: "primary.go", Status: "M"},
			}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: false},
		},
		{
			LaneID:  laneSecondary,
			Verdict: VerdictPass,
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{
				{Path: "secondary.go", Status: "M"},
			}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: false},
		},
	})
	want := "lane=primary reason=read_only_mutation_detected; lane=secondary reason=read_only_mutation_detected"
	if got.FailureReason != want {
		t.Fatalf("FailureReason = %q, want %q", got.FailureReason, want)
	}
}

func TestFinalizeCopiesFirstReadOnlyNotes(t *testing.T) {
	notes := []string{"baseline captured"}
	got := finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             VerdictPass,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true, Notes: notes},
		},
	})
	notes[0] = "mutated after finalize"
	if got.ReadOnlyEnforcement.Notes[0] != "baseline captured" {
		t.Fatalf("ReadOnlyEnforcement.Notes[0] = %q, want copied note", got.ReadOnlyEnforcement.Notes[0])
	}
}

func TestFinalizeKeepsLaneMutationsOutOfSummaryDelta(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:  laneSecondary,
			Verdict: VerdictPass,
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{
				{Path: "same.go", Status: "D"},
			}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: false},
		},
		{
			LaneID:  lanePrimary,
			Verdict: VerdictPass,
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{
				{Path: "same.go", Status: "M"},
			}},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: false},
		},
	})
	if len(got.MutationsDelta.Changed) != 0 {
		t.Fatalf("summary MutationsDelta = %+v, want synthesis-only empty delta", got.MutationsDelta)
	}
	if len(got.Lanes) != 2 {
		t.Fatalf("Lanes len = %d, want 2", len(got.Lanes))
	}
	want := MutationsDelta{Changed: []StatusEntry{{Path: "same.go", Status: "M"}}}
	if !reflect.DeepEqual(got.Lanes[0].MutationsDelta, want) {
		t.Fatalf("primary lane MutationsDelta = %+v, want %+v", got.Lanes[0].MutationsDelta, want)
	}
}
