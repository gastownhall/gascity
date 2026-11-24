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

// TestArgsHandsEachCallerItsOwnArray is the check TestArgsReturnsAFreshSlice
// cannot make. That test compares Args() with a value that shares its backing
// array whenever Args returns one shared slice, so a change made through one
// result also changes the expected value and the comparison still passes, and
// the change it leaves behind becomes the baseline of every later test. Two
// calls must hand out two arrays, whatever an earlier test did to either.
func TestArgsHandsEachCallerItsOwnArray(t *testing.T) {
	first, second := Args(), Args()
	if len(first) == 0 || len(second) == 0 {
		t.Fatal("Args() is empty; a floor needs an argv")
	}
	if &first[0] == &second[0] {
		t.Fatalf("two calls to Args() returned the same backing array (%q): a change made through one caller's argv reaches every later caller's floor", first)
	}
}
