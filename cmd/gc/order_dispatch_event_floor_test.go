package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// rareEventOrder is an exec order on an event a healthy city may never
// record, so it can go its whole life without a run cursor.
func rareEventOrder() orders.Order {
	return orders.Order{Name: "notify-stalls", Trigger: "event", On: events.AgentClaimsStalled, Exec: "scripts/notify.sh"}
}

// eventFloorDispatcher builds a dispatcher of order over store and ep. Each
// run of the order's exec adds one to *runs; the dispatcher logs to stderr.
func eventFloorDispatcher(order orders.Order, store beads.Store, ep events.Provider, runs *int, stderr *bytes.Buffer) *memoryOrderDispatcher {
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		*runs++
		return []byte("ok"), nil
	}
	m := buildOrderDispatcherFromListExec([]orders.Order{order}, store, ep, execRun, events.Discard).(*memoryOrderDispatcher)
	m.stderr = lockedStderr(stderr)
	return m
}

// startEventOrdersAtTheHead runs one dispatch pass of ad. An event order's
// check that matches nothing reads through the head, so afterwards the order
// fires only on an event recorded later.
func startEventOrdersAtTheHead(t *testing.T, ad orderDispatcher) {
	t.Helper()
	ad.dispatch(context.Background(), t.TempDir(), time.Now())
	ad.drain(context.Background())
}

// tickOrders runs one dispatch pass of m and waits for what it launched.
func tickOrders(t *testing.T, m *memoryOrderDispatcher, now time.Time) {
	t.Helper()
	m.dispatch(context.Background(), t.TempDir(), now)
	m.drain(context.Background())
}

// eventCheckSpy is an event log that records where each read of one event
// type starts (its AfterSeq), and can fail those reads while its head still
// answers.
type eventCheckSpy struct {
	*events.Fake
	typ string

	mu       sync.Mutex
	after    []uint64
	failRead bool
}

func (p *eventCheckSpy) List(filter events.Filter) ([]events.Event, error) {
	if filter.Type == p.typ {
		p.mu.Lock()
		p.after = append(p.after, filter.AfterSeq)
		fail := p.failRead
		p.mu.Unlock()
		if fail {
			return nil, errors.New("simulated event log read failure")
		}
	}
	return p.Fake.List(filter)
}

func (p *eventCheckSpy) setFailRead(fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failRead = fail
}

// readsAfter is the AfterSeq of every read of the spied type, in order.
func (p *eventCheckSpy) readsAfter() []uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.after)
}

// TestOrderDispatchNeverRunEventOrderStartsAtTheHead: an event order that has
// never run starts at the newest event when its dispatcher was built. Its
// first check opens no archive below that head, so an archive there that
// cannot be read is never read, and an event recorded after that check fires
// the order.
func TestOrderDispatchNeverRunEventOrderStartsAtTheHead(t *testing.T) {
	dir := t.TempDir()
	// History below the head that does not decompress: reading it is a read
	// error, so any check that opens it reports one.
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl.archive-20261031T070000Z-seq-1-100.gz"), []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := events.NewFileRecorder(filepath.Join(dir, "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { rec.Close() }) //nolint:errcheck // test cleanup
	rec.Record(events.Event{Type: events.BeadCreated, Actor: "test"})

	store := beads.NewMemStore()
	var runs int
	var stderr bytes.Buffer
	m := eventFloorDispatcher(rareEventOrder(), store, rec, &runs, &stderr)
	now := time.Now()

	tickOrders(t, m, now)
	if runs != 0 {
		t.Fatalf("exec runs after the first check = %d, want 0", runs)
	}
	if strings.Contains(stderr.String(), "read error") {
		t.Fatalf("the first check of a never-run order read history below the head:\n%s", stderr.String())
	}

	rec.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller", Subject: "gascity/review-codex"})
	head, err := rec.LatestSeq()
	if err != nil {
		t.Fatalf("LatestSeq: %v", err)
	}
	tickOrders(t, m, now.Add(time.Minute))
	if runs != 1 {
		t.Fatalf("exec runs after the event = %d, want 1; stderr:\n%s", runs, stderr.String())
	}
	runsRecorded := trackingBeads(t, store, "order-run:notify-stalls")
	if len(runsRecorded) != 1 || !slicesContain(runsRecorded[0].Labels, fmt.Sprintf("seq:%d", head)) {
		t.Fatalf("run records = %+v, want one stamped seq:%d", runsRecorded, head)
	}
	if strings.Contains(stderr.String(), "read error") {
		t.Fatalf("a check read history below the head:\n%s", stderr.String())
	}
}

