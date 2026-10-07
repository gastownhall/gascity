package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestCacheNotificationActor(t *testing.T) {
	for _, tc := range []struct {
		source beads.ChangeSource
		want   string
	}{
		{beads.ChangeLocal, cacheLocalActor},
		{beads.ChangeScan, cacheReconcileActor},
		{beads.ChangeRefresh, cacheReconcileActor},
		{beads.ChangeSource(0), cacheReconcileActor},
	} {
		if got := cacheNotificationActor(tc.source); got != tc.want {
			t.Errorf("cacheNotificationActor(%s) = %q, want %q", tc.source, got, tc.want)
		}
	}
	for actor, want := range map[string]bool{cacheLocalActor: true, cacheReconcileActor: true, "agent": false, "": false} {
		if got := isCacheActor(actor); got != want {
			t.Errorf("isCacheActor(%q) = %v, want %v", actor, got, want)
		}
	}
}

// TestWrapWithCachingStoreStampsNotificationSource pins the wiring: a write
// this process made is cache-local, and a close inferred from a read is
// cache-reconcile, so applyBeadEventToStores can tell them apart.
func TestWrapWithCachingStoreStampsNotificationSource(t *testing.T) {
	backing := beads.NewMemStore()
	ep := events.NewFake()
	// A non-cancellable context: the cache pre-primes but starts no
	// background prime or reconcile.
	store := wrapWithCachingStore(context.Background(), backing, ep, true)
	cache, ok := store.(*beads.CachingStore)
	if !ok {
		t.Fatalf("wrapWithCachingStore returned %T, want *beads.CachingStore", store)
	}
	local, err := cache.Create(beads.Bead{Title: "local"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(local.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	if _, err := cache.RefreshRow(local.ID); err != nil {
		t.Fatalf("RefreshRow: %v", err)
	}
	want := []string{events.BeadCreated + "/" + cacheLocalActor, events.BeadClosed + "/" + cacheReconcileActor}
	if got := beadEventsFor(ep, local.ID); fmt.Sprint(got) != fmt.Sprint(want) {
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
		notify(beads.ChangeScan, eventType, id, "", "", "", nil, json.RawMessage(payload))
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
			notify(beads.ChangeScan, events.BeadUpdated, "gc-1", "", "", "", nil, p)
		}
		if len(rec.Events) != 1 {
			t.Fatalf("recorded %d copies, want the one after the drop and no repeats", len(rec.Events))
		}
	})
	t.Run("recorder that cannot acknowledge", func(t *testing.T) {
		rec := &droppingRecorder{Recorder: events.Discard, drop: 1}
		notify := cacheChangeRecorder(rec)
		for range 5 {
			notify(beads.ChangeScan, events.BeadUpdated, "gc-1", "", "", "", nil, p)
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
	clock := sweepEpoch
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
	cacheChangeRecorder(ep)(beads.ChangeScan, events.BeadUpdated, "gc-1", "", "", "", nil, p)
	cacheChangeRecorder(ep)(beads.ChangeScan, events.BeadUpdated, "gc-1", "", "", "", nil, p)
	if len(ep.Events) != 2 {
		t.Fatalf("recorded %d, want one per cache", len(ep.Events))
	}
}

// graphStepFixture creates a convoy, and a graph.v2 root with an OPEN step
// that is the convoy's only member. It returns the step and the convoy plus a
// bead.closed payload for the step with status forced to closed: the shape of
// the reconcile scan's synthetic close (mc-zndi7.43).
func graphStepFixture(t *testing.T, store beads.Store) (step, convoy beads.Bead, payload json.RawMessage) {
	t.Helper()
	convoy, err := store.Create(beads.Bead{ID: "gcg-convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.Create(beads.Bead{ID: "gcg-run", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
	}})
	if err != nil {
		t.Fatal(err)
	}
	step, err = store.Create(beads.Bead{ID: "gcg-step", ParentID: convoy.ID, Metadata: map[string]string{
		"gc.root_bead_id": root.ID, "gc.step_id": "build", "gc.session_id": "gcs-session",
	}})
	if err != nil {
		t.Fatal(err)
	}
	closed := step
	closed.Status = "closed"
	payload, err = beads.EncodeBeadEventPayload(closed)
	if err != nil {
		t.Fatal(err)
	}
	return step, convoy, payload
}

func completedFor(rec *events.Fake, id string) int {
	n := 0
	for _, evt := range rec.Events {
		if evt.Type == events.ExecutionStepCompleted && evt.Subject == id {
			n++
		}
	}
	return n
}

func statusOf(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return b.Status
}

// TestApplyBeadEventToStoresRevalidatesInferredCloses pins I”: a scan-derived
// bead.closed drives the completion fact and autoclose only when a live read
// agrees the row is closed (or gone). A local or foreign close is acted on as
// before, with no re-read: a store that reads back stale (open) does not gate
// it.
func TestApplyBeadEventToStoresRevalidatesInferredCloses(t *testing.T) {
	for _, tc := range []struct {
		name          string
		actor         string
		store         string // open, closed, deleted, unreadable
		wantFacts     int
		wantAutoclose bool
		wantConvoy    string
		wantDeferred  bool
	}{
		// The false-close probe: the scan says closed, the store says open.
		{name: "scan close of an open row", actor: cacheReconcileActor, store: "open", wantConvoy: "open"},
		{name: "scan close confirmed", actor: cacheReconcileActor, store: "closed", wantFacts: 1, wantAutoclose: true, wantConvoy: "closed"},
		// Deleted is not completed, and every autoclose would read nothing.
		{name: "scan close of a deleted row", actor: cacheReconcileActor, store: "deleted"},
		{name: "scan close unconfirmable", actor: cacheReconcileActor, store: "unreadable", wantDeferred: true},
		{name: "local close", actor: cacheLocalActor, store: "open", wantFacts: 1, wantAutoclose: true, wantConvoy: "open"},
		{name: "foreign close", actor: "bd-close", store: "open", wantFacts: 1, wantAutoclose: true, wantConvoy: "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := beadCloseAutocloseDispatch
			beadCloseAutocloseDispatch = func(fn func()) { fn() }
			t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

			mem := beads.NewMemStore()
			mem.HonorExplicitIDs = true
			step, convoy, payload := graphStepFixture(t, mem)
			var store beads.Store = mem
			switch tc.store {
			case "closed":
				if err := mem.Close(step.ID); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := mem.Delete(step.ID); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				store = &getErrStore{Store: mem, err: errors.New("backing down")}
			}
			rec := events.NewFake()
			cs := &controllerState{cityBeadStore: store, eventProv: rec}

			cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: tc.actor, Subject: step.ID, Payload: payload})

			if got := completedFor(rec, step.ID); got != tc.wantFacts {
				t.Errorf("completion facts = %d, want %d", got, tc.wantFacts)
			}
			if got := cs.autocloseSweepOf().hasRan(step.ID); got != tc.wantAutoclose {
				t.Errorf("autoclose ran = %v, want %v", got, tc.wantAutoclose)
			}
			if tc.wantConvoy != "" {
				if got := statusOf(t, mem, convoy.ID); got != tc.wantConvoy {
					t.Errorf("convoy status = %q, want %q", got, tc.wantConvoy)
				}
			}
			if got := cs.autocloseSweepOf().isPending(step.ID); got != tc.wantDeferred {
				t.Errorf("deferred to the sweep = %v, want %v", got, tc.wantDeferred)
			}
		})
	}
}

