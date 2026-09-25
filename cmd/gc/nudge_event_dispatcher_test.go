package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// testWriter adapts t.Logf into an io.Writer so dispatcher stderr lines land
// in the test log instead of being discarded.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func testWriter(t *testing.T) testLogWriter { return testLogWriter{t: t} }

// nudgeEventedFake wraps runtime.Fake with a contract-faithful session-event
// stream: every subscription leads with a resync frame, events fan out to all
// live subscriptions, and canceling the subscribe ctx closes the channel.
// A dynamicBusy override models PR-B's "working is continuously active"
// tracker semantics without racing the Fake's Activity map.
type nudgeEventedFake struct {
	*runtime.Fake

	mu           sync.Mutex
	subs         []chan runtime.SessionEvent
	busySessions map[string]bool
	stamps       map[string]time.Time
}

func newNudgeEventedFake() *nudgeEventedFake {
	return &nudgeEventedFake{Fake: runtime.NewFake(), busySessions: map[string]bool{}, stamps: map[string]time.Time{}}
}

//nolint:unparam // signature fixed by runtime.SessionEventProvider
func (f *nudgeEventedFake) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	ch := make(chan runtime.SessionEvent, 32)
	ch <- runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()}
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		for i, sub := range f.subs {
			if sub == ch {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		close(ch)
	}()
	return ch, nil
}

func (f *nudgeEventedFake) emit(ev runtime.SessionEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sub := range f.subs {
		select {
		case sub <- ev:
		default:
		}
	}
}

// setBusy marks a session as continuously active: GetLastActivity returns
// the current time on every call, exactly like the herdr activity tracker
// reports a session whose agent status sits at working.
func (f *nudgeEventedFake) setBusy(name string, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busySessions[name] = busy
}

// setStamp freezes a synchronized activity stamp for a session, shadowing the
// embedded Fake's Activity map so tests can mutate it mid-flight without
// racing the Fake's own locking.
func (f *nudgeEventedFake) setStamp(name string, ts time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stamps[name] = ts
}

func (f *nudgeEventedFake) GetLastActivity(name string) (time.Time, error) {
	f.mu.Lock()
	busy := f.busySessions[name]
	stamp, hasStamp := f.stamps[name]
	f.mu.Unlock()
	if busy {
		return time.Now(), nil
	}
	if hasStamp {
		return stamp, nil
	}
	return f.Fake.GetLastActivity(name)
}

type mappedNudgeEventedFake struct{ *nudgeEventedFake }

func (f *mappedNudgeEventedFake) SessionEventStreamCovers(string) bool { return true }
func (f *mappedNudgeEventedFake) SessionEventMatches(name, eventName string) bool {
	return (name == "Exact--GC-Session" && eventName == "mapped-herdr-name") || eventName == "mapped-"+name
}

func TestNudgeSessionEventMatchesProviderRegistryName(t *testing.T) {
	sp := &mappedNudgeEventedFake{nudgeEventedFake: newNudgeEventedFake()}
	if !nudgeSessionEventMatches(sp, "Exact--GC-Session", "mapped-herdr-name") {
		t.Fatal("provider-native event name did not resolve to the Gas City session")
	}
}

// newNudgeDispatcherFixture builds a city dir with one running fake session
// ("worker"), returning the pieces a dispatcher test needs. The returned
// dispatcher uses shrunk timing knobs and is wired to sp.
func newNudgeDispatcherFixture(t *testing.T, sp runtime.Provider) (string, *nudgeEventDispatcher, *session.Info) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	mgr := newSessionManagerWithConfig(dir, store, sp, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	d := newNudgeEventDispatcher(ctx, dir, testWriter(t), "test")
	d.quiescence = 150 * time.Millisecond
	d.retryEpsilon = 30 * time.Millisecond
	d.update(sp, &config.City{}, true)
	t.Cleanup(func() {
		cancel()
		select {
		case <-d.workerDone:
		case <-time.After(3 * time.Second):
			t.Log("dispatcher worker did not stop within 3s")
		}
	})
	// Let the subscription's leading resync pass settle (it runs against an
	// empty queue) so tests observe only the activity they trigger.
	time.Sleep(250 * time.Millisecond)
	return dir, d, &info
}

func queueStateSnapshot(t *testing.T, cityPath string) nudgequeue.State {
	t.Helper()
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return state
}

