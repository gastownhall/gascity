package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
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

// passes is the dispatcher's completion signal, turned into something a test
// can wait on: every pass the dispatcher finishes lands here as the filter it
// ran under. Tests wait for a pass rather than for a duration, and the
// negative cases assert against a pass they deliberately trigger, so nothing
// in this file waits on elapsed time.
type passes struct {
	ch chan string
}

func newPasses() *passes { return &passes{ch: make(chan string, 64)} }

func (p *passes) record(sessionFilter string) {
	select {
	case p.ch <- sessionFilter:
	default: // a test that stops reading must not block the worker
	}
}

// next waits for the next completed pass. The deadline is a safety net, not
// the normal path: a working dispatcher notifies in milliseconds.
func (p *passes) next(t *testing.T, what string) string {
	t.Helper()
	select {
	case filter := <-p.ch:
		return filter
	case <-time.After(10 * time.Second):
		t.Fatalf("no dispatcher pass within 10s while waiting for %s", what)
		return ""
	}
}

// nextN waits for n completed passes.
func (p *passes) nextN(t *testing.T, n int, what string) {
	t.Helper()
	for i := 0; i < n; i++ {
		p.next(t, fmt.Sprintf("%s (pass %d of %d)", what, i+1, n))
	}
}

// assertQuiet fails if another pass arrives. It proves a kick DIED rather than
// rescheduled, so it has to bound the wait; bound is the only timing value in
// this file and it gates a negative assertion, never a normal success path.
func (p *passes) assertQuiet(t *testing.T, bound time.Duration, what string) {
	t.Helper()
	select {
	case filter := <-p.ch:
		t.Fatalf("unexpected dispatcher pass (filter %q) after %s: the kick rescheduled instead of dying", filter, what)
	case <-time.After(bound):
	}
}

// newNudgeDispatcherFixture builds a city dir with one running fake session
// ("worker"), returning the pieces a dispatcher test needs. The returned
// dispatcher uses shrunk timing knobs, reports its passes through the returned
// recorder, and has already settled its subscription's leading resync pass (it
// runs against an empty queue) so tests observe only what they trigger.
func newNudgeDispatcherFixture(t *testing.T, sp runtime.Provider) (string, *nudgeEventDispatcher, *session.Info, *passes) {
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
	seen := newPasses()
	d.observePasses(seen.record)
	d.update(sp, &config.City{}, true)
	t.Cleanup(func() {
		cancel()
		select {
		case <-d.workerDone:
		case <-time.After(3 * time.Second):
			t.Log("dispatcher worker did not stop within 3s")
		}
	})
	seen.next(t, "the subscription's leading resync pass")
	return dir, d, &info, seen
}

func queueStateSnapshot(t *testing.T, cityPath string) nudgequeue.State {
	t.Helper()
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return state
}

// waitForDeliveredNudge waits for the pass that empties the queue. It rereads
// durable state after each completed pass rather than polling on a timer, so
// the only way it returns false is a dispatcher that stopped running passes.
func waitForDeliveredNudge(t *testing.T, cityPath string, fake *nudgeEventedFake, seen *passes) bool {
	t.Helper()
	for attempts := 0; attempts < 16; attempts++ {
		state := queueStateSnapshot(t, cityPath)
		if len(state.Pending) == 0 && len(state.InFlight) == 0 && countFakeCalls(fake, "Nudge") > 0 {
			return true
		}
		select {
		case <-seen.ch:
		case <-time.After(10 * time.Second):
			return false
		}
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
	dir, _, info, seen := newNudgeDispatcherFixture(t, fake)

	// The agent has been idle well past the quiescence window.
	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake, seen) {
		t.Fatalf("queued nudge not delivered on idle event; state=%+v calls=%v", queueStateSnapshot(t, dir), fake.SnapshotCalls())
	}
}

func TestNudgeEventDispatcherRetriesFreshIdleStamp(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info, seen := newNudgeDispatcherFixture(t, fake)

	// The idle transition was just observed: the activity stamp is fresh, so
	// the first attempt must defer, and the scheduled retry (after the stamp
	// ages past quiescence) must deliver without any further events.
	fake.Activity = map[string]time.Time{info.SessionName: time.Now()}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake, seen) {
		t.Fatalf("queued nudge not delivered by the aged-stamp retry; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherBusyAgentStopsAfterOneRetry(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info, seen := newNudgeDispatcherFixture(t, fake)

	// A working agent reports continuously fresh activity (tracker semantics),
	// so the attempt and its single retry must both reject — and then STOP.
	// A replayed idle event for a busy agent takes exactly this path.
	fake.setBusy(info.SessionName, true)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

	// The attempt and every retry in the budget each run as one pass. Wait for
	// exactly that many, then confirm the kick DIED rather than rescheduling:
	// no further pass, no delivery, and no further observation.
	seen.nextN(t, 1+nudgeEventRetryBudget, "the attempt and its retry budget")
	afterBudget := countFakeCalls(fake, "IsRunning")
	seen.assertQuiet(t, 4*(150*time.Millisecond+30*time.Millisecond), "the retry budget was exhausted")
	if n := countFakeCalls(fake, "IsRunning"); n != afterBudget {
		t.Fatalf("IsRunning kept growing (%d -> %d): the event's attempt+retry must stop, not poll", afterBudget, n)
	}

	state := queueStateSnapshot(t, dir)
	if len(state.Pending) != 1 {
		t.Fatalf("pending = %d, want 1 (busy agent must not receive delivery); state=%+v", len(state.Pending), state)
	}
	if n := countFakeCalls(fake, "Nudge"); n != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for a busy agent", n)
	}
}

