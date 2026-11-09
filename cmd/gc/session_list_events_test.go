package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionListFeed is a controller over a primed city cache whose
// notifications reach the event log as they do in production
// (cacheChangeRecorder), and a cursor that delivers the log's bead events to
// the controller's bead event watcher in order, as the bus does: the
// watcher's own records (the cache's announcements while it applies an
// event, the derived session events) land behind the cursor.
type sessionListFeed struct {
	t       *testing.T
	cs      *controllerState
	ep      *events.Fake
	backing *beads.MemStore
	cache   *beads.CachingStore
	cursor  uint64
}

func newSessionListFeed(t *testing.T, seed ...beads.Bead) *sessionListFeed {
	t.Helper()
	return newSessionListFeedOver(t, sessionListTestBacking(t, seed...))
}

// sessionListTestBacking is a store holding seed, open, under their own ids.
func sessionListTestBacking(t *testing.T, seed ...beads.Bead) *beads.MemStore {
	t.Helper()
	backing := beads.NewMemStore()
	backing.HonorExplicitIDs = true
	for _, b := range seed {
		if _, err := backing.Create(b); err != nil {
			t.Fatalf("seed %s: %v", b.ID, err)
		}
	}
	return backing
}

// newSessionListFeedOver primes the feed's cache over backing as it stands:
// the controller starts with it.
func newSessionListFeedOver(t *testing.T, backing *beads.MemStore) *sessionListFeed {
	t.Helper()
	countBeadCloseAutocloseDispatches(t)
	ep := events.NewFake()
	cache := beads.NewCachingStore(backing, cacheChangeRecorder(ep))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cs := &controllerState{
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city", Prefix: "ct"}},
		cityName:      "test-city",
		cityBeadStore: cache,
		eventProv:     ep,
		pokeCh:        make(chan struct{}, 1),
	}
	return &sessionListFeed{t: t, cs: cs, ep: ep, backing: backing, cache: cache}
}

// deliver hands every bead event recorded since the last delivery to the
// watcher.
func (f *sessionListFeed) deliver() {
	f.t.Helper()
	for {
		pending, err := f.ep.List(events.Filter{AfterSeq: f.cursor})
		if err != nil {
			f.t.Fatalf("list events: %v", err)
		}
		if len(pending) == 0 {
			return
		}
		for _, evt := range pending {
			f.cursor = evt.Seq
			switch evt.Type {
			case events.BeadCreated, events.BeadUpdated, events.BeadClosed, events.BeadDeleted:
				f.cs.applyBeadEventToStores(evt)
			}
		}
	}
}

// record appends a writer's bead event to the log, as its hook does, and
// delivers it.
func (f *sessionListFeed) record(eventType, actor string, b beads.Bead) {
	f.t.Helper()
	f.ep.Record(events.Event{Type: eventType, Actor: actor, Subject: b.ID, Payload: sessionListEventPayload(f.t, b)})
	f.deliver()
}

// redeliver appends a copy of an event already in the log, as a delayed echo
// arrives, and delivers it.
func (f *sessionListFeed) redeliver(evt events.Event) {
	f.t.Helper()
	evt.Seq = 0
	f.ep.Record(evt)
	f.deliver()
}

// lastEvent returns the latest event of eventType for id in the log.
func (f *sessionListFeed) lastEvent(eventType, id string) events.Event {
	f.t.Helper()
	got, err := f.ep.List(events.Filter{Type: eventType, Subject: id})
	if err != nil || len(got) == 0 {
		f.t.Fatalf("no %s for %s in the log (err %v)", eventType, id, err)
	}
	return got[len(got)-1]
}

// sessionEventTypes lists the session list events derived so far.
func (f *sessionListFeed) sessionEventTypes() []string {
	f.t.Helper()
	var out []string
	for _, e := range sessionListEventsOf(f.t, f.ep) {
		out = append(out, e.Type)
	}
	return out
}

