package processfloor

import (
	"slices"
	"testing"
)

// TestArgsReturnsAFreshSlice pins the one promise Args makes beyond its value:
// a caller that changes or appends to the argv it was handed cannot change the
// floor the next caller measures.
func TestArgsReturnsAFreshSlice(t *testing.T) {
	want := Args()
	if len(want) == 0 {
		t.Fatal("Args() is empty; a floor needs an argv")
	}
	first := Args()
	first[0] = "changed"
	_ = append(first, "--extra")
	if got := Args(); !slices.Equal(got, want) {
		t.Fatalf("Args() = %q after a caller changed its own copy, want %q", got, want)
	}
}
