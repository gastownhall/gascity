package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// TestDispositionProjectionsMatchV2: over every combination of the class
// keys' values with the state and sleep_reason beside them, the disposition's
// projections answer as the v2 code they replace: OperatorDormant (with the
// kill fence, the lifecycle's half) is operatorDormant, HeartbeatHeld is
// heartbeatHeld, and RefusesPreWake is C5a1's preWakeHolds mask.
func TestDispositionProjectionsMatchV2(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	future, past, at := now.Add(time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)
	states := []string{"", "asleep", "suspended", "active", "draining"}
	intents := []string{"", "user-hold", "wait-hold", "idle-stop-pending", "other"}
	waits := []string{"", "true", "w"}
	helds := []string{"", future, past, "soon", at}
	quars := []string{"", future, past, "soon"}
	reasons := []string{"", "killed", "idle", "user-hold", "city-stop"}
	n := 0
	for _, st := range states {
		for _, in := range intents {
			for _, w := range waits {
				for _, h := range helds {
					for _, q := range quars {
						for _, r := range reasons {
							n++
							info := session.Info{MetadataState: st, SleepIntent: in, WaitHold: w, HeldUntil: h, QuarantinedUntil: q, SleepReason: r}
							d := session.DispositionOfInfo(info)
							if got, want := d.OperatorDormant(now) || session.IsKillPendingInfo(info, now), operatorDormant(info, now); got != want {
								t.Errorf("%+v: OperatorDormant = %v, operatorDormant %v", info, got, want)
							}
							if got, want := d.HeartbeatHeld(now), heartbeatHeld(info, now); got != want {
								t.Errorf("%+v: HeartbeatHeld = %v, heartbeatHeld %v", info, got, want)
							}
							if got, want := d.RefusesPreWake(now), session.HoldsInfo(info, now).In&preWakeHolds != 0; got != want {
								t.Errorf("%+v: RefusesPreWake = %v, preWakeHolds %v", info, got, want)
							}
						}
					}
				}
			}
		}
	}
	if n != 5*5*3*5*4*5 {
		t.Fatalf("walked %d rows", n)
	}
}
