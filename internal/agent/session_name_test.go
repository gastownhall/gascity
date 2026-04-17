package agent

import "testing"

func TestSanitizeQualifiedNameForSession(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "plain", input: "mayor", want: "mayor"},
		{name: "rig scoped", input: "repo/polecat", want: "repo--polecat"},
		{name: "imported city scoped", input: "wendy.wendy", want: "wendy__wendy"},
		{name: "imported rig scoped", input: "repo/wendy.wendy", want: "repo--wendy__wendy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeQualifiedNameForSession(tt.input); got != tt.want {
				t.Fatalf("SanitizeQualifiedNameForSession(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if got := SessionNameFor("city", tt.input, ""); got != tt.want {
				t.Fatalf("SessionNameFor(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestUnsanitizeQualifiedNameFromSession(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "mayor", want: "mayor"},
		{input: "repo--polecat", want: "repo/polecat"},
		{input: "wendy__wendy", want: "wendy.wendy"},
		{input: "repo--wendy__wendy", want: "repo/wendy.wendy"},
	}

	for _, tt := range tests {
		if got := UnsanitizeQualifiedNameFromSession(tt.input); got != tt.want {
			t.Fatalf("UnsanitizeQualifiedNameFromSession(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
