package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestPoolSessionIsLive_Matrix exercises the liveness predicate used by the
// runningSessions counter in buildDesiredState. An asleep or drained bead
// must not count as live; everything else is treated as live so that the
// isCold probe is never suppressed by an unknown/future state.
func TestPoolSessionIsLive_Matrix(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"awake", map[string]string{"state": "awake"}, true},
		{"active", map[string]string{"state": "active"}, true},
		{"creating", map[string]string{"state": "creating"}, true},
		{"start-pending", map[string]string{"state": "start-pending"}, true},
		{"no-metadata", nil, true},
		{"empty-state", map[string]string{"state": ""}, true},
		{"asleep-no-reason", map[string]string{"state": "asleep"}, false},
		{"asleep-idle", map[string]string{"state": "asleep", "sleep_reason": "idle"}, false},
		{"asleep-wait-hold", map[string]string{"state": "asleep", "sleep_reason": "wait-hold"}, false},
		{"drained-state", map[string]string{"state": "drained"}, false},
		{"asleep-drained-reason", map[string]string{"state": "asleep", "sleep_reason": "drained"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := poolSessionIsLive(beads.Bead{Metadata: tc.meta})
			if got != tc.want {
				t.Fatalf("poolSessionIsLive(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}

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
		{"asleep+city-stop", map[string]string{"state": "asleep", "sleep_reason": sleepReasonCityStop}, true},
		{"asleep+failed-create", map[string]string{"state": "asleep", "sleep_reason": "failed-create"}, true},
		{"asleep+runtime-missing", map[string]string{"state": "asleep", "sleep_reason": sleepReasonRuntimeMissing}, true},
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
