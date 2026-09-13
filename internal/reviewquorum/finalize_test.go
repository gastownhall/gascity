package reviewquorum

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	lanePrimary           = "primary"
	laneSecondary         = "secondary"
	reviewSubject         = "gh:gastownhall/gascity#1694"
	reviewBaseRef         = "origin/main"
	readOnlyStatusCommand = "git status --porcelain=v1 -z"
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
		if lanes[i].FailureClass == "" && lanes[i].FailureReason == "" {
			lanes[i].FailureClass = FailureClassNone
		}
		if lanes[i].ReadOnlyEnforcement.Observed &&
			lanes[i].ReadOnlyEnforcement.Enabled &&
			lanes[i].ReadOnlyEnforcement.Passed {
			if lanes[i].ReadOnlyEnforcement.BaselineCommand == "" {
				lanes[i].ReadOnlyEnforcement.BaselineCommand = readOnlyStatusCommand
			}
			if lanes[i].ReadOnlyEnforcement.AfterCommand == "" {
				lanes[i].ReadOnlyEnforcement.AfterCommand = readOnlyStatusCommand
			}
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
			name: "missing lane id",
			lane: LaneOutput{
				Provider:            "provider-a",
				Model:               "model-a",
				Verdict:             VerdictPass,
				ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
			},
			reason: "lane=unknown_lane reason=lane_id_missing",
		},
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

