package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// dedupTestEpoch is the fixed start of TestBeadUpdateDedupExpires's clock.
var dedupTestEpoch = time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)

// noopUpdateStore is a backing store that cannot be listed, so a cache over it
// is never primed, and whose Update writes nothing. Each Update through such a
// cache goes to the backing, re-reads the unchanged row and announces it
// again: the same bead.updated payload, twice.
type noopUpdateStore struct{ beads.Store }

func (noopUpdateStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list unavailable")
}

func (noopUpdateStore) Update(string, beads.UpdateOpts) error { return nil }

// TestWrapWithCachingStoreDropsRepeatedUpdateNotifications pins the wiring: a
// controller CachingStore records its notifications through
// cacheChangeRecorder, so a cache that announces the same row twice records
// one bead.updated, and a close after it is still recorded.
func TestWrapWithCachingStoreDropsRepeatedUpdateNotifications(t *testing.T) {
	backing := beads.NewMemStore()
	b, err := backing.Create(beads.Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ep := events.NewFake()
	store := wrapWithCachingStore(context.Background(), noopUpdateStore{Store: backing}, ep, false)
	cache, ok := store.(*beads.CachingStore)
	if !ok {
		t.Fatalf("wrapWithCachingStore returned %T, want *beads.CachingStore", store)
	}
	title := "after"
	for range 2 {
		if err := cache.Update(b.ID, beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	if err := cache.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	var got []string
	for _, evt := range ep.Events {
		if evt.Subject == b.ID {
			got = append(got, evt.Type+"/"+evt.Actor)
		}
	}
	want := []string{events.BeadUpdated + "/" + cacheReconcileActor, events.BeadClosed + "/" + cacheReconcileActor}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

// TestCacheChangeRecorderSuppressesRepeatedUpdates is the mc-zndi7.12 fix: a
// bead.updated whose payload equals the last one emitted for the bead carries
// nothing, and on bd cities it was 99% of the bus.
func TestCacheChangeRecorderSuppressesRepeatedUpdates(t *testing.T) {
	ep := events.NewFake()
	notify := cacheChangeRecorder(ep)
	send := func(eventType, id, payload string) {
		notify(eventType, id, "", "", "", nil, json.RawMessage(payload))
	}
	send(events.BeadCreated, "gc-1", `{"id":"gc-1","v":1}`)
	send(events.BeadUpdated, "gc-1", `{"id":"gc-1","v":1}`) // same as the create
	send(events.BeadUpdated, "gc-1", `{"id":"gc-1","v":2}`)
	send(events.BeadUpdated, "gc-1", `{"id":"gc-1","v":2}`) // repeat
	send(events.BeadUpdated, "gc-2", `{"id":"gc-2","v":2}`) // other bead, same bytes shape
	send(events.BeadUpdated, "gc-1", `{"id":"gc-1","v":1}`) // a real change back
	send(events.BeadClosed, "gc-1", `{"id":"gc-1","v":1}`)  // closes never suppress
	send(events.BeadUpdated, "gc-1", `{"id":"gc-1","v":1}`) // reopen: the close forgot it
	send(events.BeadDeleted, "gc-2", `{"id":"gc-2","v":2}`)
	send(events.BeadUpdated, "gc-2", `{"id":"gc-2","v":2}`)
	send(events.BeadCreated, "gc-3", `{"id":"gc-3"}`)
	send(events.BeadCreated, "gc-3", `{"id":"gc-3"}`) // only updates are suppressed

	var got []string
	for _, evt := range ep.Events {
		got = append(got, evt.Type+" "+evt.Subject+" "+string(evt.Payload))
	}
	want := []string{
		`bead.created gc-1 {"id":"gc-1","v":1}`,
		`bead.updated gc-1 {"id":"gc-1","v":2}`,
		`bead.updated gc-2 {"id":"gc-2","v":2}`,
		`bead.updated gc-1 {"id":"gc-1","v":1}`,
		`bead.closed gc-1 {"id":"gc-1","v":1}`,
		`bead.updated gc-1 {"id":"gc-1","v":1}`,
		`bead.deleted gc-2 {"id":"gc-2","v":2}`,
		`bead.updated gc-2 {"id":"gc-2","v":2}`,
		`bead.created gc-3 {"id":"gc-3"}`,
		`bead.created gc-3 {"id":"gc-3"}`,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("recorded:\n%v\nwant:\n%v", got, want)
	}
}

func TestBeadUpdateDedupIsBounded(t *testing.T) {
	d := newBeadUpdateDedup(4)
	ep := events.NewFake()
	update := func(id string) {
		d.record(ep, events.Event{Type: events.BeadUpdated, Subject: id, Payload: json.RawMessage(`"` + id + `"`)})
	}
	for i := range 10 {
		id := fmt.Sprintf("gc-%d", i)
		update(id)
		if len(ep.Events) != i+1 {
			t.Fatalf("first update of %s suppressed", id)
		}
		if n := d.size(); n > 4 {
			t.Fatalf("dedup holds %d ids, cap 4", n)
		}
	}
	update("gc-9")
	if len(ep.Events) != 10 {
		t.Fatal("the newest id was forgotten")
	}
	if got := d.suppressed.Load(); got != 1 {
		t.Fatalf("suppressed counter = %d, want 1", got)
	}
}

// droppingAckRecorder drops (and reports dropping) its first drop appends.
type droppingAckRecorder struct {
	*events.Fake
	drop int
}

func (r *droppingAckRecorder) RecordAck(e events.Event) error {
	if r.drop > 0 {
		r.drop--
		return errors.New("append dropped")
	}
	return r.Fake.RecordAck(e)
}

// droppingRecorder is a plain best-effort Recorder that silently drops its
// first drop events: it cannot say whether an append landed.
type droppingRecorder struct {
	events.Recorder
	drop int
	got  []events.Event
}

func (r *droppingRecorder) Record(e events.Event) {
	if r.drop > 0 {
		r.drop--
		return
	}
	r.got = append(r.got, e)
}

// TestCacheChangeRecorderRemembersOnlyAcknowledgedPayloads is the review's
// dedup probe: remembering a payload the recorder dropped would suppress
// every repeat, so the bus would never see the state until the bead changed.
func TestCacheChangeRecorderRemembersOnlyAcknowledgedPayloads(t *testing.T) {
	p := json.RawMessage(`{"id":"gc-1","v":1}`)
	t.Run("acknowledging recorder drops one append", func(t *testing.T) {
		rec := &droppingAckRecorder{Fake: events.NewFake(), drop: 1}
		notify := cacheChangeRecorder(rec)
		for range 3 {
			notify(events.BeadUpdated, "gc-1", "", "", "", nil, p)
		}
		if len(rec.Events) != 1 {
			t.Fatalf("recorded %d copies, want the one after the drop and no repeats", len(rec.Events))
		}
	})
	t.Run("recorder that cannot acknowledge", func(t *testing.T) {
		rec := &droppingRecorder{Recorder: events.Discard, drop: 1}
		notify := cacheChangeRecorder(rec)
		for range 5 {
			notify(events.BeadUpdated, "gc-1", "", "", "", nil, p)
		}
		if len(rec.got) == 0 {
			t.Fatal("the first record was dropped and every repeat was suppressed: the bus never sees gc-1's state")
		}
	})
}

// TestBeadUpdateDedupExpires: a remembered payload suppresses its repeats for
// beadUpdateDedupTTL only, so a consumer that missed the recorded copy sees
// the state again within that age.
func TestBeadUpdateDedupExpires(t *testing.T) {
	d := newBeadUpdateDedup(beadUpdateDedupCap)
	clock := dedupTestEpoch
	d.now = func() time.Time { return clock }
	ep := events.NewFake()
	update := func() {
		d.record(ep, events.Event{Type: events.BeadUpdated, Subject: "gc-1", Payload: json.RawMessage(`{"v":1}`)})
	}
	update()
	clock = clock.Add(beadUpdateDedupTTL - time.Second)
	update()
	if len(ep.Events) != 1 {
		t.Fatalf("recorded %d copies inside the TTL, want 1", len(ep.Events))
	}
	clock = clock.Add(time.Second)
	update()
	if len(ep.Events) != 2 {
		t.Fatalf("recorded %d copies at the TTL, want the repeat", len(ep.Events))
	}
	clock = clock.Add(time.Second)
	update()
	if len(ep.Events) != 2 {
		t.Fatalf("the re-recorded copy did not restart the TTL: %d copies", len(ep.Events))
	}
}

// TestCacheChangeRecorderDedupIsPerCache: each cache's recorder keeps its
// own record, so the same row seen through two caches is recorded by each.
func TestCacheChangeRecorderDedupIsPerCache(t *testing.T) {
	ep := events.NewFake()
	p := json.RawMessage(`{"id":"gc-1"}`)
	cacheChangeRecorder(ep)(events.BeadUpdated, "gc-1", "", "", "", nil, p)
	cacheChangeRecorder(ep)(events.BeadUpdated, "gc-1", "", "", "", nil, p)
	if len(ep.Events) != 2 {
		t.Fatalf("recorded %d, want one per cache", len(ep.Events))
	}
}
