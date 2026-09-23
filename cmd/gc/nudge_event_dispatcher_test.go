package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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

// nudgeEventedFakeSubscribeErr is event-capable (implements
// runtime.SessionEventProvider, so providerRetiresNudgePollers(f) is true)
// but its subscription always fails — modeling a provider that is capable in
// principle while its stream never actually establishes.
type nudgeEventedFakeSubscribeErr struct {
	*runtime.Fake
}

func newNudgeEventedFakeSubscribeErr() *nudgeEventedFakeSubscribeErr {
	return &nudgeEventedFakeSubscribeErr{Fake: runtime.NewFake()}
}

//nolint:unparam // signature fixed by runtime.SessionEventProvider
func (f *nudgeEventedFakeSubscribeErr) SubscribeSessionEvents(_ context.Context) (<-chan runtime.SessionEvent, error) {
	return nil, errSubscribeAlwaysFails
}

var errSubscribeAlwaysFails = errors.New("nudgeEventedFakeSubscribeErr: subscribe always fails")

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
	// empty queue) so tests observe only the activity they trigger. The fake's
	// leading resync frame is already buffered before Subscribe returns, but
	// forward() only starts consuming it after streamGen is stamped — so a
	// pending/fullPassDue check racing forward()'s own goroutine startup can
	// observe the dispatcher's untouched zero state (fullPassDue still false,
	// pending still empty) and mistake "the leading pass hasn't run yet" for
	// "it already settled". fullPassesQueued is only ever incremented from
	// inside kickAll, itself only ever called from forward() consuming a
	// resync frame (or an explicit external kick), so waiting for it to be
	// >=1 first proves forward() actually processed that frame before we ask
	// whether its resulting pass has drained.
	waitForNudgeDispatcherCondition(t, d.streaming, "nudge event stream established")
	waitForNudgeDispatcherCondition(t, func() bool {
		return d.fullPassesQueued.Load() >= 1
	}, "leading resync pass scheduled")
	waitForNudgeDispatcherCondition(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.fullPassDue && len(d.pending) == 0
	}, "leading resync pass settled")
	return dir, d, &info
}

// waitForNudgeDispatcherConditionTimeout bounds every
// waitForNudgeDispatcherCondition wait in this file; every caller uses the
// same generous bound today, so it lives here instead of as a per-call
// parameter.
const waitForNudgeDispatcherConditionTimeout = 2 * time.Second

