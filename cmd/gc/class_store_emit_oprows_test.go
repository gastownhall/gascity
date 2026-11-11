package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/eventfact"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/session"
)

// recordJournalRings swaps the CLI emitter's controller doorbell for one
// that records each ring and then runs answer, when set, whose result is
// whether the doorbell poked the conversation; with no answer every ring
// pokes it.
func recordJournalRings(t *testing.T, answer func(cityPath, conv string, ids []string) bool) func() []string {
	t.Helper()
	var (
		mu    sync.Mutex
		rings []string
	)
	prev := classStoreJournalRing
	classStoreJournalRing = func(cityPath, conv string, ids []string) bool {
		mu.Lock()
		rings = append(rings, conv+":"+strings.Join(ids, ","))
		mu.Unlock()
		if answer != nil {
			return answer(cityPath, conv, ids)
		}
		return true
	}
	t.Cleanup(func() { classStoreJournalRing = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), rings...)
	}
}

// DES-O1 D18 for a one-shot CLI: its writes to journal rows (an op row, the
// runtime row) journal no bead.* event, and so no message text, and ring the
// controller's doorbell with the row instead. Every other bead still
// journals its bead.* events.
func TestCLIEmitterRingsForJournalRowsInsteadOfJournaling(t *testing.T) {
	cityPath := t.TempDir()
	rings := recordJournalRings(t, nil)
	cli := &emittingClassStore{Store: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}, cityPath: cityPath}
	st, err := opjournal.NewStore(beads.SessionStore{Store: cli}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := st.Propose(opjournal.Request{
		Conversation: "gc-1", Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: "m1", Producer: opjournal.Producer{Principal: "t3:env", Actor: "user"}, Payload: "secret text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(op.ID, opjournal.StatusSubmitted, opjournal.StatusAccepted, "lane"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Cancel(op.ID, "t3:env", ""); err != nil {
		t.Fatal(err)
	}
	rt, err := opjournal.EncodeRuntimeRow("gc", opjournal.RuntimeRow{Conversation: "gc-1", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Create(rt); err != nil {
		t.Fatal(err)
	}
	task, err := cli.Create(beads.Bead{Title: "task"})
	if err != nil {
		t.Fatal(err)
	}
	journal := readCityJournal(t, cityPath)
	for _, e := range journal {
		if e.Subject != task.ID {
			t.Errorf("the CLI journaled %s %s for a journal row", e.Type, e.Subject)
		}
		if strings.Contains(string(e.Payload), "secret text") {
			t.Errorf("the CLI journaled the message text in %s", e.Type)
		}
	}
	if len(journal) != 1 || journal[0].Type != events.BeadCreated {
		t.Fatalf("journal = %+v, want the task's bead.created alone", journal)
	}
	got := rings()
	if len(got) < 4 {
		t.Fatalf("rings = %v, want one per journal-row write", got)
	}
	for _, r := range got[:3] {
		if r != "gc-1:"+op.ID {
			t.Fatalf("rings = %v, want gc-1:%s for each op write", got, op.ID)
		}
	}
	if !strings.HasPrefix(got[3], "gc-1:") || got[3] == "gc-1:"+op.ID {
		t.Fatalf("runtime row ring = %q", got[3])
	}
}

// The whole path: a one-shot CLI's writes to a lane op ring the
// controller's doorbell, whose refresh through the controller's cache makes
// the bridge record op.changed for each status, ending closed, and no
// bead.* for the row, whether or not a lane runs.
func TestCLIJournalRowWritesReachOpChangedThroughTheDoorbell(t *testing.T) {
	leaf := newLaneTestStore()
	ep := events.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Served from its synchronous prime, with no background refresh to race.
	cache, ok := wrapWithCachingStore(context.Background(), leaf, ep, true).(*beads.CachingStore)
	if !ok {
		t.Fatal("no cache")
	}
	f := newLaneFixtureOver(t, cache)
	cs := &controllerState{cityBeadStore: cache, cfg: f.cfg, sp: f.sp, cityPath: f.dir, beadsQuiescent: new(atomic.Bool)}
	h := newConversationLaneHost(cs, nil, session.StopRequestsLive)
	h.state, h.reason = laneHostStateOff, "test: no lane runs"
	registerLaneHost(ctx, f.dir, h)
	var replies []string
	recordJournalRings(t, func(cityPath, conv string, ids []string) bool {
		reply := laneSocketReply(cityPath, conv+":"+strings.Join(ids, ","))
		replies = append(replies, reply)
		return laneReplyPoked(reply)
	})

	cli := &emittingClassStore{Store: leaf, cityPath: f.dir}
	st, err := opjournal.NewStore(beads.SessionStore{Store: cli}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := st.Propose(opjournal.Request{
		Conversation: f.id, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: "m1", Producer: opjournal.Producer{Principal: "t3:env", Actor: "user"}, Payload: "secret text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(op.ID, opjournal.StatusSubmitted, opjournal.StatusAccepted, "lane"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Cancel(op.ID, "t3:env", ""); err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for _, e := range ep.Events {
		if e.Subject != op.ID {
			continue
		}
		if e.Type != events.OpChanged {
			t.Fatalf("the op row recorded %s (events %v)", e.Type, journalEventLog(ep))
		}
		if strings.Contains(string(e.Payload), "secret text") {
			t.Fatalf("op.changed carries the text: %s", e.Payload)
		}
		var p api.OpChangedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, p.Op.Status)
	}
	if strings.Join(statuses, ",") != "submitted,accepted,closed" {
		t.Fatalf("op.changed statuses = %v (replies %v, events %v)", statuses, replies, journalEventLog(ep))
	}
	if len(readCityJournal(t, f.dir)) != 0 {
		t.Fatalf("the CLI journaled %+v", readCityJournal(t, f.dir))
	}
}

// doorbellRig is a controller cache with a registered lane host that runs no
// lane, and a CLI emitter whose rings reach the controller's doorbell
// handler.
type doorbellRig struct {
	st    *opjournal.Store // the CLI's op store, over its emitter
	ep    *events.Fake     // the controller's recorded events
	f     *laneFixture
	cache *beads.CachingStore
	cs    *controllerState // the controller state the host runs over
	h     *conversationLaneHost

	mu      sync.Mutex
	replies []string
}

// newDoorbellRig builds a doorbellRig whose controller cache is over backing
// (leaf, or a store over it) while the CLI writes leaf itself. A ring reaches
// the doorbell when answer(n) says so, n counting the rings from 1.
func newDoorbellRig(t *testing.T, leaf, backing beads.Store, answer func(n int) bool) *doorbellRig {
	t.Helper()
	r := &doorbellRig{ep: events.NewFake()}
	ctx, cancel := context.WithCancel(context.Background())
	cache, ok := wrapWithCachingStore(context.Background(), backing, r.ep, true).(*beads.CachingStore)
	if !ok {
		t.Fatal("no cache")
	}
	r.cache = cache
	r.f = newLaneFixtureOver(t, cache)
	r.cs = &controllerState{cityBeadStore: cache, cfg: r.f.cfg, sp: r.f.sp, cityPath: r.f.dir, beadsQuiescent: new(atomic.Bool)}
	r.h = newConversationLaneHost(r.cs, nil, session.StopRequestsLive)
	r.h.ctx = ctx
	r.h.state, r.h.reason = laneHostStateOff, "test: no lane runs"
	registerLaneHost(ctx, r.f.dir, r.h)
	t.Cleanup(func() {
		cancel()
		r.h.wg.Wait()
	})
	n := 0
	recordJournalRings(t, func(cityPath, conv string, ids []string) bool {
		n++
		if !answer(n) {
			return false
		}
		reply := laneSocketReply(cityPath, conv+":"+strings.Join(ids, ","))
		r.mu.Lock()
		r.replies = append(r.replies, reply)
		r.mu.Unlock()
		return laneReplyPoked(reply)
	})
	cli := &emittingClassStore{Store: leaf, cityPath: r.f.dir}
	st, err := opjournal.NewStore(beads.SessionStore{Store: cli}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	r.st = st
	return r
}

// doorbellHarness is newDoorbellRig over leaf alone: its CLI op store, the
// controller's events and the conversation.
func doorbellHarness(t *testing.T, leaf beads.Store, answer func(n int) bool) (*opjournal.Store, *events.Fake, string) {
	t.Helper()
	r := newDoorbellRig(t, leaf, leaf, answer)
	return r.st, r.ep, r.f.id
}

// doorbellReplies is the doorbell's answers to the rig's rings so far.
func (r *doorbellRig) doorbellReplies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.replies...)
}

// driveRowRetry makes the rig's host a running one with no worker slot (a
// poke queues its conversation), on the fixture's clock, its retry timers
// handed to the test, and the rows still waiting after each retry round
// reported.
func (r *doorbellRig) driveRowRetry() (timers <-chan laneTimer, rounds <-chan int) {
	tc := make(chan laneTimer, 64)
	rc := make(chan int, 64)
	r.h.mu.Lock()
	defer r.h.mu.Unlock()
	r.h.state, r.h.reason, r.h.maxWorkers = laneHostStateRunning, "", 0
	r.h.clk = r.f.clk
	r.h.newTimer = func(d time.Duration) (<-chan time.Time, func()) {
		tm := laneTimer{d: d, c: make(chan time.Time, 1), stopped: make(chan struct{})}
		tc <- tm
		var once sync.Once
		return tm.c, func() { once.Do(func() { close(tm.stopped) }) }
	}
	r.h.onRowRetry = func(waiting int) { rc <- waiting }
	return tc, rc
}

// heldCreates is a backing store whose next Create, once armed, waits in its
// backing call until lift: a create of the cache over it stays in flight,
// which fences every row the cache does not hold.
type heldCreates struct {
	laneTestStore
	armed   *atomic.Bool
	entered chan struct{}
	release chan struct{}
	// lift releases the held create; it may be called more than once.
	lift func()
}

func newHeldCreates(leaf laneTestStore) heldCreates {
	release := make(chan struct{})
	var once sync.Once
	return heldCreates{
		laneTestStore: leaf, armed: new(atomic.Bool), entered: make(chan struct{}), release: release,
		lift: func() { once.Do(func() { close(release) }) },
	}
}

func (s heldCreates) Create(b beads.Bead) (beads.Bead, error) {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return s.laneTestStore.Create(b)
}

// holdCreate starts a create through cache and returns once its backing call
// is in flight, held there until held.lift; the returned channel closes when
// the create has returned. Cleanup lifts a create still held.
func holdCreate(t *testing.T, cache *beads.CachingStore, held heldCreates) <-chan struct{} {
	t.Helper()
	held.armed.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := cache.Create(beads.Bead{Title: "a write in flight"}); err != nil {
			t.Errorf("the held create: %v", err)
		}
	}()
	t.Cleanup(func() {
		held.lift()
		<-done
	})
	select {
	case <-held.entered:
	case <-time.After(laneWaitBudget):
		t.Fatal("the create never reached its backing call")
	}
	return done
}

func nextRetryTimer(t *testing.T, timers <-chan laneTimer) laneTimer {
	t.Helper()
	select {
	case tm := <-timers:
		return tm
	case <-time.After(laneWaitBudget):
		t.Fatal("the row retry set no timer")
		return laneTimer{}
	}
}

func nextRetryRound(t *testing.T, rounds <-chan int) int {
	t.Helper()
	select {
	case waiting := <-rounds:
		return waiting
	case <-time.After(laneWaitBudget):
		t.Fatal("no row retry round ran")
		return 0
	}
}

// eventsOf is the payloads of ep's events of type typ for subject.
func eventsOf(ep *events.Fake, typ, subject string) []json.RawMessage {
	var out []json.RawMessage
	for _, e := range ep.Events {
		if e.Type == typ && (subject == "" || e.Subject == subject) {
			out = append(out, e.Payload)
		}
	}
	return out
}

// A CLI op whose first ring the controller missed (a busy controller, a
// skipped leg) and that is then closed out of process is still announced:
// the doorbell's refresh announces the close of a row its cache never held,
// since the ring is the evidence the row changed just now.
func TestCLIJournalRowCloseAfterALostRingIsAnnounced(t *testing.T) {
	st, ep, conv := doorbellHarness(t, newLaneTestStore(), func(n int) bool { return n > 1 })
	op, _, err := st.Propose(opjournal.Request{
		Conversation: conv, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLegacyCLI,
		ClientKey: "m1", Producer: opjournal.Producer{Principal: "operator", Actor: "operator"}, Payload: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := eventsOf(ep, events.OpChanged, op.ID); len(got) != 0 {
		t.Fatalf("the lost ring announced %d op.changed", len(got))
	}
	if _, err := st.Close(op.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: "legacy.delivered"}}, "cli"); err != nil {
		t.Fatal(err)
	}
	got := eventsOf(ep, events.OpChanged, op.ID)
	if len(got) != 1 {
		t.Fatalf("op.changed = %d events, want the close once (events %v)", len(got), journalEventLog(ep))
	}
	var p api.OpChangedPayload
	if err := json.Unmarshal(got[0], &p); err != nil {
		t.Fatal(err)
	}
	if p.Op.Status != "closed" {
		t.Fatalf("op.changed status = %q, want closed", p.Op.Status)
	}
}

// A fact a CLI records is announced as fact.recorded through the doorbell,
// on an engine that creates the row closed (SQLite) as on one that creates
// it open and then closes it.
func TestCLIFactIsAnnouncedThroughTheDoorbell(t *testing.T) {
	for _, tc := range []struct {
		name string
		leaf func(t *testing.T) beads.Store
	}{
		{"sqlite", func(t *testing.T) beads.Store {
			leaf, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gc"))
			if err != nil {
				t.Fatal(err)
			}
			return leaf
		}},
		{"memstore", func(*testing.T) beads.Store { return newLaneTestStore() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ep, conv := doorbellHarness(t, tc.leaf(t), func(int) bool { return true })
			fact := opjournal.Fact{Kind: opjournal.FactTurnStarted, Conversation: conv, Generation: 2, Turn: "turn-1", Basis: opjournal.FactBasisRuntime, At: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}
			if _, err := st.RecordFact(fact); err != nil {
				t.Fatalf("RecordFact: %v", err)
			}
			if got := eventsOf(ep, events.FactRecorded, ""); len(got) != 1 {
				t.Fatalf("fact.recorded = %d events, want one (events %v)", len(got), journalEventLog(ep))
			}
		})
	}
}

// recordTestFact records a turn-started fact of conv through st, the CLI's
// op store, and returns its row id.
func recordTestFact(t *testing.T, st *opjournal.Store, conv string) string {
	t.Helper()
	fact := opjournal.Fact{Kind: opjournal.FactTurnStarted, Conversation: conv, Generation: 2, Turn: "turn-1", Basis: opjournal.FactBasisRuntime, At: laneT0}
	if _, err := st.RecordFact(fact); err != nil {
		t.Fatalf("RecordFact: %v", err)
	}
	id, err := opjournal.FactID(st.Prefix(), fact)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// rowRetryRunning reports whether h's row retry runs.
func rowRetryRunning(h *conversationLaneHost) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.retryingRows
}

// wantRowsWaiting fails unless the rig's doorbell answered every ring with
// the poke and one row waiting for the retry.
func (r *doorbellRig) wantRowsWaiting(t *testing.T) {
	t.Helper()
	replies := r.doorbellReplies()
	if len(replies) == 0 {
		t.Fatal("the CLI rang no doorbell")
	}
	for _, reply := range replies {
		if reply != laneReplyOK+", rows waiting for a retry: 1" || !laneReplyPoked(reply) {
			t.Fatalf("doorbell replies = %q, want the poke with the row waiting", replies)
		}
	}
}

// A doorbell whose refresh a local write of the controller's cache holds off
// leaves the row to the lane host's retry, off the socket handler, and says
// so. A fact a CLI records while a create of the cache is in flight is not
// announced while the create holds the fence, however many rounds that
// takes, and is announced once, by the round after the create returns; a
// later ring of the row announces it no more.
func TestDoorbellRetriesARowALocalWriteFenced(t *testing.T) {
	leaf := newLaneTestStore()
	held := newHeldCreates(leaf)
	r := newDoorbellRig(t, leaf, held, func(int) bool { return true })
	timers, rounds := r.driveRowRetry()
	created := holdCreate(t, r.cache, held)

	factID := recordTestFact(t, r.st, r.f.id)
	r.wantRowsWaiting(t)
	if got := eventsOf(r.ep, events.FactRecorded, ""); len(got) != 0 {
		t.Fatalf("fact.recorded = %d events while the create holds the fence", len(got))
	}
	if st := r.h.status(r.f.clk.Now()); st.RowRetries != 1 || st.RowsGivenUp != 0 {
		t.Fatalf("lane status = %+v, want the row waiting", st)
	}

	first := nextRetryTimer(t, timers)
	if first.d != laneRowRetryFirst {
		t.Fatalf("first retry after %s, want %s", first.d, laneRowRetryFirst)
	}
	first.c <- r.f.clk.Now()
	if waiting := nextRetryRound(t, rounds); waiting != 1 {
		t.Fatalf("rows waiting after a round under the fence = %d, want 1", waiting)
	}
	second := nextRetryTimer(t, timers)
	if second.d != 2*laneRowRetryFirst {
		t.Fatalf("second retry after %s, want %s", second.d, 2*laneRowRetryFirst)
	}
	if got := eventsOf(r.ep, events.FactRecorded, ""); len(got) != 0 {
		t.Fatalf("fact.recorded = %d events while the create holds the fence", len(got))
	}

	held.lift()
	<-created
	second.c <- r.f.clk.Now()
	if waiting := nextRetryRound(t, rounds); waiting != 0 || rowRetryRunning(r.h) {
		t.Fatalf("rows waiting after the fence lifted = %d (retry running %t), want none and the retry ended", waiting, rowRetryRunning(r.h))
	}
	if got := eventsOf(r.ep, events.FactRecorded, factID); len(got) != 1 {
		t.Fatalf("fact.recorded = %d events, want one (events %v)", len(got), journalEventLog(r.ep))
	}
	if reply := laneSocketReply(r.f.dir, r.f.id+":"+factID); reply != laneReplyOK {
		t.Fatalf("a later ring's reply = %q, want %s", reply, laneReplyOK)
	}
	if got := eventsOf(r.ep, events.FactRecorded, factID); len(got) != 1 {
		t.Fatalf("a later ring announced the fact again: %d events", len(got))
	}
	if st := r.h.status(r.f.clk.Now()); st.RowRetries != 0 || st.RowsGivenUp != 0 || st.Errors != 0 {
		t.Fatalf("lane status = %+v, want no row waiting or given up, and no error", st)
	}
}

// A fence that outlasts laneRowRetryLimit from the row's first failure gives
// the row up: the last round inside the limit keeps it, the first one past it
// counts it given up in lane-status with an error naming it, and the retry
// ends with no row waiting and nothing announced.
func TestDoorbellRetryGivesUpAFenceThatOutlastsItsLimit(t *testing.T) {
	leaf := newLaneTestStore()
	held := newHeldCreates(leaf)
	r := newDoorbellRig(t, leaf, held, func(int) bool { return true })
	timers, rounds := r.driveRowRetry()
	holdCreate(t, r.cache, held)

	factID := recordTestFact(t, r.st, r.f.id)
	first := nextRetryTimer(t, timers)
	r.f.clk.Advance(laneRowRetryLimit - time.Millisecond)
	first.c <- r.f.clk.Now()
	if waiting := nextRetryRound(t, rounds); waiting != 1 {
		t.Fatalf("rows waiting inside the limit = %d, want 1", waiting)
	}
	second := nextRetryTimer(t, timers)
	r.f.clk.Advance(time.Millisecond)
	second.c <- r.f.clk.Now()
	if waiting := nextRetryRound(t, rounds); waiting != 0 || rowRetryRunning(r.h) {
		t.Fatalf("rows waiting past the limit = %d (retry running %t), want none and the retry ended", waiting, rowRetryRunning(r.h))
	}
	st := r.h.status(r.f.clk.Now())
	if st.RowRetries != 0 || st.RowsGivenUp != 1 || st.Errors != 1 || !strings.Contains(st.LastError, factID) {
		t.Fatalf("lane status = %+v, want %s given up and its error", st, factID)
	}
	if got := eventsOf(r.ep, events.FactRecorded, ""); len(got) != 0 {
		t.Fatalf("fact.recorded = %d events for a row given up under the fence", len(got))
	}
}

// A doorbell the quiescent city answers reads nothing, so it does not answer
// a bare ok: the row waits for the retry however long the city stays
// quiescent, none of which counts against laneRowRetryLimit. Once the city
// resumes, the row's count starts from its last skipped read: a round a
// local write fences keeps it, and the round after the write returns
// installs it and announces it once.
func TestDoorbellRowWaitsOutAQuiescentCity(t *testing.T) {
	leaf := newLaneTestStore()
	held := newHeldCreates(leaf)
	r := newDoorbellRig(t, leaf, held, func(int) bool { return true })
	timers, rounds := r.driveRowRetry()
	r.cs.setBeadsQuiescent(true)

	factID := recordTestFact(t, r.st, r.f.id)
	r.wantRowsWaiting(t)
	// A quiescence twice the limit long: each round in it reads nothing and
	// keeps the row.
	for range 2 {
		tm := nextRetryTimer(t, timers)
		r.f.clk.Advance(laneRowRetryLimit)
		tm.c <- r.f.clk.Now()
		if waiting := nextRetryRound(t, rounds); waiting != 1 {
			t.Fatalf("rows waiting after a quiescent round = %d, want 1", waiting)
		}
	}
	if got := eventsOf(r.ep, events.FactRecorded, ""); len(got) != 0 {
		t.Fatalf("fact.recorded = %d events while the city is quiescent", len(got))
	}
	if st := r.h.status(r.f.clk.Now()); st.RowRetries != 1 || st.RowsGivenUp != 0 || st.Errors != 0 {
		t.Fatalf("lane status = %+v, want the row waiting, nothing given up and no error", st)
	}

	r.cs.setBeadsQuiescent(false)
	created := holdCreate(t, r.cache, held)
	fenced := nextRetryTimer(t, timers)
	fenced.c <- r.f.clk.Now()
	if waiting := nextRetryRound(t, rounds); waiting != 1 {
		t.Fatalf("rows waiting after a fenced round past the quiescence = %d, want 1", waiting)
	}
	held.lift()
	<-created
	last := nextRetryTimer(t, timers)
	last.c <- r.f.clk.Now()
	if waiting := nextRetryRound(t, rounds); waiting != 0 || rowRetryRunning(r.h) {
		t.Fatalf("rows waiting after the city resumed = %d (retry running %t), want none and the retry ended", waiting, rowRetryRunning(r.h))
	}
	if got := eventsOf(r.ep, events.FactRecorded, factID); len(got) != 1 {
		t.Fatalf("fact.recorded = %d events, want one (events %v)", len(got), journalEventLog(r.ep))
	}
	if reply := laneSocketReply(r.f.dir, r.f.id+":"+factID); reply != laneReplyOK {
		t.Fatalf("a later ring's reply = %q, want %s", reply, laneReplyOK)
	}
	if got := eventsOf(r.ep, events.FactRecorded, factID); len(got) != 1 {
		t.Fatalf("a later ring announced the fact again: %d events", len(got))
	}
	if st := r.h.status(r.f.clk.Now()); st.RowRetries != 0 || st.RowsGivenUp != 0 || st.Errors != 0 {
		t.Fatalf("lane status = %+v, want no row waiting or given up, and no error", st)
	}
}

// A doorbell over a sessions store the controller does not cache has no
// cache to install the row in, so it does not answer a bare ok either: the
// row waits for the retry, which installs nothing either, and is given up
// at the limit, counted in lane-status with an error naming the row and why.
func TestDoorbellGivesUpARowWithNoCacheToInstallItIn(t *testing.T) {
	leaf := newLaneTestStore()
	r := newDoorbellRig(t, leaf, leaf, func(int) bool { return true })
	r.cs.cityBeadStore = leaf
	timers, rounds := r.driveRowRetry()

	factID := recordTestFact(t, r.st, r.f.id)
	r.wantRowsWaiting(t)
	first := nextRetryTimer(t, timers)
	r.f.clk.Advance(laneRowRetryLimit)
	first.c <- r.f.clk.Now()
	if waiting := nextRetryRound(t, rounds); waiting != 0 || rowRetryRunning(r.h) {
		t.Fatalf("rows waiting past the limit = %d (retry running %t), want none and the retry ended", waiting, rowRetryRunning(r.h))
	}
	st := r.h.status(r.f.clk.Now())
	if st.RowRetries != 0 || st.RowsGivenUp != 1 || st.Errors != 1 || !strings.Contains(st.LastError, factID) || !strings.Contains(st.LastError, errLaneRowUncached.Error()) {
		t.Fatalf("lane status = %+v, want %s given up for want of a cache", st, factID)
	}
	if got := eventsOf(r.ep, events.FactRecorded, ""); len(got) != 0 {
		t.Fatalf("fact.recorded = %d events with no cache", len(got))
	}
}

// laneReplyPoked reads the doorbell's answers: ok, alone or naming the rows
// waiting for a retry, poked the conversation; no-host and a refusal did not.
func TestLaneReplyPoked(t *testing.T) {
	for reply, want := range map[string]bool{
		laneReplyOK: true,
		laneReplyOK + ", rows waiting for a retry: 2": true,
		"no-host":                           false,
		"refused the lane host is stopping": false,
		"okay":                              false,
		"":                                  false,
	} {
		if got := laneReplyPoked(reply); got != want {
			t.Errorf("laneReplyPoked(%q) = %t, want %t", reply, got, want)
		}
	}
}

// One emission batch rings each conversation once, naming each of its rows
// once however often the batch wrote it.
func TestCLIEmitterRingsEachRowOncePerBatch(t *testing.T) {
	cityPath := t.TempDir()
	rings := recordJournalRings(t, nil)
	cli := &emittingClassStore{Store: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}, cityPath: cityPath}
	row := func(id, conv string) classStoreEmission {
		return classStoreEmission{eventType: events.BeadUpdated, bead: beads.Bead{ID: id, Type: opjournal.BeadType, Labels: []string{opjournal.ConversationLabel(conv)}}}
	}
	cli.emit(row("gc-a", "gc-1"), row("gc-b", "gc-2"), row("gc-a", "gc-1"), row("gc-c", "gc-1"), row("gc-b", "gc-2"))
	if got := fmt.Sprint(rings()); got != "[gc-1:gc-a,gc-c gc-2:gc-b]" {
		t.Fatalf("rings = %s", got)
	}
	if j := readCityJournal(t, cityPath); len(j) != 0 {
		t.Fatalf("the CLI journaled %+v", j)
	}
}

// A journal row that names no conversation (a DES-BX event-fact or
// projector row) written through the CLI emitter journals no bead.* event
// and rings nothing: no lane reads it.
func TestCLIEmitterDropsJournalRowsOfNoConversation(t *testing.T) {
	cityPath := t.TempDir()
	rings := recordJournalRings(t, nil)
	cli := &emittingClassStore{Store: &beads.MemStore{IDPrefix: "gc"}, cityPath: cityPath}
	for _, label := range []string{eventfact.Label, "gc:event-projector"} {
		b, err := cli.Create(beads.Bead{Title: label, Type: opjournal.BeadType, Labels: []string{label}})
		if err != nil {
			t.Fatalf("create %s row: %v", label, err)
		}
		if err := cli.Close(b.ID); err != nil {
			t.Fatalf("close %s row: %v", label, err)
		}
	}
	if j := readCityJournal(t, cityPath); len(j) != 0 {
		t.Fatalf("the CLI journaled %+v", j)
	}
	if got := rings(); len(got) != 0 {
		t.Fatalf("rings = %v, want none", got)
	}
}

// A journal row a CLI write could not read back reaches the emitter by its
// pinned id alone (the read-miss fallbacks of the update, close and delete
// paths): it journals no bead.* event either, rings nothing, since the
// conversation it would ring is unknown, and is warned of. A bead of any
// other id still journals through the same fallback.
func TestCLIEmitterDropsAJournalRowItCouldNotReadBack(t *testing.T) {
	cityPath := t.TempDir()
	rings := recordJournalRings(t, nil)
	var warned []string
	restore := classStoreEmitWarn
	classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
	t.Cleanup(func() { classStoreEmitWarn = restore })
	leaf := &readMissAfterWriteStore{MemStore: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
	cli := &emittingClassStore{Store: leaf, cityPath: cityPath}
	row, err := opjournal.EncodeRuntimeRow("gc", opjournal.RuntimeRow{Conversation: "gc-1", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Create(row); err != nil {
		t.Fatal(err)
	}
	task, err := leaf.Create(beads.Bead{Title: "task"})
	if err != nil {
		t.Fatal(err)
	}
	leaf.missing = true
	for _, id := range []string{row.ID, task.ID} {
		if err := cli.Close(id); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	if j := readCityJournal(t, cityPath); len(j) != 1 || j[0].Subject != task.ID || j[0].Type != events.BeadClosed {
		t.Fatalf("journal = %+v, want the task's bead.closed alone", j)
	}
	if got := rings(); len(got) != 0 {
		t.Fatalf("rings = %v, want none", got)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], row.ID) {
		t.Fatalf("warnings = %q, want one naming %s", warned, row.ID)
	}
}

// ringJournalRows sends at most laneSocketMaxRows ids per verb, so a write
// of more rows than one verb takes rings in chunks the controller accepts.
func TestRingJournalRowsChunksToTheVerbCap(t *testing.T) {
	cityPath := shortSocketTempDir(t, "gc-ring-")
	var (
		mu    sync.Mutex
		lines []string
	)
	startFakeUnixSocket(t, controllerSocketPath(cityPath), func(line string) string {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
		return "ok\n"
	})
	t.Setenv(hookDeadlineEnv, "")
	ids := make([]string, laneSocketMaxRows+3)
	for i := range ids {
		ids[i] = fmt.Sprintf("gc-%d", i)
	}
	if !ringJournalRows(cityPath, "gc-1", ids) {
		t.Fatal("ringJournalRows = false, want the ok answers read as a poke")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("verbs = %d, want 2: %v", len(lines), lines)
	}
	if want := laneSocketPrefix + "gc-1:" + strings.Join(ids[:laneSocketMaxRows], ","); lines[0] != want {
		t.Fatalf("first verb = %q, want %q", lines[0], want)
	}
	if want := laneSocketPrefix + "gc-1:" + strings.Join(ids[laneSocketMaxRows:], ","); lines[1] != want {
		t.Fatalf("second verb = %q, want %q", lines[1], want)
	}
	for _, line := range lines {
		if got := laneSocketReply(t.TempDir(), strings.TrimPrefix(line, laneSocketPrefix)); got != "no-host" {
			t.Fatalf("laneSocketReply(%q) = %q, want the ids accepted (no-host)", line, got)
		}
	}
}

// The doorbell refuses a verb naming more than laneSocketMaxRows rows, and
// accepts one naming exactly that many.
func TestLaneSocketReplyCapsRowIDs(t *testing.T) {
	ids := make([]string, laneSocketMaxRows+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("gc-%d", i)
	}
	city := t.TempDir()
	if got, want := laneSocketReply(city, "gc-1:"+strings.Join(ids, ",")), fmt.Sprintf("refused %d row ids, max %d", laneSocketMaxRows+1, laneSocketMaxRows); got != want {
		t.Fatalf("over the cap: %q, want %q", got, want)
	}
	if got := laneSocketReply(city, "gc-1:"+strings.Join(ids[:laneSocketMaxRows], ",")); got != "no-host" {
		t.Fatalf("at the cap: %q, want no-host", got)
	}
}
