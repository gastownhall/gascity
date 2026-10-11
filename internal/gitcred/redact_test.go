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

// TestScrubSecrets covers the case RedactUserinfo cannot: a credential that
// appears in text a subprocess produced rather than in a URL the caller
// interpolated. git echoes the remote verbatim on transport failures, so the
// token arrives already embedded in a sentence.
func TestScrubSecrets(t *testing.T) {
	tests := []struct {
		name    string
		msg     string
		rawURL  string
		leaked  string
		wantSub string
	}{
		{
			name:    "plain credential echoed by git",
			msg:     "fatal: could not read from https://user:ghp_secret@github.com/org/repo",
			rawURL:  "https://user:ghp_secret@github.com/org/repo",
			leaked:  "ghp_secret",
			wantSub: "github.com/org/repo",
		},
		{
			// The subtle one. url.User.Password() returns the DECODED password
			// and url.User.String() re-encodes, so neither matches the bytes git
			// actually printed. Only the raw authority substring catches this.
			name:    "percent-encoded credential echoed verbatim",
			msg:     "fatal: authentication failed for https://user:se%63ret@github.com/org/repo",
			rawURL:  "https://user:se%63ret@github.com/org/repo",
			leaked:  "se%63ret",
			wantSub: "github.com/org/repo",
		},
		{
			name:    "url.Parse rejects the URL, token still masked",
			msg:     "fatal: could not read from https://user:tok en@github.com/org/repo",
			rawURL:  "https://user:tok en@github.com/org/repo",
			leaked:  "tok en",
			wantSub: "github.com/org/repo",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ScrubSecrets(tc.msg, tc.rawURL)
			if strings.Contains(got, tc.leaked) {
				t.Errorf("ScrubSecrets leaked %q: %s", tc.leaked, got)
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("ScrubSecrets(%q) = %q, want it to keep %q", tc.msg, got, tc.wantSub)
			}
		})
	}
}

// TestScrubSecretsLeavesCleanMessagesAlone pins that a message is returned
// untouched when the URL carries no credential. Over-masking would corrupt
// ordinary diagnostics.
func TestScrubSecretsLeavesCleanMessagesAlone(t *testing.T) {
	msg := "fatal: repository 'https://github.com/org/repo' not found"
	for _, rawURL := range []string{
		"https://github.com/org/repo",
		"git@github.com:org/repo.git",
		"file:///home/u/repo",
		"",
	} {
		if got := ScrubSecrets(msg, rawURL); got != msg {
			t.Errorf("ScrubSecrets(msg, %q) = %q, want it unchanged", rawURL, got)
		}
	}
}
