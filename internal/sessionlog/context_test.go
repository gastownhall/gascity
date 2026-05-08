package sessionlog

import "testing"

func TestModelContextWindow(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-opus-4-5-20251101", 200_000},
		{"claude-sonnet-4-5-20251101", 200_000},
		{"claude-haiku-4-5-20251001", 200_000},
		{"gemini-2.5-pro", 1_000_000},
		{"gpt-5-20260101", 400_000},
		{"codex-mini-latest", 400_000},
		{"gpt-4o-2024-08-06", 128_000},
		{"gpt-4-turbo", 128_000},
		{"unknown-model-xyz", 0},
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := ModelContextWindow(tt.model)
			if got != tt.want {
				t.Errorf("ModelContextWindow(%q) = %d, want %d", tt.model, got, tt.want)
			}
		})
	}
}