// waitForNudgeDispatcherCondition polls ok until it reports true or the
// timeout elapses. order_dynamic_integration_test.go has an equivalent
// helper, but it is gated behind the `integration` build tag and unavailable
// here.
func waitForNudgeDispatcherCondition(t *testing.T, ok func() bool, name string) {
	t.Helper()
	deadline := time.Now().Add(waitForNudgeDispatcherConditionTimeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", name)
}

// waitForPendingKickToDie waits for d.pending[session] to be absent AND stay
// absent for a debounce window. A kick that is rejected and still has retry
// budget left is deleted from d.pending and immediately rewritten (with a
// future dueAt) inline within the same runPass call, so a single absence
// check would catch that in-flight microsecond gap and mistake a live retry
// chain for a dead one; the debounce window is long enough to outlast that
// gap but short relative to the scheduled retry delay.
func waitForPendingKickToDie(t *testing.T, d *nudgeEventDispatcher, session string, timeout time.Duration) {
	t.Helper()
	const debounce = 100 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		_, present := d.pending[session]
		d.mu.Unlock()
		if !present {
			time.Sleep(debounce)
			d.mu.Lock()
			_, reappeared := d.pending[session]
			d.mu.Unlock()
			if !reappeared {
				return
			}
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pending kick %q to die", session)
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

func TestNudgeEventDispatcherBusyAgentStopsAfterOneRetry(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info := newNudgeDispatcherFixture(t, fake)

	// A working agent reports continuously fresh activity (tracker semantics),
	// so the attempt and its single retry must both reject — and then STOP.
	// A replayed idle event for a busy agent takes exactly this path.
	fake.setBusy(info.SessionName, true)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	// Wait for the kick to actually die (its pending entry cleared and staying
	// cleared) instead of sleeping a computed worst-case budget window: the
	// retry chain re-schedules itself via kickSessionAfter, so as long as an
	// entry keeps reappearing in d.pending the budget has not yet been
	// exhausted.
	budgetWindow := (nudgeEventRetryBudget + 2) * (150*time.Millisecond + 30*time.Millisecond)
	waitForPendingKickToDie(t, d, info.SessionName, 2*budgetWindow)
	afterFirst := countFakeCalls(fake, "IsRunning")

	// Confirm the kick stays dead: no further observation activity in a
	// short settle window (a reborn poller would keep observing).
	time.Sleep(200 * time.Millisecond)
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
	_, d, info := newNudgeDispatcherFixture(t, fake)

	// Session setup and the settled leading resync account for a baseline of
	// provider calls; an idle event against an EMPTY queue must add none —
	// the pass short-circuits at the queue-state read.
	baseline := countFakeCalls(fake, "IsRunning")
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	// Wait for the dispatcher to actually finish processing this idle kick
	// (its pending entry cleared) instead of assuming an arbitrary sleep
	// covered it — this proves the pass ran, not merely that time passed.
	waitForNudgeDispatcherCondition(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, stillPending := d.pending[info.SessionName]
		return !stillPending
	}, "idle kick processed for "+info.SessionName)

	if n := countFakeCalls(fake, "IsRunning"); n != baseline {
		t.Fatalf("IsRunning calls grew %d -> %d, want no observation for an empty queue (cheap short-circuit)", baseline, n)
	}
}

// TestNudgeEventDispatcherRunPassClosesEveryStoreItOpens pins the connection-leak
// fix in runPass: every targeted-event, resync, wake and patrol pass opens a bead
// store via openNudgeBeadStoreErr but never released it via closeBeadStoreHandle,
// so a long-lived dispatcher accumulated open store handles across passes (PR
// #4967 ship-gate finding 5). Each pass here must close what it opened.
func TestNudgeEventDispatcherRunPassClosesEveryStoreItOpens(t *testing.T) {
	// Install the seam before fixture setup: newNudgeDispatcherFixture's dispatcher
	// starts a worker goroutine immediately, so swapping the package-level
	// openNudgeBeadStoreErr var after that point races against its reads. Instead,
	// snapshot opens/closes right after the fixture returns and assert only on the
	// delta accrued afterward — newNudgeDispatcherFixture opens its own long-lived
	// session-manager store via openNudgeBeadStore (which calls openNudgeBeadStoreErr),
	// and that store is legitimately never closed mid-test, so it must not count
	// toward the runPass-specific open/close balance this test checks.
	var opens, closes atomic.Int64
	prev := openNudgeBeadStoreErr
	openNudgeBeadStoreErr = func(path string) (beads.NudgesStore, error) {
		opens.Add(1)
		store, err := prev(path)
		if err != nil {
			return store, err
		}
		return beads.NudgesStore{Store: &runPassCloseCountingStore{Store: store.Store, closes: &closes}}, nil
	}
	t.Cleanup(func() { openNudgeBeadStoreErr = prev })

	fake := newNudgeEventedFake()
	dir, d, info := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake) {
		t.Fatalf("queued nudge not delivered; state=%+v", queueStateSnapshot(t, dir))
	}

	// Drain quiescence: wait for every in-flight/scheduled pass (including any
	// retry the delivery may have earned) to finish before comparing counts,
	// so the assertion below reflects settled state rather than a pass still
	// mid-flight on the dispatcher's worker goroutine.
	waitForNudgeDispatcherCondition(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.pending) == 0 && !d.fullPassDue
	}, "dispatcher settled")

	// One store stays open for the whole test: newNudgeDispatcherFixture's own
	// session-manager store, opened once via openNudgeBeadStore and never closed
	// mid-test. So once the dispatcher has settled, exactly that one open should
	// remain outstanding — every store runPass itself opened must have been closed.
	gotOpens, gotCloses := opens.Load(), closes.Load()
	if gotOpens == 0 {
		t.Fatal("expected the nudge bead store to be opened at least once")
	}
	if outstanding := gotOpens - gotCloses; outstanding != 1 {
		t.Fatalf("nudge bead store leak: opens=%d closes=%d outstanding=%d (want exactly 1: the fixture's own long-lived store)", gotOpens, gotCloses, outstanding)
	}
}

