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
	if len(profiles) != 3 {
		t.Fatalf("profiles = %d, want 3", len(profiles))
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
				if fresh.TranscriptPath == "" {
					t.Fatal("expected discovered transcript path")
				}
				if fresh.TranscriptPathHint == "." {
					t.Fatalf("relative transcript path = %q, want provider-native file path", fresh.TranscriptPathHint)
				}
			})

			t.Run(string(RequirementTranscriptNormalization), func(t *testing.T) {
				if len(fresh.Messages) < 2 {
					t.Fatalf("messages = %d, want at least 2", len(fresh.Messages))
				}
				if fresh.History == nil {
					t.Fatal("expected history snapshot")
				}
				if fresh.History.ProviderSessionID == "" {
					t.Fatal("provider session id is empty")
				}
				if fresh.History.LogicalConversationID == "" {
					t.Fatal("logical conversation id is empty")
				}
				if fresh.History.TranscriptStreamID == "" {
					t.Fatal("transcript stream id is empty")
				}
				if fresh.History.Generation.ID == "" {
					t.Fatal("generation id is empty")
				}
				if fresh.History.Cursor.AfterEntryID == "" {
					t.Fatal("cursor after-entry id is empty")
				}
				if fresh.History.Continuity.Status == worker.ContinuityStatusUnknown {
					t.Fatal("continuity status is unknown")
				}
				if got, want := len(fresh.History.Entries), len(fresh.Messages); got != want {
					t.Fatalf("history entries = %d, want %d", got, want)
				}
				if fresh.Messages[0].Role != "user" {
					t.Fatalf("first role = %q, want user", fresh.Messages[0].Role)
				}
				if fresh.Messages[0].Text == "" {
					t.Fatal("first normalized message text is empty")
				}
				if fresh.Messages[len(fresh.Messages)-1].Text == "" {
					t.Fatal("last normalized message text is empty")
				}
			})

			t.Run(string(RequirementContinuationContinuity), func(t *testing.T) {
				result := ContinuationResult(profile, fresh, continued)
				if err := result.Err(); err != nil {
					t.Fatal(err)
				}
			})

			t.Run(string(RequirementFreshSessionIsolation), func(t *testing.T) {
				result := FreshSessionResult(profile, fresh, reset)
				if err := result.Err(); err != nil {
					t.Fatal(err)
				}
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

func mustLoadSnapshot(t *testing.T, profile Profile, fixtureRoot string) *Snapshot {
	t.Helper()

	root := filepath.Clean(fixtureRoot)
	snapshot, err := LoadSnapshot(profile, root)
	if err != nil {
		t.Fatalf("LoadSnapshot(%s, %s): %v", profile.ID, root, err)
	}
	return snapshot
}