func TestNudgeEventDispatcherFullPassDoesNotRefillTheRetryBudget(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info, seen := newNudgeDispatcherFixture(t, fake)

	// The aged-stamp retry budget belongs to an idle EVENT, and that ownership
	// is the whole bound. Full passes are backstop sweeps: a patrol tick fires
	// one every patrol interval and a stream resync fires one too, neither
	// carrying an idle transition. If a sweep granted a fresh budget, a
	// permanently busy session with a queued item would earn a new retry chain
	// every patrol interval, forever — a poll loop wearing this dispatcher's
	// name, which is the thing it was built to retire.
	fake.setBusy(info.SessionName, true)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// Burn the one budget this session is entitled to.
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})
	seen.nextN(t, 1+nudgeEventRetryBudget, "the idle event's attempt and its retry budget")
	quiet := 4 * (150*time.Millisecond + 30*time.Millisecond)
	seen.assertQuiet(t, quiet, "the idle event's retry budget was exhausted")

	// Each sweep now gets exactly one observation pass and schedules no retry.
	for i := 0; i < 3; i++ {
		d.kickAll()
		if filter := seen.next(t, "the backstop sweep"); filter != "" {
			t.Fatalf("sweep %d ran with filter %q, want a full pass", i, filter)
		}
		seen.assertQuiet(t, quiet, fmt.Sprintf("sweep %d must not restart the retry chain", i))
	}

	if n := countFakeCalls(fake, "Nudge"); n != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for a busy agent", n)
	}
	if state := queueStateSnapshot(t, dir); len(state.Pending) != 1 {
		t.Fatalf("pending = %d, want 1 (the item stays queued for a busy agent); state=%+v", len(state.Pending), state)
	}
}