func waitForDeliveredNudge(t *testing.T, cityPath string, fake *nudgeEventedFake) bool {
	t.Helper()
	stop := time.Now().Add(5 * time.Second)
	for time.Now().Before(stop) {
		state := queueStateSnapshot(t, cityPath)
		if len(state.Pending) == 0 && len(state.InFlight) == 0 && countFakeCalls(fake, "Nudge") > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func countFakeCalls(fake *nudgeEventedFake, method string) int {
	n := 0
	for _, call := range fake.SnapshotCalls() {
		if call.Method == method {
			n++
		}
	}
	return n
}

func TestNudgeEventDispatcherDeliversOnIdleEvent(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info := newNudgeDispatcherFixture(t, fake)

	// The agent has been idle well past the quiescence window.
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge not delivered on idle event; state=%+v calls=%v", queueStateSnapshot(t, dir), fake.SnapshotCalls())
	}
}

func TestNudgeEventDispatcherRetriesFreshIdleStamp(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info := newNudgeDispatcherFixture(t, fake)

	// The idle transition was just observed: the activity stamp is fresh, so
	// the first attempt must defer, and the scheduled retry (after the stamp
	// ages past quiescence) must deliver without any further events.
	fake.Activity = map[string]time.Time{info.SessionName: time.Now()}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge not delivered by the aged-stamp retry; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherWakesFutureDueFailureWithoutAnotherEvent(t *testing.T) {
	fake := newNudgeEventedFake()
	origDeliver := nudgeEventDeliverQueued
	t.Cleanup(func() { nudgeEventDeliverQueued = origDeliver })
	dir, d, info := newNudgeDispatcherFixture(t, fake)
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "retry me", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	var calls atomic.Int32
	requeueErr := make(chan error, 1)
	nudgeEventDeliverQueued = func(ctx context.Context, target nudgeTarget, store, sessStore beads.Store, sp runtime.Provider, quiescence time.Duration, obs worker.LiveObservation) (bool, error) {
		if calls.Add(1) == 1 {
			if err := nudgequeue.WithState(dir, func(state *nudgequeue.State) error {
				state.Pending[0].DeliverAfter = time.Now().Add(40 * time.Millisecond)
				return nil
			}); err != nil {
				requeueErr <- err
				return false, err
			}
			return false, nil
		}
		return origDeliver(ctx, target, store, sessStore, sp, quiescence, obs)
	}

	// Only the initial kick is supplied. The requeued future-due item must
	// arrange its own retry even though the target was already idle: without
	// queuedNudgeRetryRemaining's wake, no second idle transition ever
	// arrives to trigger another attempt.
	d.kickSessionAfter(info.SessionName, 0, nudgeEventRetryBudget)
	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("requeued failure was not retried without another event; calls=%d state=%+v", calls.Load(), queueStateSnapshot(t, dir))
	}
	select {
	case err := <-requeueErr:
		t.Fatalf("requeueing simulated failure: %v", err)
	default:
	}
}

func TestNudgeEventDispatcherBusyAgentStopsAfterOneRetry(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info := newNudgeDispatcherFixture(t, fake)

	// A working agent reports continuously fresh activity (tracker semantics),
	// so the attempt and its single retry must both reject — and then STOP.
	// A replayed idle event for a busy agent takes exactly this path.
	fake.setBusy(info.SessionName, true)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	// Allow the attempt plus the whole retry budget to elapse, then confirm
	// the kick DIED: no delivery, and no further observation activity in a
	// second same-length window (a reborn poller would keep observing).
	window := 2 * (nudgeEventRetryBudget + 2) * (150*time.Millisecond + 30*time.Millisecond)
	time.Sleep(window)
	afterFirst := countFakeCalls(fake, "IsRunning")
	time.Sleep(window)
	afterSecond := countFakeCalls(fake, "IsRunning")

	state := queueStateSnapshot(t, dir)
	if len(state.Pending) != 1 {
		t.Fatalf("pending = %d, want 1 (busy agent must not receive delivery); state=%+v", len(state.Pending), state)
	}
	if n := countFakeCalls(fake, "Nudge"); n != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for a busy agent", n)
	}
	if afterSecond != afterFirst {
		t.Fatalf("IsRunning kept growing (%d -> %d): the event's attempt+retry must stop, not poll", afterFirst, afterSecond)
	}
}

func TestNudgeEventDispatcherDeliversWhenStampLagsEvent(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info := newNudgeDispatcherFixture(t, fake)

	// Live-observed herdr timing: the idle EVENT reaches the dispatcher
	// before the activity tracker has stamped the transition, so the first
	// attempt still observes a continuously-fresh (working) stamp and its
	// retry lands just inside the re-stamped window. The bounded retry
	// budget must absorb the skew and deliver.
	fake.setBusy(info.SessionName, true)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})
	go func() {
		// The tracker's debounced poll stamps the transition a beat later.
		time.Sleep(60 * time.Millisecond)
		fake.setStamp(info.SessionName, time.Now())
		fake.setBusy(info.SessionName, false)
	}()

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge not delivered despite stamp lagging the event; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherResyncRunsFullPass(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// No targeted event: the resync (as replayed by every reconnect) must
	// trigger a full pass that finds the long-idle agent.
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge not delivered on resync full pass; state=%+v", queueStateSnapshot(t, dir))
	}
}

// countingCloseNudgesStore wraps a shared backing beads.Store and counts
// CloseStore calls instead of actually closing the backing store, so a test
// can reuse one real store across many simulated "opens" and still observe
// whether each caller closed the handle it was handed.
type countingCloseNudgesStore struct {
	beads.Store
	closes *atomic.Int32
}

func (s countingCloseNudgesStore) CloseStore() error { //nolint:unparam // signature fixed by the interface closeBeadStoreHandle asserts on
	s.closes.Add(1)
	return nil
}

