package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/eventfact"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// journalCache is a controller CachingStore over a store that keeps pinned
// ids, recording to ep, with an op store over it.
func journalCache(t *testing.T) (*beads.CachingStore, *opjournal.Store, *events.Fake) {
	t.Helper()
	ep := events.NewFake()
	store := wrapWithCachingStore(context.Background(), &beads.MemStore{HonorExplicitIDs: true}, ep, true)
	cache, ok := store.(*beads.CachingStore)
	if !ok {
		t.Fatalf("wrapWithCachingStore returned %T, want *beads.CachingStore", store)
	}
	st, err := opjournal.NewStore(beads.SessionStore{Store: cache}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return cache, st, ep
}

func journalEventLog(ep *events.Fake) []string {
	var out []string
	for _, e := range ep.Events {
		out = append(out, e.Type+" "+e.Subject)
	}
	return out
}

// DES-O1 D18: an op row's changes are recorded as op.changed, derived from
// the row's post-image, once per distinct post-image, and never as bead.*.
func TestCacheBridgeRecordsOpChangedForOpRows(t *testing.T) {
	_, st, ep := journalCache(t)
	op, _, err := st.Propose(opjournal.Request{
		Conversation: "gc-1", Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: "msg-1", Producer: opjournal.Producer{Principal: "t3:env", Actor: "user"}, Payload: "secret text",
		ClientSeq: 3, Correlation: "thread",
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if _, _, err := st.Cancel(op.ID, "t3:env", "edited"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	var changes []api.OpChangedPayload
	for _, e := range ep.Events {
		switch e.Type {
		case events.OpChanged:
			if e.Subject != op.ID || e.SessionID != "gc-1" {
				t.Fatalf("op.changed envelope = %+v", e)
			}
			var p api.OpChangedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("decode %s: %v", e.Payload, err)
			}
			if strings.Contains(string(e.Payload), "secret text") {
				t.Fatalf("op.changed carries the payload text: %s", e.Payload)
			}
			changes = append(changes, p)
		default:
			t.Fatalf("an op row recorded %s (events %v)", e.Type, journalEventLog(ep))
		}
	}
	if len(changes) != 2 {
		t.Fatalf("op.changed recorded %d times, want 2 (proposed, closed): %v", len(changes), journalEventLog(ep))
	}
	first, last := changes[0].Op, changes[1].Op
	if first.Status != "submitted" || first.Principal != "t3:env" || first.Actor != "user" || first.ClientSeq != 3 || first.Correlation != "thread" || first.Executor != "lane" {
		t.Fatalf("first op.changed = %+v", first)
	}
	if last.Status != "closed" || last.Outcome == nil || last.Outcome.Arm != "canceled" {
		t.Fatalf("last op.changed = %+v", last)
	}
}

// A fact row is recorded as fact.recorded; a runtime row records nothing.
func TestCacheBridgeRecordsFactRecordedAndNothingForRuntimeRows(t *testing.T) {
	cache, st, ep := journalCache(t)
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	f := opjournal.Fact{Kind: opjournal.FactTurnStarted, Conversation: "gc-1", Generation: 2, Turn: "turn-1", Basis: opjournal.FactBasisRuntime, At: at}
	if _, err := st.RecordFact(f); err != nil {
		t.Fatalf("RecordFact: %v", err)
	}
	if _, err := st.RecordFact(f); err != nil {
		t.Fatalf("RecordFact again: %v", err)
	}
	rt, err := opjournal.EncodeRuntimeRow("gc", opjournal.RuntimeRow{Conversation: "gc-1", Generation: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Create(rt); err != nil {
		t.Fatalf("create runtime row: %v", err)
	}
	if got := journalEventLog(ep); len(got) != 1 || ep.Events[0].Type != events.FactRecorded {
		t.Fatalf("events = %v, want one fact.recorded", got)
	}
	e := ep.Events[0]
	var p api.FactRecordedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if e.SessionID != "gc-1" || e.Subject != p.Fact.ID || p.Fact.Kind != "turn.started" || p.Fact.Turn != "turn-1" || p.Fact.Generation != 2 || !p.Fact.At.Equal(at) || p.Fact.Basis != "runtime" {
		t.Fatalf("fact.recorded = %+v %+v", e, p)
	}
}

// DES-BX's ask: the projector's event-fact and checkpoint rows record no
// event at all, neither bead.* nor a derived one.
func TestCacheBridgeSkipsEventFactAndProjectorRows(t *testing.T) {
	cache, _, ep := journalCache(t)
	for _, label := range []string{eventfact.Label, "gc:event-projector"} {
		if _, err := cache.Create(beads.Bead{Title: label, Type: opjournal.BeadType, Labels: []string{label}}); err != nil {
			t.Fatalf("create %s row: %v", label, err)
		}
	}
	if len(ep.Events) != 0 {
		t.Fatalf("journal rows recorded %v", journalEventLog(ep))
	}
	// Any other bead still records its bead.* events.
	b, err := cache.Create(beads.Bead{Title: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if got := journalEventLog(ep); fmt.Sprint(got) != fmt.Sprint([]string{events.BeadCreated + " " + b.ID}) {
		t.Fatalf("events = %v", got)
	}
}

// The derived events carry the cache's actor; a deleted journal row records
// nothing, neither bead.deleted nor a derived event, and forgets the row, so
// a row created again with the same post-image is announced again; a closed
// op forgets its row too.
func TestCacheBridgeJournalRowActorDeleteAndForget(t *testing.T) {
	cache, st, ep := journalCache(t)
	op, err := st.Preview(opjournal.Request{
		Conversation: "gc-1", Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: "m", Producer: opjournal.Producer{Principal: "t3:env", Actor: "user"}, Payload: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := opjournal.EncodeOp(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Create(row); err != nil {
		t.Fatal(err)
	}
	if len(ep.Events) != 1 || ep.Events[0].Type != events.OpChanged || ep.Events[0].Actor != cacheLocalActor {
		t.Fatalf("events = %v (actor %q), want one op.changed by %s", journalEventLog(ep), ep.Events[0].Actor, cacheLocalActor)
	}
	if err := cache.Delete(op.ID); err != nil {
		t.Fatal(err)
	}
	if len(ep.Events) != 1 {
		t.Fatalf("deleting a journal row recorded %v", journalEventLog(ep))
	}
	if _, err := cache.Create(row); err != nil {
		t.Fatal(err)
	}
	if got := journalEventLog(ep); len(got) != 2 || ep.Events[1].Type != events.OpChanged {
		t.Fatalf("events after the row came back = %v, want a second op.changed", got)
	}
}

// A closed op's op.changed forgets its row, as a bead.closed forgets its
// bead, so closes of journal rows do not fill the dedup: a repeat of the
// closed post-image is recorded again, while a repeat of an open one is
// dropped.
func TestCacheBridgeClosedOpForgetsItsRow(t *testing.T) {
	cache, st, _ := journalCache(t)
	ep := events.NewFake()
	rec := cacheChangeRecorder(ep)
	notify := func(id string) {
		t.Helper()
		b, err := cache.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := beads.EncodeBeadEventPayload(b)
		if err != nil {
			t.Fatal(err)
		}
		rec(beads.ChangeLocal, events.BeadUpdated, id, "", "", "", nil, payload)
	}
	op, _, err := st.Propose(opjournal.Request{
		Conversation: "gc-1", Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: "c", Producer: opjournal.Producer{Principal: "t3:env", Actor: "user"}, Payload: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	notify(op.ID)
	notify(op.ID)
	if n := len(ep.Events); n != 1 {
		t.Fatalf("an open op's repeat was recorded: %v", journalEventLog(ep))
	}
	if _, _, err := st.Cancel(op.ID, "t3:env", ""); err != nil {
		t.Fatal(err)
	}
	notify(op.ID)
	notify(op.ID)
	if n := len(ep.Events); n != 3 {
		t.Fatalf("events = %v, want the closed op.changed twice (the close forgot the row)", journalEventLog(ep))
	}
}

// A recorded fact forgets its row, as a closed op does: a fact row is
// closed once recorded and never changes again, so its dedup entry would
// otherwise stay until the dedup's overflow reset. A repeat of the closed
// fact is recorded again.
func TestCacheBridgeRecordedFactForgetsItsRow(t *testing.T) {
	cache, st, _ := journalCache(t)
	ep := events.NewFake()
	rec := cacheChangeRecorder(ep)
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	fact := opjournal.Fact{Kind: opjournal.FactTurnStarted, Conversation: "gc-1", Generation: 2, Turn: "turn-1", Basis: opjournal.FactBasisRuntime, At: at}
	row, err := opjournal.EncodeFact("gc", fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordFact(fact); err != nil {
		t.Fatalf("RecordFact: %v", err)
	}
	b, err := cache.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Fatalf("fact row status = %q, want closed", b.Status)
	}
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatal(err)
	}
	rec(beads.ChangeLocal, events.BeadClosed, row.ID, "", "", "", nil, payload)
	rec(beads.ChangeLocal, events.BeadClosed, row.ID, "", "", "", nil, payload)
	if got := journalEventLog(ep); len(got) != 2 || ep.Events[0].Type != events.FactRecorded || ep.Events[1].Type != events.FactRecorded {
		t.Fatalf("events = %v, want fact.recorded twice (the recorded fact forgot its row)", got)
	}
}