// assertMatchesStore fails unless the last session list event derived for id
// agrees with the row the cache serves: session.closed for a closed row,
// session.created for an open one.
func (f *sessionListFeed) assertMatchesStore(id string) {
	f.t.Helper()
	row, err := f.cache.Get(id)
	if err != nil {
		f.t.Fatalf("Get %s: %v", id, err)
	}
	want := events.SessionCreated
	if row.Status == "closed" {
		want = events.SessionClosed
	}
	derived := sessionListEventsOf(f.t, f.ep)
	if len(derived) == 0 || derived[len(derived)-1].Type != want {
		f.t.Fatalf("session list events %v end apart from the cache's %s row, want them to end with %s", f.sessionEventTypes(), row.Status, want)
	}
}

func sessionListEventPayload(t *testing.T, b beads.Bead) json.RawMessage {
	t.Helper()
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatalf("encode %s snapshot: %v", b.ID, err)
	}
	return payload
}

func sessionListEventsOf(t *testing.T, ep *events.Fake) []events.Event {
	t.Helper()
	got, err := ep.List(events.Filter{Types: []string{events.SessionCreated, events.SessionClosed}})
	if err != nil {
		t.Fatalf("list session list events: %v", err)
	}
	return got
}

// sessionListTestBead is the session bead ct-1, open.
func sessionListTestBead() beads.Bead {
	return beads.Bead{
		ID: "ct-1", Title: "worker", Type: session.BeadType, Status: "open",
		Labels:   []string{session.LabelSession, "agent:rig/worker"},
		Metadata: map[string]string{"template": "rig/worker", "alias": "w1", "agent_name": "rig/worker-1"},
	}
}

