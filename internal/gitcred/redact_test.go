package gitcred

import (
	"strings"
	"testing"
)

func TestRedactUserinfo(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://github.com/org/repo", "https://github.com/org/repo"},
		{"https://user:ghp_secret@github.com/org/repo", "https://***@github.com/org/repo"},
		{"https://ghp_secret@github.com/org/repo", "https://***@github.com/org/repo"},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"file:///home/u/repo", "file:///home/u/repo"},
		// Malformed userinfo (invalid %-escape) makes url.Parse fail; the string
		// fallback must still mask the userinfo rather than leak the raw token.
		{"https://user:ghp_x%SS@github.com/org/repo", "https://***@github.com/org/repo"},
		{"https://ghp_x%SS@github.com/org/repo", "https://***@github.com/org/repo"},
	}
	for _, tc := range tests {
		if got := RedactUserinfo(tc.in); got != tc.want {
			t.Errorf("RedactUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRedactUserinfoNeverLeaksOnParseFailure asserts the raw token never
// survives redaction even when url.Parse rejects the URL for reasons other than
// a bad escape (a raw space, caret, or pipe in the userinfo).
func TestRedactUserinfoNeverLeaksOnParseFailure(t *testing.T) {
	for _, in := range []string{
		"https://user:ghp_x%SS@github.com/org/repo",
		"https://user:secret^tok@github.com/org/repo",
		"https://user:bad tok@github.com/org/repo",
		"https://user:pipe|tok@github.com/org/repo",
	} {
		got := RedactUserinfo(in)
		if strings.Contains(got, "ghp_x") || strings.Contains(got, "secret^tok") ||
			strings.Contains(got, "bad tok") || strings.Contains(got, "pipe|tok") {
			t.Fatalf("RedactUserinfo(%q) leaked the token: %q", in, got)
		}
		if !strings.Contains(got, "***@") {
			t.Fatalf("RedactUserinfo(%q) = %q, want a redacted userinfo marker", in, got)
		}
	}
}
