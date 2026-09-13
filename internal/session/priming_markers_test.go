package session

import (
	"testing"
	"time"
)

// TestPromptHash pins the empty-prompt gate, determinism, and distinctness.
func TestPromptHash(t *testing.T) {
	if got := PromptHash(""); got != "" {
		t.Fatalf("PromptHash(\"\") = %q, want empty (the P5 empty-prompt gate)", got)
	}
	a1 := PromptHash("hello world")
	a2 := PromptHash("hello world")
	if a1 == "" {
		t.Fatal("PromptHash of a non-empty prompt must be non-empty")
	}
	if a1 != a2 {
		t.Fatalf("PromptHash not deterministic: %q vs %q", a1, a2)
	}
	if b := PromptHash("hello world!"); b == a1 {
		t.Fatalf("distinct prompts hashed to the same value %q", a1)
	}
}

// TestCommitStartedPatchPriming proves the confirmation pair is both-or-neither
// and never stamps priming_attempted_at.
func TestCommitStartedPatchPriming(t *testing.T) {
	now := time.Date(2026, 7, 8, 1, 2, 3, 0, time.UTC)

	t.Run("zero PrimedAt stamps no priming keys", func(t *testing.T) {
		patch := CommitStartedPatch(CommitStartedPatchInput{CoreHash: "c", PromptHash: "h", Now: now})
		assertNoPrimingKeys(t, patch)
		if _, ok := patch["started_config_hash"]; !ok {
			t.Error("started_config_hash must still be written")
		}
	})

	t.Run("PrimedAt with empty hash stamps nothing (P5)", func(t *testing.T) {
		patch := CommitStartedPatch(CommitStartedPatchInput{CoreHash: "c", PrimedAt: now, PromptHash: "", Now: now})
		assertNoPrimingKeys(t, patch)
	})

	t.Run("both set stamps both, RFC3339 + verbatim", func(t *testing.T) {
		patch := CommitStartedPatch(CommitStartedPatchInput{CoreHash: "c", PrimedAt: now, PromptHash: "abc123", Now: now})
		if got, want := patch[PrimedAtMetadataKey], now.UTC().Format(time.RFC3339); got != want {
			t.Errorf("primed_at = %q, want %q", got, want)
		}
		if got := patch[PromptHashMetadataKey]; got != "abc123" {
			t.Errorf("prompt_hash = %q, want %q", got, "abc123")
		}
		if _, ok := patch[PrimingAttemptedAtMetadataKey]; ok {
			t.Error("CommitStartedPatch must never emit priming_attempted_at")
		}
	})

	t.Run("started_config_hash writer set unchanged", func(t *testing.T) {
		// The priming pair must not perturb started_config_hash's value.
		patch := CommitStartedPatch(CommitStartedPatchInput{CoreHash: "core-x", PrimedAt: now, PromptHash: "h", Now: now})
		if got := patch["started_config_hash"]; got != "core-x" {
			t.Errorf("started_config_hash = %q, want core-x", got)
		}
	})
}

// TestPrimingKeysClearedWhereverStartedConfigHashClears is the greppable form of
// the priming-key lifetime rule for the internal/session-owned clear sites
// (C-1..C-3): every one clears the three priming keys exactly when it clears
// started_config_hash.
func TestPrimingKeysClearedWhereverStartedConfigHashClears(t *testing.T) {
	t.Run("C-1 applyFreshWakeConversationReset", func(t *testing.T) {
		patch := MetadataPatch{}
		applyFreshWakeConversationReset(patch)
		assertClearsStartedHashAndPriming(t, patch)
	})

	t.Run("C-2 ConversationResetPatch clears when hash clears", func(t *testing.T) {
		cleared := ConversationResetPatch(true)
		assertClearsStartedHashAndPriming(t, cleared)

		// Churn arm keeps the hash — and therefore the markers.
		kept := ConversationResetPatch(false)
		if _, ok := kept["started_config_hash"]; ok {
			t.Fatal("churn arm must not clear started_config_hash")
		}
		for _, k := range primingResetKeys {
			if _, ok := kept[k]; ok {
				t.Errorf("churn arm must not clear priming key %s", k)
			}
		}
	})

	t.Run("C-7 RestartRequestPatch", func(t *testing.T) {
		patch := RestartRequestPatch("sess-key", time.Now())
		assertClearsStartedHashAndPriming(t, patch)
	})
}

// TestFreshWakeResetKeysAlignWithApply enforces the C-1 alignment note: the
// three priming keys appear in BOTH freshWakeConversationResetKeys and the
// applyFreshWakeConversationReset output.
func TestFreshWakeResetKeysAlignWithApply(t *testing.T) {
	listed := map[string]bool{}
	for _, k := range freshWakeConversationResetKeys {
		listed[k] = true
	}
	applied := MetadataPatch{}
	applyFreshWakeConversationReset(applied)
	for _, k := range primingResetKeys {
		if !listed[k] {
			t.Errorf("priming key %s missing from freshWakeConversationResetKeys", k)
		}
		if v, ok := applied[k]; !ok || v != "" {
			t.Errorf("priming key %s not cleared by applyFreshWakeConversationReset", k)
		}
	}
}

func assertNoPrimingKeys(t *testing.T, patch MetadataPatch) {
	t.Helper()
	for _, k := range primingResetKeys {
		if _, ok := patch[k]; ok {
			t.Errorf("unexpected priming key %s in patch", k)
		}
	}
}

func assertClearsStartedHashAndPriming(t *testing.T, patch MetadataPatch) {
	t.Helper()
	if got, ok := patch["started_config_hash"]; !ok || got != "" {
		t.Fatalf("started_config_hash not cleared (got %q, ok=%v)", got, ok)
	}
	for _, k := range primingResetKeys {
		if got, ok := patch[k]; !ok || got != "" {
			t.Errorf("priming key %s not cleared (got %q, ok=%v)", k, got, ok)
		}
	}
}
