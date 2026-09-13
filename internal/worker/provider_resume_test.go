package worker

import "testing"

func TestDerivedResumeSessionKeyCodexStaysEmpty(t *testing.T) {
	threadID := "019e1b65-5457-7301-a550-57a3d0d0919a"
	got := derivedResumeSessionKey("codex/tmux-cli", "rollout-2026-05-12T08-54-46-"+threadID+".jsonl")
	if got != "" {
		t.Fatalf("derivedResumeSessionKey(codex) = %q, want empty", got)
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

func TestDerivedResumeSessionKeyHookManagedProvidersStayEmpty(t *testing.T) {
	tests := []struct {
		provider string
		key      string
	}{
		{provider: "opencode/tmux-cli", key: "ses_21523e55fffeqoQOyaIoQtfdf5"},
		{provider: "mimocode/tmux-cli", key: "ses_31523e55fffeqoQOyaIoQtfdf6"},
		{provider: "kimi/tmux-cli", key: "fe8717c9-1903-4bd4-b8e5-159caeb56f1a"},
		{provider: "pi/tmux-cli", key: "pi-session-123"},
		{provider: "omp/tmux-cli", key: "omp-session-123"},
		{provider: "antigravity/tmux-cli", key: "750fa972-4c56-4215-99b9-893382aee2b4"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := derivedResumeSessionKey(tt.provider, tt.key); got != "" {
				t.Fatalf("derivedResumeSessionKey(%q) = %q, want empty; hook-time GC_PROVIDER_SESSION_ID should persist it", tt.provider, got)
			}
		})
	}
}
