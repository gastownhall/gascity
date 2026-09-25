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
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type runPassCloseCountingStore struct {
	beads.Store
	closes *atomic.Int64
}

func (s *runPassCloseCountingStore) CloseStore() error {
	s.closes.Add(1)
	if closer, ok := s.Store.(interface{ CloseStore() error }); ok {
		return closer.CloseStore()
	}
	return nil
}

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

type blockingNudgeSubscriptionProvider struct {
	*runtime.Fake
	entered chan struct{}
	release chan struct{}
}

func (p *blockingNudgeSubscriptionProvider) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	close(p.entered)
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ch := make(chan runtime.SessionEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
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
	origDeliver := nudgePollDeliverQueued
	t.Cleanup(func() { nudgePollDeliverQueued = origDeliver })
	dir, d, info := newNudgeDispatcherFixture(t, fake)
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "retry me", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	var calls atomic.Int32
	requeueErr := make(chan error, 1)
	nudgePollDeliverQueued = func(target nudgeTarget, store, sessStore beads.Store, sp runtime.Provider, quiescence time.Duration, obs worker.LiveObservation) (bool, error) {
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
		return origDeliver(target, store, sessStore, sp, quiescence, obs)
	}

	// Only the initial kick is supplied. The requeued future-due item must
	// arrange its own retry even though the target was already idle.
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

func TestNudgeEventDispatcherRunPassReadsSessionsFromWorkStoreWhenNudgesRelocate(t *testing.T) {
	fake := runtime.NewFake()
	dir, d, info := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	entry := cliStorageRoutesEntryFor(filepath.Clean(dir))
	entry.routes = &storageRoutes{
		binding: "test-nudges-only",
		stores: map[coordclass.Class]beads.Store{
			coordclass.ClassNudges: beads.NewMemStore(),
		},
	}
	t.Cleanup(func() { entry.routes = nil })

	d.runPass(info.SessionName, 0)

	var nudges int
	for _, call := range fake.SnapshotCalls() {
		if call.Method == "Nudge" {
			nudges++
		}
	}
	if nudges != 1 {
		t.Fatalf("Nudge calls = %d, want 1; runPass must read the live session from the work store when nudges relocate; calls=%v state=%+v", nudges, fake.SnapshotCalls(), queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherRunPassPreservesRelocatedStoreAcrossPasses(t *testing.T) {
	cityPath := t.TempDir()
	var sharedCloses atomic.Int64
	shared := &runPassCloseCountingStore{Store: beads.NewMemStore(), closes: &sharedCloses}
	var openedCloses atomic.Int64
	originalOpenWorkStore := openNudgeWorkStore
	openNudgeWorkStore = func(string, string) (beads.Store, error) {
		return &runPassCloseCountingStore{Store: beads.NewMemStore(), closes: &openedCloses}, nil
	}
	t.Cleanup(func() { openNudgeWorkStore = originalOpenWorkStore })
	entry := cliStorageRoutesEntryFor(cityPath)
	entry.once.Do(func() {
		entry.routes = &storageRoutes{
			binding: "infra",
			stores: map[coordclass.Class]beads.Store{
				coordclass.ClassNudges: shared,
			},
		}
	})
	t.Cleanup(func() {
		cliStorageRoutesMu.Lock()
		delete(cliStorageRoutesByCity, cityPath)
		cliStorageRoutesMu.Unlock()
	})

	d := &nudgeEventDispatcher{
		parent:     context.Background(),
		cityPath:   cityPath,
		stderr:     io.Discard,
		logPrefix:  "test",
		quiescence: defaultNudgePollQuiescence,
		cfg:        &config.City{},
		sp:         runtime.NewFake(),
		pending:    make(map[string]nudgeEventKick),
		kicked:     make(chan struct{}, 1),
		workerDone: make(chan struct{}),
	}

	d.runPass("", nudgeEventRetryBudget)
	d.runPass("some-session", 0)

	if got := sharedCloses.Load(); got != 0 {
		t.Fatalf("runPass closed the shared relocated-class store %d time(s) across two passes, want 0", got)
	}
	if got := openedCloses.Load(); got != 2 {
		t.Fatalf("runPass closed its one-shot work-store handles %d time(s) across two passes, want 2", got)
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

func TestNudgeEventDispatcherUpdateDoesNotHoldLockWhileSubscribing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := newNudgeEventDispatcher(ctx, t.TempDir(), testWriter(t), "test")
	t.Cleanup(func() {
		cancel()
		<-d.workerDone
	})

	blocking := &blockingNudgeSubscriptionProvider{Fake: runtime.NewFake(), entered: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan struct{})
	go func() {
		d.update(blocking, &config.City{}, true)
		close(firstDone)
	}()
	select {
	case <-blocking.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("subscription did not start")
	}

	replacementDone := make(chan struct{})
	go func() {
		d.update(runtime.NewFake(), &config.City{}, true)
		close(replacementDone)
	}()
	select {
	case <-replacementDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("provider replacement blocked behind SubscribeSessionEvents while dispatcher mutex was held")
	}
	close(blocking.release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked subscription did not return")
	}
	if d.active() || d.streaming() {
		t.Fatal("stale subscription replaced the current non-event provider")
	}
}

func TestNudgeEventDispatcherClosesPerPassStore(t *testing.T) {
	spy := &closeStoreSpy{Store: beads.NewMemStore()}
	originalOpenWorkStore := openNudgeWorkStore
	openNudgeWorkStore = func(string, string) (beads.Store, error) { return spy, nil }
	t.Cleanup(func() { openNudgeWorkStore = originalOpenWorkStore })

	d := &nudgeEventDispatcher{cityPath: t.TempDir(), stderr: testWriter(t), logPrefix: "test", cfg: &config.City{}, sp: runtime.NewFake()}
	d.runPass("", 0)
	if got := spy.closeCount(); got != 1 {
		t.Fatalf("CloseStore calls = %d, want 1 per dispatcher pass", got)
	}
}
