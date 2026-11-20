package workladder

import "testing"

func TestLandingTargetUsesShortPropertyBudget(t *testing.T) {
	if !testing.Short() {
		t.Skip("landing target budget assertion applies only with -test.short")
	}
	if got, want := sequences(), 10_000; got != want {
		t.Fatalf("sequences() = %d, want %d for the landing target", got, want)
	}
}
