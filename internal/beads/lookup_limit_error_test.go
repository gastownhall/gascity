package beads

import (
	"fmt"
	"testing"
)

func TestIsLookupLimitError(t *testing.T) {
	if !IsLookupLimitError(LookupLimitError{Kind: "wait", Label: "gc:wait", Limit: 1000}) {
		t.Fatal("IsLookupLimitError returned false for value error")
	}
	err := fmt.Errorf("wrapped: %w", &LookupLimitError{Kind: "nudge", Label: "nudge:1", Limit: 20})
	if !IsLookupLimitError(err) {
		t.Fatal("IsLookupLimitError returned false for wrapped pointer error")
	}
	if IsLookupLimitError(fmt.Errorf("other")) {
		t.Fatal("IsLookupLimitError returned true for non-limit error")
	}
}
