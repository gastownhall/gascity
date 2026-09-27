package dispatch

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/reviewquorum"
	"github.com/gastownhall/gascity/internal/rsipolicy"
)

func TestProcessRSIPromotionGatePromotesFromDurableEvidence(t *testing.T) {
	store := beads.NewMemStore()
	evidence := passingRSICandidateEvidence()
	candidate := mustCreate(t, store, beads.Bead{
		Title: "candidate",
		Metadata: map[string]string{
			beadmeta.RSIRoleMetadataKey:    beadmeta.RSIRoleImprover,
			beadmeta.OutputJSONMetadataKey: mustJSON(t, evidence),
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		},
	})
	correctness := mustCreate(t, store, beads.Bead{
		Title: "correctness judge",
		Metadata: map[string]string{
			beadmeta.RSIRoleMetadataKey:    beadmeta.RSIRoleJudge,
			beadmeta.OutputJSONMetadataKey: mustJSON(t, passingRSILane("correctness", "judge-a")),
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		},
	})
	performance := mustCreate(t, store, beads.Bead{
		Title: "performance judge",
		Metadata: map[string]string{
			beadmeta.RSIRoleMetadataKey:    beadmeta.RSIRoleJudge,
			beadmeta.OutputJSONMetadataKey: mustJSON(t, passingRSILane("performance", "judge-b")),
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		},
	})
	gate := mustCreate(t, store, beads.Bead{
		Title: "rsi promotion gate",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindRSIPromotionGate,
		},
	})
	mustDep(t, store, gate.ID, candidate.ID, "blocks")
	mustDep(t, store, gate.ID, correctness.ID, "blocks")
	mustDep(t, store, gate.ID, performance.ID, "blocks")

	result, err := ProcessControl(store, mustGet(t, store, gate.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "rsi-promote" {
		t.Fatalf("result = %+v, want processed rsi-promote", result)
	}
	after := mustGet(t, store, gate.ID)
	if after.Status != "closed" || after.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		t.Fatalf("gate status/outcome = %q/%q, want closed/pass", after.Status, after.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if after.Metadata[beadmeta.RSIPromoteMetadataKey] != "true" {
		t.Fatalf("gc.rsi_promote = %q, want true", after.Metadata[beadmeta.RSIPromoteMetadataKey])
	}
	var decision rsipolicy.Decision
	if err := json.Unmarshal([]byte(after.Metadata[beadmeta.OutputJSONMetadataKey]), &decision); err != nil {
		t.Fatalf("unmarshal decision: %v", err)
	}
	if !decision.Promote || decision.RollbackBundleID != evidence.Current.ID {
		t.Fatalf("decision = %+v, want promote with rollback %q", decision, evidence.Current.ID)
	}
}

func TestProcessRSIPromotionGateRequiresHumanApprovalForSensitiveAuthority(t *testing.T) {
	store := beads.NewMemStore()
	evidence := passingRSICandidateEvidence()
	candidate := mustCreate(t, store, beads.Bead{
		Title: "candidate",
		Metadata: map[string]string{
			beadmeta.RSIRoleMetadataKey:    beadmeta.RSIRoleImprover,
			beadmeta.OutputJSONMetadataKey: mustJSON(t, evidence),
		},
	})
	correctness := mustCreate(t, store, beads.Bead{Title: "correctness judge", Metadata: map[string]string{
		beadmeta.RSIRoleMetadataKey: beadmeta.RSIRoleJudge, beadmeta.OutputJSONMetadataKey: mustJSON(t, passingRSILane("correctness", "judge-a")),
	}})
	performance := mustCreate(t, store, beads.Bead{Title: "performance judge", Metadata: map[string]string{
		beadmeta.RSIRoleMetadataKey: beadmeta.RSIRoleJudge, beadmeta.OutputJSONMetadataKey: mustJSON(t, passingRSILane("performance", "judge-b")),
	}})
	gate := mustCreate(t, store, beads.Bead{Title: "rsi promotion gate", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindRSIPromotionGate, beadmeta.RSIAuthorityClassMetadataKey: "safety_policy",
	}})
	mustDep(t, store, gate.ID, candidate.ID, "blocks")
	mustDep(t, store, gate.ID, correctness.ID, "blocks")
	mustDep(t, store, gate.ID, performance.ID, "blocks")

	result, err := ProcessControl(store, mustGet(t, store, gate.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "rsi-human-approval" {
		t.Fatalf("Action = %q, want rsi-human-approval", result.Action)
	}
	after := mustGet(t, store, gate.ID)
	if after.Metadata[beadmeta.RSIPromoteMetadataKey] != "false" || after.Metadata[beadmeta.RSIManualApprovalMetadataKey] != "true" {
		t.Fatalf("gate decision metadata = %v, want pending human approval", after.Metadata)
	}
}

func TestProcessRSIPromotionGateRejectsMissingIndependentJudge(t *testing.T) {
	store := beads.NewMemStore()
	evidence := passingRSICandidateEvidence()
	candidate := mustCreate(t, store, beads.Bead{
		Title: "candidate",
		Metadata: map[string]string{
			beadmeta.RSIRoleMetadataKey:    beadmeta.RSIRoleImprover,
			beadmeta.OutputJSONMetadataKey: mustJSON(t, evidence),
		},
	})
	judge := mustCreate(t, store, beads.Bead{
		Title: "only judge",
		Metadata: map[string]string{
			beadmeta.RSIRoleMetadataKey:    beadmeta.RSIRoleJudge,
			beadmeta.OutputJSONMetadataKey: mustJSON(t, passingRSILane("correctness", "judge-a")),
		},
	})
	gate := mustCreate(t, store, beads.Bead{
		Title:    "rsi promotion gate",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRSIPromotionGate},
	})
	mustDep(t, store, gate.ID, candidate.ID, "blocks")
	mustDep(t, store, gate.ID, judge.ID, "blocks")

	result, err := ProcessControl(store, mustGet(t, store, gate.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "rsi-reject" {
		t.Fatalf("Action = %q, want rsi-reject", result.Action)
	}
	after := mustGet(t, store, gate.ID)
	if after.Metadata[beadmeta.RSIPromoteMetadataKey] != "false" || after.Metadata[beadmeta.RSIReasonMetadataKey] != rsipolicy.ReasonInsufficientJudges {
		t.Fatalf("gate decision metadata = %v, want rejected for insufficient judges", after.Metadata)
	}
}

func passingRSICandidateEvidence() rsipolicy.CandidateEvidence {
	return rsipolicy.CandidateEvidence{
		Objective: "improve discovery latency",
		Current: rsipolicy.Bundle{
			ID: "bundle-001", TypesCommit: "types-1", AICommit: "ai-1", FrontendCommit: "frontend-1", InktreeCommit: "inktree-1", EvalSuiteHash: "suite-1",
		},
		Candidate: rsipolicy.Bundle{
			ID: "bundle-002", TypesCommit: "types-1", AICommit: "ai-2", FrontendCommit: "frontend-1", InktreeCommit: "inktree-1", EvalSuiteHash: "suite-1", Parent: "bundle-001",
		},
		Baseline:         rsipolicy.Metrics{Score: 0.8, SafetyScore: 0.96, LatencyMS: 100, CostUSD: 1, DependencyCount: 3},
		CandidateMetrics: rsipolicy.Metrics{Score: 0.9, SafetyScore: 0.97, LatencyMS: 110, CostUSD: 1.1, DependencyCount: 3},
		Limits:           rsipolicy.Limits{MinSafetyScore: 0.95, MaxLatencyMS: 200, MaxCostUSD: 2, MaxDependencies: 4},
		Improver:         "jules", Judges: []string{"correctness", "performance"}, ReviewSubject: "candidate:bundle-002", ReviewBaseRef: "bundle-001", Attempts: 1, MaxAttempts: 3,
	}
}

func passingRSILane(id, provider string) reviewquorum.LaneOutput {
	return reviewquorum.LaneOutput{
		LaneID: id, Provider: provider, Model: "model", Verdict: reviewquorum.VerdictPass, FailureClass: reviewquorum.FailureClassNone,
		ReadOnlyEnforcement: reviewquorum.ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true, BaselineCommand: "git status --porcelain=v1 -z", AfterCommand: "git status --porcelain=v1 -z"},
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}
