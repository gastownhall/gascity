package worker

import "testing"

func TestDerivedResumeSessionKeyOpenCodeUsesProviderSessionID(t *testing.T) {
	got := derivedResumeSessionKey("opencode/tmux-cli", "ses_21523e55fffeqoQOyaIoQtfdf5")
	if got != "ses_21523e55fffeqoQOyaIoQtfdf5" {
		t.Fatalf("derivedResumeSessionKey(opencode) = %q, want provider session id", got)
	}
}

func TestDerivedResumeSessionKeyKimiUsesProviderSessionID(t *testing.T) {
	got := derivedResumeSessionKey("kimi/tmux-cli", "fe8717c9-1903-4bd4-b8e5-159caeb56f1a")
	if got != "fe8717c9-1903-4bd4-b8e5-159caeb56f1a" {
		t.Fatalf("derivedResumeSessionKey(kimi) = %q, want provider session id", got)
	}
}

func TestDerivedResumeSessionKeyCodexExtractsThreadID(t *testing.T) {
	threadID := "019e1b65-5457-7301-a550-57a3d0d0919a"
	got := derivedResumeSessionKey("codex/tmux-cli", "rollout-2026-05-12T08-54-46-"+threadID+".jsonl")
	if got != threadID {
		t.Fatalf("derivedResumeSessionKey(codex) = %q, want %q", got, threadID)
	}
}

func TestDerivedResumeSessionKeyClaudeStaysEmpty(t *testing.T) {
	got := derivedResumeSessionKey("claude/tmux-cli", "gc-123")
	if got != "" {
		t.Fatalf("derivedResumeSessionKey(claude) = %q, want empty", got)
	}
}

func TestDerivedResumeSessionKeyNonResumeProviderStaysEmpty(t *testing.T) {
	got := derivedResumeSessionKey("gemini/tmux-cli", "ses_21523e55fffeqoQOyaIoQtfdf5")
	if got != "" {
		t.Fatalf("derivedResumeSessionKey(gemini) = %q, want empty", got)
	}
}

func TestDerivedResumeSessionKeyDoesNotClassifyPiSubstringAsPi(t *testing.T) {
	got := derivedResumeSessionKey("api/tmux-cli", "ses_21523e55fffeqoQOyaIoQtfdf5")
	if got != "" {
		t.Fatalf("derivedResumeSessionKey(api) = %q, want empty", got)
	}
}
