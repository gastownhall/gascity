package main

import (
	"bytes"
	"sync"
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

// TestNudgePollDeliveredBackoffNeverBelowInterval proves the cap bounds the
// backoff, not the configured interval: a poller running slower than the cap
// still sleeps its own interval after a delivered tick.
func TestNudgePollDeliveredBackoffNeverBelowInterval(t *testing.T) {
	interval := 2 * nudgePollDeliveredBackoffCap
	for _, consecutive := range []int{0, 1, 2, 3, 10} {
		if got := nudgePollDeliveredBackoff(interval, consecutive); got != interval {
			t.Errorf("nudgePollDeliveredBackoff(%v, %d) = %v, want %v", interval, consecutive, got, interval)
		}
	}
}

// TestCmdNudgePollDeliveredBackoffResetsWhenQueueDrains proves the backoff
// counts ticks where the same item stays due, not deliveries over the
// poller's life: a delivery that takes (the next tick finds nothing due)
// resets it, so the next separate delivery sleeps one interval again rather
// than a doubled one.
func TestCmdNudgePollDeliveredBackoffResetsWhenQueueDrains(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	origControllerAlive := nudgePollControllerAlive
	nudgePollControllerAlive = func(_ string) int { return 1 }
	defer func() { nudgePollControllerAlive = origControllerAlive }()

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
	enqueue := func(text string) queuedNudge {
		item := newQueuedNudgeWithOptions("worker", text, "session", time.Now().Add(-time.Minute), queuedNudgeOptions{
			SessionID: created.ID,
		})
		if err := enqueueQueuedNudgeWithStore(cityDir, beads.NudgesStore{Store: store}, item); err != nil {
			t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
		}
		return item
	}

	// Every delivery takes: the seam acks the due item, so the next tick
	// finds nothing due. The second item is enqueued only once the poller
	// has observed an empty queue after the first delivery.
	var mu sync.Mutex
	var pending []string
	var deliverCalls int
	var sleeps []time.Duration
	var sleepAfterSecondDelivery int
	sleepAfterSecondDelivery = -1
	first := enqueue("first")
	mu.Lock()
	pending = append(pending, first.ID)
	mu.Unlock()

	origDeliver := nudgePollDeliverQueued
	nudgePollDeliverQueued = func(_ nudgeTarget, _, _ beads.Store, _ runtime.Provider, _ time.Duration, _ worker.LiveObservation) (bool, error) {
		mu.Lock()
		deliverCalls++
		ids := append([]string(nil), pending...)
		pending = nil
		mu.Unlock()
		_ = ackQueuedNudges(cityDir, ids)
		return true, nil
	}
	defer func() { nudgePollDeliverQueued = origDeliver }()

	origSleep := nudgePollSleep
	nudgePollSleep = func(d time.Duration) {
		mu.Lock()
		sleeps = append(sleeps, d)
		n := deliverCalls
		if n == 2 && sleepAfterSecondDelivery < 0 {
			sleepAfterSecondDelivery = len(sleeps) - 1
		}
		enqueueSecond := n == 1 && len(sleeps) == 2 // the idle tick after the first delivery
		mu.Unlock()
		if enqueueSecond {
			second := enqueue("second")
			mu.Lock()
			pending = append(pending, second.ID)
			mu.Unlock()
		}
		origSleep(d)
	}
	defer func() { nudgePollSleep = origSleep }()

	origObserve := nudgeObserveTarget
	nudgeObserveTarget = func(_ nudgeTarget, _ beads.Store, _ runtime.Provider) (worker.LiveObservation, error) {
		mu.Lock()
		done := deliverCalls >= 2
		mu.Unlock()
		if done {
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

	mu.Lock()
	defer mu.Unlock()
	if deliverCalls != 2 {
		t.Fatalf("delivery attempts = %d, want 2; sleeps=%v", deliverCalls, sleeps)
	}
	if sleepAfterSecondDelivery < 0 {
		t.Fatalf("no sleep recorded after the second delivery; sleeps=%v", sleeps)
	}
	if got := sleeps[0]; got != testInterval {
		t.Fatalf("sleep after the first delivery = %v, want %v; sleeps=%v", got, testInterval, sleeps)
	}
	if got := sleeps[sleepAfterSecondDelivery]; got != testInterval {
		t.Fatalf("sleep after the second, separate delivery = %v, want %v (the backoff must reset once the queue drained); sleeps=%v",
			got, testInterval, sleeps)
	}
}