// TestOrderDispatchNeverRunEventOrderStartsWhenItsDispatcherIsBuilt: an event
// order that has never run starts at the head read when its dispatcher was
// built, which is when the controller starts watching, not at its first check,
// which can come passes later. An event recorded between the two fires it; one
// recorded before the dispatcher was built does not.
func TestOrderDispatchNeverRunEventOrderStartsWhenItsDispatcherIsBuilt(t *testing.T) {
	t.Run("an event recorded before the first check fires it", func(t *testing.T) {
		ep := events.NewFake()
		ep.Record(events.Event{Type: events.BeadCreated, Actor: "test"})
		store := beads.NewMemStore()
		var runs int
		var stderr bytes.Buffer
		m := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)
		ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})

		tickOrders(t, m, time.Now())
		if runs != 1 {
			t.Fatalf("exec runs = %d, want 1: the event recorded after the dispatcher was built must fire the order; stderr:\n%s", runs, stderr.String())
		}
	})
	t.Run("an event recorded before the dispatcher was built does not", func(t *testing.T) {
		ep := events.NewFake()
		ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})
		store := beads.NewMemStore()
		var runs int
		var stderr bytes.Buffer
		m := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)
		now := time.Now()

		tickOrders(t, m, now)
		if runs != 0 {
			t.Fatalf("exec runs = %d, want 0: an event recorded before the dispatcher was built is history, not replayed; stderr:\n%s", runs, stderr.String())
		}
		ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})
		tickOrders(t, m, now.Add(time.Minute))
		if runs != 1 {
			t.Fatalf("exec runs after a new event = %d, want 1; stderr:\n%s", runs, stderr.String())
		}
	})
}

// TestOrderDispatchEventOrderReadsOnlyWhatItHasNotChecked: a check that finds
// no matching event moves the order's start up to the head it read, so each
// check reads only the events recorded since the previous one, both before the
// order first fires and after it, when its run cursor would otherwise hold it
// back.
func TestOrderDispatchEventOrderReadsOnlyWhatItHasNotChecked(t *testing.T) {
	ep := &eventCheckSpy{Fake: events.NewFake(), typ: events.AgentClaimsStalled}
	noise := func() { ep.Record(events.Event{Type: events.BeadCreated, Actor: "test"}) }
	store := beads.NewMemStore()
	var runs int
	var stderr bytes.Buffer
	m := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)
	now := time.Now()
	tick := func() {
		t.Helper()
		now = now.Add(time.Minute)
		tickOrders(t, m, now)
	}

	noise()
	noise()
	tick() // never run: starts where its dispatcher began watching, 0; nothing matched up to 2
	noise()
	noise()
	tick() // reads past 2; nothing matched up to 4
	noise()
	tick() // reads past 4
	ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})
	tick() // reads past 5 and fires; the run stamps cursor 6
	noise()
	tick() // reads past the run cursor, 6
	noise()
	tick() // reads past 7, not back from the run cursor

	if got, want := ep.readsAfter(), []uint64{0, 2, 4, 5, 6, 7}; !slices.Equal(got, want) {
		t.Fatalf("checks read past %v, want %v", got, want)
	}
	if runs != 1 {
		t.Fatalf("exec runs = %d, want 1; stderr:\n%s", runs, stderr.String())
	}
}