func TestFinalizeReportsSummaryIdentityFailuresBeforeLaneFailures(t *testing.T) {
	got := Finalize(" ", "", []LaneOutput{
		{
			LaneID:              lanePrimary,
			Provider:            "",
			Model:               "model-a",
			Verdict:             VerdictPass,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})

	wantPrefix := "summary_subject_missing; summary_base_ref_missing; lane=primary reason=provider_missing"
	if !strings.HasPrefix(got.FailureReason, wantPrefix) {
		t.Fatalf("FailureReason = %q, want prefix %q", got.FailureReason, wantPrefix)
	}
}

func TestFinalizeRejectsMissingSummaryIdentityWithoutLanes(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		baseRef string
		reason  string
	}{
		{
			name:    "missing subject",
			subject: "",
			baseRef: reviewBaseRef,
			reason:  "summary_subject_missing",
		},
		{
			name:    "missing base ref",
			subject: reviewSubject,
			baseRef: " ",
			reason:  "summary_base_ref_missing",
		},
		{
			name:    "missing both",
			subject: " ",
			baseRef: "",
			reason:  "summary_subject_missing; summary_base_ref_missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Finalize(tt.subject, tt.baseRef, nil)
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

func TestFinalizeSkipsContractInvalidLaneDataFromSummary(t *testing.T) {
	got := Finalize(reviewSubject, reviewBaseRef, []LaneOutput{
		{
			LaneID:        lanePrimary,
			Provider:      "",
			Model:         "model-a",
			Verdict:       VerdictPassWithFindings,
			Summary:       "invalid lane summary",
			FindingsCount: 1,
			Findings: []Finding{
				{Title: "must not be merged", File: "internal/reviewquorum/finalize.go", Start: 12},
			},
			Evidence: []Evidence{{Kind: "file", Path: "internal/reviewquorum/finalize.go"}},
			Usage:    &Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed: true,
				Enabled:  true,
				Passed:   true,
				Notes:    []string{"invalid lane note"},
			},
		},
		{
			LaneID:        laneSecondary,
			Provider:      "provider-b",
			Model:         "model-b",
			Verdict:       VerdictPass,
			Summary:       "valid lane summary",
			FindingsCount: 0,
			Usage:         &Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed:        true,
				Enabled:         true,
				Passed:          true,
				BaselineCommand: readOnlyStatusCommand,
				AfterCommand:    readOnlyStatusCommand,
				Notes:           []string{"valid lane note"},
			},
			FailureClass: FailureClassNone,
		},
	})

	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureReason != "lane=primary reason=provider_missing" {
		t.Fatalf("FailureReason = %q, want provider_missing", got.FailureReason)
	}
	if got.FindingsCount != 0 || len(got.Findings) != 0 {
		t.Fatalf("Findings = %d/%d, want no invalid lane findings", got.FindingsCount, len(got.Findings))
	}
	if len(got.Evidence) != 0 {
		t.Fatalf("Evidence len = %d, want no invalid lane evidence", len(got.Evidence))
	}
	if got.Usage == nil || got.Usage.TotalTokens != 3 {
		t.Fatalf("Usage = %+v, want valid lane usage only", got.Usage)
	}
	if !reflect.DeepEqual(got.ReadOnlyEnforcement.Notes, []string{"valid lane note"}) {
		t.Fatalf("ReadOnlyEnforcement.Notes = %+v, want valid lane notes only", got.ReadOnlyEnforcement.Notes)
	}
	if !got.ReadOnlyEnforcement.Observed || !got.ReadOnlyEnforcement.Enabled || !got.ReadOnlyEnforcement.Passed {
		t.Fatalf("ReadOnlyEnforcement = %+v, want valid lane enforcement only", got.ReadOnlyEnforcement)
	}
	if len(got.Lanes) != 2 {
		t.Fatalf("Lanes len = %d, want traceability for both lanes", len(got.Lanes))
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

func TestFinalizeDeepCopiesLaneOutputs(t *testing.T) {
	outputs := []LaneOutput{
		{
			LaneID:        lanePrimary,
			Provider:      "provider-a",
			Model:         "model-a",
			Verdict:       VerdictPassWithFindings,
			Summary:       "has finding",
			FindingsCount: 1,
			Findings: []Finding{
				{
					Title:    "finding",
					File:     "internal/reviewquorum/finalize.go",
					Start:    12,
					Evidence: []Evidence{{Kind: "file", Path: "finding.go"}},
				},
			},
			Evidence: []Evidence{{Kind: "file", Path: "lane.go"}},
			Usage:    &Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed: true,
				Enabled:  true,
				Passed:   true,
				Notes:    []string{"captured"},
			},
			MutationsDelta: MutationsDelta{Changed: []StatusEntry{{Path: "clean.go", Status: "M"}}},
		},
	}
	got := Finalize(reviewSubject, reviewBaseRef, outputs)

	outputs[0].Findings[0].Title = "mutated"
	outputs[0].Findings[0].Evidence[0].Path = "mutated.go"
	outputs[0].Evidence[0].Path = "mutated-lane.go"
	outputs[0].Usage.TotalTokens = 99
	outputs[0].ReadOnlyEnforcement.Notes[0] = "mutated"
	outputs[0].MutationsDelta.Changed[0].Path = "mutated.go"

	lane := got.Lanes[0]
	if lane.Findings[0].Title != "finding" {
		t.Fatalf("lane finding title = %q, want copied finding", lane.Findings[0].Title)
	}
	if lane.Findings[0].Evidence[0].Path != "finding.go" {
		t.Fatalf("lane finding evidence = %+v, want copied finding evidence", lane.Findings[0].Evidence)
	}
	if lane.Evidence[0].Path != "lane.go" {
		t.Fatalf("lane evidence = %+v, want copied lane evidence", lane.Evidence)
	}
	if lane.Usage.TotalTokens != 3 {
		t.Fatalf("lane usage = %+v, want copied usage", lane.Usage)
	}
	if lane.ReadOnlyEnforcement.Notes[0] != "captured" {
		t.Fatalf("lane notes = %+v, want copied notes", lane.ReadOnlyEnforcement.Notes)
	}
	if lane.MutationsDelta.Changed[0].Path != "clean.go" {
		t.Fatalf("lane mutations = %+v, want copied mutations", lane.MutationsDelta)
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

func TestFinalizeRejectsMissingSuccessFailureClass(t *testing.T) {
	got := Finalize(reviewSubject, reviewBaseRef, []LaneOutput{
		{
			LaneID:        lanePrimary,
			Provider:      "test-provider",
			Model:         "test-model",
			Verdict:       VerdictPass,
			FindingsCount: 0,
			ReadOnlyEnforcement: ReadOnlyEnforcement{
				Observed:        true,
				Enabled:         true,
				Passed:          true,
				BaselineCommand: readOnlyStatusCommand,
				AfterCommand:    readOnlyStatusCommand,
			},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureClass != FailureClassHard {
		t.Fatalf("FailureClass = %q, want hard", got.FailureClass)
	}
	if got.FailureReason != "lane=primary reason=failure_class_missing" {
		t.Fatalf("FailureReason = %q, want failure_class_missing", got.FailureReason)
	}
}

func TestFinalizeRejectsMissingReadOnlyProofCommands(t *testing.T) {
	tests := []struct {
		name            string
		baselineCommand string
		afterCommand    string
		reason          string
	}{
		{
			name:            "missing baseline command",
			baselineCommand: "",
			afterCommand:    readOnlyStatusCommand,
			reason:          "lane=primary reason=read_only_baseline_command_missing",
		},
		{
			name:            "missing after command",
			baselineCommand: readOnlyStatusCommand,
			afterCommand:    " ",
			reason:          "lane=primary reason=read_only_after_command_missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Finalize(reviewSubject, reviewBaseRef, []LaneOutput{
				{
					LaneID:        lanePrimary,
					Provider:      "test-provider",
					Model:         "test-model",
					Verdict:       VerdictPass,
					FindingsCount: 0,
					ReadOnlyEnforcement: ReadOnlyEnforcement{
						Observed:        true,
						Enabled:         true,
						Passed:          true,
						BaselineCommand: tt.baselineCommand,
						AfterCommand:    tt.afterCommand,
					},
					FailureClass: FailureClassNone,
				},
			})
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

func TestFinalizeRejectsLaneProvidedFindingProvenance(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:        lanePrimary,
			Verdict:       VerdictPassWithFindings,
			FindingsCount: 1,
			Findings: []Finding{
				{
					Title: "finding",
					File:  "internal/reviewquorum/finalize.go",
					Start: 12,
					Lanes: []string{"reviewer-supplied"},
				},
			},
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %q, want fail", got.Verdict)
	}
	if got.FailureReason != "lane=primary reason=finding_lanes_not_allowed" {
		t.Fatalf("FailureReason = %q, want finding_lanes_not_allowed", got.FailureReason)
	}
	if got.FindingsCount != 0 || len(got.Findings) != 0 {
		t.Fatalf("Findings = %d/%d, want invalid lane finding excluded", got.FindingsCount, len(got.Findings))
	}
}

func TestFinalizeNormalizesNilLaneArraysInSummaryJSON(t *testing.T) {
	got := finalize([]LaneOutput{
		{
			LaneID:              lanePrimary,
			Verdict:             VerdictPass,
			FindingsCount:       0,
			ReadOnlyEnforcement: ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true},
		},
	})
	if len(got.Lanes) != 1 {
		t.Fatalf("Lanes len = %d, want 1", len(got.Lanes))
	}
	if got.Lanes[0].Findings == nil {
		t.Fatal("lane Findings = nil, want empty array for durable JSON")
	}
	if got.Lanes[0].Evidence == nil {
		t.Fatal("lane Evidence = nil, want empty array for durable JSON")
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var summary struct {
		Lanes []struct {
			Findings json.RawMessage `json:"findings"`
			Evidence json.RawMessage `json:"evidence"`
		} `json:"lanes"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if string(summary.Lanes[0].Findings) != "[]" {
		t.Fatalf("lane findings JSON = %s, want []", summary.Lanes[0].Findings)
	}
	if string(summary.Lanes[0].Evidence) != "[]" {
		t.Fatalf("lane evidence JSON = %s, want []", summary.Lanes[0].Evidence)
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
