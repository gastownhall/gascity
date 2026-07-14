package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestOpenPoolSessionCountForTemplateExcludesClosed guards the min-floor scan's
// read-after-close contract: a session whose Info snapshot is Closed (the shape
// the reconciler produces after refreshing a mid-tick close onto infoByID) must
// not count toward the pool's open floor. Only open, same-template sessions are
// counted; a closed same-template session and an open other-template session are
// both excluded.
func TestOpenPoolSessionCountForTemplateExcludesClosed(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	// Ranged as a map (Step 5e): membership + Closed/template drive the count, not
	// order. Two open workers count; the closed worker and the scout are excluded.
	infoByID := map[string]sessionpkg.Info{
		"s-open-1":        {ID: "s-open-1", Template: "worker"},
		"s-open-2":        {ID: "s-open-2", Template: "worker"},
		"s-closed-worker": {ID: "s-closed-worker", Template: "worker", Closed: true},
		"s-open-scout":    {ID: "s-open-scout", Template: "scout"},
	}

	if got := openPoolSessionCountForTemplate(infoByID, cfg, "worker"); got != 2 {
		t.Fatalf("openPoolSessionCountForTemplate = %d, want 2 (two open workers; the closed worker and the scout must be excluded)", got)
	}
}

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

// TestProgressStall_MinFloorIdleWorker_NotRecycled verifies that a pool worker
// sitting below the min_active_sessions floor is exempt from the stall recycler.
func TestProgressStall_MinFloorIdleWorker_NotRecycled(t *testing.T) {
	tests := []struct {
		name       string
		min        int
		open       int
		wantExempt bool
	}{
		// pool with min=1, exactly 1 open session → at floor, exempt
		{"at floor: open == min", 1, 1, true},
		// pool with min=2, 1 open session → below floor, exempt
		{"below floor: open < min", 2, 1, true},
		// pool with min=1, 2 open sessions → above floor, not exempt
		{"above floor: open > min", 1, 2, false},
		// pool with min=0 (no floor) → not exempt regardless of open count
		{"no floor: min == 0", 0, 1, false},
		// pool with min=0, open=0 → also not exempt
		{"no floor, empty pool", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isMinFloorIdleWorker(tc.min, tc.open)
			if got != tc.wantExempt {
				t.Errorf("isMinFloorIdleWorker(%d, %d) = %v, want %v", tc.min, tc.open, got, tc.wantExempt)
			}
		})
	}
}

// TestProgressStall_DemandWorkerLostClaim_IsRecycled verifies that a demand
// worker (pool with no floor, or pool above its floor) that holds no claim
// and has stale progress IS recycled by sessionProgressStalled.
func TestProgressStall_DemandWorkerLostClaim_IsRecycled(t *testing.T) {
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-time.Hour)
	const threshold = 30 * time.Minute

	tests := []struct {
		name        string
		min         int
		open        int
		wantRecycle bool
	}{
		// min=0: no floor at all, demand worker is recycled
		{"demand pool: min=0, open=1", 0, 1, true},
		// min=1 but 2 open sessions: above floor, demand worker is recycled
		{"above floor: min=1, open=2", 1, 2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			floorExempt := isMinFloorIdleWorker(tc.min, tc.open)
			recycled := sessionProgressStalled(threshold, false, true, floorExempt, stale, now)
			if recycled != tc.wantRecycle {
				t.Errorf("demand worker: isMinFloorIdleWorker(%d,%d)=%v; sessionProgressStalled=%v, want %v",
					tc.min, tc.open, floorExempt, recycled, tc.wantRecycle)
			}
		})
	}
}