// TestNudgeEventDispatcherRunPassClosesBeadStore reproduces the #4968 leak:
// runPass opened a nudges-class store via openNudgeBeadStore on every pass
// but never closed it, leaking one store handle per dispatch pass on a
// long-running controller. It mints an independent counting wrapper per open
// over one shared backing store and asserts opens and closes stay balanced
// across the dispatcher's background resync passes.
func TestNudgeEventDispatcherRunPassClosesBeadStore(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info := newNudgeDispatcherFixture(t, fake)

	backing := openNudgeBeadStore(dir)
	var opens, closes atomic.Int32
	orig := openOwnedNudgeBeadStore
	openOwnedNudgeBeadStore = func(_ string) (beads.NudgesStore, beads.Store) {
		opens.Add(1)
		owned := countingCloseNudgesStore{Store: backing.Store, closes: &closes}
		return beads.NudgesStore{Store: backing.Store}, owned
	}
	t.Cleanup(func() { openOwnedNudgeBeadStore = orig })

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge not delivered; state=%+v calls=%v", queueStateSnapshot(t, dir), fake.SnapshotCalls())
	}

	// waitForDeliveredNudge observes queue/provider state only, so delivery can
	// become visible before runPass's deferred close has run; poll until the
	// counts settle instead of asserting immediately after delivery.
	deadline := time.Now().Add(2 * time.Second)
	var gotOpens, gotCloses int32
	for {
		gotOpens, gotCloses = opens.Load(), closes.Load()
		if gotOpens > 0 && gotCloses == gotOpens {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if gotOpens == 0 {
		t.Fatalf("expected runPass to open at least one nudges store, opened=%d", gotOpens)
	}
	if gotCloses != gotOpens {
		t.Fatalf("runPass leaked store handles: opened=%d closed=%d", gotOpens, gotCloses)
	}
	_ = d
}

// TestNudgeEventDispatcherRunPassDoesNotCloseRelocatedSharedStore reproduces
// the regression in ac9715472c: an unconditional defer closeBeadStoreHandle
// in runPass closed whatever openNudgeBeadStore returned, including the
// shared, process-scoped store cliStorageRoutes memoizes when the nudges
// class is relocated. Closing that shared instance per pass would tear the
// binding down for every other consumer routed onto it (messaging, orders,
// sessions, graph) after the first dispatch pass. This test relocates the
// nudges class to a counting-close wrapper standing in for that shared store
// and calls runPass directly (an empty queue, so deliverPendingQueuedNudges /
// nudgeMaintenanceStore never opens) to isolate runPass's own close-guard
// behavior from that separate, pre-existing, unconditional-close code path.
//
// Built directly rather than via newNudgeDispatcherFixture, and without
// driving update()'s resubscribe path: the fixture's fake session stream
// leads with a buffered resync frame that the forward goroutine turns into
// an async kickAll/runPass the moment update(..., true) subscribes, which
// raced this test's own direct entry.routes mutation and runPass call under
// -race (both read/write cliStorageRoutes concurrently with no
// synchronization between the two runPass callers).
func TestNudgeEventDispatcherRunPassDoesNotCloseRelocatedSharedStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	openNudgeBeadStore(dir)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := newNudgeEventDispatcher(ctx, dir, testWriter(t), "test")
	d.mu.Lock()
	d.cfg = &config.City{}
	d.sp = newNudgeEventedFake()
	d.mu.Unlock()

	var closes atomic.Int32
	shared := countingCloseNudgesStore{Store: beads.NewMemStore(), closes: &closes}

	// Relocate the NUDGES class to the shared, counting-close store, mirroring
	// TestNudgeEventDispatcherRunPassResolvesSessionStoreIndependentlyOfNudges's
	// direct-mutation approach: cliStorageRoutes(dir) has already resolved (and
	// cached) to nil via the openNudgeBeadStore call above.
	entry := cliStorageRoutesEntryFor(filepath.Clean(dir))
	entry.routes = &storageRoutes{
		stores: map[coordclass.Class]beads.Store{
			coordclass.ClassNudges: shared,
		},
		binding: "test-nudges-relocated",
	}
	t.Cleanup(func() { entry.routes = nil })

	d.runPass("", 0)

	if got := closes.Load(); got != 0 {
		t.Fatalf("runPass closed the shared relocated-class store %d time(s); it does not own that handle and must never close it", got)
	}
}

// TestNudgeEventDispatcherRunPassClosesRawSessionStore reproduces the #4968
// leak in the OTHER handle runPass opens: the raw city store it resolves for
// session-class reads (openRawCityStoreForSessionResolution, wired to
// openCityStoreAt). That store is always a fresh one-shot handle -- never
// routed through cliStorageRoutes' memoized entries -- so runPass
// unconditionally owns it and must close it on every pass, unlike the
// conditionally-owned nudges-class store above.
//
// Built directly rather than via newNudgeDispatcherFixture, and without
// driving update()'s resubscribe path, for the same reason documented on
// TestNudgeEventDispatcherRunPassDoesNotCloseRelocatedSharedStore: the
// fixture's fake session stream leads with a buffered resync frame that the
// forward goroutine turns into an async kickAll/runPass the moment
// update(..., true) subscribes, which races this test's own unsynchronized
// swap of the package-level openRawCityStoreForSessionResolution var (opens
// and closes are already atomic.Int32 and safe; the var swap itself is not).
// Calling runPass directly with no live worker/subscription means the
// counters need no synchronization beyond the atomics already in use.
func TestNudgeEventDispatcherRunPassClosesRawSessionStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	backing := openNudgeBeadStore(dir)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := newNudgeEventDispatcher(ctx, dir, testWriter(t), "test")
	d.mu.Lock()
	d.cfg = &config.City{}
	d.sp = newNudgeEventedFake()
	d.mu.Unlock()

	var opens, closes atomic.Int32
	orig := openRawCityStoreForSessionResolution
	openRawCityStoreForSessionResolution = func(_ string) (beads.Store, error) {
		opens.Add(1)
		return countingCloseNudgesStore{Store: backing.Store, closes: &closes}, nil
	}
	t.Cleanup(func() { openRawCityStoreForSessionResolution = orig })

	d.runPass("", 0)

	if gotOpens := opens.Load(); gotOpens == 0 {
		t.Fatalf("expected runPass to open the raw session store, opened=%d", gotOpens)
	} else if gotCloses := closes.Load(); gotCloses != gotOpens {
		t.Fatalf("runPass leaked the raw session store handle: opened=%d closed=%d", gotOpens, gotCloses)
	}
}

func TestNudgeEventDispatcherDeliveryHasDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := newNudgeEventDispatcher(ctx, t.TempDir(), io.Discard, "test")
	d.deliveryTimeout = 20 * time.Millisecond

	orig := nudgeEventDeliverQueued
	nudgeEventDeliverQueued = func(ctx context.Context, _ nudgeTarget, _, _ beads.Store, _ runtime.Provider, _ time.Duration, _ worker.LiveObservation) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	t.Cleanup(func() { nudgeEventDeliverQueued = orig })

	started := time.Now()
	deliveryCtx, deliveryCancel := d.targetContext()
	defer deliveryCancel()
	_, err := d.deliverQueued(deliveryCtx, nudgeTarget{}, nil, nil, runtime.NewFake(), worker.LiveObservation{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deliverQueued error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("delivery returned after %v; deadline did not bound the provider call", elapsed)
	}
}