func TestNudgeEventDispatcherDeliversWhenStampLagsEvent(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info, seen := newNudgeDispatcherFixture(t, fake)

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
	// The tracker stamps the transition a beat AFTER the event: the first
	// attempt is what observes the still-busy stamp, so the stamp lands once
	// that attempt has completed rather than after a guessed delay.
	seen.next(t, "the first attempt, which must observe the pre-stamp state")
	fake.setStamp(info.SessionName, time.Now())
	fake.setBusy(info.SessionName, false)

	if !waitForDeliveredNudge(t, dir, fake, seen) {
		t.Fatalf("queued nudge not delivered despite stamp lagging the event; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherResyncRunsFullPass(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info, seen := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// No targeted event: the resync (as replayed by every reconnect) must
	// trigger a full pass that finds the long-idle agent.
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()})

	if !waitForDeliveredNudge(t, dir, fake, seen) {
		t.Fatalf("queued nudge not delivered on resync full pass; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherKickAllDeliversAlreadyIdle(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info, seen := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// The enqueue-time wake ping path: no event fires for an agent that is
	// already idle, so the wake must land as a worker full pass.
	d.kickAll()

	if !waitForDeliveredNudge(t, dir, fake, seen) {
		t.Fatalf("queued nudge not delivered on kickAll; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherEmptyQueueSkipsObservation(t *testing.T) {
	fake := newNudgeEventedFake()
	_, _, info, seen := newNudgeDispatcherFixture(t, fake)

	// Session setup and the settled leading resync account for a baseline of
	// provider calls; an idle event against an EMPTY queue must add none —
	// the pass short-circuits at the queue-state read.
	baseline := countFakeCalls(fake, "IsRunning")
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})
	seen.next(t, "the pass the idle event triggers")

	if n := countFakeCalls(fake, "IsRunning"); n != baseline {
		t.Fatalf("IsRunning calls grew %d -> %d, want no observation for an empty queue (cheap short-circuit)", baseline, n)
	}
}

func TestNudgeEventDispatcherIgnoresNonIdleStatuses(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info, seen := newNudgeDispatcherFixture(t, fake)

	fake.Activity = map[string]time.Time{info.SessionName: time.Now().Add(-10 * time.Second)}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentStateChanged, Session: info.SessionName, Time: time.Now()})
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventExited, Session: info.SessionName, Time: time.Now()})
	// One stream, one forward goroutine, so events are consumed in order: a
	// pass for a session that does not exist proves the two above were read
	// and ignored, without waiting on a clock and without delivering anything.
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "no-such-session", Time: time.Now()})
	if filter := seen.next(t, "the barrier pass"); filter != "no-such-session" {
		t.Fatalf("barrier pass ran with filter %q, want %q: a non-idle event scheduled a pass", filter, "no-such-session")
	}

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
	// streaming() flips on the forward goroutine, which has no completion
	// signal of its own; this is the one boundary here without one, so it
	// polls on a ticker with a bounded deadline rather than a fixed wait.
	waitStreaming := func(want bool) bool {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		deadline := time.After(5 * time.Second)
		for {
			if d.streaming() == want {
				return true
			}
			select {
			case <-tick.C:
			case <-deadline:
				return d.streaming() == want
			}
		}
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

// A delivery cannot be canceled: runtime.Provider.Nudge takes no context and
// the herdr client applies no default timeout, so a wedged pane holds whoever
// called it. Before this dispatcher existed each session had its own sidecar
// poller and a wedged pane stranded only that session's queue. Running every
// delivery on the dispatcher's one scheduler goroutine would have widened that
// to the whole city, which is why each due session gets a goroutine of its own.
//
// This pins the scheduler property directly: with one session's pass parked
// inside the dispatcher, another session's pass still runs to completion.
// Mutation: replace the two spawnPass calls in worker() with d.pass and this
// test times out, because the scheduler never reaches the second session.
func TestNudgeEventDispatcherOneParkedSessionDoesNotBlockAnother(t *testing.T) {
	fake := newNudgeEventedFake()
	_, d, _, _ := newNudgeDispatcherFixture(t, fake)

	const parked = "parked-session"
	const other = "other-session"

	release := make(chan struct{})
	completed := make(chan string, 8)
	d.observePasses(func(sessionFilter string) {
		if sessionFilter == parked {
			<-release
		}
		select {
		case completed <- sessionFilter:
		default:
		}
	})

	d.kickSessionAfter(parked, 0, 0)
	d.kickSessionAfter(other, 0, 0)

	select {
	case got := <-completed:
		if got != other {
			t.Fatalf("first completed pass was %q, want %q: the parked session should not be able to complete while it is held", got, other)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no pass completed within 10s while one session was parked mid-pass: the scheduler is running deliveries serially, so one wedged pane strands every session's queue")
	}

	close(release)
	select {
	case got := <-completed:
		if got != parked {
			t.Fatalf("second completed pass was %q, want %q", got, parked)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked session's pass never completed after release")
	}
}

// A second kick for a session whose pass is already running is dropped rather
// than queued behind it, so a burst of idle events for one session cannot pile
// up goroutines. The running pass re-reads the queue and the observation, so
// the dropped kick carries nothing the running pass will not see.
func TestNudgeEventDispatcherCoalescesKicksForARunningSession(t *testing.T) {
	fake := newNudgeEventedFake()
	_, d, _, _ := newNudgeDispatcherFixture(t, fake)

	const name = "busy-session"
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	completed := make(chan string, 8)
	d.observePasses(func(sessionFilter string) {
		select {
		case completed <- sessionFilter:
		default:
		}
	})

	d.mu.Lock()
	if d.inflight == nil {
		d.inflight = map[string]bool{}
	}
	d.inflight[name] = true
	d.mu.Unlock()

	d.spawnPass(name, 0)
	select {
	case <-entered:
		t.Fatal("spawnPass started a second pass for a session already in flight")
	case <-completed:
		t.Fatal("spawnPass ran a pass for a session already in flight")
	case <-time.After(250 * time.Millisecond):
	}

	d.mu.Lock()
	delete(d.inflight, name)
	d.mu.Unlock()
	close(release)

	d.spawnPass(name, 0)
	select {
	case got := <-completed:
		if got != name {
			t.Fatalf("completed pass was %q, want %q", got, name)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("spawnPass did not run once the session left the in-flight set")
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

// TestNudgeDispatchStoresDeriveBothClassesFromTheCityStore pins the base
// argument, which is the part no other test in this package can see.
//
// The routes below relocate nudges and leave sessions where they are, which is
// a supported configuration: [beads.classes.<name>] is per-class, and
// storageRoutes.storeFor is a per-class map lookup. Under it, the two resolvers
// answer differently for the same base, and a pass that derived one store from
// the other resolves both to the relocated one. Every resolver still agrees with
// the placement plan while that happens, because neither side of that agreement
// can see whose store it was handed.
func TestNudgeDispatchStoresDeriveBothClassesFromTheCityStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	relocatedNudges := beads.NewMemStore()
	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{
		coordclass.ClassNudges: relocatedNudges,
	}}

	nudges, sessStore := nudgeDispatchStores(routes, cityStore, nil, t.TempDir())

	if nudges.Store != beads.Store(relocatedNudges) {
		t.Errorf("nudge store resolved to %p, want the relocated nudges store %p", nudges.Store, relocatedNudges)
	}
	if sessStore == beads.Store(relocatedNudges) {
		t.Fatalf("the session store resolved to the NUDGES store: on a city that relocates nudges alone, every session read in the pass would land in the nudges database")
	}
	if sessStore != beads.Store(cityStore) {
		t.Errorf("session store resolved to %p, want the city store %p", sessStore, cityStore)
	}
}
