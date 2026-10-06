package main

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// Every test in this file runs inside a testing/synctest bubble, so the lane's
// timer, the passes' durations and the test's waits share the bubble's virtual
// clock: a receive on time.After advances it exactly, at no wall-clock cost,
// and synctest.Wait returns only once the lane goroutine is blocked again, so
// every count read after it is settled rather than polled for.

// ordersLaneCadenceInterval is the patrol interval the cadence tests run at.
const ordersLaneCadenceInterval = 10 * time.Second

// startOrdersLaneInBubble starts the lane inside the current bubble and stops
// it when the bubble's test returns.
func startOrdersLaneInBubble(t *testing.T, cr *CityRuntime) *ordersLane {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := cr.startOrdersLane(ctx, cr.cityPath)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cr.ordersLaneOf()
}

// advanceLane moves the bubble clock forward by d and lets the lane settle.
func advanceLane(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

// wakeLane wakes the lane n times and lets it settle.
func wakeLane(lane *ordersLane, n int) {
	for i := 0; i < n; i++ {
		lane.wake()
	}
	synctest.Wait()
}

func wantLanePasses(t *testing.T, lane *ordersLane, od *recordingOrderDispatcher, wakes, cadence int64, when string) {
	t.Helper()
	gotWakes, gotCadence := lane.wakePasses.Load(), lane.cadencePasses.Load()
	if gotWakes != wakes || gotCadence != cadence || int64(od.calls.Load()) != wakes+cadence {
		t.Fatalf("%s: passes wake=%d timer=%d dispatches=%d, want wake=%d timer=%d dispatches=%d",
			when, gotWakes, gotCadence, od.calls.Load(), wakes, cadence, wakes+cadence)
	}
}

// passDurations makes each dispatch take the next duration of virtual time,
// and the last one for every dispatch after that.
func passDurations(ds ...time.Duration) *recordingOrderDispatcher {
	var n int
	return &recordingOrderDispatcher{onDispatch: func(context.Context, string, time.Time) {
		d := ds[len(ds)-1]
		if n < len(ds) {
			d = ds[n]
		}
		n++
		<-time.After(d)
	}}
}

// The lane timer is only a backstop. While wakes keep arriving it runs no pass
// of its own; once they stop it runs exactly one pass per interval. A
// free-running ticker would fire on its own grid (every 10s from lane start)
// regardless of when passes end, so starting the first pass 3s in puts the
// two schedules apart. Passes here take no time, so every wake runs at once.
func TestOrdersLaneTimerIsOnlyABackstop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		od := &recordingOrderDispatcher{}
		cr := ordersLaneTestRuntime(t, od, ordersLaneCadenceInterval.String(), nil)
		lane := startOrdersLaneInBubble(t, cr)

		advanceLane(3 * time.Second)
		wakeLane(lane, 1)
		wantLanePasses(t, lane, od, 1, 0, "first wake")

		for i := int64(2); i <= 6; i++ {
			advanceLane(9 * time.Second)
			wakeLane(lane, 1)
			wantLanePasses(t, lane, od, i, 0, "wake 9s after the previous pass")
		}

		// No more wakes: one timer pass per interval, and nothing between.
		advanceLane(ordersLaneCadenceInterval - time.Second)
		wantLanePasses(t, lane, od, 6, 0, "idle, before the interval")
		advanceLane(time.Second)
		wantLanePasses(t, lane, od, 6, 1, "idle for one interval")
		advanceLane(ordersLaneCadenceInterval - time.Second)
		wantLanePasses(t, lane, od, 6, 1, "idle, before the next interval")
	})
}

