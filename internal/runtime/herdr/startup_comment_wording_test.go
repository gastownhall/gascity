package herdr

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// commentText returns the Go line comments of src with the leading "//"
// markers removed and every whitespace run collapsed to one space, so a
// phrase wrapped across comment lines can be matched as one string.
func commentText(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "//"); ok {
			b.WriteString(rest)
			b.WriteByte(' ')
		}
	}
	return regexp.MustCompile(`\s+`).ReplaceAllString(b.String(), " ")
}

// The startup-delivery comments must not present nudgeStalledPoolClaims as
// the one backstop of last resort: it covers pool slots only, and the
// execution backstop and the named-session startup kickoff metadata sit
// beside it.
func TestProviderCommentsDoNotCallPoolClaimNudgeLastResort(t *testing.T) {
	text := commentText(t, "provider.go")
	for _, stale := range []string{
		"backstop of last resort",
		"sole backstop",
		"only backstop",
	} {
		if strings.Contains(text, stale) {
			t.Errorf("provider.go comments still say %q; nudgeStalledPoolClaims is one backstop among several", stale)
		}
	}
}

// GC_STARTUP_PROMPT_DELIVERED records that a delivery mechanism was selected
// or attempted for this incarnation. It is not confirmed receipt, so any
// comment that names it says so.
func TestProviderCommentsDescribeDeliveredFlagAsAttemptNotReceipt(t *testing.T) {
	text := commentText(t, "provider.go")
	const flag = "GC_STARTUP_PROMPT_DELIVERED"
	idx := strings.Index(text, flag)
	if idx < 0 {
		t.Fatalf("provider.go comments no longer mention %s; update this test with the comment", flag)
	}
	window := text[idx:min(len(text), idx+400)]
	if !strings.Contains(window, "not confirmed receipt") {
		t.Errorf("provider.go comment on %s does not say it is not confirmed receipt; got: %q", flag, window)
	}
}