type blockingObservationProvider struct {
	*nudgeEventedFake
	unblock <-chan struct{}
	result  runtime.Liveness
	err     error
}

type lateLegacyNudgeProvider struct {
	*nudgeEventedFake
	started     chan struct{}
	release     chan struct{}
	delivered   chan struct{}
	startOnce   sync.Once
	deliverOnce sync.Once
	deliveries  atomic.Int32
	resultErr   error
}

func (p *lateLegacyNudgeProvider) Nudge(_ string, _ []runtime.ContentBlock) error {
	p.startOnce.Do(func() {
		close(p.started)
		<-p.release
	})
	p.deliveries.Add(1)
	p.deliverOnce.Do(func() { close(p.delivered) })
	return p.resultErr
}

func TestNudgeEventDispatcherDoesNotRetryUnknownLegacyDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := &lateLegacyNudgeProvider{
			nudgeEventedFake: newNudgeEventedFake(),
			started:          make(chan struct{}),
			release:          make(chan struct{}),
			delivered:        make(chan struct{}),
		}
		defer func() {
			select {
			case <-sp.release:
			default:
				close(sp.release)
			}
		}()
		dir, d, info := newNudgeDispatcherFixture(t, sp)
		d.deliveryTimeout = 20 * time.Millisecond
		sp.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-time.Minute)}
		if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "deliver once", time.Now().Add(-time.Minute))); err != nil {
			t.Fatalf("enqueueQueuedNudge: %v", err)
		}

		done := make(chan struct{})
		go func() {
			d.runPass(info.SessionName, nudgeEventRetryBudget)
			close(done)
		}()
		<-sp.started
		<-time.After(d.deliveryTimeout + time.Millisecond)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("dispatcher returned before the non-context provider reported success")
		default:
		}

		close(sp.release)
		synctest.Wait()
		<-done
		<-sp.delivered

		state := queueStateSnapshot(t, dir)
		if len(state.Pending) != 0 || len(state.InFlight) != 0 || len(state.Dead) != 0 {
			t.Fatalf("successful legacy delivery remained queued: state=%+v", state)
		}
		// A later dispatcher pass must remain a no-op: waiting for the original
		// provider result preserves the no-double-inject property without
		// terminalizing an outcome that the provider has not reported yet.
		d.runPass(info.SessionName, nudgeEventRetryBudget)
		if got := sp.deliveries.Load(); got != 1 {
			t.Fatalf("deliveries = %d, want exactly 1 across the retry pass", got)
		}
	})
}

func TestNudgeEventDispatcherRequeuesLegacyErrorAfterDeliveryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := &lateLegacyNudgeProvider{
			nudgeEventedFake: newNudgeEventedFake(),
			started:          make(chan struct{}),
			release:          make(chan struct{}),
			delivered:        make(chan struct{}),
			resultErr:        errors.New("agent busy, timed out waiting for idle"),
		}
		defer func() {
			select {
			case <-sp.release:
			default:
				close(sp.release)
			}
		}()
		dir, d, info := newNudgeDispatcherFixture(t, sp)
		d.deliveryTimeout = 20 * time.Millisecond
		sp.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-time.Minute)}
		if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "retry after busy", time.Now().Add(-time.Minute))); err != nil {
			t.Fatalf("enqueueQueuedNudge: %v", err)
		}

		done := make(chan struct{})
		go func() {
			d.runPass(info.SessionName, nudgeEventRetryBudget)
			close(done)
		}()
		<-sp.started
		<-time.After(d.deliveryTimeout + time.Millisecond)
		synctest.Wait()

		select {
		case <-done:
			t.Fatal("dispatcher returned before the non-context provider reported its delivery result")
		default:
		}
		state := queueStateSnapshot(t, dir)
		if len(state.InFlight) != 1 || len(state.Pending) != 0 || len(state.Dead) != 0 {
			t.Fatalf("blocked legacy delivery state = %+v, want one in-flight item", state)
		}

		close(sp.release)
		synctest.Wait()
		<-done

		state = queueStateSnapshot(t, dir)
		if len(state.Pending) != 1 || len(state.InFlight) != 0 || len(state.Dead) != 0 {
			t.Fatalf("failed legacy delivery state = %+v, want one requeued pending item", state)
		}
		if got := state.Pending[0].LastError; got != sp.resultErr.Error() {
			t.Fatalf("requeued LastError = %q, want %q", got, sp.resultErr)
		}
	})
}

func TestNudgeEventDispatcherRequeuesAutoRoutedLegacyErrorAfterDeliveryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		legacy := &lateLegacyNudgeProvider{
			nudgeEventedFake: newNudgeEventedFake(),
			started:          make(chan struct{}),
			release:          make(chan struct{}),
			delivered:        make(chan struct{}),
			resultErr:        errors.New("agent busy, timed out waiting for idle"),
		}
		defer func() {
			select {
			case <-legacy.release:
			default:
				close(legacy.release)
			}
		}()
		sp := sessionauto.New(legacy, runtime.NewFake())
		dir, d, info := newNudgeDispatcherFixture(t, sp)
		d.deliveryTimeout = 20 * time.Millisecond
		legacy.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-time.Minute)}
		if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "retry routed busy", time.Now().Add(-time.Minute))); err != nil {
			t.Fatalf("enqueueQueuedNudge: %v", err)
		}

		done := make(chan struct{})
		go func() {
			d.runPass(info.SessionName, nudgeEventRetryBudget)
			close(done)
		}()
		<-legacy.started
		<-time.After(d.deliveryTimeout + time.Millisecond)
		synctest.Wait()

		select {
		case <-done:
			t.Fatal("dispatcher returned before the auto-routed legacy provider reported its delivery result")
		default:
		}
		state := queueStateSnapshot(t, dir)
		if len(state.InFlight) != 1 || len(state.Pending) != 0 || len(state.Dead) != 0 {
			t.Fatalf("blocked auto-routed legacy delivery state = %+v, want one in-flight item", state)
		}

		close(legacy.release)
		synctest.Wait()
		<-done

		state = queueStateSnapshot(t, dir)
		if len(state.Pending) != 1 || len(state.InFlight) != 0 || len(state.Dead) != 0 {
			t.Fatalf("failed auto-routed legacy delivery state = %+v, want one requeued pending item", state)
		}
		if got := state.Pending[0].LastError; got != legacy.resultErr.Error() {
			t.Fatalf("requeued LastError = %q, want %q", got, legacy.resultErr)
		}
	})
}

