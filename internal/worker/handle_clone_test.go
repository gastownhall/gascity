package worker

import "testing"

// TestProfileFamily pins the profile-to-family mapping used by clone and
// continuation handling. Losing a case here silently routes that profile's
// clone behavior through the empty-family default.
func TestProfileFamily(t *testing.T) {
	tests := []struct {
		profile Profile
		want    string
	}{
		{profile: ProfileClaudeTmuxCLI, want: "claude"},
		{profile: ProfileCodexTmuxCLI, want: "codex"},
		{profile: ProfileGeminiTmuxCLI, want: "gemini"},
		{profile: ProfileKimiTmuxCLI, want: "kimi"},
		{profile: ProfileOpenCodeTmuxCLI, want: "opencode"},
		{profile: ProfilePiTmuxCLI, want: "pi"},
		{profile: ProfileAntigravityTmuxCLI, want: "antigravity"},
		{profile: Profile("unknown/tmux-cli"), want: ""},
	}
	for _, tt := range tests {
		name := string(tt.profile)
		t.Run(name, func(t *testing.T) {
			if got := profileFamily(tt.profile); got != tt.want {
				t.Fatalf("profileFamily(%q) = %q, want %q", tt.profile, got, tt.want)
			}
		})
	}
}