// The controller's bead event watcher turns a session bead's create and close
// into exactly one session.created and one session.closed, with typed
// payloads, however many times the ledger repeats the mutation.
func TestBeadEventWatcherDerivesSessionListEvents(t *testing.T) {
	f := newSessionListFeed(t)
	if _, err := f.cache.Create(sessionListTestBead()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.deliver()
	// The same create inferred by a refresh, and a metadata update.
	created := f.lastEvent(events.BeadCreated, "ct-1")
	created.Actor = cacheReconcileActor
	f.redeliver(created)
	if err := f.cache.SetMetadata("ct-1", "close_reason", "drained"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := f.cache.Close("ct-1"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.deliver()
	// A double close: the cache's own, then a writer's.
	closedRow, err := f.cache.Get("ct-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	f.record(events.BeadClosed, "gc", closedRow)

	got := sessionListEventsOf(t, f.ep)
	if len(got) != 2 {
		t.Fatalf("session list events = %+v, want one session.created then one session.closed", got)
	}
	if got[0].Type != events.SessionCreated || got[1].Type != events.SessionClosed {
		t.Fatalf("session list event types = [%s %s], want [%s %s]", got[0].Type, got[1].Type, events.SessionCreated, events.SessionClosed)
	}
	for _, e := range got {
		if e.Subject != "ct-1" || e.SessionID != "ct-1" {
			t.Fatalf("%s envelope subject=%q session_id=%q, want ct-1", e.Type, e.Subject, e.SessionID)
		}
	}
	createdPayload, _, err := events.DecodePayload(events.SessionCreated, got[0].Payload)
	if err != nil {
		t.Fatalf("decode session.created: %v", err)
	}
	if want := (api.SessionCreatedPayload{SessionID: "ct-1", Template: "rig/worker", Title: "worker", Alias: "w1", AgentName: "rig/worker-1"}); createdPayload != want {
		t.Fatalf("session.created payload = %+v, want %+v", createdPayload, want)
	}
	closedPayload, _, err := events.DecodePayload(events.SessionClosed, got[1].Payload)
	if err != nil {
		t.Fatalf("decode session.closed: %v", err)
	}
	if want := (api.SessionClosedPayload{SessionID: "ct-1", Template: "rig/worker", Title: "worker", Alias: "w1", AgentName: "rig/worker-1", Reason: "drained"}); closedPayload != want {
		t.Fatalf("session.closed payload = %+v, want %+v", closedPayload, want)
	}
}

// A session open when the controller started is unknown to the fold. A
// writer outside the controller that reports its close as a bead.updated
// carrying the closed row is followed by the bead.closed the cache
// re-announces: that close is announced once.
func TestBeadEventWatcherAnnouncesWriterCloseOfSessionOpenAtBoot(t *testing.T) {
	f := newSessionListFeed(t, sessionListTestBead())
	if err := f.backing.Close("ct-1"); err != nil {
		t.Fatalf("close in the backing: %v", err)
	}
	closedRow, err := f.backing.Get("ct-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	f.record(events.BeadUpdated, "human", closedRow)

	if reannounced, err := f.ep.List(events.Filter{Type: events.BeadClosed, Subject: "ct-1"}); err != nil || len(reannounced) == 0 {
		t.Fatalf("the cache re-announced no bead.closed for the writer's closing update (err %v); the scenario is vacuous", err)
	}
	if got := f.sessionEventTypes(); len(got) != 1 || got[0] != events.SessionClosed {
		t.Fatalf("session list events = %v, want one session.closed", got)
	}
}

// A configured named session closed before the controller started and
// reopened after it (in place, by a bead.updated) joins the open set while
// clients watch: it is announced.
func TestBeadEventWatcherAnnouncesReopenOfSessionClosedBeforeBoot(t *testing.T) {
	backing := sessionListTestBacking(t, sessionListTestBead())
	if err := backing.Close("ct-1"); err != nil {
		t.Fatalf("close before boot: %v", err)
	}
	f := newSessionListFeedOver(t, backing)
	if err := f.cache.Reopen("ct-1"); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	f.deliver()

	if got := f.sessionEventTypes(); len(got) != 1 || got[0] != events.SessionCreated {
		t.Fatalf("session list events = %v, want one session.created", got)
	}
}

// Notifications the cache refuses as stale after a close (the cache's own
// create echo delivered late, a writer's snapshot taken before the close)
// announce no reopen: the session list events end as the cache's row does,
// closed.
func TestBeadEventWatcherIgnoresStaleOpenNotificationsAfterClose(t *testing.T) {
	f := newSessionListFeed(t)
	if _, err := f.cache.Create(sessionListTestBead()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.deliver()
	createdEcho := f.lastEvent(events.BeadCreated, "ct-1")
	// A writer outside the controller updates the open row; its hook's event
	// is delayed past the close.
	if err := f.backing.SetMetadata("ct-1", "alias", "w2"); err != nil {
		t.Fatalf("writer SetMetadata: %v", err)
	}
	openSnapshot, err := f.backing.Get("ct-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := f.cache.Close("ct-1"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.deliver()

	f.redeliver(createdEcho)
	f.record(events.BeadUpdated, "human", openSnapshot)

	if got := f.sessionEventTypes(); len(got) != 2 || got[0] != events.SessionCreated || got[1] != events.SessionClosed {
		t.Fatalf("session list events = %v, want [%s %s]", got, events.SessionCreated, events.SessionClosed)
	}
	f.assertMatchesStore("ct-1")
}

// A close delivered after a reopen (a writer's delayed close) announces no
// close: the session list events end as the cache's row does, open.
func TestBeadEventWatcherIgnoresStaleCloseAfterReopen(t *testing.T) {
	f := newSessionListFeed(t)
	if _, err := f.cache.Create(sessionListTestBead()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.deliver()
	if err := f.cache.Close("ct-1"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.deliver()
	staleClose := f.lastEvent(events.BeadClosed, "ct-1")
	if err := f.cache.Reopen("ct-1"); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	f.deliver()

	staleClose.Actor = "gc"
	f.redeliver(staleClose)

	want := []string{events.SessionCreated, events.SessionClosed, events.SessionCreated}
	if got := f.sessionEventTypes(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("session list events = %v, want %v", got, want)
	}
	f.assertMatchesStore("ct-1")
}

// Bead events for beads that are not sessions derive nothing.
func TestBeadEventWatcherIgnoresNonSessionBeads(t *testing.T) {
	f := newSessionListFeed(t)
	if _, err := f.cache.Create(beads.Bead{ID: "ct-2", Title: "task", Type: "task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.cache.Close("ct-2"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.deliver()
	if got := sessionListEventsOf(t, f.ep); len(got) != 0 {
		t.Fatalf("session list events for a task bead = %+v, want none", got)
	}
}
