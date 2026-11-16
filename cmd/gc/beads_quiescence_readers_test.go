package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// quiescenceReadGate holds an actual backing read until the test releases it.
type quiescenceReadGate struct {
	*beads.MemStore
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	open    sync.Once
}

func newQuiescenceReadGate(mem *beads.MemStore) *quiescenceReadGate {
	return &quiescenceReadGate{MemStore: mem, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *quiescenceReadGate) hold() {
	if g.armed.Load() {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
}

func (g *quiescenceReadGate) unblock() { g.open.Do(func() { close(g.release) }) }

func (g *quiescenceReadGate) Get(id string) (beads.Bead, error) {
	g.hold()
	return g.MemStore.Get(id)
}

func (g *quiescenceReadGate) List(query beads.ListQuery) ([]beads.Bead, error) {
	g.hold()
	return g.MemStore.List(query)
}

func assertQuiescentReaderNotRetired(t *testing.T, cr *CityRuntime, logPath, reader string) {
	t.Helper()
	for range 3 {
		if !cr.enterBeadsQuiescenceIfDue(context.Background()) {
			t.Fatal("a suspended city with nothing running is not quiescent")
		}
	}
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("provider ops while %s = %q, want none", reader, ops)
	}
}

func assertQuiescentReaderRetired(t *testing.T, cr *CityRuntime, logPath string) {
	t.Helper()
	// These are explicit settle/retire/idempotency ticks, not a retry loop.
	for range 3 {
		cr.enterBeadsQuiescenceIfDue(context.Background())
	}
	if ops := providerOpsLogged(t, logPath); ops != "stop" {
		t.Fatalf("provider ops after the reader finished = %q, want exactly one stop", ops)
	}
}

func TestBeadsQuiescenceRetiresOnlyAfterAnInFlightCacheReconcileFinishes(t *testing.T) {
	for _, scope := range []string{"city", "rig"} {
		t.Run(scope, func(t *testing.T) {
			cr, _, _, logPath := quiescenceRuntime(t, "")
			cr.cfg.Workspace.SuspendedOnStart = true
			cs := &controllerState{beadsQuiescent: new(atomic.Bool)}
			cr.cs = cs
			option := cs.reconcileGate()
			if scope == "rig" {
				option = cs.rigReconcileGate("r1")
			}
			gate := newQuiescenceReadGate(beads.NewMemStore())
			cache := beads.NewCachingStore(gate, nil, option)
			gate.armed.Store(true)
			done := make(chan struct{})
			go func() {
				defer close(done)
				cache.ReconcileIfDueForTest()
			}()
			t.Cleanup(func() {
				gate.unblock()
				awaitClose(t, done, "cache reconcile cleanup")
			})
			awaitClose(t, gate.entered, "cache reconcile entering its backing read")

			assertQuiescentReaderNotRetired(t, cr, logPath, "a cache reconcile was still reading")

			gate.unblock()
			awaitClose(t, done, "cache reconcile finishing")
			assertQuiescentReaderRetired(t, cr, logPath)
		})
	}
}

func TestBeadsQuiescenceRetiresOnlyAfterAnInFlightCachePrimeFinishes(t *testing.T) {
	for _, mode := range []string{"active", "full"} {
		t.Run(mode, func(t *testing.T) {
			cr, _, _, logPath := quiescenceRuntime(t, "")
			cr.cfg.Workspace.SuspendedOnStart = true
			cs := &controllerState{beadsQuiescent: new(atomic.Bool)}
			cr.cs = cs
			gate := newQuiescenceReadGate(beads.NewMemStore())
			cache := beads.NewCachingStore(gate, nil, cs.reconcileGate())
			gate.armed.Store(true)
			done := make(chan struct{})
			var primeErr error
			go func() {
				defer close(done)
				if mode == "active" {
					primeErr = cache.PrimeActive()
				} else {
					primeErr = cache.Prime(context.Background())
				}
			}()
			t.Cleanup(func() {
				gate.unblock()
				awaitClose(t, done, "cache prime cleanup")
			})
			awaitClose(t, gate.entered, "cache prime entering its backing read")

			assertQuiescentReaderNotRetired(t, cr, logPath, "a cache prime was still reading")

			gate.unblock()
			awaitClose(t, done, "cache prime finishing")
			if primeErr != nil {
				t.Fatalf("prime: %v", primeErr)
			}
			assertQuiescentReaderRetired(t, cr, logPath)
		})
	}
}

// quiescenceEventLog delegates to the standard event fake. caughtUp marks the
// watcher asking for another event after handling everything currently logged;
// closed marks its tail closing. Neither signal depends on elapsed time.
type quiescenceEventLog struct {
	*events.Fake
	failHead atomic.Bool
	caughtUp chan struct{}
	closed   chan struct{}
	close    sync.Once
}

func newQuiescenceEventLog() *quiescenceEventLog {
	return &quiescenceEventLog{
		Fake: events.NewFake(), caughtUp: make(chan struct{}, 1), closed: make(chan struct{}),
	}
}

func (p *quiescenceEventLog) LatestSeq() (uint64, error) {
	if p.failHead.Load() {
		return 0, errors.New("event log head unavailable")
	}
	return p.Fake.LatestSeq()
}

func (p *quiescenceEventLog) Watch(ctx context.Context, after uint64) (events.Watcher, error) {
	w, err := p.Fake.Watch(ctx, after)
	if err != nil {
		return nil, err
	}
	return &quiescenceEventTail{Watcher: w, log: p, seq: after}, nil
}

type quiescenceEventTail struct {
	events.Watcher
	log *quiescenceEventLog
	seq uint64
}

func (w *quiescenceEventTail) Next() (events.Event, error) {
	if head, err := w.log.Fake.LatestSeq(); err == nil && w.seq == head {
		select {
		case w.log.caughtUp <- struct{}{}:
		default:
		}
	}
	evt, err := w.Watcher.Next()
	if err == nil {
		w.seq = evt.Seq
	}
	return evt, err
}

func (w *quiescenceEventTail) Close() error {
	err := w.Watcher.Close()
	w.log.close.Do(func() { close(w.log.closed) })
	return err
}

func startQuiescenceEventWatcher(t *testing.T, cs *controllerState, ep *quiescenceEventLog) {
	t.Helper()
	cs.eventProv = ep
	cs.beadEventStartSeqOK = true
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		awaitClose(t, ep.closed, "bead event watcher cleanup")
	})
	cs.startBeadEventWatcher(ctx)
	awaitClose(t, ep.caughtUp, "bead event watcher starting its tail")
}

