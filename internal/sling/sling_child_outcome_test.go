package sling

import "testing"

func TestSlingChildResultOutcome(t *testing.T) {
	cases := []struct {
		name  string
		child SlingChildResult
		want  string
	}{
		{"routed", SlingChildResult{Routed: true}, ChildOutcomeRouted},
		{"failed", SlingChildResult{Failed: true, FailReason: "boom"}, ChildOutcomeFailed},
		{"skipped idempotent", SlingChildResult{Skipped: true}, ChildOutcomeSkipped},
		{"skipped not open", SlingChildResult{Skipped: true, Status: "closed"}, ChildOutcomeSkipped},
	}
	for _, tc := range cases {
		if got := tc.child.Outcome(); got != tc.want {
			t.Errorf("%s: Outcome() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSlingResultPartialFailure(t *testing.T) {
	cases := []struct {
		name   string
		result SlingResult
		want   bool
	}{
		{"all routed", SlingResult{Routed: 2}, false},
		{"all failed", SlingResult{Failed: 2}, false},
		{"failed with skipped only", SlingResult{Failed: 1, Skipped: 1}, false},
		{"some routed some failed", SlingResult{Routed: 1, Failed: 1}, true},
	}
	for _, tc := range cases {
		if got := tc.result.PartialFailure(); got != tc.want {
			t.Errorf("%s: PartialFailure() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
