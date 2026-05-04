package worker

import "testing"

func TestDerivedResumeSessionKeyOpenCodeUsesProviderSessionID(t *testing.T) {
	got := derivedResumeSessionKey("opencode/tmux-cli", "ses_21523e55fffeqoQOyaIoQtfdf5")
	if got != "ses_21523e55fffeqoQOyaIoQtfdf5" {
		t.Fatalf("derivedResumeSessionKey(opencode) = %q, want provider session id", got)
	}
}

func TestDerivedResumeSessionKeyNonResumeProviderStaysEmpty(t *testing.T) {
	got := derivedResumeSessionKey("gemini/tmux-cli", "ses_21523e55fffeqoQOyaIoQtfdf5")
	if got != "" {
		t.Fatalf("derivedResumeSessionKey(gemini) = %q, want empty", got)
	}
}
