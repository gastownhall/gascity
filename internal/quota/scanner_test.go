package quota

import "testing"

func TestScannerDetectsRateLimit(t *testing.T) {
	s := NewScanner(map[string][]string{
		"claude": {
			`(?i)you've hit your limit`,
			`(?i)rate limit`,
			`(?i)too many requests`,
		},
	})

	tests := []struct {
		name     string
		output   string
		provider string
		want     bool
	}{
		{"exact match", "You've hit your limit", "claude", true},
		{"case insensitive", "RATE LIMIT exceeded", "claude", true},
		{"mixed output", "normal line\ntoo many requests\nmore output", "claude", true},
		{"no match", "everything is fine\nno problems here", "claude", false},
		{"unknown provider", "rate limit", "gemini", false},
		{"empty output", "", "claude", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Scan(tt.output, tt.provider)
			if got != tt.want {
				t.Errorf("Scan() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScannerInvalidPattern(t *testing.T) {
	// Invalid regex should be silently skipped.
	s := NewScanner(map[string][]string{
		"bad": {"[invalid"},
	})
	if len(s.patterns["bad"]) != 0 {
		t.Errorf("expected 0 compiled patterns for invalid regex, got %d", len(s.patterns["bad"]))
	}
}
