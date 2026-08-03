package modelwindow

import "testing"

func TestWindow(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		// Modern Claude variants resolve to 1M WITHOUT the "[1m]" suffix — 1M is
		// their plain default, and the provider echoes the model ID back without
		// the launch flag, so a session log only ever carries the bare form.
		{"claude-opus-4-8", Million},
		{"claude-opus-4-7", Million},
		{"claude-opus-4-6", Million},
		{"claude-opus-5", Million},
		{"claude-sonnet-4-6", Million},
		{"claude-sonnet-5", Million},
		{"claude-sonnet-5-20260101", Million}, // dated variant still matches
		{"claude-opus-4-8-20260101", Million}, // dated variant still matches
		{"claude-opus-5-20260724", Million},   // dated variant still matches
		{"CLAUDE-OPUS-5", Million},            // case-insensitive
		{"claude-fable-5", Million},
		{"claude-mythos-1", Million},
		// The explicit "[1m]" suffix forces 1M for any Claude family, including
		// ones whose bare form is 200K.
		{"claude-opus-4-8[1m]", Million},
		{"sonnet[1m]", Million},
		{"claude-haiku-4-5-20251001[1m]", Million},
		// Older Claude families stay at the conservative default. The opus-5
		// marker must not swallow opus-4-5/opus-4-1 by substring.
		{"claude-opus-4-5-20251101", Default},
		{"claude-opus-4-1-20250805", Default},
		{"claude-sonnet-4-5-20250929", Default},
		{"claude-haiku-4-5-20251001", Default},
		// Non-Claude families.
		{"gemini-2.5-pro", Million},
		{"gpt-5-20260101", 258_000},
		{"codex-mini-latest", 258_000},
		{"gpt-4o-2024-08-06", 128_000},
		{"gpt-4-turbo", 128_000},
		// Unrecognized families return 0 so callers apply their own policy.
		{"unknown-model-xyz", 0},
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := Window(tt.model); got != tt.want {
				t.Errorf("Window(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}