func (p *blockingObservationProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	<-p.unblock
	return p.result, p.err
}

func TestWorkerObserveNudgeTargetContextBoundsProviderPreflight(t *testing.T) {
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	sp := &blockingObservationProvider{nudgeEventedFake: newNudgeEventedFake(), unblock: unblock}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := workerObserveNudgeTargetContext(ctx, nudgeTarget{sessionName: "worker"}, nil, sp)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("workerObserveNudgeTargetContext error = %v, want context deadline exceeded", err)
	}
}

func TestWorkerHandleForNudgeTargetContextBoundsSecondObservation(t *testing.T) {
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	sp := &blockingObservationProvider{nudgeEventedFake: newNudgeEventedFake(), unblock: unblock}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	target := nudgeTarget{sessionName: "worker", continuationEpoch: "epoch-1"}

	_, err := workerHandleForNudgeTargetContext(ctx, target, nil, sp)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("workerHandleForNudgeTargetContext error = %v, want context deadline exceeded", err)
	}
}

type blockingObservationStageProvider struct {
	*nudgeEventedFake
	stage       string
	blockedName string
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
}

func (p *blockingObservationStageProvider) block(name, stage string) {
	if name != p.blockedName || p.stage != stage {
		return
	}
	p.startOnce.Do(func() { close(p.started) })
	<-p.release
}

func (p *blockingObservationStageProvider) GetMeta(name, key string) (string, error) {
	p.block(name, "metadata")
	return p.Fake.GetMeta(name, key)
}

func (p *blockingObservationStageProvider) IsAttached(name string) bool {
	p.block(name, "attachment")
	return p.Fake.IsAttached(name)
}

func (p *blockingObservationStageProvider) GetLastActivity(name string) (time.Time, error) {
	p.block(name, "last-activity")
	return p.nudgeEventedFake.GetLastActivity(name)
}

func TestNudgeEventDispatcherObservationDeadlineDoesNotStallNextDelivery(t *testing.T) {
	for _, stage := range []string{"metadata", "attachment", "last-activity"} {
		t.Run(stage, func(t *testing.T) {
			sp := &blockingObservationStageProvider{
				nudgeEventedFake: newNudgeEventedFake(),
				stage:            stage,
				started:          make(chan struct{}),
				release:          make(chan struct{}),
			}
			t.Cleanup(func() { close(sp.release) })
			dir, d, blocked := newNudgeDispatcherFixture(t, sp)
			d.deliveryTimeout = 20 * time.Millisecond
			sp.blockedName = blocked.SessionName

			store := openNudgeBeadStore(dir)
			mgr := newSessionManagerWithConfig(dir, store, sp, nil)
			ready, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "ready", Title: "Ready", Command: "codex", WorkDir: dir, Provider: "codex", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
			if err != nil {
				t.Fatalf("CreateSession(ready): %v", err)
			}
			if err := mgr.Start(context.Background(), ready.ID, "", runtime.Config{WorkDir: dir}); err != nil {
				t.Fatalf("Start(ready): %v", err)
			}
			sp.Activity = map[string]time.Time{
				blocked.SessionName: time.Now().Add(-time.Minute),
				ready.SessionName:   time.Now().Add(-time.Minute),
			}
			for _, agent := range []string{"worker", "ready"} {
				if err := enqueueQueuedNudge(dir, newQueuedNudge(agent, "continue", time.Now().Add(-time.Minute))); err != nil {
					t.Fatalf("enqueueQueuedNudge(%s): %v", agent, err)
				}
			}

			startedAt := time.Now()
			d.runPass("", 0)
			select {
			case <-sp.started:
			default:
				t.Fatalf("%s observation did not reach the blocking provider call", stage)
			}
			if elapsed := time.Since(startedAt); elapsed > time.Second {
				t.Fatalf("dispatcher pass took %v; %s exceeded the per-delivery deadline", elapsed, stage)
			}
			if got := sp.CountCalls("Nudge", ready.SessionName); got != 1 {
				t.Fatalf("next queued delivery calls = %d, want 1 after %s timed out", got, stage)
			}
		})
	}
}

func TestNudgeEventDispatcherMappedEventRearmsFutureRetry(t *testing.T) {
	sp := &mappedNudgeEventedFake{nudgeEventedFake: newNudgeEventedFake()}
	dir, d, info := newNudgeDispatcherFixture(t, sp)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	eventName := "mapped-" + info.SessionName
	d.runPass(eventName, 2)
	d.mu.Lock()
	_, rearmed := d.pending[eventName]
	d.mu.Unlock()
	if !rearmed {
		t.Fatalf("future retry was not rearmed under mapped event name %q", eventName)
	}
}

