package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/ledgerfeed"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/session"
	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/journalops"
)

// scriptedJournal is an in-memory durable mutation journal.
type scriptedJournal struct {
	mu     sync.Mutex
	rows   []journalops.Row
	head   int64
	floor  int64 // lowest retained seq; 0 retains everything
	active atomic.Bool
	// failWith, while set, fails every read.
	failWith error
	reads    atomic.Int64
	// onRead runs before each read is served.
	onRead func()
}

func newScriptedJournal() *scriptedJournal {
	j := &scriptedJournal{}
	j.active.Store(true)
	return j
}

func (j *scriptedJournal) add(op, id, actor, issueJSON string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.head++
	j.rows = append(j.rows, journalops.Row{
		Seq: j.head, Op: op, IssueID: id, Actor: actor, IssueJSON: issueJSON,
		TS: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// truncate prunes every row below floor.
func (j *scriptedJournal) truncate(floor int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.floor = floor
	j.rows = slices.DeleteFunc(j.rows, func(r journalops.Row) bool { return r.Seq < floor })
}

func (j *scriptedJournal) setFail(err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.failWith = err
}

func (j *scriptedJournal) ReadEventsJournalPage(_ context.Context, since int64, limit int) (journalops.Page, error) {
	j.reads.Add(1)
	if j.onRead != nil {
		j.onRead()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failWith != nil {
		return journalops.Page{}, j.failWith
	}
	if !j.active.Load() {
		return journalops.Page{}, beads.ErrEventsJournalInactive
	}
	if j.floor > 1 && since < j.floor-1 && since < j.head {
		return journalops.Page{}, &journalops.TruncatedError{Since: since, Floor: j.floor, Head: j.head}
	}
	page := journalops.Page{Head: j.head}
	for _, r := range j.rows {
		if r.Seq <= since {
			continue
		}
		if limit > 0 && len(page.Rows) >= limit {
			break
		}
		page.Rows = append(page.Rows, r)
	}
	return page, nil
}

// journalingStore journals every write it takes, as a journaling bd does,
// and reports its journal active while the journal is.
type journalingStore struct {
	beads.Store
	j *scriptedJournal
	// lists counts List calls; listed records the journal reads made before
	// the first one.
	lists        atomic.Int64
	readsAtFirst atomic.Int64
	// afterCreate runs after a create is journaled, before it returns.
	afterCreate func(id string)
	// afterWrite runs after an update, metadata write or close is
	// journaled, before it returns.
	afterWrite func(id string)
	// getHook runs before each Get.
	getHook func(id string) error
}

func newJournalingStore() *journalingStore {
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	return &journalingStore{Store: mem, j: newScriptedJournal()}
}

func (s *journalingStore) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	return s.j.ReadEventsJournalPage(ctx, since, limit)
}

func (s *journalingStore) EventsJournalActive() bool { return s.j.active.Load() }

func (s *journalingStore) image(id string) string {
	b, err := s.Store.Get(id)
	if err != nil {
		return ""
	}
	data, _ := json.Marshal(b)
	return string(data)
}

func (s *journalingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.lists.Add(1) == 1 {
		s.readsAtFirst.Store(s.j.reads.Load())
	}
	return s.Store.List(q)
}

func (s *journalingStore) Get(id string) (beads.Bead, error) {
	if s.getHook != nil {
		if err := s.getHook(id); err != nil {
			return beads.Bead{}, err
		}
	}
	return s.Store.Get(id)
}

func (s *journalingStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err == nil {
		s.j.add(journalOpCreate, created.ID, "gascity", s.image(created.ID))
		if s.afterCreate != nil {
			s.afterCreate(created.ID)
		}
	}
	return created, err
}

func (s *journalingStore) Update(id string, opts beads.UpdateOpts) error {
	err := s.Store.Update(id, opts)
	if err == nil {
		s.j.add("update", id, "gascity", s.image(id))
		s.wrote(id)
	}
	return err
}

func (s *journalingStore) wrote(id string) {
	if s.afterWrite != nil {
		s.afterWrite(id)
	}
}

func (s *journalingStore) Close(id string) error {
	err := s.Store.Close(id)
	if err == nil {
		s.j.add(journalOpClose, id, "gascity", s.image(id))
		s.wrote(id)
	}
	return err
}

func (s *journalingStore) SetMetadata(id, key, value string) error {
	err := s.Store.SetMetadata(id, key, value)
	if err == nil {
		s.j.add("update", id, "gascity", s.image(id))
		s.wrote(id)
	}
	return err
}

func (s *journalingStore) Delete(id string) error {
	err := s.Store.Delete(id)
	if err == nil {
		s.j.add("delete", id, "gascity", "")
	}
	return err
}

// feedHarness is one feed over one cache, stepped by hand.
type feedHarness struct {
	t       *testing.T
	ctx     context.Context
	store   *journalingStore
	cache   *beads.CachingStore
	feed    *ledgerFeed
	set     *ledgerFeedSet
	ep      *events.Fake
	pokeCh  chan struct{}
	ctrlCh  chan struct{}
	work    atomic.Int64
	maxWork atomic.Int64
	// deferred records the ids handed to the autoclose sweep.
	deferredMu sync.Mutex
	deferred   []string
}

func newFeedHarness(t *testing.T, store *journalingStore) *feedHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &feedHarness{t: t, ctx: ctx, store: store, ep: events.NewFake(), pokeCh: make(chan struct{}, 1), ctrlCh: make(chan struct{}, 1)}
	h.set = newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{
		work: func() func() {
			n := h.work.Add(1)
			for {
				m := h.maxWork.Load()
				if n <= m || h.maxWork.CompareAndSwap(m, n) {
					break
				}
			}
			var once sync.Once
			return func() { once.Do(func() { h.work.Add(-1) }) }
		},
		deferClose: func(id string) {
			h.deferredMu.Lock()
			defer h.deferredMu.Unlock()
			h.deferred = append(h.deferred, id)
		},
	})
	h.set.setWake(newLegacyWake(h.pokeCh, h.ctrlCh))
	req := &ledgerFeedRequest{set: h.set, scope: ledgerFeedCityScope, mode: config.BeadsLedgerFeedAuto}
	att := attachLedgerFeed(ctx, req, store, true)
	if att == nil || att.entry.feed == nil {
		t.Fatalf("no feed attached: %+v", att)
	}
	h.cache = beads.NewCachingStore(store, att.observe(cacheChangeRecorder(h.ep)))
	att.bind(ctx, h.set, h.cache)
	h.feed = att.entry.feed
	if err := h.cache.Prime(ctx); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return h
}

// step runs tailer steps until the journal is caught up.
func (h *feedHarness) step() {
	h.t.Helper()
	for range 10 {
		more, err := h.feed.tailer.Step(h.ctx)
		if err != nil {
			h.t.Fatalf("Step: %v", err)
		}
		if !more {
			return
		}
	}
	h.t.Fatal("the feed did not catch up in 10 steps")
}

// pokes drains and counts the legacy wake's two signals.
func (h *feedHarness) pokes() (poke, dispatch int) {
	for {
		select {
		case <-h.pokeCh:
			poke++
		case <-h.ctrlCh:
			dispatch++
		default:
			return poke, dispatch
		}
	}
}

// eventsFor lists type/actor of the recorded events for id.
func (h *feedHarness) eventsFor(id string) []string {
	return beadEventsFor(h.ep, id)
}

func (h *feedHarness) reconcileEvents() int {
	n := 0
	for _, evt := range h.ep.Events {
		if evt.Actor == cacheReconcileActor {
			n++
		}
	}
	return n
}

func TestPlanLedgerFeedPage(t *testing.T) {
	rows := []journalops.Row{
		{Seq: 1, Op: "update", IssueID: "gc-2", Actor: "a", IssueJSON: `{"status":"closed"}`},
		{Seq: 2, Op: journalOpComment, IssueID: "gc-9", Actor: "a"},
		{Seq: 3, Op: journalOpCreate, IssueID: "gc-1", Actor: "b"},
		{Seq: 4, Op: "update", IssueID: "gc-2", Actor: "b", IssueJSON: `{"status":"closed"}`},
		{Seq: 5, Op: "update", IssueID: "gc-3", IssueJSON: `{"status":"in_progress"}`},
		{Seq: 6, Op: journalOpClose, IssueID: "gc-4"},
		{Seq: 7, Op: "dep_add", IssueID: ""},
		{Seq: 8, Op: "update", IssueID: "gc-5", IssueJSON: `not json`},
	}
	plan := planLedgerFeedPage(rows)
	if want := []string{"gc-2", "gc-1", "gc-3", "gc-4", "gc-5"}; !slices.Equal(plan.ids, want) {
		t.Fatalf("ids = %v, want %v (comments and empty ids skipped, first-appearance order)", plan.ids, want)
	}
	for id, want := range map[string]bool{"gc-1": true, "gc-2": false, "gc-3": true, "gc-4": true, "gc-5": false} {
		if plan.closing[id] != want {
			t.Errorf("closing[%s] = %v, want %v", id, plan.closing[id], want)
		}
	}
	if plan.actors["a"] != 2 || plan.actors["b"] != 2 || plan.actors[""] != 4 {
		t.Fatalf("actors = %v", plan.actors)
	}
	var many []journalops.Row
	for i := range ledgerFeedActorCap + 5 {
		many = append(many, journalops.Row{Seq: int64(i + 1), IssueID: "gc-1", Actor: fmt.Sprintf("actor-%d", i)})
	}
	capped := planLedgerFeedPage(many).actors
	if len(capped) != ledgerFeedActorCap+1 || capped[ledgerFeedActorOther] != 5 {
		t.Fatalf("capped actors = %v, want %d named plus other=5", capped, ledgerFeedActorCap)
	}
}

func TestLedgerFeedWakeKeys(t *testing.T) {
	plain := beads.Bead{ID: "gc-1"}
	sess := beads.Bead{ID: "gc-s", Type: session.BeadType}
	graph := beads.Bead{ID: "gc-g", Metadata: map[string]string{"gc.kind": "workflow"}}
	step := beads.Bead{ID: "gc-t", Metadata: map[string]string{"gc.root_bead_id": "gc-g"}}

	if got := ledgerFeedWakeKeys([]beads.Bead{plain}); !slices.Equal(got, []reconcilekey.Key{reconcilekey.Allocator()}) {
		t.Fatalf("plain row keys = %v, want the allocator only", got)
	}
	got := ledgerFeedWakeKeys([]beads.Bead{sess, sess, plain})
	if want := []reconcilekey.Key{reconcilekey.Allocator(), reconcilekey.Session("gc-s")}; !slices.Equal(got, want) {
		t.Fatalf("session row keys = %v, want %v", got, want)
	}
	for _, b := range []beads.Bead{graph, step} {
		got := ledgerFeedWakeKeys([]beads.Bead{b})
		if want := []reconcilekey.Key{reconcilekey.Allocator(), reconcilekey.ControlDispatch()}; !slices.Equal(got, want) {
			t.Fatalf("graph row %s keys = %v, want %v", b.ID, got, want)
		}
	}
	var sessions []beads.Bead
	for i := range ledgerFeedWakeSessionCap + 10 {
		sessions = append(sessions, beads.Bead{ID: fmt.Sprintf("gc-s%d", i), Type: session.BeadType})
	}
	keys := ledgerFeedWakeKeys(sessions)
	if len(keys) != ledgerFeedWakeSessionCap+1 || keys[0] != reconcilekey.Allocator() {
		t.Fatalf("capped keys = %d (first %v), want the allocator plus %d sessions", len(keys), keys[0], ledgerFeedWakeSessionCap)
	}
}

// An out-of-band write reaches the cache within one step, as a
// cache-reconcile event, and wakes the legacy reconciler; a graph row also
// wakes the control dispatcher.
func TestLedgerFeedRefreshesAnOutOfBandWrite(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	h.pokes()

	row, err := h.store.Create(beads.Bead{Title: "out of band"})
	if err != nil {
		t.Fatal(err)
	}
	h.step()
	if got, err := h.cache.Get(row.ID); err != nil || got.ID != row.ID {
		t.Fatalf("cache Get = %v, %v; want the row", got.ID, err)
	}
	if want := []string{events.BeadCreated + "/" + cacheReconcileActor}; !slices.Equal(h.eventsFor(row.ID), want) {
		t.Fatalf("events = %v, want %v", h.eventsFor(row.ID), want)
	}
	if poke, dispatch := h.pokes(); poke != 1 || dispatch != 0 {
		t.Fatalf("pokes = %d/%d, want one allocator poke", poke, dispatch)
	}

	graph, err := h.store.Create(beads.Bead{Title: "step", Metadata: map[string]string{"gc.root_bead_id": "gc-root"}})
	if err != nil {
		t.Fatal(err)
	}
	h.step()
	if poke, dispatch := h.pokes(); poke != 1 || dispatch != 1 {
		t.Fatalf("graph row pokes = %d/%d, want the allocator and the control dispatcher", poke, dispatch)
	}
	if len(h.eventsFor(graph.ID)) != 1 {
		t.Fatalf("graph row events = %v", h.eventsFor(graph.ID))
	}
	st := h.feed.status(time.Now())
	if st.Wakes != 2 || st.WakesLastMinute != 2 || st.Refreshed != 2 || st.State != ledgerFeedStateTailing {
		t.Fatalf("status = %+v", st)
	}
}

// Writes through the cache come back as journal rows whose refresh finds
// nothing new: no event and no wake (ga-yoix1).
func TestLedgerFeedEchoesOfOwnWritesWakeNothing(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	for i := range 5 {
		b, err := h.cache.Create(beads.Bead{Title: fmt.Sprintf("local %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.cache.SetMetadata(b.ID, "k", "v"); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if err := h.cache.Close(b.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.pokes()
	before := len(h.ep.Events)
	h.step()
	h.step()
	if poke, dispatch := h.pokes(); poke+dispatch != 0 {
		t.Fatalf("echo steps woke %d/%d, want nothing", poke, dispatch)
	}
	if n := len(h.ep.Events) - before; n != 0 || h.reconcileEvents() != 0 {
		t.Fatalf("echo steps recorded %d events (%d cache-reconcile), want none", n, h.reconcileEvents())
	}
}

// The cache's own close, evicted by a re-scan before the feed reads its
// journal row, is not announced again, woken on, or deferred to the sweep.
func TestLedgerFeedEchoOfAnEvictedOwnCloseIsSilent(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	h.step()
	b, err := h.cache.Create(beads.Bead{Title: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cache.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	h.cache.ReconcileNowForTest()
	h.pokes()
	before := len(h.ep.Events)
	h.step()
	if poke, dispatch := h.pokes(); poke+dispatch != 0 || len(h.ep.Events) != before || len(h.deferred) != 0 {
		t.Fatalf("the echo of an evicted own close recorded %v, woke %d/%d and deferred %v; want nothing",
			h.ep.Events[before:], poke, dispatch, h.deferred)
	}
}

// A feed step that lands between a cache write's backing commit and its
// absorb yields to the write: no event, no wake, and a retry once the write
// returned finds nothing new (ga-yoix1).
func TestLedgerFeedYieldsToAnInFlightOwnWrite(t *testing.T) {
	store := newJournalingStore()
	h := newFeedHarness(t, store)
	b, err := h.cache.Create(beads.Bead{Title: "local"})
	if err != nil {
		t.Fatal(err)
	}
	h.step()
	h.pokes()
	before := len(h.ep.Events)
	store.afterWrite = func(string) { h.step() }
	if err := h.cache.SetMetadata(b.ID, "k", "v"); err != nil {
		t.Fatal(err)
	}
	title := "renamed"
	if err := h.cache.Update(b.ID, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := h.cache.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	store.afterWrite = nil
	if st := h.feed.status(time.Now()); st.Fenced == 0 || st.PendingRetries == 0 {
		t.Fatalf("status fenced=%d pending=%d, want the in-flight refreshes fenced and kept for a retry", st.Fenced, st.PendingRetries)
	}
	h.feed.retryPending()
	h.step()
	if poke, dispatch := h.pokes(); poke+dispatch != 0 {
		t.Fatalf("own writes raced by the feed woke %d/%d, want nothing", poke, dispatch)
	}
	if got := h.ep.Events[before:]; h.reconcileEvents() != 0 {
		t.Fatalf("own writes raced by the feed recorded %v, want no cache-reconcile event", got)
	}
	if st := h.feed.status(time.Now()); st.PendingRetries != 0 {
		t.Fatalf("pending retries = %d after the retry, want 0", st.PendingRetries)
	}
}

// A refresh that lands between the backing create and the cache's absorb
// yields to the create: one created event, the create's own, and no wake.
func TestLedgerFeedRaceWithALocalCreateRecordsOneCreate(t *testing.T) {
	store := newJournalingStore()
	h := newFeedHarness(t, store)
	store.afterCreate = func(string) {
		if _, err := h.feed.tailer.Step(h.ctx); err != nil {
			t.Errorf("Step inside the create: %v", err)
		}
	}
	h.pokes()
	var ids []string
	for i := range 3 {
		b, err := h.cache.Create(beads.Bead{Title: fmt.Sprintf("raced %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}
	store.afterCreate = nil
	h.feed.retryPending()
	if poke, _ := h.pokes(); poke != 0 || h.reconcileEvents() != 0 {
		t.Fatalf("raced creates poked %d times and recorded %d cache-reconcile events, want none", poke, h.reconcileEvents())
	}
	for _, id := range ids {
		created := 0
		for _, e := range h.eventsFor(id) {
			if strings.HasPrefix(e, events.BeadCreated+"/") {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("%s: events %v, want exactly one bead.created", id, h.eventsFor(id))
		}
	}
	before := len(h.ep.Events)
	h.step()
	if poke, _ := h.pokes(); poke != 0 || len(h.ep.Events) != before {
		t.Fatalf("after the race the feed woke %d and recorded %d events, want quiet", poke, len(h.ep.Events)-before)
	}
}

// Under v2 the feed never enqueues; the planner learns of the change from
// the recorded event.
func TestLedgerFeedNeverEnqueuesUnderV2(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	p := newTestPlanner()
	h.set.setWake(&controllerWake{planner: p})
	row, err := h.store.Create(beads.Bead{Title: "out of band"})
	if err != nil {
		t.Fatal(err)
	}
	h.step()
	if got := plannerWakes(p, wakeReasonLedgerFeed); got != 0 {
		t.Fatalf("planner woken %d times by the feed, want 0", got)
	}
	if len(h.eventsFor(row.ID)) != 1 {
		t.Fatalf("events = %v, want the refresh's created event", h.eventsFor(row.ID))
	}
	if st := h.feed.status(time.Now()); st.Wakes != 0 {
		t.Fatalf("status wakes = %d, want 0", st.Wakes)
	}
}

// A row created and closed inside one page is announced once, as a close,
// handed to the autoclose sweep, and yields one completion fact through the
// live-confirmed close path. An update to a long-closed row announces nothing.
func TestLedgerFeedAnnouncesANeverHeldCloseOnce(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	store := newJournalingStore()
	root, err := store.Create(beads.Bead{ID: "gcg-run", Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}})
	if err != nil {
		t.Fatal(err)
	}
	h := newFeedHarness(t, store)
	h.step()

	step, err := store.Create(beads.Bead{ID: "gcg-step", Metadata: map[string]string{"gc.root_bead_id": root.ID, "gc.step_id": "build", "gc.session_id": "gcs-session"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	h.pokes()
	h.step()
	if want := []string{events.BeadClosed + "/" + cacheReconcileActor}; !slices.Equal(h.eventsFor(step.ID), want) {
		t.Fatalf("events = %v, want %v", h.eventsFor(step.ID), want)
	}
	if !slices.Equal(h.deferred, []string{step.ID}) {
		t.Fatalf("deferred to the sweep = %v, want [%s]", h.deferred, step.ID)
	}
	if poke, dispatch := h.pokes(); poke != 1 || dispatch != 1 {
		t.Fatalf("pokes = %d/%d, want the allocator and the control dispatcher", poke, dispatch)
	}
	if st := h.feed.status(time.Now()); st.UnheldCloses != 1 {
		t.Fatalf("unheld closes = %d, want 1", st.UnheldCloses)
	}

	rec := events.NewFake()
	cs := &controllerState{cityBeadStore: h.cache, eventProv: rec}
	for _, evt := range h.ep.Events {
		if evt.Subject == step.ID {
			cs.applyBeadEventToStores(evt)
		}
	}
	if got := completedFor(rec, step.ID); got != 1 {
		t.Fatalf("completion facts = %d, want 1", got)
	}

	// A long-closed row the cache does not hold: an update shows no
	// evidence of a recent close, so it is installed silently.
	old, err := store.Store.Create(beads.Bead{Title: "long closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Store.Close(old.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(old.ID, "note", "x"); err != nil {
		t.Fatal(err)
	}
	h.step()
	if got := h.eventsFor(old.ID); len(got) != 0 {
		t.Fatalf("update of a long-closed row recorded %v, want nothing", got)
	}
}

// closedOutOfBand creates and closes a row behind the cache's back, both
// journaled.
func closedOutOfBand(t *testing.T, store *journalingStore) string {
	t.Helper()
	row, err := store.Create(beads.Bead{Title: "agent work"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(row.ID); err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// countFeedEvents counts the events whose type/actor is want.
func countFeedEvents(got []string, want string) int {
	n := 0
	for _, e := range got {
		if e == want {
			n++
		}
	}
	return n
}

// A never-held close whose refresh the feed had to drop (a read error, a
// fence, a cache that is not live) is handed to the autoclose sweep at once
// and kept for a retry, which announces it once (critic A3): the re-scan
// lists active rows only and would never see it.
func TestLedgerFeedKeepsADroppedNeverHeldClose(t *testing.T) {
	closedEvent := events.BeadClosed + "/" + cacheReconcileActor
	t.Run("refresh_error", func(t *testing.T) {
		h := newFeedHarness(t, newJournalingStore())
		h.step()
		id := closedOutOfBand(t, h.store)
		var fails atomic.Int32
		fails.Store(1)
		h.store.getHook = func(got string) error {
			if got == id && fails.Add(-1) >= 0 {
				return errors.New("transient read failure")
			}
			return nil
		}
		h.step()
		if !slices.Contains(h.deferred, id) || len(h.eventsFor(id)) != 0 {
			t.Fatalf("after the dropped refresh: deferred %v events %v, want the id deferred and no event yet", h.deferred, h.eventsFor(id))
		}
		if st := h.feed.status(time.Now()); st.PendingRetries != 1 || st.RefreshErrors != 1 {
			t.Fatalf("status pending=%d errors=%d, want one row kept for a retry", st.PendingRetries, st.RefreshErrors)
		}
		h.cache.ReconcileNowForTest() // the requested re-scan: active rows only
		h.feed.retryPending()
		if got := h.eventsFor(id); countFeedEvents(got, closedEvent) != 1 {
			t.Fatalf("events = %v, want one %s from the retry", got, closedEvent)
		}
		if st := h.feed.status(time.Now()); st.PendingRetries != 0 || st.UnheldCloses != 1 {
			t.Fatalf("status pending=%d unheld=%d, want the retry done and the close counted", st.PendingRetries, st.UnheldCloses)
		}
	})
	t.Run("fenced", func(t *testing.T) {
		store := newJournalingStore()
		h := newFeedHarness(t, store)
		h.step()
		id := closedOutOfBand(t, store)
		// A local create in flight fences every yielding refresh.
		store.afterCreate = func(string) { h.step() }
		if _, err := h.cache.Create(beads.Bead{Title: "local"}); err != nil {
			t.Fatal(err)
		}
		store.afterCreate = nil
		if !slices.Contains(h.deferred, id) || len(h.eventsFor(id)) != 0 {
			t.Fatalf("after the fenced refresh: deferred %v events %v, want the id deferred and no event yet", h.deferred, h.eventsFor(id))
		}
		h.feed.retryPending()
		if got := h.eventsFor(id); countFeedEvents(got, closedEvent) != 1 {
			t.Fatalf("events = %v, want one %s from the retry", got, closedEvent)
		}
	})
	t.Run("cache_unavailable", func(t *testing.T) {
		store := newJournalingStore()
		var deferred []string
		set := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{deferClose: func(id string) { deferred = append(deferred, id) }})
		req := &ledgerFeedRequest{set: set, scope: ledgerFeedCityScope, mode: config.BeadsLedgerFeedAuto}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ep := events.NewFake()
		att := attachLedgerFeed(ctx, req, store, true)
		cache := beads.NewCachingStore(store, att.observe(cacheChangeRecorder(ep)))
		att.bind(ctx, set, cache)
		f := att.entry.feed
		id := closedOutOfBand(t, store)
		if _, err := f.tailer.Step(ctx); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(deferred, []string{id}) || len(beadEventsFor(ep, id)) != 0 {
			t.Fatalf("cold cache: deferred %v events %v, want the id deferred and no event yet", deferred, beadEventsFor(ep, id))
		}
		if err := cache.Prime(ctx); err != nil {
			t.Fatal(err)
		}
		f.retryPending()
		if got := beadEventsFor(ep, id); countFeedEvents(got, closedEvent) != 1 {
			t.Fatalf("events = %v, want one %s from the retry", got, closedEvent)
		}
	})
	t.Run("gives_up", func(t *testing.T) {
		h := newFeedHarness(t, newJournalingStore())
		h.step()
		id := closedOutOfBand(t, h.store)
		h.store.getHook = func(got string) error {
			if got == id {
				return errors.New("unreadable")
			}
			return nil
		}
		h.step()
		requests := h.cache.Stats().ReconcileRequests
		for range ledgerFeedRetryLimit {
			h.feed.retryPending()
		}
		st := h.feed.status(time.Now())
		if st.PendingRetries != 0 || st.RetryGiveUps != 1 || h.cache.Stats().ReconcileRequests <= requests {
			t.Fatalf("status pending=%d give-ups=%d requests %d->%d, want the row given up to a re-scan",
				st.PendingRetries, st.RetryGiveUps, requests, h.cache.Stats().ReconcileRequests)
		}
		if !slices.Contains(h.deferred, id) {
			t.Fatalf("deferred = %v, want the given-up close with the sweep", h.deferred)
		}
	})
}

// A truncation gap requests one re-scan, is counted, and the feed continues
// from the head.
func TestLedgerFeedGapRequestsARescan(t *testing.T) {
	store := newJournalingStore()
	h := newFeedHarness(t, store)
	for i := range 3 {
		if _, err := store.Store.Create(beads.Bead{Title: fmt.Sprintf("pruned %d", i)}); err != nil {
			t.Fatal(err)
		}
		store.j.add("update", fmt.Sprintf("gc-x%d", i), "", "")
	}
	store.j.truncate(3)
	if _, err := h.feed.tailer.Step(h.ctx); err != nil {
		t.Fatalf("Step over the gap: %v", err)
	}
	if got := h.cache.Stats().ReconcileRequests; got != 1 {
		t.Fatalf("ReconcileRequests = %d, want 1", got)
	}
	st := h.feed.status(time.Now())
	if st.Gaps[string(ledgerfeed.GapTruncated)] != 1 || st.Seq != 3 {
		t.Fatalf("status gaps=%v seq=%d, want one truncated gap and the checkpoint at the head", st.Gaps, st.Seq)
	}
	row, err := store.Create(beads.Bead{Title: "after the gap"})
	if err != nil {
		t.Fatal(err)
	}
	h.step()
	if _, err := h.cache.Get(row.ID); err != nil {
		t.Fatalf("the feed did not continue after the gap: %v", err)
	}
}

// The baseline head read comes before the cache's first List, and a failed
// baseline makes the feed late: its run re-baselines and forces a re-scan.
func TestWrapWithCachingStoreFeedBaselinesBeforePrime(t *testing.T) {
	store := newJournalingStore()
	if _, err := store.Create(beads.Bead{Title: "before"}); err != nil {
		t.Fatal(err)
	}
	set := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := &ledgerFeedRequest{set: set, scope: ledgerFeedCityScope, mode: config.BeadsLedgerFeedAuto}
	wrapped := wrapWithCachingStoreFeed(ctx, store, events.NewFake(), true, req)
	cache := wrapped.(*beads.CachingStore)
	defer cache.StopReconciler()
	if store.readsAtFirst.Load() == 0 {
		t.Fatal("the cache listed the backing before the feed read the journal head")
	}
	_, entries := set.snapshot()
	if len(entries) != 1 || entries[0].feed == nil || entries[0].feed.late {
		t.Fatalf("entries = %+v, want one feed with its baseline", entries)
	}
	if got := entries[0].feed.tailer.Checkpoint().Seq; got != 1 {
		t.Fatalf("baseline seq = %d, want the head (1)", got)
	}
}

func TestLedgerFeedLateBaselineForcesARescan(t *testing.T) {
	store := newJournalingStore()
	store.j.setFail(errors.New("journal down"))
	set := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := &ledgerFeedRequest{set: set, scope: ledgerFeedCityScope, mode: config.BeadsLedgerFeedAuto}
	cache := wrapWithCachingStoreFeed(ctx, store, events.NewFake(), true, req).(*beads.CachingStore)
	defer cache.StopReconciler()
	_, entries := set.snapshot()
	f := entries[0].feed
	if !f.late {
		t.Fatal("a failed baseline did not leave the feed late")
	}
	store.j.setFail(nil)
	waitForLedgerFeed(t, 3*time.Second, func() bool { return cache.Stats().ReconcileRequests >= 1 }, "the late baseline to request a re-scan")
	waitForLedgerFeed(t, 3*time.Second, func() bool { return f.status(time.Now()).State == ledgerFeedStateTailing }, "the late feed to tail")
}

// A closed gate keeps every journal read away, and Apply runs as store work.
func TestLedgerFeedHonorsTheGateAndCountsStoreWork(t *testing.T) {
	store := newJournalingStore()
	var open atomic.Bool
	set := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{})
	req := &ledgerFeedRequest{set: set, scope: ledgerFeedRigScope("r"), mode: config.BeadsLedgerFeedAuto, admit: open.Load}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	att := attachLedgerFeed(ctx, req, store, true)
	cache := beads.NewCachingStore(store, att.observe(cacheChangeRecorder(events.NewFake())))
	att.bind(ctx, set, cache)
	f := att.entry.feed
	if got := store.j.reads.Load(); got != 0 || !f.late {
		t.Fatalf("closed gate: %d journal read(s), late=%v; want none and a late baseline", got, f.late)
	}
	if _, err := f.tailer.Step(ctx); !errors.Is(err, errLedgerFeedGated) {
		t.Fatalf("gated Step err = %v, want errLedgerFeedGated", err)
	}
	if got := store.j.reads.Load(); got != 0 {
		t.Fatalf("gated Step read the journal %d time(s)", got)
	}

	h := newFeedHarness(t, newJournalingStore())
	h.store.getHook = func(string) error {
		if h.work.Load() == 0 {
			t.Error("a refresh read ran outside store work")
		}
		return nil
	}
	if _, err := h.store.Create(beads.Bead{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	h.step()
	if h.maxWork.Load() == 0 {
		t.Fatal("no store work was counted")
	}
}

func TestLedgerFeedOnError(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	for _, tc := range []struct {
		err   error
		stop  bool
		state string
	}{
		{beads.ErrEventsJournalUnsupported, true, ""},
		// A proxied store's journal read can surface its replaced leaf's
		// ErrStoreClosed while the store itself is open; the feed ends with
		// its cache (StopReconciler), never on this error.
		{beads.ErrStoreClosed, false, ledgerFeedStateRetrying},
		{beads.ErrEventsJournalInactive, false, ledgerFeedStateInactive},
		{beads.ErrEventsJournalUnavailable, false, ledgerFeedStateUnavailable},
		{fmt.Errorf("applying: %w", errLedgerFeedGated), false, ledgerFeedStateGated},
		{errors.New("transient"), false, ledgerFeedStateRetrying},
	} {
		stop := h.feed.onError(fmt.Errorf("wrapped: %w", tc.err))
		if (stop != nil) != tc.stop {
			t.Fatalf("onError(%v) stop = %v, want %v", tc.err, stop, tc.stop)
		}
		if !tc.stop && h.feed.status(time.Now()).State != tc.state {
			t.Fatalf("onError(%v) state = %q, want %q", tc.err, h.feed.status(time.Now()).State, tc.state)
		}
	}
}

// An inactive journal owes a re-scan at the first good read after it.
func TestLedgerFeedInactiveJournalResyncsOnRecovery(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	h.store.j.active.Store(false)
	_, err := h.feed.tailer.Step(h.ctx)
	if !errors.Is(err, beads.ErrEventsJournalInactive) {
		t.Fatalf("Step err = %v, want inactive", err)
	}
	if stop := h.feed.onError(err); stop != nil {
		t.Fatalf("inactive stopped the feed: %v", stop)
	}
	if got := h.cache.Stats().ReconcileRequests; got != 0 {
		t.Fatalf("ReconcileRequests while inactive = %d, want 0", got)
	}
	h.store.j.active.Store(true)
	h.step()
	if got := h.cache.Stats().ReconcileRequests; got != 1 {
		t.Fatalf("ReconcileRequests after recovery = %d, want 1", got)
	}
	if st := h.feed.status(time.Now()).State; st != ledgerFeedStateTailing {
		t.Fatalf("state after recovery = %q, want tailing", st)
	}
}

// A row the cache cannot refresh is dropped and covered by a re-scan; the
// page still advances.
func TestLedgerFeedDropsAnUnrefreshableRowAndRequestsARescan(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	h.store.getHook = func(string) error { return errors.New("read failed") }
	unreadable, err := h.store.Store.Create(beads.Bead{Title: "unreadable"})
	if err != nil {
		t.Fatal(err)
	}
	h.store.j.add(journalOpCreate, unreadable.ID, "", "")
	h.step()
	st := h.feed.status(time.Now())
	if st.RefreshErrors != 1 || st.Seq != 1 || h.cache.Stats().ReconcileRequests != 1 {
		t.Fatalf("status = %+v, requests=%d; want one dropped row, the page applied, one re-scan request", st, h.cache.Stats().ReconcileRequests)
	}

	cold := newJournalingStore()
	set := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{})
	req := &ledgerFeedRequest{set: set, scope: ledgerFeedCityScope, mode: config.BeadsLedgerFeedAuto}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	att := attachLedgerFeed(ctx, req, cold, true)
	cache := beads.NewCachingStore(cold, att.observe(cacheChangeRecorder(nil)))
	att.bind(ctx, set, cache)
	if _, err := cold.Create(beads.Bead{Title: "cold"}); err != nil {
		t.Fatal(err)
	}
	if _, err := att.entry.feed.tailer.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if st := att.entry.feed.status(time.Now()); st.Unavailable != 1 || cache.Stats().ReconcileRequests != 1 {
		t.Fatalf("cold cache status = %+v, want the row dropped and a re-scan requested", st)
	}
}

// The feed stops with its cache, and a journal that cannot be tailed stops it.
func TestLedgerFeedRunStops(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	done := make(chan struct{})
	go func() {
		h.feed.run(h.ctx)
		close(done)
	}()
	h.cache.StopReconciler()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the feed did not stop with its cache")
	}
	_, entries := h.set.snapshot()
	if len(entries) != 0 {
		t.Fatalf("a stopped cache's entry was not pruned: %+v", entries)
	}

	h2 := newFeedHarness(t, newJournalingStore())
	h2.store.j.setFail(fmt.Errorf("storage: %w", beads.ErrEventsJournalUnsupported))
	done2 := make(chan struct{})
	go func() {
		h2.feed.run(h2.ctx)
		close(done2)
	}()
	select {
	case <-done2:
	case <-time.After(3 * time.Second):
		t.Fatal("an unsupported journal did not stop the feed")
	}
	if st := h2.feed.status(time.Now()); st.State != ledgerFeedStateStopped || !strings.Contains(st.Reason, "unsupported") {
		t.Fatalf("status = %q (%s), want stopped with the error", st.State, st.Reason)
	}
}

func TestAttachLedgerFeedDecision(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	set := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{})
	req := func(mode string) *ledgerFeedRequest {
		return &ledgerFeedRequest{set: set, scope: "s", mode: mode}
	}
	if att := attachLedgerFeed(ctx, req(config.BeadsLedgerFeedOff), newJournalingStore(), true); att != nil {
		t.Fatal("mode off attached")
	}
	if att := attachLedgerFeed(ctx, nil, newJournalingStore(), true); att != nil {
		t.Fatal("no request attached")
	}
	inactive := newJournalingStore()
	inactive.j.active.Store(false)
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		store      beads.Store
		background bool
		reason     string
	}{
		{"suspended", ctx, newJournalingStore(), false, "suspended at start"},
		{"no cancellable ctx", context.Background(), newJournalingStore(), true, "no background refresh"},
		{"untailable lane", ctx, beads.NewMemStore(), true, "lane *beads.MemStore cannot be tailed"},
		{"inactive", ctx, inactive, true, "journal not active"},
		{"tailed", ctx, newJournalingStore(), true, ""},
	} {
		att := attachLedgerFeed(tc.ctx, req(config.BeadsLedgerFeedAuto), tc.store, tc.background)
		if att == nil || att.entry.reason != tc.reason || (att.entry.feed != nil) != (tc.reason == "") {
			t.Fatalf("%s: attachment %+v, want reason %q", tc.name, att, tc.reason)
		}
	}
}

// A binding cache and the shared legacy file store take wrapWithCachingStore,
// which never tails.
func TestWrapWithCachingStoreNeverTails(t *testing.T) {
	store := newJournalingStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := wrapWithCachingStore(ctx, store, events.NewFake(), true).(*beads.CachingStore)
	defer cache.StopReconciler()
	if got := store.j.reads.Load(); got != 0 {
		t.Fatalf("wrapWithCachingStore read the journal %d time(s)", got)
	}
}

func TestLedgerFeedMissDetector(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	d := newLedgerFeedMissDetector(func() time.Time { return t0 })

	// A scan create with no journal row is a miss once its grace passes.
	d.noteScan("gc-miss", t0)
	if m, _, _ := d.report(t0.Add(ledgerFeedMissGrace / 2)); m != 0 {
		t.Fatalf("decided a miss inside the grace: %d", m)
	}
	m, _, recent := d.report(t0.Add(ledgerFeedMissGrace))
	if m != 1 || !slices.Equal(recent, []string{"gc-miss"}) {
		t.Fatalf("misses = %d %v, want gc-miss", m, recent)
	}

	// A journal row before or shortly after the scan change excuses it.
	t1 := t0.Add(time.Hour)
	d.noteJournal([]string{"gc-before"}, map[string]bool{"gc-before": true}, t1)
	d.noteScan("gc-before", t1.Add(time.Minute))
	d.noteScan("gc-after", t1.Add(time.Minute))
	d.noteJournal([]string{"gc-after"}, nil, t1.Add(time.Minute+time.Second))
	if m, _, _ := d.report(t1.Add(time.Hour)); m != 1 {
		t.Fatalf("misses = %d, want the first only", m)
	}

	// An unhealthy feed voids a scan change within the window.
	t2 := t1.Add(2 * time.Hour)
	d.noteUnhealthy(t2.Add(-time.Minute))
	d.noteScan("gc-void", t2)
	if m, _, _ := d.report(t2.Add(time.Minute)); m != 1 {
		t.Fatalf("misses = %d, want the unhealthy window to void gc-void", m)
	}

	// A scan change for an id whose refresh found nothing new is the E3
	// visibility suspect.
	t3 := t2.Add(time.Hour)
	d.noteJournal([]string{"gc-lag"}, map[string]bool{"gc-lag": false}, t3)
	d.noteScan("gc-lag", t3.Add(time.Second))
	if _, sar, _ := d.report(t3.Add(time.Minute)); sar != 1 {
		t.Fatalf("scan_after_refresh = %d, want 1", sar)
	}

	// observe watches only the scan's creates and closes.
	var forwarded int
	wrapped := d.observe(func(beads.ChangeSource, string, string, string, string, string, *[]string, json.RawMessage) {
		forwarded++
	})
	wrapped(beads.ChangeScan, events.BeadUpdated, "gc-upd", "", "", "", nil, nil)
	wrapped(beads.ChangeRefresh, events.BeadCreated, "gc-ref", "", "", "", nil, nil)
	if forwarded != 2 || len(d.pending) != 0 {
		t.Fatalf("forwarded=%d pending=%v, want both forwarded and none watched", forwarded, d.pending)
	}
	wrapped(beads.ChangeScan, events.BeadClosed, "gc-cl", "", "", "", nil, nil)
	if len(d.pending) != 1 {
		t.Fatalf("pending = %v, want the scan close watched", d.pending)
	}

	// A stopped feed's detector judges nothing more.
	d.disable()
	d.noteScan("gc-after-stop", t3.Add(2*time.Hour))
	if m, _, _ := d.report(t3.Add(3 * time.Hour)); m != 1 {
		t.Fatalf("misses after disable = %d, want unchanged", m)
	}
}

// A healthy feed that is behind has not read the journal row of a scan
// change yet: the change is not judged until the feed's checkpoint passes
// the head it had seen when the scan reported it.
func TestLedgerFeedMissDetectorWaitsForALaggingFeed(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	d := newLedgerFeedMissDetector(func() time.Time { return t0 })
	var seq, head atomic.Int64
	seq.Store(10)
	head.Store(5000)
	d.progress = func() (int64, int64) { return seq.Load(), head.Load() }

	d.noteScan("gc-behind", t0)
	if m, _, _ := d.report(t0.Add(time.Minute)); m != 0 {
		t.Fatalf("misses = %d while the feed is behind the head it had seen, want 0", m)
	}
	// The feed catches up and reads the row.
	seq.Store(4000)
	d.noteJournal([]string{"gc-behind"}, nil, t0.Add(time.Minute))
	seq.Store(5000)
	if m, _, _ := d.report(t0.Add(2 * time.Minute)); m != 0 {
		t.Fatalf("misses = %d after the lagging feed read the row, want 0", m)
	}

	// Caught up past the head, a change with no journal row is a miss.
	d.noteScan("gc-miss", t0.Add(3*time.Minute))
	seq.Store(5001)
	head.Store(5001)
	if m, _, recent := d.report(t0.Add(4 * time.Minute)); m != 1 || !slices.Equal(recent, []string{"gc-miss"}) {
		t.Fatalf("misses = %d %v, want gc-miss", m, recent)
	}
}

func TestLedgerFeedStatusOverTheControllerSocket(t *testing.T) {
	h := newFeedHarness(t, newJournalingStore())
	registerLedgerFeedSet(context.Background(), h.set)
	t.Cleanup(func() { ledgerFeedSets.Delete(normalizePathForCompare(h.set.cityPath)) })
	h.set.setMode(config.BeadsLedgerFeedAuto)

	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck
	go handleControllerConn(server, h.set.cityPath, controllerHostingMode(""), func() {}, nil, nil, nil, nil, nil)
	if _, err := client.Write([]byte(ledgerFeedStatusCommand + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var st ledgerFeedStatus
	if err := json.Unmarshal(line, &st); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if st.Mode != config.BeadsLedgerFeedAuto || len(st.Scopes) != 1 || st.Scopes[0].Scope != ledgerFeedCityScope {
		t.Fatalf("status = %+v", st)
	}
	if got := ledgerFeedStatusFor(t.TempDir(), time.Now()); got.Mode != "" {
		t.Fatalf("an unknown city reported %+v", got)
	}
}

func TestLedgerFeedDoctorCheck(t *testing.T) {
	cfgWith := func(mode string) *config.City {
		return &config.City{Beads: config.BeadsConfig{LedgerFeed: mode}}
	}
	tailed := func(mut func(*ledgerFeedScopeStatus)) ledgerFeedStatus {
		s := ledgerFeedScopeStatus{Scope: "city", State: ledgerFeedStateTailing, Lane: "*beads.NativeDoltStore"}
		if mut != nil {
			mut(&s)
		}
		return ledgerFeedStatus{Mode: config.BeadsLedgerFeedAuto, Scopes: []ledgerFeedScopeStatus{s, {Scope: "rig:r", State: ledgerFeedStateScanOnly, Reason: "lane *beads.BdStore cannot be tailed"}}}
	}
	for _, tc := range []struct {
		name    string
		mode    string
		st      ledgerFeedStatus
		err     error
		status  doctor.CheckStatus
		message string
	}{
		{"off", "", ledgerFeedStatus{Mode: config.BeadsLedgerFeedOff}, nil, doctor.StatusOK, "ledger feed off"},
		{"off, controller down", "off", ledgerFeedStatus{}, errors.New("dial"), doctor.StatusOK, "ledger feed off"},
		{"auto, controller down", "auto", ledgerFeedStatus{}, errors.New("dial"), doctor.StatusOK, "feed state unknown"},
		{"healthy", "auto", tailed(nil), nil, doctor.StatusOK, "1 scope(s) tailed, 1 on the re-scan"},
		{"drift", "off", tailed(nil), nil, doctor.StatusWarning, "until its next reload"},
		{"stopped", "auto", tailed(func(s *ledgerFeedScopeStatus) { s.State, s.Reason = ledgerFeedStateStopped, "unsupported" }), nil, doctor.StatusWarning, "feed stopped: unsupported"},
		{"misses", "auto", tailed(func(s *ledgerFeedScopeStatus) { s.Misses, s.RecentMisses = 2, []string{"gc-1"} }), nil, doctor.StatusWarning, "no journal row (recent: gc-1)"},
		{"lag", "auto", tailed(func(s *ledgerFeedScopeStatus) { s.Lag = ledgerFeedDoctorLagRows + 1 }), nil, doctor.StatusWarning, "journal rows behind"},
		{"stuck", "auto", tailed(func(s *ledgerFeedScopeStatus) {
			s.State, s.StateAgeMS = ledgerFeedStateRetrying, (2 * time.Minute).Milliseconds()
		}), nil, doctor.StatusWarning, "feed retrying for 2m0s"},
		{"not yet stuck", "auto", tailed(func(s *ledgerFeedScopeStatus) { s.State, s.StateAgeMS = ledgerFeedStateRetrying, 1000 }), nil, doctor.StatusOK, "tailed"},
		{"echo loop", "auto", tailed(func(s *ledgerFeedScopeStatus) { s.WakesLastMinute = ledgerFeedDoctorWakesPerMinute + 1 }), nil, doctor.StatusWarning, "possible echo loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := newLedgerFeedDoctorCheck(cfgWith(tc.mode), func(string) (ledgerFeedStatus, error) { return tc.st, tc.err })
			r := check.Run(&doctor.CheckContext{CityPath: "/city"})
			if r.Status != tc.status || !strings.Contains(r.Message, tc.message) {
				t.Fatalf("result %v %q, want %v containing %q", r.Status, r.Message, tc.status, tc.message)
			}
			if r.Status == doctor.StatusWarning && r.Severity != doctor.SeverityAdvisory {
				t.Fatalf("warning severity = %v, want advisory", r.Severity)
			}
		})
	}
	check := newLedgerFeedDoctorCheck(cfgWith("auto"), nil)
	if check.CanFix() || check.WarmupEligible() {
		t.Fatal("the ledger feed check must be neither fixable nor warm-up eligible")
	}
}

func TestStoreMetadataSignatureCarriesTheLedgerFeedMode(t *testing.T) {
	dir := t.TempDir()
	off := storeMetadataSignature(dir, &config.City{})
	auto := storeMetadataSignature(dir, &config.City{Beads: config.BeadsConfig{LedgerFeed: "auto"}})
	if off == auto || !strings.Contains(auto, "ledger_feed=auto") {
		t.Fatalf("signatures off=%q auto=%q, want the mode in the signature", off, auto)
	}
	if storeMetadataSignature(dir, &config.City{Beads: config.BeadsConfig{LedgerFeed: "OFF"}}) != off {
		t.Fatal("an unnormalized off changed the signature")
	}
}

// lockedBeadEventsFor is beadEventsFor read under the fake's lock, for a
// test whose events are recorded by a running feed.
func lockedBeadEventsFor(t *testing.T, ep *events.Fake, id string) []string {
	t.Helper()
	all, err := ep.List(events.Filter{})
	if err != nil {
		t.Fatalf("List events: %v", err)
	}
	var got []string
	for _, evt := range all {
		if evt.Subject == id && strings.HasPrefix(evt.Type, "bead.") {
			got = append(got, evt.Type+"/"+evt.Actor)
		}
	}
	return got
}

// waitForLedgerFeed polls cond until it holds or timeout passes.
func waitForLedgerFeed(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// TestLedgerFeedOverEmbeddedDolt tails a real native store with the journal
// on: a Create and then a Close made through the backing, behind the cache,
// reach the cache and the legacy wake within 2s, long before any re-scan
// (the first is at least 30s out). Writes through the cache neither notify
// as cache-reconcile nor wake.
func TestLedgerFeedOverEmbeddedDolt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BD_EVENTS_JOURNAL", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scopeRoot := t.TempDir()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	seed, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Skipf("upstream beads storage unavailable: %v", err)
	}
	if err := seed.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("SetConfig(issue_prefix): %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed storage: %v", err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("events-journal: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	native, err := beads.OpenNativeDoltStoreAt(ctx, scopeRoot, nil)
	if err != nil {
		t.Skipf("native store unavailable: %v", err)
	}
	t.Cleanup(func() { _ = native.CloseStore() })
	if !native.EventsJournalActive() {
		t.Fatal("the native store's journal is not active")
	}

	pokeCh, ctrlCh := make(chan struct{}, 1), make(chan struct{}, 1)
	set := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{})
	set.setWake(newLegacyWake(pokeCh, ctrlCh))
	ep := events.NewFake()
	req := &ledgerFeedRequest{set: set, scope: ledgerFeedCityScope, mode: config.BeadsLedgerFeedAuto}
	cache := wrapWithCachingStoreFeed(ctx, native, ep, true, req).(*beads.CachingStore)
	defer cache.StopReconciler()
	_, entries := set.snapshot()
	if len(entries) != 1 || entries[0].feed == nil || entries[0].feed.late {
		t.Fatalf("entries = %+v, want one baselined feed", entries)
	}
	waitForLedgerFeed(t, 10*time.Second, cache.IsLive, "the cache to go live")
	drain := func() int {
		n := 0
		for {
			select {
			case <-pokeCh:
				n++
			default:
				return n
			}
		}
	}

	// A burst of writes through the cache: the feed reads every one back,
	// racing their absorbs, and yields to each in flight, so none notifies
	// as cache-reconcile or wakes the reconciler (ga-yoix1).
	feed := entries[0].feed
	head0 := feed.status(time.Now()).Head
	var own []string
	for i := range 5 {
		b, err := cache.Create(beads.Bead{Title: fmt.Sprintf("local %d", i)})
		if err != nil {
			t.Fatalf("cache Create: %v", err)
		}
		own = append(own, b.ID)
		if err := cache.SetMetadata(b.ID, "k", "v"); err != nil {
			t.Fatalf("cache SetMetadata: %v", err)
		}
		status := "in_progress"
		if err := cache.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("cache Update: %v", err)
		}
		if err := cache.Close(b.ID); err != nil {
			t.Fatalf("cache Close: %v", err)
		}
	}
	waitForLedgerFeed(t, 20*time.Second, func() bool {
		st := feed.status(time.Now())
		return st.Head >= head0+20 && st.Seq == st.Head && st.PendingRetries == 0
	}, "the feed to read back the cache's own writes")
	if n := drain(); n != 0 {
		t.Fatalf("20 writes through the cache woke the reconciler %d time(s), want none", n)
	}
	for _, id := range own {
		for _, e := range lockedBeadEventsFor(t, ep, id) {
			if strings.HasSuffix(e, "/"+cacheReconcileActor) {
				t.Fatalf("%s: events %v, want no cache-reconcile echo of the cache's own writes", id, lockedBeadEventsFor(t, ep, id))
			}
		}
	}

	row, err := native.Create(beads.Bead{Title: "out of band"})
	if err != nil {
		t.Fatalf("backing Create: %v", err)
	}
	waitForLedgerFeed(t, 2*time.Second, func() bool {
		got, err := cache.Get(row.ID)
		return err == nil && got.Status == "open" && slices.Contains(lockedBeadEventsFor(t, ep, row.ID), events.BeadCreated+"/"+cacheReconcileActor)
	}, "the out-of-band create to reach the cache")
	if n := drain(); n == 0 {
		t.Fatal("the out-of-band create did not wake the reconciler")
	}
	if err := native.Close(row.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	waitForLedgerFeed(t, 2*time.Second, func() bool {
		return slices.Contains(lockedBeadEventsFor(t, ep, row.ID), events.BeadClosed+"/"+cacheReconcileActor)
	}, "the out-of-band close to reach the cache")
	if n := drain(); n == 0 {
		t.Fatal("the out-of-band close did not wake the reconciler")
	}
	if got := cache.Stats().LastReconcileAt; !got.IsZero() {
		t.Fatalf("a re-scan ran at %v; the feed must have delivered without one", got)
	}
}
