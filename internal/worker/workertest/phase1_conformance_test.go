package workertest

import (
	"path/filepath"
	"testing"
)

func TestPhase1CatalogProfilesStayAligned(t *testing.T) {
	catalog := Phase1Catalog()
	if len(catalog) != 4 {
		t.Fatalf("catalog entries = %d, want 4", len(catalog))
	}

	profiles := Phase1Profiles()
	if len(profiles) != 3 {
		t.Fatalf("profiles = %d, want 3", len(profiles))
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

func TestContinuationOracleRequiresRestartRecallSignal(t *testing.T) {
	profile := Phase1Profiles()[0]
	before := &Snapshot{
		SessionID:          "session-1",
		TranscriptPathHint: "session.jsonl",
		Messages: []NormalizedMessage{
			{Role: "user", Text: "Summarize the worker transcript contract."},
			{Role: "assistant", Text: profile.Continuation.AnchorText},
		},
	}
	after := &Snapshot{
		SessionID:          "session-1",
		TranscriptPathHint: "session.jsonl",
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