// TestNudgeEventDispatcherWorkerFullPassPreservesFutureKick reproduces a
// retry-schedule drop: worker() used to delete every entry in d.pending on a
// full pass (kickAll), including kicks not yet due. A full pass's own
// runPass("", ...) only delivers to queue items whose DeliverAfter has
// already arrived (deliverPendingQueuedNudges), so a session still in its
// kickSessionAfter backoff window had its scheduled retry silently erased
// with nothing left to re-fire it, other than the coarser patrol-tick
// fallback. The fix keeps not-yet-due kicks in d.pending across a full pass.
func TestNudgeEventDispatcherWorkerFullPassPreservesFutureKick(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := newNudgeEventDispatcher(ctx, dir, testWriter(t), "test")
	d.mu.Lock()
	d.cfg = &config.City{}
	d.sp = newNudgeEventedFake()
	d.mu.Unlock()

	const target = "gc-worker-not-yet-due"
	d.kickSessionAfter(target, time.Hour, 2)
	d.kickAll()

	deadline := time.Now().Add(2 * time.Second)
	for {
		d.mu.Lock()
		_, stillPending := d.pending[target]
		fullDone := !d.fullPassDue
		d.mu.Unlock()
		if stillPending && fullDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kick for %q was dropped by the full pass instead of being preserved for its own retry timer", target)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNudgeEventDispatcherFullPassFoldedKickStillRearms reproduces a gap
// found independently by code-reviewer (minor) and codex:codex-rescue
// (major) during ship-gate review: kickSessionAfter coalesces repeated kicks
// for a session to the EARLIEST dueAt, which can be earlier than the queue
// item's own real DeliverAfter. When that coalesced kick becomes due in the
// same worker() tick as a full pass (kickAll), the tick used to run only
// d.runPass("", ...): the due kick is unconditionally deleted from
// d.pending, but the full pass's sessionFilter=="" means runPass's own
// re-arm-from-DeliverAfter loop never executes (it only runs when
// sessionFilter != ""), and deliverPendingQueuedNudges excludes the item
// from delivery because its real DeliverAfter has not arrived. The retry
// was silently dropped with nothing left to fire it: the worker has no
// pending entries and no further event is coming. The fix re-runs each due
// targeted kick through its own filtered pass alongside the full pass, so
// it keeps its individual re-arm coverage.
func TestNudgeEventDispatcherFullPassFoldedKickStillRearms(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info := newNudgeDispatcherFixture(t, fake)
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}

	// The queue item's real DeliverAfter is in the near future.
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(80*time.Millisecond))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// Simulate an earlier kickSessionAfter coalescing to a dueAt EARLIER than
	// the queue item's real DeliverAfter (already due now), landing in the
	// same tick as a full pass — with no further event to rescue it.
	d.mu.Lock()
	d.pending[info.SessionName] = nudgeEventKick{dueAt: time.Now(), retriesLeft: nudgeEventRetryBudget}
	d.mu.Unlock()
	d.kickAll()

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge folded into a full pass was never retried once its real DeliverAfter arrived; state=%+v", queueStateSnapshot(t, dir))
	}
}

// TestNudgeEventDispatcherRunPassResolvesSessionStoreIndependentlyOfNudges
// reproduces gc-08u54r finding #5: runPass used to derive the session-class
// store by handing store.Store (already routed to the NUDGES class) to
// cliSessionStore as its fallback workStore. cliSessionStore only diverges
// from that fallback when sessions themselves relocate, so when nudges
// relocate independently of sessions, session reads silently landed on the
// nudges store instead of the (unrelocated) session store — finding nothing,
// because the session bead lives on the real work store.
//
// This test relocates the nudges class to an isolated, empty MemStore while
// leaving sessions unmapped (falls through to the work store). Before the
// fix, runPass's session-bead snapshot comes up empty and delivery never
// happens; after the fix, it reads the work store where the session bead
// actually lives and delivery proceeds.
func TestNudgeEventDispatcherRunPassResolvesSessionStoreIndependentlyOfNudges(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// Relocate the NUDGES class only, to a store distinct from (and empty
	// relative to) the on-disk work store the session bead was created in.
	// cliStorageRoutes(dir) has already been resolved (and cached) to nil by
	// the fixture's own openNudgeBeadStore call, so mutate the cached entry
	// directly rather than going through config/city.toml.
	entry := cliStorageRoutesEntryFor(filepath.Clean(dir))
	entry.routes = &storageRoutes{
		stores: map[coordclass.Class]beads.Store{
			coordclass.ClassNudges: beads.NewMemStore(),
		},
		binding: "test-nudges-only",
	}
	t.Cleanup(func() { entry.routes = nil })

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge not delivered: session-store resolution followed the relocated nudges store instead of the work store; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherKickAllDeliversAlreadyIdle(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// The enqueue-time wake ping path: no event fires for an agent that is
	// already idle, so the wake must land as a worker full pass.
	d.kickAll()

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge not delivered on kickAll; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherEmptyQueueSkipsObservation(t *testing.T) {
	fake := newNudgeEventedFake()
	_, _, info := newNudgeDispatcherFixture(t, fake)

	// Session setup and the settled leading resync account for a baseline of
	// provider calls; an idle event against an EMPTY queue must add none —
	// the pass short-circuits at the queue-state read.
	baseline := countFakeCalls(fake, "IsRunning")
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})
	time.Sleep(300 * time.Millisecond)

	if n := countFakeCalls(fake, "IsRunning"); n != baseline {
		t.Fatalf("IsRunning calls grew %d -> %d, want no observation for an empty queue (cheap short-circuit)", baseline, n)
	}
}

func TestNudgeEventDispatcherIgnoresNonIdleStatuses(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentStateChanged, Session: info.SessionName, Time: time.Now()})
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: info.SessionName, Time: time.Now()})
	time.Sleep(300 * time.Millisecond)

	if n := countFakeCalls(fake, "Nudge"); n != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for non-idle statuses", n)
	}
}