// TestOrderDispatchEventOrderThatHasRunCatchesUpAfterARestart: how far a
// dispatcher's checks have read is memory only. An order that has run resumes
// from its run cursor in a new dispatcher, so an event recorded while no
// dispatcher was watching still fires it.
func TestOrderDispatchEventOrderThatHasRunCatchesUpAfterARestart(t *testing.T) {
	ep := events.NewFake()
	store := beads.NewMemStore()
	var runs int
	var stderr bytes.Buffer
	now := time.Now()

	first := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)
	tickOrders(t, first, now)
	ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})
	tickOrders(t, first, now.Add(time.Minute))
	if runs != 1 {
		t.Fatalf("exec runs before the restart = %d, want 1; stderr:\n%s", runs, stderr.String())
	}

	ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})
	restarted := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)
	tickOrders(t, restarted, now.Add(2*time.Minute))
	if runs != 2 {
		t.Fatalf("exec runs after the restart = %d, want 2: the event recorded while no dispatcher watched must fire the order; stderr:\n%s", runs, stderr.String())
	}
}

// TestReplaceOrderDispatcherCarriesEventFloors: a reload or rescan rebuilds
// the dispatcher, and the new one keeps how far the old one's checks had read.
// An event order that has never run keeps its start, so an event recorded
// between the old dispatcher's last check and the new one's first still fires
// it. That includes a first check that read an empty journal: its floor of 0 is
// carried as a floor, not mistaken for no floor at all.
func TestReplaceOrderDispatcherCarriesEventFloors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(ep *events.Fake)
	}{
		{name: "after an unrelated event", setup: func(ep *events.Fake) {
			ep.Record(events.Event{Type: events.BeadCreated, Actor: "test"})
		}},
		{name: "on an empty journal", setup: func(*events.Fake) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep := events.NewFake()
			tc.setup(ep)
			store := beads.NewMemStore()
			var runs int
			var stderr bytes.Buffer
			now := time.Now()

			prev := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)
			tickOrders(t, prev, now) // never run: reads through the head
			ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})

			next := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)
			cr := &CityRuntime{od: prev}
			cr.replaceOrderDispatcher(next)
			tickOrders(t, next, now.Add(time.Minute))
			if runs != 1 {
				t.Fatalf("exec runs after the rebuild = %d, want 1: the event recorded across the rebuild must fire the order; stderr:\n%s", runs, stderr.String())
			}
		})
	}
}

// TestReplaceOrderDispatcherCarriesTheStartOfEventOrders: a rebuilt dispatcher
// starts an event order the previous one already had where the previous one
// started it, even when the previous one never checked it, so an event
// recorded before the rebuild still fires it. An order the rebuild adds starts
// at the head read when the rebuild was built.
func TestReplaceOrderDispatcherCarriesTheStartOfEventOrders(t *testing.T) {
	t.Run("an order the previous dispatcher never checked", func(t *testing.T) {
		ep := &eventCheckSpy{Fake: events.NewFake(), typ: events.AgentClaimsStalled}
		ep.Record(events.Event{Type: events.BeadCreated, Actor: "test"})
		store := beads.NewMemStore()
		var runs int
		var stderr bytes.Buffer

		prev := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr) // starts at 1
		ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})
		next := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr) // built at 2
		(&CityRuntime{od: prev}).replaceOrderDispatcher(next)
		tickOrders(t, next, time.Now())

		if got, want := ep.readsAfter(), []uint64{1}; !slices.Equal(got, want) {
			t.Fatalf("checks read past %v, want %v: the rebuild must keep the start the previous dispatcher gave the order", got, want)
		}
		if runs != 1 {
			t.Fatalf("exec runs = %d, want 1; stderr:\n%s", runs, stderr.String())
		}
	})
	t.Run("an order the rebuild adds", func(t *testing.T) {
		ep := &eventCheckSpy{Fake: events.NewFake(), typ: events.AgentClaimsStalled}
		store := beads.NewMemStore()
		var runs int
		var stderr bytes.Buffer
		resumes := orders.Order{Name: "notify-resumes", Trigger: "event", On: events.AgentClaimsResumed, Exec: "scripts/notify.sh"}

		prev := eventFloorDispatcher(resumes, store, ep, &runs, &stderr)
		ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"}) // before the rebuild adds the order
		next := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)     // built at 1
		(&CityRuntime{od: prev}).replaceOrderDispatcher(next)
		ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"}) // after the rebuild, before its first check
		tickOrders(t, next, time.Now())

		if got, want := ep.readsAfter(), []uint64{1}; !slices.Equal(got, want) {
			t.Fatalf("checks read past %v, want %v: an order a rebuild adds starts at the rebuild's head", got, want)
		}
		if runs != 1 {
			t.Fatalf("exec runs = %d, want 1; stderr:\n%s", runs, stderr.String())
		}
	})
}

