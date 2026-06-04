package main

import (
	"testing"
	"time"
)

func TestSessionProgressStalled(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-time.Hour)    // well past any sane threshold
	recent := now.Add(-time.Second) // within threshold
	const threshold = 30 * time.Minute

	tests := []struct {
		name            string
		threshold       time.Duration
		holdsClaim      bool
		providerHealthy bool
		exempt          bool
		lastProgress    time.Time
		want            bool
	}{
		{"stalled: alive, no claim, healthy, not exempt, old progress", threshold, false, true, false, stale, true},
		{"disabled when threshold is zero", 0, false, true, false, stale, false},
		{"not stalled when progress is recent", threshold, false, true, false, recent, false},
		{"holds a claim -> reaper's job, not recycled", threshold, true, true, false, stale, false},
		{"provider unhealthy -> never recycle into a dead provider", threshold, false, false, false, stale, false},
		{"exempt (attached/interactive/startup) -> left alone", threshold, false, true, true, stale, false},
		{"unknown progress (zero) -> conservative, not recycled", threshold, false, true, false, time.Time{}, false},
		{"exactly at threshold is not yet stalled", threshold, false, true, false, now.Add(-threshold), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionProgressStalled(tc.threshold, tc.holdsClaim, tc.providerHealthy, tc.exempt, tc.lastProgress, now)
			if got != tc.want {
				t.Errorf("sessionProgressStalled = %v, want %v", got, tc.want)
			}
		})
	}
}