// closeCountingStore wraps a beads.Store and counts CloseStore calls so a test
// can assert every open is released. closeBeadStoreHandle type-asserts against
// interface{ CloseStore() error }.
type runPassCloseCountingStore struct {
	beads.Store
	closes *atomic.Int64
}

//nolint:unparam // error return mandated by the CloseStore interface
func (s *runPassCloseCountingStore) CloseStore() error {
	s.closes.Add(1)
	if c, ok := s.Store.(interface{ CloseStore() error }); ok {
		return c.CloseStore()
	}
	return nil
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

// TestNudgeDispatcherIsHostingCapableButSubscribeFails covers the case both
// cross-provider review legs identified as untested: a provider that IS
// event-capable (satisfies runtime.SessionEventProvider) but whose stream
// never actually comes up. A bare type assertion would report true here —
// this proves the live wake-socket check does not.
func TestNudgeDispatcherIsHostingCapableButSubscribeFails(t *testing.T) {
	dir := t.TempDir()
	failing := newNudgeEventedFakeSubscribeErr()

	if _, ok := runtime.Provider(failing).(runtime.SessionEventProvider); !ok {
		t.Fatal("fixture must satisfy SessionEventProvider to exercise the capable-but-failing case")
	}
	if nudgeDispatcherIsHosting(dir) {
		t.Fatal("no dispatcher is listening, but nudgeDispatcherIsHosting reports true")
	}
}

// TestCityRuntimeReloadConfigTracedClosesNudgeWakeListenerOnLegacyReload
// drives reloadConfigTraced itself (not ensureNudgeWakeListener directly):
// a prior version of ensureNudgeWakeListener returned early once a listener
// existed and never closed it, so a reload out of supervisor mode left the
// wake socket answering forever. Producers dial that socket
// (nudgequeue.DispatcherIsHosting) to decide whether to suppress their
// fallback poller; a stale listener makes that check a false positive while
// neither delivery path (event-dispatcher or supervisor patrol) is actually
// draining the queue anymore, stranding deferred nudges to idle sessions.
func TestCityRuntimeReloadConfigTracedClosesNudgeWakeListenerOnLegacyReload(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfigWithDaemonMode(t, tomlPath, "fake", "supervisor")

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

	// Startup: supervisor mode satisfies the gate on its own, independent
	// of provider capability. The listener must start.
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener == nil {
		t.Fatal("wake listener did not start in supervisor mode at startup")
	}
	if !nudgeDispatcherIsHosting(cityPath) {
		t.Fatal("precondition: nudgeDispatcherIsHosting must report true once the listener is up")
	}

	// Reload into legacy mode: rewrite the on-disk config without
	// [daemon] nudge_dispatcher = "supervisor" and drive the real reload
	// path. Nothing here calls cr.ensureNudgeWakeListener directly — this
	// is the production wiring exercising the teardown branch.
	writeCityRuntimeConfigWithDaemonMode(t, tomlPath, "fake", "")
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(ctx, &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reloadConfigTraced failed: %s", reply.Error)
	}
	if nudgeDispatcherIsSupervisor(cr.cfg) {
		t.Fatal("precondition: reloaded config must be legacy mode")
	}
	if cr.nudgeEvents.active() {
		t.Fatal("precondition: provider must not be event-capable after reload (fake provider stays non-event-capable)")
	}
	if cr.nudgeWakeListener != nil {
		t.Fatal("reloadConfigTraced left the wake listener running after a reload out of supervisor mode into legacy mode")
	}
	if nudgeDispatcherIsHosting(cityPath) {
		t.Fatal("wake socket still answers after the listener should have been closed on legacy reload")
	}
}

// writeCityRuntimeConfigWithDaemonMode is writeCityRuntimeConfig plus an
// optional [daemon] nudge_dispatcher setting; an empty mode omits the
// [daemon] section entirely (legacy mode).
func writeCityRuntimeConfigWithDaemonMode(t *testing.T, tomlPath, provider, nudgeDispatcherMode string) {
	t.Helper()
	writeCityRuntimeConfig(t, tomlPath, provider)
	if nudgeDispatcherMode == "" {
		return
	}
	existing, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	suffix := []byte("\n[daemon]\nnudge_dispatcher = \"" + nudgeDispatcherMode + "\"\n")
	data := append(append([]byte{}, existing...), suffix...)
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
