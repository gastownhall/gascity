package session

import (
	"maps"
	"testing"
	"time"
)

// gateStopHalf is the stop request's controller half the v2 drain begin
// writes for incarnation (cmd/gc's stopBeginPatch).
func gateStopHalf(incarnation string) map[string]string {
	return map[string]string{
		DrainIntentReasonKey:      "idle",
		DrainIntentAtKey:          "2026-10-16T11:59:00Z",
		DrainIntentIncarnationKey: incarnation,
	}
}

// gateStopHalfWithout is gateStopHalf without key: a partial triple.
func gateStopHalfWithout(incarnation, key string) map[string]string {
	m := gateStopHalf(incarnation)
	delete(m, key)
	return m
}

// gateStop reads half as info's row carries it, by reading: the stop
// request a lane reads in the same read as info.
func gateStop(info Info, half map[string]string, reading StopRequestReading) StopRequest {
	meta := map[string]string{"generation": info.Generation}
	maps.Copy(meta, half)
	return ReadStopRequest(meta, reading)
}

// TestContentGate pins one reason per row state, and "" for a ready row,
// including the v2 stop request at the row's generation (V2-18), another
// generation's, and partial triples.
func TestContentGate(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ready := Info{ID: "gc-1", MetadataState: string(StateActive), State: StateActive, Generation: "4"}
	for _, tc := range []struct {
		name    string
		mod     func(*Info)
		running bool
		want    string
		stop    map[string]string
		reading StopRequestReading
	}{
		{"ready", func(*Info) {}, true, "", nil, StopRequestsLive},
		{"ready, not running", func(*Info) {}, false, "", nil, StopRequestsLive},
		{"kill pending", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateAsleep), KillPendingReason
			i.SleepReason, i.SleptAt = string(SleepReasonKilled), now.Add(-time.Second).Format(time.RFC3339)
		}, true, ContentGateKillPending, nil, StopRequestsLive},
		{"stale kill fence", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateAsleep), KillPendingReason
			i.SleepReason, i.SleptAt = string(SleepReasonKilled), now.Add(-2*KillPendingGrace).Format(time.RFC3339)
		}, false, "", nil, StopRequestsLive},
		{"operator hold", func(i *Info) {
			i.MetadataState = string(StateSuspended)
			i.SleepIntent = string(SleepReasonUserHold)
			i.HeldUntil = now.Add(time.Hour).Format(time.RFC3339)
		}, false, ContentGateHeld, nil, StopRequestsLive},
		{"draining", func(i *Info) { i.MetadataState = string(StateDraining) }, true, ContentGateDraining, nil, StopRequestsLive},
		{"idle stop pending", func(i *Info) { i.SleepIntent = string(sleepIntentIdleStopPending) }, true, ContentGateStopPending, nil, StopRequestsLive},
		{"restart requested", func(i *Info) { i.RestartRequested = "true" }, true, ContentGateRestartPending, nil, StopRequestsLive},
		{"reset pending", func(i *Info) { i.ContinuationResetPending = "true" }, true, ContentGateRestartPending, nil, StopRequestsLive},
		{"creating", func(i *Info) { i.MetadataState = string(StateCreating) }, true, ContentGateStarting, nil, StopRequestsLive},
		{"start pending", func(i *Info) { i.MetadataState = string(StateStartPending) }, false, ContentGateStarting, nil, StopRequestsLive},
		{"create claim", func(i *Info) { i.PendingCreateClaim = true }, true, ContentGateStarting, nil, StopRequestsLive},
		{"kill beats hold", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateAsleep), KillPendingReason
			i.SleepReason, i.SleptAt = string(SleepReasonKilled), now.Format(time.RFC3339)
			i.SleepIntent, i.HeldUntil = string(SleepReasonUserHold), now.Add(time.Hour).Format(time.RFC3339)
		}, false, ContentGateKillPending, nil, StopRequestsLive},
		{"stop requested at the row's generation", func(*Info) {}, true, ContentGateStopRequested, gateStopHalf("4"), StopRequestsLive},
		{"stop requested, runtime not running", func(*Info) {}, false, ContentGateStopRequested, gateStopHalf("4"), StopRequestsLive},
		{"stop request of another generation", func(*Info) {}, true, "", gateStopHalf("3"), StopRequestsLive},
		{"partial triple: no time", func(*Info) {}, true, ContentGateStopRequested, gateStopHalfWithout("4", DrainIntentAtKey), StopRequestsLive},
		{"partial triple: no reason", func(*Info) {}, true, "", gateStopHalfWithout("4", DrainIntentReasonKey), StopRequestsLive},
		{"partial triple: no incarnation", func(*Info) {}, true, "", gateStopHalfWithout("4", DrainIntentIncarnationKey), StopRequestsLive},
		{"a request a legacy controller ignores", func(*Info) {}, true, "", gateStopHalf("4"), StopRequestsIgnored},
		{"kill beats the request", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateAsleep), KillPendingReason
			i.SleepReason, i.SleptAt = string(SleepReasonKilled), now.Add(-time.Second).Format(time.RFC3339)
		}, true, ContentGateKillPending, gateStopHalf("4"), StopRequestsLive},
		{"hold beats the request", func(i *Info) {
			i.MetadataState = string(StateSuspended)
			i.SleepIntent = string(SleepReasonUserHold)
			i.HeldUntil = now.Add(time.Hour).Format(time.RFC3339)
		}, false, ContentGateHeld, gateStopHalf("4"), StopRequestsLive},
		{"the request beats draining (v2-signaled)", func(i *Info) { i.MetadataState = string(StateDraining) }, true, ContentGateStopRequested, gateStopHalf("4"), StopRequestsLive},
		{"draining, the request read as residue", func(i *Info) { i.MetadataState = string(StateDraining) }, true, ContentGateDraining, gateStopHalf("4"), StopRequestsIgnored},
		{"the request beats idle stop", func(i *Info) { i.SleepIntent = string(sleepIntentIdleStopPending) }, true, ContentGateStopRequested, gateStopHalf("4"), StopRequestsLive},
		{"the request beats restart", func(i *Info) { i.RestartRequested = "true" }, true, ContentGateStopRequested, gateStopHalf("4"), StopRequestsLive},
		{"the request beats reset", func(i *Info) { i.ContinuationResetPending = "true" }, true, ContentGateStopRequested, gateStopHalf("4"), StopRequestsLive},
		{"the request beats starting", func(i *Info) { i.MetadataState = string(StateCreating) }, true, ContentGateStopRequested, gateStopHalf("4"), StopRequestsLive},
		{"the request beats a create claim", func(i *Info) { i.PendingCreateClaim = true }, true, ContentGateStopRequested, gateStopHalf("4"), StopRequestsLive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := ready
			tc.mod(&info)
			stop := gateStop(info, tc.stop, tc.reading)
			if got := ContentGate(info, stop, tc.running, now, GateContentAny); got != tc.want {
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
			if got := ContentGate(info, StopRequest{}, tc.running, now, tc.content); got != tc.want {
				t.Fatalf("ContentGate = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestContentGateAdmitsTheDrainNoticeIntoAStopRequestedRow pins I7's
// exemption on a drain's first phase (V2-18): the drain protocol's notice
// passes "stop_requested" as it passes "draining", permanently, so its
// admission does not depend on how the stop request was read; every later
// reason still holds it, and every other content stays refused by the
// request.
func TestContentGateAdmitsTheDrainNoticeIntoAStopRequestedRow(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	requested := Info{ID: "gc-1", MetadataState: string(StateActive), State: StateActive, Generation: "2"}
	for _, tc := range []struct {
		name    string
		mod     func(*Info)
		running bool
		content GateContent
		want    string
	}{
		{"drain notice into a stop-requested row", func(*Info) {}, true, GateContentDrainNotice, ""},
		{"drain notice into a v2-signaled row", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateDraining), DrainAckStopPendingReason
		}, true, GateContentDrainNotice, ""},
		{"other content into a stop-requested row", func(*Info) {}, true, GateContentAny, ContentGateStopRequested},
		{"other content into a v2-signaled row", func(i *Info) {
			i.MetadataState, i.StateReason = string(StateDraining), DrainAckStopPendingReason
		}, true, GateContentAny, ContentGateStopRequested},
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
		{"drain notice, starting", func(i *Info) { i.MetadataState = string(StateStartPending) }, true, GateContentDrainNotice, ContentGateStarting},
		{"drain notice, create claim", func(i *Info) { i.PendingCreateClaim = true }, true, GateContentDrainNotice, ContentGateStarting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := requested
			tc.mod(&info)
			live := gateStop(info, gateStopHalf("2"), StopRequestsLive)
			if !live.Requested() {
				t.Fatal("the fixture's request does not stand")
			}
			if got := ContentGate(info, live, tc.running, now, tc.content); got != tc.want {
				t.Fatalf("ContentGate = %q, want %q", got, tc.want)
			}
			if tc.content == GateContentDrainNotice {
				ignored := gateStop(info, gateStopHalf("2"), StopRequestsIgnored)
				if got := ContentGate(info, ignored, tc.running, now, tc.content); got != tc.want {
					t.Fatalf("ContentGate with the request read as residue = %q, want %q", got, tc.want)
				}
			}
		})
	}
}
