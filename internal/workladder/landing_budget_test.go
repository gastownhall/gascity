package workladder

import "testing"

func TestLandingTargetUsesShortPropertyBudget(t *testing.T) {
	if !testing.Short() {
		t.Fatal("landing target must use -test.short; full property enumeration belongs to qualification")
	}
	if got, want := sequences(), 10_000; got != want {
		t.Fatalf("sequences() = %d, want %d for the landing target", got, want)
	}
}
