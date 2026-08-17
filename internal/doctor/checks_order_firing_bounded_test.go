package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// eventReadCall records one event-log read issued by the check.
type eventReadCall struct {
	filter events.Filter
	limit  int
}

// spyEventReader wraps the real reader and records every call so a test can
// assert the read shape (bounded vs unbounded) rather than only its result.
func spyEventReader(calls *[]eventReadCall) orderFiringEventReadFunc {
	return func(path string, filter events.Filter, limit int) ([]events.Event, error) {
		*calls = append(*calls, eventReadCall{filter: filter, limit: limit})
		return events.ReadFilteredTail(path, filter, limit)
	}
}

// TestOrderFiringCurrent_EventReadsAreBounded is the regression guard for
// ga-klv: the check must never issue an unbounded read against the city event
// log. On a busy city that log reaches hundreds of megabytes, and a full scan
// (36s per read, measured on a 161MB/253k-line log) blows the 15s check budget
// and turns this check permanently red for a reason unrelated to order firing.
//
// The check needs only the newest firing per order, so every read it issues up
// front must carry a positive limit. The one sanctioned unbounded read is the
// controller-start fallback, and only after the bounded read came back empty.
func TestOrderFiringCurrent_EventReadsAreBounded(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-10 * time.Minute)},
	)

	var calls []eventReadCall
	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	check.readEvents = spyEventReader(&calls)

	result := check.Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if len(calls) == 0 {
		t.Fatal("check issued no event-log reads; the spy seam is not wired")
	}

	var sawFired, sawStarted bool
	for i, call := range calls {
		switch call.filter.Type {
		case events.OrderFired:
			sawFired = true
			if call.limit <= 0 {
				t.Fatalf("call %d: order.fired read is unbounded (limit=%d); a full event-log scan blows the check budget", i, call.limit)
			}
		case events.ControllerStarted:
			// The first controller.started read must be bounded. A later
			// unbounded read is the sanctioned fallback for a log whose
			// active file holds no controller start at all.
			if !sawStarted && call.limit <= 0 {
				t.Fatalf("call %d: first controller.started read is unbounded (limit=%d)", i, call.limit)
			}
			sawStarted = true
		default:
			t.Fatalf("call %d: unexpected event filter type %q", i, call.filter.Type)
		}
	}
	if !sawFired {
		t.Fatal("check never read order.fired events")
	}
	if !sawStarted {
		t.Fatal("check never read controller.started events")
	}
}

// TestOrderFiringCurrent_LargeEventLogStaysInsideBudget is the behavioral half
// of the guard: with a log large enough that a full scan is measurably slow, the
// check must still finish well inside its budget. It fails if anyone reinstates
// an unbounded read, independent of the call-shape assertions above.
func TestOrderFiringCurrent_LargeEventLogStaysInsideBudget(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")

	// Oldest-first: the controller start and a large body of unrelated noise,
	// then the firing we care about last. A tail read reaches the firing after
	// a few lines; a full scan pays for every one of them.
	evts := []events.Event{{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)}}
	for i := 0; i < 40000; i++ {
		evts = append(evts, events.Event{
			Type:    events.OrderFired,
			Subject: fmt.Sprintf("noise-order-%d", i%64),
			Ts:      now.Add(-12 * time.Hour),
		})
	}
	evts = append(evts, events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-10 * time.Minute)})
	writeOrderFiringTestEvents(t, cityPath, evts...)

	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	check.lastRun = func(orders.Order) (time.Time, error) {
		return time.Time{}, fmt.Errorf("lastRun must not be consulted: the firing is in the event tail")
	}

	start := time.Now()
	result := check.Run(&CheckContext{CityPath: cityPath})
	elapsed := time.Since(start)

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	// Generous relative to a bounded read (milliseconds) and far under the 15s
	// budget, but tight enough that a full-scan regression trips it.
	if budget := 5 * time.Second; elapsed > budget {
		t.Fatalf("check took %s on a large event log, want under %s; the event read is likely unbounded again", elapsed, budget)
	}
}

