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
			if got := ContentGate(info, tc.running, now, GateContentAny); got != tc.want {
				t.Fatalf("ContentGate = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestContentGateAdmitsTheDrainNoticeIntoADrainingRow pins I7's one
// exemption: the drain protocol's notice passes "draining", and only that
// reason; every other reason still applies to it, in its order, and every
// other content stays refused by a draining row.
func TestContentGateAdmitsTheDrainNoticeIntoADrainingRow(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	draining := Info{ID: "gc-1", MetadataState: string(StateDraining), State: StateDraining, StateReason: DrainAckStopPendingReason}
	for _, tc := range []struct {
		name    string
		mod     func(*Info)
		running bool
		content GateContent
		want    string
	}{
		{"drain notice into a draining row", func(*Info) {}, true, GateContentDrainNotice, ""},
		{"drain notice, runtime not running", func(*Info) {}, false, GateContentDrainNotice, ""},
		{"other content into a draining row", func(*Info) {}, true, GateContentAny, ContentGateDraining},
		{"drain notice, kill pending", func(i *Info) {
			i.StateReason = KillPendingReason
			i.MetadataState = string(StateAsleep)
			i.SleepReason, i.SleptAt = string(SleepReasonKilled), now.Add(-time.Second).Format(time.RFC3339)
		}, true, GateContentDrainNotice, ContentGateKillPending},
		{"drain notice, held", func(i *Info) {
			i.SleepIntent = string(SleepReasonUserHold)
			i.HeldUntil = now.Add(time.Hour).Format(time.RFC3339)
		}, false, GateContentDrainNotice, ContentGateHeld},
		{"drain notice, idle stop pending", func(i *Info) { i.SleepIntent = string(sleepIntentIdleStopPending) }, true, GateContentDrainNotice, ContentGateStopPending},
		{"drain notice, restart requested", func(i *Info) { i.RestartRequested = "true" }, true, GateContentDrainNotice, ContentGateRestartPending},
		{"drain notice, reset pending", func(i *Info) { i.ContinuationResetPending = "true" }, true, GateContentDrainNotice, ContentGateRestartPending},
		{"drain notice, create claim", func(i *Info) { i.PendingCreateClaim = true }, true, GateContentDrainNotice, ContentGateStarting},
		{"drain notice into a ready row", func(i *Info) { i.MetadataState, i.State, i.StateReason = string(StateActive), StateActive, "" }, true, GateContentDrainNotice, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := draining
			tc.mod(&info)
			if got := ContentGate(info, tc.running, now, tc.content); got != tc.want {
				t.Fatalf("ContentGate = %q, want %q", got, tc.want)
			}
		})
	}
}
