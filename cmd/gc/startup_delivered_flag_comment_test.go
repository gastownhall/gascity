package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// GC_STARTUP_PROMPT_DELIVERED records that a delivery mechanism was selected
// or attempted for this incarnation. It is not confirmed receipt: the durable
// confirmation is primed_at. The converger's comments must not describe the
// flag as proof the prompt landed.
func TestConvergeCommentsDescribeDeliveredFlagAsAttemptNotReceipt(t *testing.T) {
	raw, err := os.ReadFile("session_level_converge.go")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "//"); ok {
			b.WriteString(rest)
			b.WriteByte(' ')
		}
	}
	text := regexp.MustCompile(`\s+`).ReplaceAllString(b.String(), " ")
	for _, overstated := range []string{
		"the prompt was delivered this runtime",
		"the prompt already landed this incarnation",
	} {
		if strings.Contains(text, overstated) {
			t.Errorf("session_level_converge.go comments still say %q; the flag means attempted, not received", overstated)
		}
	}
	const flag = "GC_STARTUP_PROMPT_DELIVERED"
	if !strings.Contains(text, flag) {
		t.Fatalf("session_level_converge.go comments no longer mention %s; update this test with the comment", flag)
	}
	if !strings.Contains(text, "not confirmed receipt") {
		t.Errorf("session_level_converge.go comments never say %s is not confirmed receipt", flag)
	}
}