// TestOrderFiringCurrent_FiringOlderThanTailFallsBackToLastRun pins the
// correctness contract that makes the bounded read safe: an order whose newest
// firing predates the tail window is not silently reported as never-fired — the
// check falls through to the authoritative (already bounded) order-run lookup.
func TestOrderFiringCurrent_FiringOlderThanTailFallsBackToLastRun(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-10 * time.Minute)},
	)

	lastRunCalled := false
	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	// Simulate a firing that fell outside the tail window: the event read
	// returns nothing for this order, so only order-run history can answer.
	check.readEvents = func(path string, filter events.Filter, limit int) ([]events.Event, error) {
		if filter.Type == events.OrderFired {
			return nil, nil
		}
		return events.ReadFilteredTail(path, filter, limit)
	}
	check.lastRun = func(orders.Order) (time.Time, error) {
		lastRunCalled = true
		return now.Add(-10 * time.Minute), nil
	}

	result := check.Run(&CheckContext{CityPath: cityPath})
	if !lastRunCalled {
		t.Fatal("lastRun was not consulted for a firing outside the event tail; the bounded read would report a false stale")
	}
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok (order-run history has a fresh run); msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
}

// TestOrderFiringCurrent_TimeoutHintNamesQueryCost pins the corrected hint. The
// old text blamed "beads/Dolt connectivity", which sent triage at the data
// plane while the data plane was healthy and cost a full triage cycle (ga-klv).
// A timeout here is a query-cost problem, so the hint must say so.
func TestOrderFiringCurrent_TimeoutHintNamesQueryCost(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "mol-dog-stalled-history", "cron", "0 */4 * * *")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "mol-dog-stalled-history", Ts: now.Add(-13 * time.Hour)},
	)

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	check.historyTimeout = 20 * time.Millisecond
	check.lastRun = func(orders.Order) (time.Time, error) {
		<-release
		return time.Time{}, nil
	}

	result := check.Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error; msg = %s", result.Status, result.Message)
	}
	if strings.Contains(strings.ToLower(result.FixHint), "connectivity") {
		t.Fatalf("FixHint = %q, must not blame connectivity: a timeout here is a query-cost problem", result.FixHint)
	}
	for _, want := range []string{"gc order history", "--limit"} {
		if !strings.Contains(result.FixHint, want) {
			t.Fatalf("FixHint = %q, want it to mention %q", result.FixHint, want)
		}
	}
}

// TestOrderFiringCurrent_LastRunLookupsRunInParallel is the regression guard
// for the second half of ga-klv. Each order-run lookup is a store round-trip
// costing about a second on a busy city; issued serially across the monitored
// orders they exceed the check budget on their own, and the check then reports
// a blocking failure that says nothing about whether orders are firing.
func TestOrderFiringCurrent_LastRunLookupsRunInParallel(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)

	// Every order is stale by events, so all of them need the lookup.
	const orderCount = 8
	var evts []events.Event
	evts = append(evts, events.Event{Type: events.ControllerStarted, Ts: now.Add(-240 * time.Hour)})
	for i := 0; i < orderCount; i++ {
		name := fmt.Sprintf("cooldown-order-%d", i)
		writeOrderFiringTestOrder(t, cityPath, name, "cooldown", "1h")
		evts = append(evts, events.Event{Type: events.OrderFired, Subject: name, Ts: now.Add(-9 * time.Hour)})
	}
	writeOrderFiringTestEvents(t, cityPath, evts...)

	// Prove the fan-out overlaps deterministically instead of racing a wall
	// clock: every lookup rendezvouses at a barrier and only returns once
	// wantConcurrent of them are in flight at the same time. A serial fan-out
	// can never gather the quorum, so it trips the failsafe and fails the
	// maxInFlight assertion below rather than passing by luck. The failsafe
	// never fires while the lookups genuinely run in parallel; it only bounds a
	// future regression to serial so the test fails fast instead of hanging.
	const wantConcurrent = 2
	const barrierFailsafe = 5 * time.Second
	var inFlight, maxInFlight int32
	rendezvous := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(rendezvous) }) }
	failsafe := time.AfterFunc(barrierFailsafe, release)
	defer failsafe.Stop()

	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	check.lastRun = func(orders.Order) (time.Time, error) {
		cur := atomic.AddInt32(&inFlight, 1)
		defer atomic.AddInt32(&inFlight, -1)
		for {
			observed := atomic.LoadInt32(&maxInFlight)
			if cur <= observed || atomic.CompareAndSwapInt32(&maxInFlight, observed, cur) {
				break
			}
		}
		if cur >= wantConcurrent {
			release()
		}
		<-rendezvous
		return now.Add(-30 * time.Minute), nil
	}

	result := check.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Fatalf("status = %v, want ok (every order has a fresh run); msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if got := atomic.LoadInt32(&maxInFlight); got < wantConcurrent {
		t.Fatalf("max concurrent order-run lookups = %d, want at least %d; the fan-out is still serial", got, wantConcurrent)
	}
}

