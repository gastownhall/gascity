package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// seedClosedRun records a closed run of the scoped order stamped seq, as a
// run the controller recorded against an earlier event log.
func seedClosedRun(t *testing.T, store *beads.MemStore, scoped string, seq uint64) {
	t.Helper()
	run, err := store.Create(beads.Bead{
		Title:  "order run " + scoped,
		Labels: []string{orders.RunLabel(scoped), "order-tracking", "seq:" + strconv.FormatUint(seq, 10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(run.ID); err != nil {
		t.Fatal(err)
	}
}

// headFailingLog is an event log whose head reads fail from the nth read on
// (counted from arm) and whose event reads can fail.
type headFailingLog struct {
	*events.Fake

	mu        sync.Mutex
	headReads int
	failFrom  int
	failList  bool
}

func (l *headFailingLog) arm(failHeadFrom int, failList bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.headReads = 0
	l.failFrom = failHeadFrom
	l.failList = failList
}

func (l *headFailingLog) LatestSeq() (uint64, error) {
	l.mu.Lock()
	l.headReads++
	fail := l.failFrom > 0 && l.headReads >= l.failFrom
	l.mu.Unlock()
	if fail {
		return 0, errors.New("simulated event head failure")
	}
	return l.Fake.LatestSeq()
}

func (l *headFailingLog) List(filter events.Filter) ([]events.Event, error) {
	l.mu.Lock()
	fail := l.failList
	l.mu.Unlock()
	if fail {
		return nil, errors.New("simulated event list failure")
	}
	return l.Fake.List(filter)
}

// TestOrderDispatchEventReadFailureKeepsTheSuppressionStreak (AC-5): an event
// check that cannot read the log's head or its events logs the error, creates
// no run, executes nothing, and leaves the order's open-work suppression
// streak as it was.
func TestOrderDispatchEventReadFailureKeepsTheSuppressionStreak(t *testing.T) {
	tests := []struct {
		name         string
		failHeadFrom int
		failList     bool
		wantLog      string
	}{
		// The dispatcher reads the head for the check's start, then the
		// check reads it again: fail the check's own read.
		{name: "head read in the check", failHeadFrom: 2, wantLog: "simulated event head failure"},
		{name: "head read for the start", failHeadFrom: 1, wantLog: "simulated event head failure"},
		{name: "event list", failList: true, wantLog: "simulated event list failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order := rareEventOrder()
			scoped := order.ScopedName()
			store := beads.NewMemStore()
			seedClosedRun(t, store, scoped, 500)
			ep := &headFailingLog{Fake: events.NewFake()}
			ep.Record(events.Event{Type: order.On, Actor: "controller"})
			var runs int
			var stderr bytes.Buffer
			m := eventFloorDispatcher(order, store, ep, &runs, &stderr)
			since := time.Now().Add(-time.Hour)
			streak := orderOpenWorkSuppression{consecutive: 4, since: since}
			m.cacheMu.Lock()
			m.openWorkSuppression = map[string]orderOpenWorkSuppression{scoped: streak}
			m.cacheMu.Unlock()

			ep.arm(tt.failHeadFrom, tt.failList)
			tickOrders(t, m, time.Now())

			if runs != 0 {
				t.Fatalf("exec runs = %d, want 0", runs)
			}
			records, err := store.List(beads.ListQuery{Label: orders.RunLabel(scoped), IncludeClosed: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 {
				t.Fatalf("run records = %d, want only the seeded one", len(records))
			}
			if !strings.Contains(stderr.String(), tt.wantLog) {
				t.Fatalf("stderr does not report %q:\n%s", tt.wantLog, stderr.String())
			}
			m.cacheMu.Lock()
			got, ok := m.openWorkSuppression[scoped]
			m.cacheMu.Unlock()
			if !ok || got != streak {
				t.Fatalf("suppression streak = %+v (present %v), want %+v preserved", got, ok, streak)
			}
		})
	}
}