// TestApplyBeadEventToStoresDefersAnUnconfirmableClose: an inferred close
// whose row cannot be read drives nothing yet. The autoclose sweep re-reads
// it, and once the read returns closed it records the fact and runs autoclose,
// once: a later replay of the close repeats neither.
func TestApplyBeadEventToStoresDefersAnUnconfirmableClose(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	step, convoy, payload := graphStepFixture(t, mem)
	if err := mem.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	store := &downGetStore{Store: mem}
	store.down.Store(true)
	rec := events.NewFake()
	// The graph store stays readable, so only the gate keeps the fact back.
	cs := &controllerState{cityBeadStore: store, storageRoutes: splitRoutes(mem), eventProv: rec}
	scanClose := events.Event{Type: events.BeadClosed, Actor: cacheReconcileActor, Subject: step.ID, Payload: payload}

	cs.applyBeadEventToStores(scanClose)
	if got := completedFor(rec, step.ID); got != 0 {
		t.Fatalf("completion facts = %d on an unconfirmed close", got)
	}
	if cs.autocloseSweepOf().hasRan(step.ID) || !cs.autocloseSweepOf().isPending(step.ID) {
		t.Fatal("the unconfirmed close was not left to the sweep")
	}

	store.down.Store(false)
	if got := cs.runAutocloseSweepPass(time.Now().Add(time.Second)); got.Ran != 1 {
		t.Fatalf("sweep = %+v, want one confirmed close", got)
	}
	if got := completedFor(rec, step.ID); got != 1 {
		t.Fatalf("completion facts = %d after the sweep confirmed, want 1", got)
	}
	if got := statusOf(t, mem, convoy.ID); got != "closed" {
		t.Fatalf("convoy %s after the sweep, want closed", got)
	}

	cs.applyBeadEventToStores(scanClose)
	if got := completedFor(rec, step.ID); got != 1 {
		t.Fatalf("completion facts = %d after a replay, want 1", got)
	}
}