// TestOrderFiringCurrent_PrefetchPreservesLookupErrors makes sure moving the
// lookups off the classification loop did not swallow their failures: a lookup
// error must still surface as a blocking check error, exactly as it did when
// the loop called the resolver inline.
func TestOrderFiringCurrent_PrefetchPreservesLookupErrors(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-240 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-9 * time.Hour)},
	)

	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	check.lastRun = func(orders.Order) (time.Time, error) {
		return time.Time{}, fmt.Errorf("store unreachable")
	}

	result := check.Run(&CheckContext{CityPath: cityPath})
	if result.Status != StatusError {
		t.Fatalf("status = %v, want error when the order-run lookup fails", result.Status)
	}
	if joined := strings.Join(result.Details, "\n"); !strings.Contains(joined, "store unreachable") {
		t.Fatalf("details = %v, want the lookup error surfaced", result.Details)
	}
	if result.Severity != SeverityBlocking {
		t.Fatalf("Severity = %v, want SeverityBlocking for a failed lookup", result.Severity)
	}
}

// TestOrderFiringCurrent_PrefetchSkipsOrdersTheEventLogAnswers keeps the
// parallel pre-pass from turning into a store stampede: an order the event log
// already proves current must not be looked up at all.
func TestOrderFiringCurrent_PrefetchSkipsOrdersTheEventLogAnswers(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "fresh-cooldown", "cooldown", "1h")
	writeOrderFiringTestOrder(t, cityPath, "stale-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-240 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "fresh-cooldown", Ts: now.Add(-10 * time.Minute)},
		events.Event{Type: events.OrderFired, Subject: "stale-cooldown", Ts: now.Add(-9 * time.Hour)},
	)

	var mu sync.Mutex
	var lookedUp []string
	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	check.lastRun = func(o orders.Order) (time.Time, error) {
		mu.Lock()
		lookedUp = append(lookedUp, o.ScopedName())
		mu.Unlock()
		return now.Add(-30 * time.Minute), nil
	}

	check.Run(&CheckContext{CityPath: cityPath})

	mu.Lock()
	defer mu.Unlock()
	if len(lookedUp) != 1 || lookedUp[0] != "stale-cooldown" {
		t.Fatalf("looked up %v, want only the order the event log cannot answer", lookedUp)
	}
}

// TestOrderFiringEventTailLimitIsPositive keeps the tail bound from being
// zeroed out, which would silently restore the unbounded read: the reader
// treats a non-positive limit as "read everything".
func TestOrderFiringEventTailLimitIsPositive(t *testing.T) {
	if orderFiringEventTailLimit <= 0 {
		t.Fatalf("orderFiringEventTailLimit = %d, want positive; a non-positive limit means an unbounded read", orderFiringEventTailLimit)
	}
}

// TestOrderFiringCurrent_ReadsCityEventLogPath guards against the check reading
// a path other than the city event log; it keeps the bounded read pointed at the
// file the rest of the suite writes.
func TestOrderFiringCurrent_ReadsCityEventLogPath(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	cityPath, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, cityPath, "cleanup-cooldown", "cooldown", "1h")
	writeOrderFiringTestEvents(t, cityPath,
		events.Event{Type: events.ControllerStarted, Ts: now.Add(-24 * time.Hour)},
		events.Event{Type: events.OrderFired, Subject: "cleanup-cooldown", Ts: now.Add(-10 * time.Minute)},
	)

	want := filepath.Join(cityPath, ".gc", "events.jsonl")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("event log not written where the suite expects: %v", err)
	}

	var paths []string
	check := NewOrderFiringCurrentCheck(cfg, cityPath)
	check.clock = func() time.Time { return now }
	check.readEvents = func(path string, filter events.Filter, limit int) ([]events.Event, error) {
		paths = append(paths, path)
		return events.ReadFilteredTail(path, filter, limit)
	}
	check.Run(&CheckContext{CityPath: cityPath})

	if len(paths) == 0 {
		t.Fatal("check issued no event-log reads")
	}
	for _, got := range paths {
		if got != want {
			t.Fatalf("read path = %q, want %q", got, want)
		}
	}
}
