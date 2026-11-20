package beads

import (
	"fmt"
	"testing"
)

func TestIsLookupLimitError(t *testing.T) {
	if !IsLookupLimitError(LookupLimitError{Kind: "wait", Label: "gc:wait", Limit: 1000}) {
		t.Fatal("IsLookupLimitError returned false for value error")
	}
	// The pointer travels as an error value: Go 1.27's vet rejects %w on a
	// *LookupLimitError operand, since LookupLimitError (not the pointer) is
	// the error type and errors.Is against a value target cannot match it.
	var ptrErr error = &LookupLimitError{Kind: "nudge", Label: "nudge:1", Limit: 20}
	err := fmt.Errorf("wrapped: %w", ptrErr)
	if !IsLookupLimitError(err) {
		t.Fatal("IsLookupLimitError returned false for wrapped pointer error")
	}
	if IsLookupLimitError(fmt.Errorf("other")) {
		t.Fatal("IsLookupLimitError returned true for non-limit error")
	}
}
