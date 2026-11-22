package main

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// orderOutcomeAlertRecorder is the order dispatcher's events.Recorder. It
// forwards every event unchanged and feeds each order outcome to a failure
// streak table, recording order.failing when an order's streak alerts.
//
// Wrapping the recorder, rather than editing each order.failed emit site,
// means every way an order can fail is counted, including ones added later.
// It needs no config load, store or CLI, which are what a failing order is
// usually failing on. It observes only: dispatch never reads it.
type orderOutcomeAlertRecorder struct {
	next    events.Recorder
	streaks *orders.FailureStreaks
	now     func() time.Time
}

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
	if e.Subject == "" {
		return
	}
	if e.Type == events.OrderFailed {
		if alert, ok := r.streaks.ObserveFailure(e.Subject, e.Message, r.now()); ok {
			r.next.Record(orderFailingEvent(alert))
		}
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
