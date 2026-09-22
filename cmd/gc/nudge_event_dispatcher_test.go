package main

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
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
	dir, d, info := newNudgeDispatcherFixture(t, fake)
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "retry me", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	origDeliver := nudgePollDeliverQueued
	calls := 0
	requeueErr := make(chan error, 1)
	nudgePollDeliverQueued = func(target nudgeTarget, store, sessStore beads.Store, sp runtime.Provider, quiescence time.Duration, obs worker.LiveObservation) (bool, error) {
		calls++
		if calls == 1 {
			if err := nudgequeue.WithState(dir, func(state *nudgequeue.State) error {
				state.Pending[0].DeliverAfter = time.Now().Add(40 * time.Millisecond)
				return nil
			}); err != nil {
				requeueErr <- err
				return false, err
			}
			return false, nil
		}
		return origDeliver(target, store, sessStore, sp, quiescence, obs)
	}
	t.Cleanup(func() { nudgePollDeliverQueued = origDeliver })

	// Only the initial kick is supplied. The requeued future-due item must
	// arrange its own retry even though the target was already idle: without
	// queuedNudgeRetryRemaining's wake, no second idle transition ever
	// arrives to trigger another attempt.
	d.kickSessionAfter(info.SessionName, 0, nudgeEventRetryBudget)
	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("requeued failure was not retried without another event; calls=%d state=%+v", calls, queueStateSnapshot(t, dir))
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
	orig := openNudgeBeadStore
	openNudgeBeadStore = func(_ string) beads.NudgesStore {
		opens.Add(1)
		return beads.NudgesStore{Store: countingCloseNudgesStore{Store: backing.Store, closes: &closes}}
	}
	t.Cleanup(func() { openNudgeBeadStore = orig })

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
// conditionally-owned nudges-class store above. Calls runPass directly (no
// background goroutine) so the counters need no synchronization.
func TestNudgeEventDispatcherRunPassClosesRawSessionStore(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, _ := newNudgeDispatcherFixture(t, fake)

	backing := openNudgeBeadStore(dir)
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

	maybeStartNudgePoller(target, newNudgeEventedFake())
	if spawns != 0 {
		t.Fatalf("spawns = %d, want 0 for an event-capable provider", spawns)
	}

	maybeStartNudgePoller(target, runtime.NewFake())
	if spawns != 1 {
		t.Fatalf("spawns = %d, want 1 for a plain provider", spawns)
	}

	// Callers without a resolved provider fail open to today's behavior.
	maybeStartNudgePoller(target, nil)
	if spawns != 2 {
		t.Fatalf("spawns = %d, want 2 for a nil provider", spawns)
	}
}

func TestProviderRetiresNudgePollers(t *testing.T) {
	if providerRetiresNudgePollers(nil) {
		t.Fatal("nil provider must not retire pollers")
	}
	if providerRetiresNudgePollers(runtime.NewFake()) {
		t.Fatal("plain provider must not retire pollers")
	}
	if !providerRetiresNudgePollers(newNudgeEventedFake()) {
		t.Fatal("event-capable provider must retire pollers")
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

// TestCityRuntimeReloadConfigTracedActivatesNudgeWakeListener drives
// reloadConfigTraced itself, not a hand-rolled stand-in for it. The unit
// test above asserts the gate logic in ensureNudgeWakeListener is correct;
// this one asserts reloadConfigTraced actually calls it. A prior version of
// reloadConfigTraced dropped that call, and the unit test above kept passing
// because it invokes cr.ensureNudgeWakeListener directly rather than going
// through reloadConfigTraced -- exactly the gap this test closes.
func TestCityRuntimeReloadConfigTracedActivatesNudgeWakeListener(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

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

	// Startup: legacy dispatcher mode on a non-event provider satisfies
	// neither half of the gate. No listener should start.
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener != nil {
		t.Fatal("wake listener started for a non-event provider under legacy dispatcher mode, want none")
	}

	// Swap in an event-capable provider directly (the config's session
	// provider name is unchanged, so reloadConfigTraced will not rebuild
	// it from the registry -- it carries this cr.sp forward as nextSp).
	// This is the reload reloadConfigTraced itself must wire up correctly;
	// unlike the unit test above, nothing here calls
	// cr.ensureNudgeWakeListener directly.
	cr.sp = newNudgeEventedFake()
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(ctx, &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reloadConfigTraced failed: %s", reply.Error)
	}
	if !cr.nudgeEvents.active() {
		t.Fatal("precondition: dispatcher must report active() after reloadConfigTraced observes the event-capable provider")
	}
	if cr.nudgeWakeListener == nil {
		t.Fatal("reloadConfigTraced did not start the wake listener after the provider became event-capable")
	}
}
