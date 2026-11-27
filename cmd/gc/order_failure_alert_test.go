package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orderdispatch"
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

func recordOrderCompleted(m *memoryOrderDispatcher, scoped string) {
	m.rec.Record(events.Event{Type: events.OrderCompleted, Actor: "controller", Subject: scoped})
}

func TestOrderDispatcherRecordsOrderRecoveredOnceAfterAnAlertedStreak(t *testing.T) {
	var rec memRecorder
	m, clock := alertingOrderDispatcher(t, &rec, nil)
	const scoped = "gate-sweep"

	first := clock.now()
	for i := 0; i < orders.FailureAlertAfter; i++ {
		recordOrderFailed(m, scoped, "boom")
		clock.advance(time.Minute)
	}
	recordOrderCompleted(m, scoped)
	recordOrderCompleted(m, scoped)

	recovered := rec.eventsOfType(events.OrderRecovered)
	if len(recovered) != 1 {
		t.Fatalf("order.recovered events = %d, want exactly 1", len(recovered))
	}
	if recovered[0].Subject != scoped || recovered[0].Actor != "controller" {
		t.Errorf("order.recovered subject/actor = %q/%q", recovered[0].Subject, recovered[0].Actor)
	}
	var p events.OrderRecoveredPayload
	if err := json.Unmarshal(recovered[0].Payload, &p); err != nil {
		t.Fatalf("decoding order.recovered payload %s: %v", recovered[0].Payload, err)
	}
	want := events.OrderRecoveredPayload{
		OrderName:   scoped,
		Consecutive: orders.FailureAlertAfter,
		FirstFailed: first.UTC().Format(time.RFC3339),
		FailedForMS: (time.Duration(orders.FailureAlertAfter) * time.Minute).Milliseconds(),
	}
	if p != want {
		t.Errorf("payload = %+v, want %+v", p, want)
	}

	clock.advance(time.Minute)
	restart := clock.now()
	for i := 0; i < orders.FailureAlertAfter; i++ {
		recordOrderFailed(m, scoped, "again")
	}
	failing := rec.eventsOfType(events.OrderFailing)
	if len(failing) != 2 {
		t.Fatalf("order.failing events = %d, want 2 (one per streak)", len(failing))
	}
	var second events.OrderFailingPayload
	if err := json.Unmarshal(failing[1].Payload, &second); err != nil {
		t.Fatal(err)
	}
	if second.Consecutive != orders.FailureAlertAfter || second.FirstFailed != restart.UTC().Format(time.RFC3339) {
		t.Errorf("new streak = %+v, want it to restart at 1 from %s", second, restart.UTC().Format(time.RFC3339))
	}
}

// The wiring, not just the table: a reload that rebuilds the dispatcher
// through replaceOrderDispatcher continues a still-configured order's streak
// and drops a removed order's.
func TestReplaceOrderDispatcherCarriesOrderFailureStreaks(t *testing.T) {
	var rec memRecorder
	order := func(name string) orders.Order {
		return orders.Order{Name: name, Trigger: "cooldown", Interval: "1h", Exec: "true"}
	}
	prev, _ := alertingOrderDispatcher(t, &rec, []orders.Order{order("gate-sweep"), order("retired")})
	for i := 0; i < orders.FailureAlertAfter-1; i++ {
		recordOrderFailed(prev, "gate-sweep", "boom")
		recordOrderFailed(prev, "retired", "boom")
	}

	next, _ := alertingOrderDispatcher(t, &rec, []orders.Order{order("gate-sweep")})
	(&CityRuntime{od: prev}).replaceOrderDispatcher(next)

	recordOrderFailed(next, "gate-sweep", "boom")
	failing := rec.eventsOfType(events.OrderFailing)
	if len(failing) != 1 || failing[0].Subject != "gate-sweep" {
		t.Fatalf("order.failing after the rebuild = %+v, want gate-sweep's streak continued to %d", failing, orders.FailureAlertAfter)
	}
	for i := 0; i < orders.FailureAlertAfter-1; i++ {
		recordOrderFailed(next, "retired", "boom")
	}
	if got := rec.eventsOfType(events.OrderFailing); len(got) != 1 {
		t.Fatalf("a removed order's streak was carried across the rebuild: %+v", got)
	}
}

// A run still in flight when a reload retires its dispatcher records its
// outcome through the retired dispatcher after the swap. That outcome counts
// in the streak the active dispatcher continues: a late success ends the
// carried streak, so the next failure is the first of a new one, not the
// third in a row.
func TestReplaceOrderDispatcherCountsALateOutcomeOfTheRetiredDispatcher(t *testing.T) {
	var rec memRecorder
	aa := []orders.Order{{Name: "gate-sweep", Trigger: "cooldown", Interval: "1h", Exec: "true"}}
	prev, _ := alertingOrderDispatcher(t, &rec, aa)
	for i := 0; i < orders.FailureAlertAfter-1; i++ {
		recordOrderFailed(prev, "gate-sweep", "boom")
	}

	next, _ := alertingOrderDispatcher(t, &rec, aa)
	(&CityRuntime{od: prev}).replaceOrderDispatcher(next)
	recordOrderCompleted(prev, "gate-sweep")

	recordOrderFailed(next, "gate-sweep", "boom")
	if got := rec.eventsOfType(events.OrderFailing); len(got) != 0 {
		t.Fatalf("order.failing after a late success and one failure = %+v, want none: the success ended the streak", got)
	}
	for i := 1; i < orders.FailureAlertAfter; i++ {
		recordOrderFailed(next, "gate-sweep", "boom")
	}
	if got := rec.eventsOfType(events.OrderFailing); len(got) != 1 {
		t.Fatalf("order.failing after %d failures following the late success = %d, want 1", orders.FailureAlertAfter, len(got))
	}
}

