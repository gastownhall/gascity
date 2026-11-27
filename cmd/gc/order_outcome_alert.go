package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderOutcomeAlertRecorder is the order dispatcher's events.Recorder. It
// forwards every event unchanged and feeds each order outcome to a failure
// streak table, recording order.failing when an order's streak alerts and
// order.recovered when a success ends an alerted streak.
//
// Wrapping the recorder, rather than editing each order.failed emit site,
// means every way an order can fail is counted, including ones added later.
// It needs no config load, store or CLI, which are what a failing order is
// usually failing on. It observes only: dispatch never reads it. It is an
// events.AckRecorder too, so the dispatch's execution-fact projection can
// still confirm a step_defined append and mark the step (RecordAck).
type orderOutcomeAlertRecorder struct {
	next    events.Recorder
	streaks *orders.FailureStreaks
	now     func() time.Time
}

var _ events.AckRecorder = (*orderOutcomeAlertRecorder)(nil)

// errOrderOutcomeAppendUnconfirmed is RecordAck's answer when the wrapped
// recorder cannot acknowledge an append (events.Discard, any bare Recorder).
var errOrderOutcomeAppendUnconfirmed = errors.New("order outcome recorder: the wrapped recorder cannot acknowledge an append")

// newOrderOutcomeAlertRecorder wraps next for a controller started at
// startedAt.
func newOrderOutcomeAlertRecorder(next events.Recorder, startedAt time.Time) *orderOutcomeAlertRecorder {
	return &orderOutcomeAlertRecorder{
		next:    next,
		streaks: orders.NewFailureStreaks(startedAt),
		now:     time.Now,
	}
}

// Record forwards e, then records any alert its outcome triggers.
func (r *orderOutcomeAlertRecorder) Record(e events.Event) {
	r.next.Record(e)
	r.observe(e)
}

// RecordAck forwards e through next's acknowledged append, then records any
// alert its outcome triggers. When next cannot acknowledge, e is still
// recorded best-effort and the error reports the append unconfirmed, so a
// caller never marks a fact durable on the strength of it: EmitCurrent marks
// a step's step_defined emitted only on a nil acknowledgement.
func (r *orderOutcomeAlertRecorder) RecordAck(e events.Event) error {
	ack, ok := r.next.(events.AckRecorder)
	if !ok {
		r.Record(e)
		return errOrderOutcomeAppendUnconfirmed
	}
	err := ack.RecordAck(e)
	r.observe(e)
	return err
}

// observe feeds e to the failure streak table when it is an order outcome,
// and records the alert the outcome triggers.
func (r *orderOutcomeAlertRecorder) observe(e events.Event) {
	if e.Subject == "" {
		return
	}
	switch e.Type {
	case events.OrderFailed:
		if alert, ok := r.streaks.ObserveFailure(e.Subject, e.Message, r.now()); ok {
			r.next.Record(orderFailingEvent(alert))
		}
	case events.OrderCompleted:
		if recovery, ok := r.streaks.ObserveSuccess(e.Subject, r.now()); ok {
			r.next.Record(orderRecoveredEvent(recovery))
		}
	}
}

func orderRecoveredEvent(rc orders.OrderRecovery) events.Event {
	firstFailed := rc.FirstFailed.UTC().Format(time.RFC3339)
	return events.Event{
		Type:    events.OrderRecovered,
		Actor:   "controller",
		Subject: rc.OrderName,
		Message: fmt.Sprintf("order %s recovered after %d consecutive failures since %s", rc.OrderName, rc.Consecutive, firstFailed),
		Payload: events.OrderRecoveredPayloadJSON(events.OrderRecoveredPayload{
			OrderName:   rc.OrderName,
			Consecutive: rc.Consecutive,
			FirstFailed: firstFailed,
			FailedForMS: rc.FailedFor.Milliseconds(),
		}),
	}
}

func orderFailingEvent(a orders.OrderFailingAlert) events.Event {
	firstFailed := a.FirstFailed.UTC().Format(time.RFC3339)
	return events.Event{
		Type:    events.OrderFailing,
		Actor:   "controller",
		Subject: a.OrderName,
		Message: fmt.Sprintf("order %s has failed %d times in a row since %s: %s", a.OrderName, a.Consecutive, firstFailed, a.LastError),
		Payload: events.OrderFailingPayloadJSON(events.OrderFailingPayload{
			OrderName:    a.OrderName,
			Consecutive:  a.Consecutive,
			FirstFailed:  firstFailed,
			FailingForMS: a.FailingFor.Milliseconds(),
			LastError:    a.LastError,
		}),
	}
}

// carryOrderFailureStreaksFrom makes m record into prev's failure streak
// table, pruned to the orders m still dispatches, so a reload or rescan
// neither resets a running streak nor restarts the startup grace. Sharing
// the table, rather than copying it, also counts the outcome of a run still
// in flight on prev when the drain gave up: it records through prev after
// the swap. Call before m dispatches, so its own table holds no outcome yet.
func (m *memoryOrderDispatcher) carryOrderFailureStreaksFrom(prev *memoryOrderDispatcher) {
	if m == nil || prev == nil {
		return
	}
	if _, ok := m.rec.(*orderOutcomeAlertRecorder); !ok {
		return
	}
	old, ok := prev.rec.(*orderOutcomeAlertRecorder)
	if !ok {
		return
	}
	live := make(map[string]struct{}, len(m.aa))
	for i := range m.aa {
		live[m.aa[i].ScopedName()] = struct{}{}
	}
	old.streaks.Prune(func(name string) bool {
		_, ok := live[name]
		return ok
	})
	m.countOrderFailuresIn(old.streaks)
}

// countOrderFailuresIn makes m count order outcomes in streaks rather than in
// the table it was built with. Call before m dispatches, so its own table
// holds no outcome yet.
func (m *memoryOrderDispatcher) countOrderFailuresIn(streaks *orders.FailureStreaks) {
	if alerts, ok := m.rec.(*orderOutcomeAlertRecorder); ok {
		alerts.streaks = streaks
	}
}

// webhookOrderFailureStreaksOf returns the failure streak table the webhook
// dispatch seam counts in for this controller boot, created on first use. Its
// startup grace runs from the controller start, not from a delivery.
func (cs *controllerState) webhookOrderFailureStreaksOf() *orders.FailureStreaks {
	cs.webhookOrderFailureStreaksOnce.Do(func() {
		cs.webhookOrderFailureStreaks = orders.NewFailureStreaks(cs.startedAt)
	})
	return cs.webhookOrderFailureStreaks
}
