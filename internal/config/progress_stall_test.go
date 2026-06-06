package config

import (
	"testing"
	"time"
)

func TestProgressStallTimeoutDuration(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"unset disables (zero)", "", 0},
		{"valid duration", "30m", 30 * time.Minute},
		{"too small clamps to safety floor", "30s", ProgressStallTimeoutMinimum},
		{"unparseable disables (zero)", "not-a-duration", 0},
		{"negative disables (zero)", "-5m", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &SessionConfig{ProgressStallTimeout: tc.value}
			if got := s.ProgressStallTimeoutDuration(); got != tc.want {
				t.Errorf("ProgressStallTimeoutDuration(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