// TestCarryEventFloorsFromKeepsAFloorOfZero: a floor of 0, the floor of a first
// check that read an empty journal, is a floor like any other, so the carry
// copies it rather than treating it as absent.
func TestCarryEventFloorsFromKeepsAFloorOfZero(t *testing.T) {
	prev := &memoryOrderDispatcher{eventFloors: map[string]uint64{"notify-stalls": 0}}
	next := &memoryOrderDispatcher{aa: []orders.Order{rareEventOrder()}}
	next.carryEventFloorsFrom(prev)

	if want := map[string]uint64{"notify-stalls": 0}; !maps.Equal(next.eventFloors, want) {
		t.Fatalf("carried floors = %v, want %v", next.eventFloors, want)
	}
}

// TestCarryEventFloorsFromDropsOrdersTheRebuildNoLongerWatches bounds the maps:
// a floor or a start is carried only for an event order the rebuilt dispatcher
// still has.
func TestCarryEventFloorsFromDropsOrdersTheRebuildNoLongerWatches(t *testing.T) {
	prev := &memoryOrderDispatcher{
		eventFloors: map[string]uint64{
			"notify-stalls": 7,
			"removed-order": 9,
			"now-cooldown":  11,
		},
		eventSeeds: map[string]uint64{
			"notify-stalls": 3,
			"removed-order": 4,
			"now-cooldown":  5,
		},
	}
	next := &memoryOrderDispatcher{aa: []orders.Order{
		rareEventOrder(),
		{Name: "now-cooldown", Trigger: "cooldown", Interval: "1m", Exec: "true"},
	}}
	next.carryEventFloorsFrom(prev)

	if want := map[string]uint64{"notify-stalls": 7}; !maps.Equal(next.eventFloors, want) {
		t.Fatalf("carried floors = %v, want %v", next.eventFloors, want)
	}
	if want := map[string]uint64{"notify-stalls": 3}; !maps.Equal(next.eventSeeds, want) {
		t.Fatalf("carried starts = %v, want %v", next.eventSeeds, want)
	}
}

// TestOrderDispatchEventCheckThatCannotReadKeepsItsStart: a check that cannot
// read the event log is logged and moves nothing, so the event it could not
// read fires the order once the log reads again.
func TestOrderDispatchEventCheckThatCannotReadKeepsItsStart(t *testing.T) {
	ep := &eventCheckSpy{Fake: events.NewFake(), typ: events.AgentClaimsStalled}
	ep.Record(events.Event{Type: events.BeadCreated, Actor: "test"})
	store := beads.NewMemStore()
	var runs int
	var stderr bytes.Buffer
	m := eventFloorDispatcher(rareEventOrder(), store, ep, &runs, &stderr)
	now := time.Now()

	tickOrders(t, m, now) // never run: starts at the head, 1
	ep.Record(events.Event{Type: events.AgentClaimsStalled, Actor: "controller"})
	ep.setFailRead(true)
	tickOrders(t, m, now.Add(time.Minute))
	if runs != 0 {
		t.Fatalf("exec runs while the log cannot be read = %d, want 0", runs)
	}
	if !strings.Contains(stderr.String(), "notify-stalls event: read error: simulated event log read failure") {
		t.Fatalf("stderr does not report the failed read:\n%s", stderr.String())
	}

	ep.setFailRead(false)
	tickOrders(t, m, now.Add(2*time.Minute))
	if runs != 1 {
		t.Fatalf("exec runs once the log reads again = %d, want 1; stderr:\n%s", runs, stderr.String())
	}
}