// TestApplyInferredCloseTakesTheFactFromTheLiveRead: the inferred payload is
// a stale snapshot; the completion fact carries what the confirming read
// returned.
func TestApplyInferredCloseTakesTheFactFromTheLiveRead(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	step, _, payload := graphStepFixture(t, mem)
	if err := mem.SetMetadata(step.ID, "gc.session_id", "gcs-live"); err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	cs := &controllerState{cityBeadStore: mem, eventProv: rec}
	cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: cacheReconcileActor, Subject: step.ID, Payload: payload})

	var sessions []string
	for _, evt := range rec.Events {
		if evt.Type == events.ExecutionStepCompleted {
			sessions = append(sessions, evt.SessionID)
		}
	}
	if fmt.Sprint(sessions) != "[gcs-live]" {
		t.Fatalf("completion facts carry sessions %v, want [gcs-live] from the live read", sessions)
	}
}

// deleteOnUpdateStore deletes a row right after updating it: a delete that
// lands between Update's write and its refetch.
type deleteOnUpdateStore struct{ beads.Store }

func (s deleteOnUpdateStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	return s.Delete(id)
}

// TestUpdateOfADeletedStepRecordsNoCompletion is mc-zndi7.60: Update found
// the step gone and notified bead.closed. That is an inference, recorded as
// cache-reconcile, so the live re-read finds no row and no completion fact is
// recorded for a step that was deleted, not completed.
func TestUpdateOfADeletedStepRecordsNoCompletion(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	step, _, _ := graphStepFixture(t, mem)
	rec := events.NewFake()
	cache := beads.NewCachingStore(deleteOnUpdateStore{mem}, cacheChangeRecorder(rec))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs := &controllerState{cityBeadStore: cache, eventProv: rec}
	title := "renamed"
	if err := cache.Update(step.ID, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	var closes []events.Event
	for _, evt := range rec.Events {
		if evt.Type == events.BeadClosed && evt.Subject == step.ID {
			closes = append(closes, evt)
		}
	}
	if len(closes) != 1 || closes[0].Actor != cacheReconcileActor {
		t.Fatalf("bead.closed notifications = %+v, want one under %s", closes, cacheReconcileActor)
	}
	cs.applyBeadEventToStores(closes[0])
	if got := completedFor(rec, step.ID); got != 0 {
		t.Fatalf("completion facts = %d for a deleted step", got)
	}
}

func TestConfirmInferredClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    beads.Bead
		err  error
		want closeVerdict
	}{
		{"closed", beads.Bead{Status: "closed"}, nil, closeConfirmed},
		{"gone", beads.Bead{}, fmt.Errorf("get: %w", beads.ErrNotFound), closeRefuted},
		{"open", beads.Bead{Status: "open"}, nil, closeRefuted},
		{"in progress", beads.Bead{Status: "in_progress"}, nil, closeRefuted},
		{"read failed", beads.Bead{}, errors.New("boom"), closeUnconfirmed},
	} {
		if got := confirmInferredClose(tc.b, tc.err); got != tc.want {
			t.Errorf("%s: confirmInferredClose = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCacheNotificationStress drives the dedup, the inferred-close path and
// the sweep from many goroutines at once, for -race.
func TestCacheNotificationStress(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	mem := beads.NewMemStore()
	cached := beads.NewCachingStoreForTest(mem, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 16)
	for i := range ids {
		b, err := mem.Create(beads.Bead{Title: fmt.Sprintf("b%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = b.ID
	}
	ep := events.NewFake()
	cs := &controllerState{beadStores: map[string]beads.Store{"r": cached}, eventProv: ep, pokeCh: make(chan struct{}, 1)}
	notify := cacheChangeRecorder(ep)

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i, id := range ids {
				payload, _ := beads.EncodeBeadEventPayload(beads.Bead{ID: id, Status: "closed"})
				notify(beads.ChangeScan, events.BeadUpdated, id, "", "", "", nil, payload)
				if (i+g)%3 == 0 {
					_ = mem.Close(id)
				}
				cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: cacheReconcileActor, Subject: id, Payload: payload})
				cs.runAutocloseSweepPass(sweepTestClock(i))
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		b, err := mem.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "closed" && cs.autocloseSweepOf().hasRan(id) {
			t.Errorf("autoclose ran for %s, which the store holds open", id)
		}
	}
}
