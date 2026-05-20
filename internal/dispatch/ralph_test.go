package dispatch

import "testing"

func TestFormatGateExitCode(t *testing.T) {
	t.Parallel()

	intPtr := func(v int) *int {
		return &v
	}

	tests := []struct {
		name string
		code *int
		want string
	}{
		{name: "nil", code: nil, want: "<nil>"},
		{name: "zero", code: intPtr(0), want: "0"},
		{name: "positive", code: intPtr(42), want: "42"},
		{name: "negative", code: intPtr(-7), want: "-7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatGateExitCode(tt.code); got != tt.want {
				t.Fatalf("formatGateExitCode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTraceClipString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		limit int
		want  string
	}{
		{name: "empty", input: "", limit: 4, want: ""},
		{name: "below limit", input: "abc", limit: 4, want: "abc"},
		{name: "exact limit", input: "abcd", limit: 4, want: "abcd"},
		{name: "over limit", input: "abcde", limit: 4, want: "abcd...[clipped]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := traceClipString(tt.input, tt.limit); got != tt.want {
				t.Fatalf("traceClipString(%q, %d) = %q, want %q", tt.input, tt.limit, got, tt.want)
			}
		})
	}
}
