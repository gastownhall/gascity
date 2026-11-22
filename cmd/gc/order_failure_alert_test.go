package main

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// fakeOrderClock is a settable clock for the order-outcome alert recorder.
type fakeOrderClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeOrderClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeOrderClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// alertingOrderDispatcher builds a dispatcher through the production
// constructor, so the outcome-alert recorder is the one every order.failed
// emit site records through, and points its clock past the startup grace.
func alertingOrderDispatcher(t *testing.T, rec events.Recorder, aa []orders.Order) (*memoryOrderDispatcher, *fakeOrderClock) {
	t.Helper()
	m := newMemoryOrderDispatcher(nil, aa, t.TempDir(), &config.City{}, rec, io.Discard)
	t.Cleanup(func() { m.dispatchCancel() })
	alerts, ok := m.rec.(*orderOutcomeAlertRecorder)
	if !ok {
		t.Fatalf("dispatcher records through %T, want *orderOutcomeAlertRecorder", m.rec)
	}
	clock := &fakeOrderClock{t: time.Now().Add(orders.StartupFailureGrace + time.Minute)}
	alerts.now = clock.now
	return m, clock
}

func recordOrderFailed(m *memoryOrderDispatcher, scoped, msg string) {
	m.rec.Record(events.Event{Type: events.OrderFailed, Actor: "controller", Subject: scoped, Message: msg})
}

func (r *memRecorder) eventsOfType(typ string) []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Event
	for _, e := range r.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestOrderDispatcherRecordsOrderFailingAfterThreeConsecutiveFailures(t *testing.T) {
	var rec memRecorder
	m, clock := alertingOrderDispatcher(t, &rec, nil)
	const scoped = "gate-sweep"

	first := clock.now()
	recordOrderFailed(m, scoped, "fail 1")
	clock.advance(30 * time.Second)
	recordOrderFailed(m, "other-order", "unrelated")
	recordOrderFailed(m, scoped, "fail 2")
	if got := rec.eventsOfType(events.OrderFailing); len(got) != 0 {
		t.Fatalf("order.failing after 2 failures: %+v", got)
	}
	clock.advance(30 * time.Second)
	recordOrderFailed(m, scoped, "fail 3: gc bd: loading config")

	if got := rec.eventsOfType(events.OrderFailed); len(got) != 4 {
		t.Fatalf("order.failed events forwarded = %d, want 4", len(got))
	}
	failing := rec.eventsOfType(events.OrderFailing)
	if len(failing) != 1 {
		t.Fatalf("order.failing events = %d, want exactly 1", len(failing))
	}
	e := failing[0]
	if e.Subject != scoped || e.Actor != "controller" {
		t.Errorf("order.failing subject/actor = %q/%q, want %q/controller", e.Subject, e.Actor, scoped)
	}
	var p events.OrderFailingPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decoding order.failing payload %s: %v", e.Payload, err)
	}
	want := events.OrderFailingPayload{
		OrderName:    scoped,
		Consecutive:  3,
		FirstFailed:  first.UTC().Format(time.RFC3339),
		FailingForMS: time.Minute.Milliseconds(),
		LastError:    "fail 3: gc bd: loading config",
	}
	if p != want {
		t.Errorf("payload = %+v, want %+v", p, want)
	}

	clock.advance(time.Minute)
	recordOrderFailed(m, scoped, "fail 4")
	if got := rec.eventsOfType(events.OrderFailing); len(got) != 1 {
		t.Fatalf("a 4th failure within the hour recorded another order.failing (%d total)", len(got))
	}
	clock.advance(orders.FailureAlertRepeat)
	recordOrderFailed(m, scoped, "fail 5")
	if got := rec.eventsOfType(events.OrderFailing); len(got) != 2 {
		t.Fatalf("order.failing events after a failure an hour past the alert = %d, want 2", len(got))
	}
}

// A dispatcher built now sits inside the startup grace: the #3898 burst of
// spurious failures right after a controller start never alerts.
func TestOrderDispatcherIgnoresFailuresRightAfterControllerStart(t *testing.T) {
	var rec memRecorder
	m := newMemoryOrderDispatcher(nil, nil, t.TempDir(), &config.City{}, &rec, io.Discard)
	t.Cleanup(func() { m.dispatchCancel() })
	for i := 0; i < 2*orders.FailureAlertAfter; i++ {
		recordOrderFailed(m, "gate-sweep", "spurious")
	}
	if got := rec.eventsOfType(events.OrderFailing); len(got) != 0 {
		t.Fatalf("order.failing inside the startup grace: %+v", got)
	}
}