func TestNudgeEventDispatcherActivationAndProviderSwap(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	d := newNudgeEventDispatcher(ctx, dir, testWriter(t), "test")
	defer func() {
		cancel()
		select {
		case <-d.workerDone:
		case <-time.After(3 * time.Second):
			t.Log("dispatcher worker did not stop within 3s")
		}
	}()

	plain := runtime.NewFake()
	d.update(plain, &config.City{}, true)
	if d.active() {
		t.Fatal("active() = true for a provider without an event stream")
	}
	if d.streaming() {
		t.Fatal("streaming() = true for a provider without an event stream")
	}

	evented := newNudgeEventedFake()
	d.update(evented, &config.City{}, true)
	if !d.active() {
		t.Fatal("active() = false after swapping in an event-capable provider")
	}
	waitStreaming := func(want bool) bool {
		stop := time.Now().Add(2 * time.Second)
		for time.Now().Before(stop) {
			if d.streaming() == want {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	if !waitStreaming(true) {
		t.Fatal("streaming() never became true after subscribing")
	}

	// Swap back to a plain provider: the old subscription must be canceled
	// and the dispatcher deactivated.
	d.update(plain, &config.City{}, true)
	if d.active() {
		t.Fatal("active() = true after swapping back to a plain provider")
	}
	if !waitStreaming(false) {
		t.Fatal("streaming() stayed true after the subscription was canceled")
	}

	// A cfg-only reload (no resubscribe) must not tear the stream down.
	d.update(evented, &config.City{}, true)
	if !waitStreaming(true) {
		t.Fatal("streaming() never recovered after re-subscribing")
	}
	d.update(evented, &config.City{}, false)
	if !d.active() || !d.streaming() {
		t.Fatal("cfg-only update deactivated the dispatcher")
	}
}

func TestMaybeStartNudgePollerSuppressedForEventCapableProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()

	spawns := 0
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error {
		spawns++
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{},
		agent:       config.Agent{Name: "worker"},
		sessionName: "gc-worker",
	}

	// No dispatcher is hosting the wake socket yet: even an event-capable
	// provider must not suppress the poller, since nothing else guarantees
	// delivery.
	maybeStartNudgePoller(target, newNudgeEventedFake())
	if spawns != 1 {
		t.Fatalf("spawns = %d, want 1 for an event-capable provider with no live dispatcher", spawns)
	}

	// Once a dispatcher is actually hosting the wake socket, the
	// event-capable provider's poller is suppressed.
	if err := os.MkdirAll(filepath.Dir(nudgequeue.WakeSocketPath(dir)), 0o755); err != nil {
		t.Fatalf("creating wake socket dir: %v", err)
	}
	wakeLis, err := net.Listen("unix", nudgequeue.WakeSocketPath(dir))
	if err != nil {
		t.Fatalf("listening on wake socket: %v", err)
	}
	t.Cleanup(func() { _ = wakeLis.Close() })

	maybeStartNudgePoller(target, newNudgeEventedFake())
	if spawns != 1 {
		t.Fatalf("spawns = %d, want 1 for an event-capable provider with a live dispatcher", spawns)
	}

	maybeStartNudgePoller(target, runtime.NewFake())
	if spawns != 2 {
		t.Fatalf("spawns = %d, want 2 for a plain provider", spawns)
	}

	// Callers without a resolved provider fail open to today's behavior.
	maybeStartNudgePoller(target, nil)
	if spawns != 3 {
		t.Fatalf("spawns = %d, want 3 for a nil provider", spawns)
	}
}

func TestProviderRetiresNudgePollers(t *testing.T) {
	dir := t.TempDir()

	if providerRetiresNudgePollers(nil, dir) {
		t.Fatal("nil provider must not retire pollers")
	}
	if providerRetiresNudgePollers(runtime.NewFake(), dir) {
		t.Fatal("plain provider must not retire pollers")
	}
	if providerRetiresNudgePollers(newNudgeEventedFake(), dir) {
		t.Fatal("event-capable provider must not retire pollers when no dispatcher is hosting the wake socket")
	}

	if err := os.MkdirAll(filepath.Dir(nudgequeue.WakeSocketPath(dir)), 0o755); err != nil {
		t.Fatalf("creating wake socket dir: %v", err)
	}
	wakeLis, err := net.Listen("unix", nudgequeue.WakeSocketPath(dir))
	if err != nil {
		t.Fatalf("listening on wake socket: %v", err)
	}
	t.Cleanup(func() { _ = wakeLis.Close() })

	if !providerRetiresNudgePollers(newNudgeEventedFake(), dir) {
		t.Fatal("event-capable provider must retire pollers once a dispatcher is hosting the wake socket")
	}
}

// TestCityRuntimeEnsureNudgeWakeListenerActivatesOnReload proves finding #8:
// wake-listener ownership must be re-established when a later call (modeling
// a config reload) newly satisfies the activation gate, not fixed forever at
// whatever the first call observed. A city that starts legacy-mode on a
// non-event provider has no listener; once the provider swaps to one that
// implements SessionEventProvider (the nudge-event dispatcher goes active),
// the very next ensureNudgeWakeListener call -- which reloadConfigTraced now
// makes after every cr.nudgeEvents.update -- must start it.
func TestCityRuntimeEnsureNudgeWakeListenerActivatesOnReload(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	_ = openNudgeBeadStore(dir) // materializes an empty, queryable bead store
	ctx, cancel := context.WithCancel(context.Background())

	cr := &CityRuntime{
		cfg:         &config.City{}, // legacy dispatcher mode (nudgeDispatcherIsSupervisor == false)
		cityPath:    dir,
		stderr:      testWriter(t),
		logPrefix:   "test",
		nudgeWakeCh: make(chan struct{}, 1),
	}
	cr.nudgeEvents = newNudgeEventDispatcher(ctx, dir, cr.stderr, cr.logPrefix)
	t.Cleanup(func() {
		cancel()
		select {
		case <-cr.nudgeEvents.workerDone:
		case <-time.After(3 * time.Second):
			t.Log("dispatcher worker did not stop within 3s")
		}
	})

	// Startup: a plain (non-event) provider under legacy mode satisfies
	// neither half of the gate. No listener should start.
	cr.nudgeEvents.update(runtime.NewFake(), cr.cfg, true)
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener != nil {
		t.Fatal("wake listener started for a non-event provider under legacy dispatcher mode, want none")
	}

	// Simulate a config reload that swaps in an event-capable provider,
	// exactly what reloadConfigTraced does: update() first (which the
	// dispatcher uses to decide active()), then ensureNudgeWakeListener.
	cr.nudgeEvents.update(newNudgeEventedFake(), cr.cfg, true)
	if !cr.nudgeEvents.active() {
		t.Fatal("precondition: dispatcher must report active() after swapping to an event-capable provider")
	}
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener == nil {
		t.Fatal("wake listener was not started after a reload made the provider event-capable")
	}
}

// TestCityRuntimeEnsureNudgeWakeListenerTearsDownWhenGateCloses proves the
// fix for a false-positive nudgequeue.DispatcherIsHosting result: a listener
// left running after a reload drops back to legacy dispatch on a non-event
// provider would still answer dials, so a deferred submit would suppress its
// fallback sidecar poller believing the supervisor still delivers, while
// neither nudgeDispatchTick's event-dispatcher path nor its supervisor path
// picks the item up. The listener must close when the activation gate it
// itself uses stops being satisfied, so the socket stops answering and the
// fallback poller is no longer suppressed.
func TestCityRuntimeEnsureNudgeWakeListenerTearsDownWhenGateCloses(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	_ = openNudgeBeadStore(dir)
	ctx, cancel := context.WithCancel(context.Background())

	cr := &CityRuntime{
		cfg:         &config.City{},
		cityPath:    dir,
		stderr:      testWriter(t),
		logPrefix:   "test",
		nudgeWakeCh: make(chan struct{}, 1),
	}
	cr.nudgeEvents = newNudgeEventDispatcher(ctx, dir, cr.stderr, cr.logPrefix)
	t.Cleanup(func() {
		cancel()
		select {
		case <-cr.nudgeEvents.workerDone:
		case <-time.After(3 * time.Second):
			t.Log("dispatcher worker did not stop within 3s")
		}
	})

	// Bring the gate up with an event-capable provider and confirm the
	// listener is live and answering.
	cr.nudgeEvents.update(newNudgeEventedFake(), cr.cfg, true)
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener == nil {
		t.Fatal("precondition: wake listener did not start for an event-capable provider")
	}
	if !nudgequeue.DispatcherIsHosting(dir) {
		t.Fatal("precondition: socket must answer while the listener is up")
	}

	// Reload back onto a non-event provider under legacy dispatcher mode:
	// neither half of the gate is satisfied any more.
	cr.nudgeEvents.update(runtime.NewFake(), cr.cfg, true)
	if cr.nudgeEvents.active() {
		t.Fatal("precondition: dispatcher must not report active() for a non-event provider")
	}
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener != nil {
		t.Fatal("wake listener was not torn down after the gate closed")
	}
	if nudgequeue.DispatcherIsHosting(dir) {
		t.Fatal("socket still answers after the wake listener was torn down; a deferred submit would wrongly suppress its fallback poller")
	}
}

// TestCityRuntimeReloadConfigTracedUpdatesNudgeWakeListenerHosting drives
// reloadConfigTraced itself, not a hand-rolled stand-in for it. The unit
// test above asserts the gate logic in ensureNudgeWakeListener is correct;
// this one asserts reloadConfigTraced actually calls it. A prior version of
// reloadConfigTraced dropped that call, and the unit test above kept passing
// because it invokes cr.ensureNudgeWakeListener directly rather than going
// through reloadConfigTraced -- exactly the gap this test closes.
func TestCityRuntimeReloadConfigTracedUpdatesNudgeWakeListenerHosting(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	clearInheritedBeadsEnv(t)
	requireNoLeakedDoltAfterForPaths(t, cityPath)
	writeConfig := func(mode string) {
		t.Helper()
		data := []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nnudge_dispatcher = \"" + mode + "\"\n")
		if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	writeConfig("supervisor")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath,
		CityName: "test-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: testWriter(t),
	})
	cr.sessionDrains = newDrainTracker()
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	cr.nudgeWakeCh = make(chan struct{}, 1)
	cr.nudgeEvents = newNudgeEventDispatcher(ctx, cityPath, cr.stderr, cr.logPrefix)
	t.Cleanup(func() {
		cancel()
		select {
		case <-cr.nudgeEvents.workerDone:
		case <-time.After(3 * time.Second):
			t.Log("dispatcher worker did not stop within 3s")
		}
	})
	cr.nudgeEvents.update(cr.sp, cr.cfg, true)
	cr.ensureNudgeWakeListener(ctx)
	if !nudgequeue.DispatcherIsHosting(cityPath) {
		t.Fatal("DispatcherIsHosting() = false with supervisor dispatcher, want true")
	}

	lastProviderName := "fake"
	writeConfig("legacy")
	reply := cr.reloadConfigTraced(ctx, &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("legacy reload outcome = %q, want %q; error=%q", reply.Outcome, reloadOutcomeApplied, reply.Error)
	}
	if nudgequeue.DispatcherIsHosting(cityPath) {
		t.Fatal("DispatcherIsHosting() = true after legacy reload, want false")
	}

	writeConfig("supervisor")
	reply = cr.reloadConfigTraced(ctx, &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("supervisor reload outcome = %q, want %q; error=%q", reply.Outcome, reloadOutcomeApplied, reply.Error)
	}
	if !nudgequeue.DispatcherIsHosting(cityPath) {
		t.Fatal("DispatcherIsHosting() = false after supervisor reload, want true")
	}
}
