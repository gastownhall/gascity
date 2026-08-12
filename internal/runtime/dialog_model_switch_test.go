package runtime

import "testing"

// TestContainsModelSwitchModal verifies the high-confidence matcher for the
// mid-session Codex/GPT "approaching rate limits — switch to a cheaper model?"
// modal. It must match the real modal but NOT ordinary agent output that merely
// mentions rate limits, so a mid-session dismiss cannot send spurious keystrokes
// into a working pane.
func TestContainsModelSwitchModal(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name: "codex model-switch modal",
			content: "Approaching rate limits\n" +
				"Switch to gpt-5.4-mini for lower credit usage?\n" +
				"› 1. Switch to gpt-5.4-mini\n" +
				"  2. Keep current model\n" +
				"  3. Keep current model (never show again)\n" +
				"Press enter to confirm or esc to go back",
			want: true,
		},
		{
			name: "ordinary work output mentioning rate limits does not match",
			content: "We're approaching rate limits on the gpt endpoint; the rate " +
				"limit is 1000 req/min. I'll switch to exponential backoff and " +
				"keep the current model config.",
			want: false,
		},
		{
			name:    "switch offer alone (no keep option) does not match",
			content: "Switch to a faster branch strategy for the release.",
			want:    false,
		},
		{
			name:    "empty",
			content: "",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsModelSwitchModal(tt.content); got != tt.want {
				t.Errorf("ContainsModelSwitchModal(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}
