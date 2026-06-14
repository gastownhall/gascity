package workertest

import (
	"path/filepath"
	"testing"

	worker "github.com/gastownhall/gascity/internal/worker"
)

func TestPhase1CatalogProfilesStayAligned(t *testing.T) {
	catalog := Phase1Catalog()
	expectedCodes := []RequirementCode{
		RequirementTranscriptDiscovery,
		RequirementTranscriptNormalization,
		RequirementTranscriptUsage,
		RequirementInvocationUsageCost,
		RequirementContinuationContinuity,
		RequirementFreshSessionIsolation,
	}
	if len(catalog) != len(expectedCodes) {
		t.Fatalf("catalog entries = %d, want %d", len(catalog), len(expectedCodes))
	}
	seen := make(map[RequirementCode]Requirement, len(catalog))
	for _, requirement := range catalog {
		if requirement.Group == "" {
			t.Fatalf("requirement %s has empty group", requirement.Code)
		}
		if requirement.Description == "" {
			t.Fatalf("requirement %s has empty description", requirement.Code)
		}
		seen[requirement.Code] = requirement
	}
	for _, code := range expectedCodes {
		if _, ok := seen[code]; !ok {
			t.Fatalf("catalog missing requirement %s", code)
		}
	}

	profiles := Phase1Profiles()
	if len(profiles) != 8 {
		t.Fatalf("profiles = %d, want 8", len(profiles))
	}
	for _, profile := range profiles {
		if profile.Continuation.AnchorText == "" {
			t.Fatalf("profile %s missing continuation anchor text", profile.ID)
		}
		if profile.Continuation.RecallPromptContains == "" {
			t.Fatalf("profile %s missing recall prompt matcher", profile.ID)
		}
		if profile.Continuation.RecallResponseContains == "" {
			t.Fatalf("profile %s missing recall response matcher", profile.ID)
		}
		if profile.Continuation.ResetResponseContains == "" {
			t.Fatalf("profile %s missing reset response matcher", profile.ID)
		}
	}
}

func TestPhase1Conformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase1", map[string]string{
		"tier": "worker-core",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			fresh := mustLoadSnapshot(t, profile, profile.Fixtures.FreshRoot)
			continued := mustLoadSnapshot(t, profile, profile.Fixtures.ContinuationRoot)
			reset := mustLoadSnapshot(t, profile, profile.Fixtures.ResetRoot)

			t.Run(string(RequirementTranscriptDiscovery), func(t *testing.T) {
				reporter.Require(t, TranscriptDiscoveryResult(profile, fresh))
			})

			t.Run(string(RequirementTranscriptNormalization), func(t *testing.T) {
				reporter.Require(t, TranscriptNormalizationResult(profile, fresh))
			})

			t.Run(string(RequirementTranscriptUsage), func(t *testing.T) {
				result := TranscriptUsageResult(profile, fresh)
				if result.Status == ResultUnsupported {
					// Families without an invocation-usage extractor are out of
					// scope: record the outcome but do not fail the suite.
					reporter.Record(result)
					t.Skip(result.Detail)
					return
				}
				reporter.Require(t, result)
			})

			t.Run(string(RequirementInvocationUsageCost), func(t *testing.T) {
				result := TranscriptUsageCostResult(profile, fresh)
				if result.Status == ResultUnsupported {
					reporter.Record(result)
					t.Skip(result.Detail)
					return
				}
				reporter.Require(t, result)
			})

			t.Run(string(RequirementContinuationContinuity), func(t *testing.T) {
				reporter.Require(t, ContinuationResult(profile, fresh, continued))
			})

			t.Run(string(RequirementFreshSessionIsolation), func(t *testing.T) {
				reporter.Require(t, FreshSessionResult(profile, fresh, reset))
			})
		})
	}
}

