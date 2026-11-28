package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestPoolSessionIsLiveInfo_Matrix exercises the liveness predicate used by the
// runningSessions counter in buildDesiredState. An asleep or drained session
// must not count as live; everything else is treated as live so that the
// isCold probe is never suppressed by an unknown/future state. Fed through the
// session.Info codec, matching the production read path.
func TestPoolSessionIsLiveInfo_Matrix(t *testing.T) {
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
			got := poolSessionIsLiveInfo(sessionpkg.Info{MetadataState: tc.meta["state"], SleepReason: tc.meta["sleep_reason"]})
			if got != tc.want {
				t.Fatalf("poolSessionIsLiveInfo(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}

// TestIsPoolSessionSlotFreeable_Matrix exercises the deny-by-default contract
// of the freeable allowlist. The allowlist is tiny, so regressions that widen
// it (e.g., adding `default: true`) or narrow it (e.g., removing `idle-timeout`)
// must be caught by an explicit table rather than by accident.
func TestIsPoolSessionSlotFreeable_Matrix(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Minute).Format(time.RFC3339)
	stale := now.Add(-sessionpkg.KillPendingGrace - time.Minute).Format(time.RFC3339)
	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"drained-state", map[string]string{"state": "drained"}, true},
		{"asleep+drained-reason", map[string]string{"state": "asleep", "sleep_reason": "drained"}, true},
		{"asleep+idle", map[string]string{"state": "asleep", "sleep_reason": "idle"}, true},
		{"asleep+idle-timeout", map[string]string{"state": "asleep", "sleep_reason": "idle-timeout"}, true},
		{"asleep+city-stop", map[string]string{"state": "asleep", "sleep_reason": string(sessionpkg.SleepReasonCityStop)}, true},
		{"asleep+failed-create", map[string]string{"state": "asleep", "sleep_reason": "failed-create"}, true},
		{"asleep+runtime-missing", map[string]string{"state": "asleep", "sleep_reason": string(sessionpkg.SleepReasonRuntimeMissing)}, true},
		{"asleep+provider-terminal-error", map[string]string{"state": "asleep", "sleep_reason": string(sessionpkg.SleepReasonProviderTerminalError)}, true},
		{"asleep+max-session-age", map[string]string{"state": "asleep", "sleep_reason": string(sessionpkg.SleepReasonMaxSessionAge)}, true},
		// A config-drifted pool session is never woken in place (that would run
		// stale config), so it must be freeable or the pool's slot is lost
		// forever once every seat drifts (srvcity sr-sxbc7: st/support.dispatch
		// dark 17.5h with both slots asleep-for-drift and 59 ready steps queued).
		{"asleep+config-drift", map[string]string{"state": "asleep", "sleep_reason": string(sessionpkg.SleepReasonConfigDrift)}, true},
		// Owner ruling B1: a killed seat frees its slot unless the kill still
		// owns the row (an honored kill fence). A fence past KillPendingGrace,
		// or a malformed one, is not honored.
		{"asleep+killed", map[string]string{"state": "asleep", "sleep_reason": "killed", "slept_at": recent}, true},
		{"asleep+killed+fence-pending", map[string]string{"state": "asleep", "sleep_reason": "killed", "state_reason": sessionpkg.KillPendingReason, "slept_at": recent}, false},
		{"asleep+killed+fence-stale", map[string]string{"state": "asleep", "sleep_reason": "killed", "state_reason": sessionpkg.KillPendingReason, "slept_at": stale}, true},
		{"asleep+killed+fence-malformed", map[string]string{"state": "asleep", "sleep_reason": "killed", "state_reason": sessionpkg.KillPendingReason, "slept_at": "not-a-time"}, true},
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
			got := isPoolSessionSlotFreeable(beads.Bead{Metadata: tc.meta}, now)
			if got != tc.want {
				t.Fatalf("isPoolSessionSlotFreeable(%v) = %v, want %v", tc.meta, got, tc.want)
			}
			info := sessionpkg.Info{
				MetadataState: tc.meta["state"],
				StateReason:   tc.meta["state_reason"],
				SleepReason:   tc.meta["sleep_reason"],
				SleptAt:       tc.meta["slept_at"],
			}
			if gotInfo := isPoolSessionSlotFreeableInfo(info, now); gotInfo != tc.want {
				t.Fatalf("isPoolSessionSlotFreeableInfo(%v) = %v, want %v", tc.meta, gotInfo, tc.want)
			}
		})
	}
}

// TestPoolSlotGate_WaitParksAnIdleSeat pins the pool-slot gate on a seat a
// wait parks (gastownhall/gascity#5561). A drain that keeps assigned work
// completes as an idle sleep and keeps the wait (the Sleep cell, D-J 3), so
// the gate reads the wait, not the reason: an idle seat a wait parks keeps its
// slot. The scope is the idle reason only: a drained seat, a killed seat and
// every other freeable reason free their slot as before, and both twins (bead
// and Info) agree.
func TestPoolSlotGate_WaitParksAnIdleSeat(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	slept := now.Add(-time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"idle, no wait", map[string]string{"state": "asleep", "sleep_reason": "idle", "slept_at": slept}, true},
		{"idle under a wait", map[string]string{"state": "asleep", "sleep_reason": "idle", "slept_at": slept, "wait_hold": "true", "sleep_intent": "wait-hold"}, false},
		{"idle under a bare wait_hold", map[string]string{"state": "asleep", "sleep_reason": "idle", "slept_at": slept, "wait_hold": "true"}, false},
		{"idle with a wait's intent only", map[string]string{"state": "asleep", "sleep_reason": "idle", "slept_at": slept, "sleep_intent": "wait-hold"}, true},
		{"drained under a wait", map[string]string{"state": "drained", "wait_hold": "true", "sleep_intent": "wait-hold"}, true},
		{"killed under a wait", map[string]string{"state": "asleep", "sleep_reason": "killed", "slept_at": slept, "wait_hold": "true"}, true},
		{"idle-timeout under a wait", map[string]string{"state": "asleep", "sleep_reason": "idle-timeout", "slept_at": slept, "wait_hold": "true"}, true},
		{"operator suspend", map[string]string{"state": "asleep", "sleep_reason": "idle", "slept_at": slept, "sleep_intent": "user-hold"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := makeBead("gc-1", tc.meta)
			if got := isPoolSessionSlotFreeable(b, now); got != tc.want {
				t.Errorf("isPoolSessionSlotFreeable(%v) = %v, want %v", tc.meta, got, tc.want)
			}
			if got := isPoolSessionSlotFreeableInfo(seedSessionInfo(b), now); got != tc.want {
				t.Errorf("isPoolSessionSlotFreeableInfo(%v) = %v, want %v", tc.meta, got, tc.want)
			}
		})
	}
}