// (i) A short pass leaves a short duty-cycle gap: a wake inside it runs as soon
// as the lane has idled as long as the pass ran, far ahead of the patrol
// interval, and a wake after the gap runs at once. This is the tick's poke
// latency on a city with cheap passes.
func TestOrdersLaneWakeAfterShortPassRunsAfterItsDutyGap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		od := passDurations(time.Second)
		cr := ordersLaneTestRuntime(t, od, ordersLaneCadenceInterval.String(), nil)
		lane := startOrdersLaneInBubble(t, cr)

		wakeLane(lane, 1) // t=0: first pass, runs to t=1
		advanceLane(time.Second)
		advanceLane(time.Second / 2)
		wakeLane(lane, 1) // t=1.5: inside the 1s gap
		wantLanePasses(t, lane, od, 1, 0, "wake inside the duty gap")
		advanceLane(time.Second/2 - time.Millisecond)
		wantLanePasses(t, lane, od, 1, 0, "just before the duty gap closes")
		advanceLane(time.Millisecond)
		wantLanePasses(t, lane, od, 2, 0, "duty gap closed at t=2, 8s before the backstop")

		advanceLane(time.Second + 3*time.Second) // pass ends t=3; now t=6
		wakeLane(lane, 1)
		wantLanePasses(t, lane, od, 3, 0, "wake after the duty gap runs at once")
	})
}

// (ii) A long pass leaves a long gap: a wake waits until end + pass duration,
// or for the backstop at end + patrol interval if that comes first.
func TestOrdersLaneWakeAfterLongPassWaitsForGapOrBackstop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		od := passDurations(6*time.Second, 12*time.Second, time.Second)
		cr := ordersLaneTestRuntime(t, od, ordersLaneCadenceInterval.String(), nil)
		lane := startOrdersLaneInBubble(t, cr)

		wakeLane(lane, 1) // t=0: 6s pass, ends t=6
		advanceLane(7 * time.Second)
		wakeLane(lane, 1) // t=7: gap ends t=12, backstop t=16
		advanceLane(5*time.Second - time.Millisecond)
		wantLanePasses(t, lane, od, 1, 0, "before end + pass duration")
		advanceLane(time.Millisecond)
		wantLanePasses(t, lane, od, 2, 0, "at end + pass duration (t=12)")

		advanceLane(12*time.Second + time.Second) // 12s pass ends t=24; now t=25
		wakeLane(lane, 1)                         // gap would end t=36, backstop t=34
		advanceLane(9*time.Second - time.Millisecond)
		wantLanePasses(t, lane, od, 2, 0, "before the backstop")
		advanceLane(time.Millisecond)
		wantLanePasses(t, lane, od, 3, 0, "at the backstop (t=34), ahead of end + pass duration")
	})
}

// (iii) A wake that lands mid-pass never produces a back-to-back pass: it waits
// out the duty gap after the pass, and further wakes join it.
func TestOrdersLaneMidPassWakeNeverRunsBackToBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		od := passDurations(4 * time.Second)
		cr := ordersLaneTestRuntime(t, od, ordersLaneCadenceInterval.String(), nil)
		lane := startOrdersLaneInBubble(t, cr)

		wakeLane(lane, 1) // t=0: pass runs to t=4
		advanceLane(2 * time.Second)
		wakeLane(lane, 2) // t=2: mid-pass
		advanceLane(2 * time.Second)
		wantLanePasses(t, lane, od, 1, 0, "pass ended with a mid-pass wake pending")
		advanceLane(2 * time.Second)
		wakeLane(lane, 3) // t=6: joins the pending wake
		advanceLane(2*time.Second - time.Millisecond)
		wantLanePasses(t, lane, od, 1, 0, "inside the duty gap after the pass")
		advanceLane(time.Millisecond)
		wantLanePasses(t, lane, od, 2, 0, "one pass for every wake, at end + pass duration (t=8)")

		// That pass ends t=12 with every wake consumed: the next pass is the
		// backstop's, at t=22.
		advanceLane(4*time.Second + ordersLaneCadenceInterval - time.Millisecond)
		wantLanePasses(t, lane, od, 2, 0, "no wake pending, inside the backstop interval")
		advanceLane(time.Millisecond)
		wantLanePasses(t, lane, od, 2, 1, "backstop")
	})
}