func TestPhase1ContinuationOracleRequiresRestartRecallSignal(t *testing.T) {
	profile := Phase1Profiles()[0]
	before := &Snapshot{
		SessionID:          "session-1",
		TranscriptPathHint: "session.jsonl",
		History: &worker.HistorySnapshot{
			LogicalConversationID: "logical-1",
			Cursor:                worker.Cursor{AfterEntryID: "a1"},
		},
		Messages: []NormalizedMessage{
			{Role: "user", Text: "Summarize the worker transcript contract."},
			{Role: "assistant", Text: profile.Continuation.AnchorText},
		},
	}
	after := &Snapshot{
		SessionID:          "session-1",
		TranscriptPathHint: "session.jsonl",
		History: &worker.HistorySnapshot{
			LogicalConversationID: "logical-1",
			Cursor:                worker.Cursor{AfterEntryID: "a2"},
		},
		Messages: []NormalizedMessage{
			{Role: "user", Text: "Summarize the worker transcript contract."},
			{Role: "assistant", Text: profile.Continuation.AnchorText},
			{Role: "user", Text: profile.Continuation.RecallPromptContains},
			{Role: "assistant", Text: "Continuation preserves normalized history."},
		},
	}

	result := ContinuationResult(profile, before, after)
	if err := result.Err(); err == nil {
		t.Fatal("ContinuationResult should fail without recall response anchor")
	}
}

func TestTranscriptUsageResultDetectsMismatchAndDrift(t *testing.T) {
	base := Phase1Profiles()[0] // claude/tmux-cli, a supported family
	snapshot := mustLoadSnapshot(t, base, base.Fixtures.FreshRoot)

	t.Run("expected totals must match the extracted usage", func(t *testing.T) {
		bad := base
		bad.Usage.InputTokens = base.Usage.InputTokens + 999
		result := TranscriptUsageResult(bad, snapshot)
		if result.Err() == nil {
			t.Fatal("TranscriptUsageResult should fail when expected input tokens diverge from the fixture")
		}
	})

	t.Run("supported family declaring no expectation is drift", func(t *testing.T) {
		drift := base
		drift.Usage = UsageExpectation{} // Supported:false while the worker supports claude
		result := TranscriptUsageResult(drift, snapshot)
		if result.Status != ResultFail {
			t.Fatalf("status = %q, want fail (worker supports the family but the profile declares none)", result.Status)
		}
	})

	t.Run("unsupported family is reported out of scope", func(t *testing.T) {
		var pi Profile
		for _, profile := range Phase1Profiles() {
			if profile.ID == ProfilePiTmuxCLI {
				pi = profile
				break
			}
		}
		result := TranscriptUsageResult(pi, &Snapshot{})
		if result.Status != ResultUnsupported {
			t.Fatalf("status = %q, want unsupported for a family with no invocation-usage extractor", result.Status)
		}
	})
}

func TestTranscriptUsageCostResultDetectsModelAndPricingDrift(t *testing.T) {
	base := Phase1Profiles()[0] // claude/tmux-cli, default-priced
	snapshot := mustLoadSnapshot(t, base, base.Fixtures.FreshRoot)

	t.Run("happy path prices the extracted usage", func(t *testing.T) {
		if err := TranscriptUsageCostResult(base, snapshot).Err(); err != nil {
			t.Fatalf("TranscriptUsageCostResult should pass for the claude fixture: %v", err)
		}
	})

	t.Run("wrong expected model fails", func(t *testing.T) {
		bad := base
		bad.Usage.Model = "not-the-fixture-model"
		if TranscriptUsageCostResult(bad, snapshot).Err() == nil {
			t.Fatal("TranscriptUsageCostResult should fail when the expected model does not match the extracted usage")
		}
	})

	t.Run("default-cost-priced drift fails", func(t *testing.T) {
		bad := base
		bad.Usage.DefaultCostPriced = false // claude IS default-priced
		if TranscriptUsageCostResult(bad, snapshot).Err() == nil {
			t.Fatal("TranscriptUsageCostResult should fail when DefaultCostPriced disagrees with the default registry")
		}
	})
}

func TestTelemetryHandleCatalogRegistersRecordingRequirement(t *testing.T) {
	catalog := TelemetryHandleCatalog()
	if len(catalog) != 1 {
		t.Fatalf("TelemetryHandleCatalog() entries = %d, want 1", len(catalog))
	}
	req := catalog[0]
	if req.Code != RequirementInvocationUsageRecording {
		t.Fatalf("code = %q, want %q", req.Code, RequirementInvocationUsageRecording)
	}
	if req.Group == "" || req.Description == "" {
		t.Fatalf("requirement %s has empty group or description", req.Code)
	}
}

func mustLoadSnapshot(t *testing.T, profile Profile, fixtureRoot string) *Snapshot {
	t.Helper()

	root := filepath.Clean(fixtureRoot)
	snapshot, err := LoadSnapshot(profile, root)
	if err != nil {
		t.Fatalf("LoadSnapshot(%s, %s): %v", profile.ID, root, err)
	}
	return snapshot
}
