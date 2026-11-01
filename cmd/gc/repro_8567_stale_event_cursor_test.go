package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestRepro_8567_StaleEventCursorBoundsToLogHeadNotZero reproduces gascity#8567:
// an event order's run record still carries a seq:<N> label from before
// .gc/events.jsonl was lost or replaced (host move, restore-from-backup,
// manual deletion). FileRecorder restarts its seq counter at 1 on the new
// log, so the run record's seq (999999, here) now sits far above the new
// log's actual head (3).
//
// Left unbounded, bdCursor's naive seq value feeds AfterSeq: 999999 into
// checkEvent, which then matches nothing — ever — until the new log's seq
// climbs back past 999999 (gc order#bdCursor / internal/orders/triggers.go#checkEvent).
//
// A naive clamp-to-0 "fix" is explicitly insufficient (per the issue report):
// it would resurrect every event already sitting in the reset log as newly
// "due" on the very next tick, and keep doing so every tick thereafter since
// nothing ever advances the cursor off 0 without a successful run. That is
// the repeat-fire loop this test also guards against.
//
// The actual fix bounds the cursor to the log's real head: boundEventCursorToHead
// clamps bdCursor's stale 999999 down to head=3, so the 3 pre-existing
// (post-reset) events are correctly treated as already-seen, and only a
// genuinely new event (seq 4) fires the trigger.
func TestRepro_8567_StaleEventCursorBoundsToLogHeadNotZero(t *testing.T) {
	const orderName = "digest"

	// Bead store holds a run record whose seq:<N> label is stale relative to
	// the (reset) event log: it predates the log loss described in #8567.
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{
		Title:  "run",
		Labels: []string{orders.RunLabel(orderName), "order:" + orderName, "seq:999999", "order-tracking"},
	}); err != nil {
		t.Fatal(err)
	}

	rawCursor, err := bdCursorAcrossStores(orderName, mem)
	if err != nil {
		t.Fatalf("bdCursorAcrossStores: %v", err)
	}
	if rawCursor != 999999 {
		t.Fatalf("rawCursor = %d, want 999999 (the stale label, unbounded)", rawCursor)
	}

	// The event log was reset: it now holds only 3 events, far below the
	// stale 999999 cursor above.
	ep := events.NewFake()
	ep.Record(events.Event{Type: "digest.ready"})
	ep.Record(events.Event{Type: "digest.ready"})
	ep.Record(events.Event{Type: "digest.ready"})

	bounded, ok := boundEventCursorToHead(orderName, rawCursor, ep)
	if !ok {
		t.Fatalf("boundEventCursorToHead: ok=false, want true (head read succeeded)")
	}
	if bounded != 3 {
		t.Fatalf("boundEventCursorToHead = %d, want 3 (the log's actual head, NOT 0)", bounded)
	}

	a := orders.Order{Name: orderName, Trigger: "event", On: "digest.ready"}
	cursorFn := func(string) uint64 { return bounded }

	// Tick 1: nothing new since the (bounded) cursor. The 3 pre-existing
	// events must NOT be reported as due — that would be the clamp-to-0
	// repeat-fire bug the issue calls out.
	result := orders.CheckTriggerWithOptions(a, time.Now(), nil, ep, cursorFn, orders.TriggerOptions{})
	if result.Due {
		t.Fatalf("tick 1: Due = true (%q), want false: bounding to head must not resurrect pre-reset events", result.Reason)
	}

	// Tick 2, same cursor, same log: confirm the non-due result is stable and
	// not a fluke of ordering (guards the "infinite repeat-fire loop"
	// scenario — a buggy clamp-to-0 would flip to Due=true here and stay
	// Due=true forever).
	result = orders.CheckTriggerWithOptions(a, time.Now(), nil, ep, cursorFn, orders.TriggerOptions{})
	if result.Due {
		t.Fatalf("tick 2: Due = true (%q), want false: repeat-fire against already-seen events", result.Reason)
	}

	// A genuinely new event after the reset must still be detected: bounding
	// to head (not leaving the cursor at the stale 999999) must not black-hole
	// future events either.
	ep.Record(events.Event{Type: "digest.ready"})
	result = orders.CheckTriggerWithOptions(a, time.Now(), nil, ep, cursorFn, orders.TriggerOptions{})
	if !result.Due {
		t.Fatalf("tick 3: Due = false (%q), want true: a genuinely new event must still fire", result.Reason)
	}
}

// TestRepro_8567_BoundEventCursorFailsClosedWhenHeadUnreadable covers the
// fail-closed requirement: when the event log's head itself cannot be read,
// boundEventCursorToHead must not guess a cursor (neither the stale label nor
// 0) — callers use ok=false to skip the tick as "no new events yet".
func TestRepro_8567_BoundEventCursorFailsClosedWhenHeadUnreadable(t *testing.T) {
	ep := events.NewFailFake()
	bounded, ok := boundEventCursorToHead("digest", 999999, ep)
	if ok {
		t.Fatalf("boundEventCursorToHead: ok=true, want false when LatestSeq() fails")
	}
	if bounded != 0 {
		t.Fatalf("boundEventCursorToHead = %d on failure, want 0 (caller must ignore this and skip the tick, not use it as a cursor)", bounded)
	}
}

// TestRepro_8567_BoundEventCursorNilProviderPassesThrough documents that a
// nil events.Provider (no events backend configured) leaves the cursor
// unchanged rather than failing closed: checkEvent itself already reports
// "no events provider" and never calls AfterSeq in that case, so there is no
// head to bound against.
func TestRepro_8567_BoundEventCursorNilProviderPassesThrough(t *testing.T) {
	bounded, ok := boundEventCursorToHead("digest", 42, nil)
	if !ok || bounded != 42 {
		t.Fatalf("boundEventCursorToHead(nil ep) = (%d, %v), want (42, true)", bounded, ok)
	}
}