func TestBeadsQuiescenceRetiresOnlyAfterTheBeadEventWatcherFinishesEarlierEvents(t *testing.T) {
	cr, _, _, logPath := quiescenceRuntime(t, "")
	cr.cfg.Workspace.SuspendedOnStart = true
	backing := sessionListTestBacking(t, sessionListTestBead())
	gate := newQuiescenceReadGate(backing)
	ep := newQuiescenceEventLog()
	cache := beads.NewCachingStore(gate, cacheChangeRecorder(ep))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cs := &controllerState{
		beadsQuiescent: new(atomic.Bool),
		cfg:            &config.City{Workspace: config.Workspace{Name: "test-city", Prefix: "ct"}},
		cityName:       "test-city",
		cityBeadStore:  cache,
		pokeCh:         make(chan struct{}, 1),
	}
	cr.cs = cs
	// The watcher hands a bead.closed's live confirmation to this dispatch and
	// moves on, so its caught-up signal can fire while that run still holds
	// store work. autocloseDone closes when the run has returned, after its
	// store work ended: the one signal that the run is finished.
	autocloseDone := make(chan struct{})
	prevDispatch := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) {
		go func() {
			defer close(autocloseDone)
			fn()
		}()
	}
	t.Cleanup(func() { beadCloseAutocloseDispatch = prevDispatch })
	startQuiescenceEventWatcher(t, cs, ep)
	// Release the backing before canceling/joining the watcher on any failure.
	t.Cleanup(gate.unblock)

	closed := sessionListTestBead()
	if err := backing.Close(closed.ID); err != nil {
		t.Fatalf("close drained session: %v", err)
	}
	closed.Status = "closed"
	gate.armed.Store(true)
	ep.Record(events.Event{Type: events.BeadClosed, Actor: "gc", Subject: closed.ID, Payload: sessionListEventPayload(t, closed)})
	awaitClose(t, gate.entered, "bead event watcher confirming the drained session")

	assertQuiescentReaderNotRetired(t, cr, logPath, "the bead event watcher was still applying an earlier event")

	gate.unblock()
	awaitClose(t, ep.caughtUp, "bead event watcher finishing its backlog")
	awaitClose(t, autocloseDone, "bead close confirmation finishing its live read")
	assertQuiescentReaderRetired(t, cr, logPath)
}

func TestBeadsQuiescenceWaitsWhenTheEventLogHeadCannotBeRead(t *testing.T) {
	cs := &controllerState{}
	ep := newQuiescenceEventLog()
	startQuiescenceEventWatcher(t, cs, ep)
	ep.failHead.Store(true)
	cr := &CityRuntime{cs: cs}
	if !cr.offTickStoreWorkInFlight() {
		t.Fatal("an unreadable event log head allowed retirement with a live watcher")
	}
}

func TestBeadsQuiescenceDoesNotWaitWithoutAnEventWatcher(t *testing.T) {
	for _, provider := range []string{"absent", "not-started", "start-failed"} {
		t.Run(provider, func(t *testing.T) {
			cs := &controllerState{}
			if provider == "not-started" {
				ep := events.NewFake()
				ep.Record(events.Event{Type: events.SessionStopped, Subject: "ct-session"})
				cs.eventProv = ep
			}
			if provider == "start-failed" {
				cs.eventProv = events.NewFailFake()
				cs.startBeadEventWatcher(context.Background())
			}
			cr := &CityRuntime{cs: cs}
			if cr.offTickStoreWorkInFlight() {
				t.Fatal("retirement held by an event watcher that is not running")
			}
		})
	}
}

func TestBeadsQuiescenceDoesNotWaitAfterNonBeadEventsAreConsumed(t *testing.T) {
	cs := &controllerState{}
	ep := newQuiescenceEventLog()
	startQuiescenceEventWatcher(t, cs, ep)
	ep.Record(events.Event{Type: events.SessionStopped, Subject: "ct-session"})
	awaitClose(t, ep.caughtUp, "bead event watcher consuming a non-bead event")
	cr := &CityRuntime{cs: cs}
	if cr.offTickStoreWorkInFlight() {
		t.Fatal("a consumed non-bead event kept the idle watcher pending")
	}
}
