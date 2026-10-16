package session

import (
	"testing"
	"time"
)

// TestContentGate pins one reason per row state, and "" for a ready row.
func TestContentGate(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ready := Info{ID: "gc-1", MetadataState: string(StateActive), State: StateActive}
	for _, tc := range []struct {
		name    string
		mod     func(*Info)
		running bool
		want    string
	}{
		{"ready", func(*Info) {}, true, ""},
		{"ready, not running", func(*Info) {}, false, ""},
		{"kill pending", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateAsleep), KillPendingReason
			i.SleepReason, i.SleptAt = string(SleepReasonKilled), now.Add(-time.Second).Format(time.RFC3339)
		}, true, ContentGateKillPending},
		{"stale kill fence", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateAsleep), KillPendingReason
			i.SleepReason, i.SleptAt = string(SleepReasonKilled), now.Add(-2*KillPendingGrace).Format(time.RFC3339)
		}, false, ""},
		{"operator hold", func(i *Info) {
			i.MetadataState = string(StateSuspended)
			i.SleepIntent = string(SleepReasonUserHold)
			i.HeldUntil = now.Add(time.Hour).Format(time.RFC3339)
		}, false, ContentGateHeld},
		{"draining", func(i *Info) { i.MetadataState = string(StateDraining) }, true, ContentGateDraining},
		{"idle stop pending", func(i *Info) { i.SleepIntent = string(sleepIntentIdleStopPending) }, true, ContentGateStopPending},
		{"restart requested", func(i *Info) { i.RestartRequested = "true" }, true, ContentGateRestartPending},
		{"reset pending", func(i *Info) { i.ContinuationResetPending = "true" }, true, ContentGateRestartPending},
		{"creating", func(i *Info) { i.MetadataState = string(StateCreating) }, true, ContentGateStarting},
		{"start pending", func(i *Info) { i.MetadataState = string(StateStartPending) }, false, ContentGateStarting},
		{"create claim", func(i *Info) { i.PendingCreateClaim = true }, true, ContentGateStarting},
		{"kill beats hold", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateAsleep), KillPendingReason
			i.SleepReason, i.SleptAt = string(SleepReasonKilled), now.Format(time.RFC3339)
			i.SleepIntent, i.HeldUntil = string(SleepReasonUserHold), now.Add(time.Hour).Format(time.RFC3339)
		}, false, ContentGateKillPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := ready
			tc.mod(&info)
			if got := ContentGate(info, tc.running, now); got != tc.want {
				t.Fatalf("ContentGate = %q, want %q", got, tc.want)
			}
		})
	}
}
