package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestIsPoolSessionSlotFreeable_Matrix exercises the deny-by-default contract
// of the freeable allowlist. The allowlist is tiny, so regressions that widen
// it (e.g., adding `default: true`) or narrow it (e.g., removing `idle-timeout`)
// must be caught by an explicit table rather than by accident.
func TestIsPoolSessionSlotFreeable_Matrix(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"drained-state", map[string]string{"state": "drained"}, true},
		{"asleep+drained-reason", map[string]string{"state": "asleep", "sleep_reason": "drained"}, true},
		{"asleep+idle", map[string]string{"state": "asleep", "sleep_reason": "idle"}, true},
		{"asleep+idle-timeout", map[string]string{"state": "asleep", "sleep_reason": "idle-timeout"}, true},
		{"asleep+empty-reason", map[string]string{"state": "asleep", "sleep_reason": ""}, false},
		{"asleep+missing-reason", map[string]string{"state": "asleep"}, false},
		{"asleep+wait-hold", map[string]string{"state": "asleep", "sleep_reason": "wait-hold"}, false},
		{"asleep+context-churn", map[string]string{"state": "asleep", "sleep_reason": "context-churn"}, false},
		{"asleep+unknown", map[string]string{"state": "asleep", "sleep_reason": "future-reason"}, false},
		{"awake", map[string]string{"state": "awake"}, false},
		{"creating", map[string]string{"state": "creating"}, false},
		{"no-metadata", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isPoolSessionSlotFreeable(beads.Bead{Metadata: tc.meta})
			if got != tc.want {
				t.Fatalf("isPoolSessionSlotFreeable(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}