// A webhook order fires through a dispatcher the controller builds per
// delivery, so its failures count in the controller's webhook table rather
// than in a table each delivery starts afresh: the third failing delivery
// alerts once, and a delivery that then succeeds records the recovery. Each
// delivery runs the real Dispatch seam; only its store and its run are stubbed.
func TestWebhookDeliveriesCountTowardOneFailureStreak(t *testing.T) {
	ep := events.NewFake()
	cs := &controllerState{
		cityPath:  t.TempDir(),
		cfg:       &config.City{Workspace: config.Workspace{Name: "t"}},
		eventProv: ep,
	}
	hook := orders.Order{Name: "deploy-hook", Trigger: "webhook", Exec: "deploy"}
	runs := 0
	deliver := func(runErr error) {
		t.Helper()
		md := controllerWebhookDispatcher{cs: cs}.dispatcher()
		t.Cleanup(md.dispatchCancel)
		md.storeFn = func(execStoreTarget) (beads.Store, error) { return beads.NewMemStore(), nil }
		md.execRun = func(context.Context, string, string, []string) ([]byte, error) {
			runs++
			return nil, runErr
		}
		res, err := md.Dispatch(context.Background(), orderdispatch.DispatchRequest{Order: hook})
		if err != nil || !res.Fired {
			t.Fatalf("delivery = %+v, %v; want fired", res, err)
		}
		md.drain(context.Background())
	}
	eventsOfType := func(typ string) []events.Event {
		t.Helper()
		got, err := ep.List(events.Filter{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	for i := 0; i < orders.FailureAlertAfter; i++ {
		deliver(errors.New("exit status 3"))
	}
	if got := eventsOfType(events.OrderFailed); len(got) != orders.FailureAlertAfter {
		t.Fatalf("order.failed events = %d, want %d", len(got), orders.FailureAlertAfter)
	}
	failing := eventsOfType(events.OrderFailing)
	if len(failing) != 1 || failing[0].Subject != "deploy-hook" {
		t.Fatalf("order.failing after %d failing deliveries = %+v, want exactly one for deploy-hook", orders.FailureAlertAfter, failing)
	}

	deliver(nil)
	recovered := eventsOfType(events.OrderRecovered)
	if len(recovered) != 1 || recovered[0].Subject != "deploy-hook" {
		t.Fatalf("order.recovered after a succeeding delivery = %+v, want exactly one for deploy-hook", recovered)
	}
	if runs != orders.FailureAlertAfter+1 {
		t.Fatalf("runs = %d, want %d: every delivery must reach the run", runs, orders.FailureAlertAfter+1)
	}
}

// The alert recorder wraps the recorder the order dispatch projects its
// execution facts through. It must stay an acknowledging recorder over an
// acknowledging one, or a graph order's steps are never marked as defined
// and every later projection emits their step_defined again.
func TestOrderDispatcherMarksGraphStepsDefinedThroughTheAlertRecorder(t *testing.T) {
	cityPath, cfg, a := newGraphOrderFixture(t)
	store := beads.NewMemStore()
	ep := events.NewFake()
	m := newMemoryOrderDispatcher(nil, []orders.Order{a}, cityPath, cfg, ep, io.Discard)
	t.Cleanup(m.dispatchCancel)
	m.storeFn = func(execStoreTarget) (beads.Store, error) { return store, nil }

	dispatchGraphOrder(t, m, cityPath)

	defined, err := ep.List(events.Filter{Type: events.ExecutionStepDefined})
	if err != nil {
		t.Fatal(err)
	}
	if len(defined) == 0 {
		t.Fatalf("events = %+v, want the steps' step_defined facts", ep.Events)
	}
	for _, e := range defined {
		step, err := store.Get(e.Subject)
		if err != nil {
			t.Fatalf("step %s: %v", e.Subject, err)
		}
		if step.Metadata[beadmeta.StepDefinedEmittedMetadataKey] == "" {
			t.Errorf("step %s emitted step_defined but is not marked %s", e.Subject, beadmeta.StepDefinedEmittedMetadataKey)
		}
	}
}

// RecordAck confirms an append only when the wrapped recorder can. Over a
// bare recorder it still records the event, but reports it unconfirmed, so no
// caller marks a fact durable on the strength of it. Either way it counts the
// outcome.
func TestOrderOutcomeAlertRecorderAcknowledgesOnlyThroughAnAcknowledgingRecorder(t *testing.T) {
	failed := events.Event{Type: events.OrderFailed, Actor: "controller", Subject: "gate-sweep", Message: "boom"}

	acking := events.NewFake()
	if err := newOrderOutcomeAlertRecorder(acking, time.Time{}).RecordAck(failed); err != nil {
		t.Fatalf("RecordAck over an acknowledging recorder = %v, want nil", err)
	}
	if len(acking.Events) != 1 {
		t.Fatalf("acknowledging recorder holds %d events, want 1", len(acking.Events))
	}

	var bare memRecorder
	alerts := newOrderOutcomeAlertRecorder(&bare, time.Time{})
	for i := 0; i < orders.FailureAlertAfter; i++ {
		if err := alerts.RecordAck(failed); err == nil {
			t.Fatal("RecordAck over a bare recorder = nil, want an unconfirmed append")
		}
	}
	if got := bare.eventsOfType(events.OrderFailed); len(got) != orders.FailureAlertAfter {
		t.Fatalf("bare recorder holds %d order.failed events, want %d recorded best-effort", len(got), orders.FailureAlertAfter)
	}
	if got := bare.eventsOfType(events.OrderFailing); len(got) != 1 {
		t.Fatalf("order.failing through RecordAck = %d, want 1", len(got))
	}
}
