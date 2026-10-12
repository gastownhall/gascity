package main

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// TestCmdNudgePollDeliveredTickRespectsInterval proves that a tick whose
// delivery "succeeds" but whose queue item is still due on the next read
// does not tick more than once per interval. Before the fix, `if delivered
// { continue }` skipped the sleep entirely, spinning the poller at query
// speed (~40 ticks/second) and consuming a full core of store server time.
func TestCmdNudgePollDeliveredTickRespectsInterval(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"state":        string(session.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("store.Create session: %v", err)
	}

	item := newQueuedNudgeWithOptions("worker", "check the logs", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{
		SessionID: created.ID,
	})
	if err := enqueueQueuedNudgeWithStore(cityDir, beads.NudgesStore{Store: store}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}

	// Delivery "succeeds" (returns true) but never acks the queue item, so
	// nudgePollTargetHasDueWork returns true again on the next tick — the
	// exact shape that caused the spin. After enough deliveries, ack the
	// item so the poller can exit cleanly when observe returns not-running.
	origControllerAlive := nudgePollControllerAlive
	nudgePollControllerAlive = func(_ string) int { return 1 }
	defer func() { nudgePollControllerAlive = origControllerAlive }()

	const deliveriesBeforeAck = 5
	var deliverCalls atomic.Int64
	origDeliver := nudgePollDeliverQueued
	nudgePollDeliverQueued = func(_ nudgeTarget, _, _ beads.Store, _ runtime.Provider, _ time.Duration, _ worker.LiveObservation) (bool, error) {
		n := deliverCalls.Add(1)
		if n == deliveriesBeforeAck {
			_ = ackQueuedNudges(cityDir, []string{item.ID})
		}
		return true, nil
	}
	defer func() { nudgePollDeliverQueued = origDeliver }()

	// Count sleeps and verify every sleep is at least the requested interval.
	var sleepCalls atomic.Int64
	var minSleep atomic.Int64
	minSleep.Store(int64(time.Hour))
	origSleep := nudgePollSleep
	nudgePollSleep = func(d time.Duration) {
		sleepCalls.Add(1)
		for {
			cur := minSleep.Load()
			if int64(d) >= cur || minSleep.CompareAndSwap(cur, int64(d)) {
				break
			}
		}
		origSleep(d)
	}
	defer func() { nudgePollSleep = origSleep }()

	// Observe: running=true while deliveries < deliveriesBeforeAck, then
	// not-running so the poller exits (queue is acked by then).
	origObserve := nudgeObserveTarget
	nudgeObserveTarget = func(_ nudgeTarget, _ beads.Store, _ runtime.Provider) (worker.LiveObservation, error) {
		if deliverCalls.Load() >= deliveriesBeforeAck {
			return worker.LiveObservation{Running: false}, nil
		}
		idle := time.Now().Add(-time.Hour)
		return worker.LiveObservation{Running: true, LastActivity: &idle}, nil
	}
	defer func() { nudgeObserveTarget = origObserve }()

	const testInterval = 10 * time.Millisecond
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cmdNudgePoll([]string{created.ID}, "worker-session", testInterval, 0, true, &stdout, &stderr)
	}()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cmdNudgePoll = %d, want 0; stderr=%s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cmdNudgePoll did not exit within 10s")
	}

	actual := deliverCalls.Load()
	if actual > deliveriesBeforeAck+2 {
		t.Fatalf("delivery attempts = %d, want <= %d — more deliveries than expected means ticks are not sleeping between delivered attempts",
			actual, deliveriesBeforeAck+2)
	}
	if sleepCalls.Load() == 0 {
		t.Fatal("no sleep calls recorded — the delivered path must sleep before continuing")
	}
	if time.Duration(minSleep.Load()) < testInterval {
		t.Fatalf("minimum sleep = %v, want >= %v — delivered ticks must sleep at least the poll interval",
			time.Duration(minSleep.Load()), testInterval)
	}
}

// TestNudgePollDeliveredBackoff verifies the exponential backoff formula for
// consecutive delivered ticks.
func TestNudgePollDeliveredBackoff(t *testing.T) {
	interval := 2 * time.Second
	tests := []struct {
		consecutive int
		want        time.Duration
	}{
		{0, interval},
		{1, interval},
		{2, 2 * interval},
		{3, 4 * interval},
		{4, 8 * interval},
		{5, nudgePollDeliveredBackoffCap},
		{6, nudgePollDeliveredBackoffCap},
		{10, nudgePollDeliveredBackoffCap},
		{100, nudgePollDeliveredBackoffCap},
	}
	for _, tt := range tests {
		got := nudgePollDeliveredBackoff(interval, tt.consecutive)
		if got != tt.want {
			t.Errorf("nudgePollDeliveredBackoff(%v, %d) = %v, want %v", interval, tt.consecutive, got, tt.want)
		}
	}
}
