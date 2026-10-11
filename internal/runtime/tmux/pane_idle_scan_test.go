package tmux

import "testing"

// PaneShowsIdlePrompt is the scan both local tmux (snapshotPaneIdleWithPrefix)
// and remote tmux-in-box runtimes apply to a captured pane, so a busy
// indicator must win over a visible prompt, the prompt may sit above a status
// footer, and NBSP or a boxed prompt must still match.
// Kills: a scan that ignores the busy indicator, stops at the last non-blank
// line, applies a default for a blank prefix, or drops the NBSP / box-border
// normalization.
func TestPaneShowsIdlePrompt(t *testing.T) {
	tests := []struct {
		name   string
		lines  []string
		prefix string
		want   bool
	}{
		{"idle claude prompt", []string{"output", "❯ ", ""}, DefaultReadyPromptPrefix, true},
		{"prompt above status footer", []string{"❯ ", "", "  ⏵⏵ bypass permissions on", ""}, DefaultReadyPromptPrefix, true},
		{"busy esc to interrupt", []string{"❯ ", "  esc to interrupt  "}, DefaultReadyPromptPrefix, false},
		{"busy spinner footer", []string{"❯ ", "· Boogieing… (2m 28s · ↓ 10.9k tokens)"}, DefaultReadyPromptPrefix, false},
		{"nbsp after prompt glyph", []string{"❯\u00a0"}, DefaultReadyPromptPrefix, true},
		{"boxed prompt", []string{"╭──────╮", "│ ❯ ", "╰──────╯"}, DefaultReadyPromptPrefix, true},
		{"custom prefix matches", []string{"› "}, "› ", true},
		{"custom prefix ignores default glyph", []string{"❯ "}, "› ", false},
		{"no prompt", []string{"building...", "still going"}, DefaultReadyPromptPrefix, false},
		{"empty pane", nil, DefaultReadyPromptPrefix, false},
		{"blank prefix is not defaulted", []string{"❯ "}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PaneShowsIdlePrompt(tt.lines, tt.prefix); got != tt.want {
				t.Errorf("PaneShowsIdlePrompt(%q, %q) = %v, want %v", tt.lines, tt.prefix, got, tt.want)
			}
		})
	}
}
