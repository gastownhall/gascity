package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
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
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
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

	// nudgeHeld wedges Nudge for a session until the channel is closed, which
	// is the only way to park a delivery mid-flight: the pass observer fires
	// after a pass returns, so it can hold a finished pass but not a hanging
	// one. nudgeEntered reports that a delivery has actually reached Nudge.
	nudgeHeld    map[string]<-chan struct{}
	nudgeEntered chan string
	observeHeld  map[string]<-chan struct{}
}

// holdObserve makes IsRunning for session block until release is closed. The
// sweep's observation goes through IsRunning, and observation is the call that
// still stalled the sweep after the delivery was moved off it, so parking Nudge
// alone proves less than it looks like it does.
func (f *nudgeEventedFake) holdObserve(session string, release <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observeHeld == nil {
		f.observeHeld = map[string]<-chan struct{}{}
	}
	f.observeHeld[session] = release
}

func (f *nudgeEventedFake) IsRunning(name string) bool {
	f.mu.Lock()
	release := f.observeHeld[name]
	f.mu.Unlock()
	if release != nil {
		<-release
	}
	return f.Fake.IsRunning(name)
}

// holdNudge makes Nudge for session block until release is closed, and gives
// the test a channel that reports when a delivery has reached it.
func (f *nudgeEventedFake) holdNudge(session string, release <-chan struct{}) <-chan string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nudgeHeld == nil {
		f.nudgeHeld = map[string]<-chan struct{}{}
	}
	if f.nudgeEntered == nil {
		f.nudgeEntered = make(chan string, 8)
	}
	f.nudgeHeld[session] = release
	return f.nudgeEntered
}

func (f *nudgeEventedFake) Nudge(name string, content []runtime.ContentBlock) error {
	f.mu.Lock()
	release := f.nudgeHeld[name]
	entered := f.nudgeEntered
	f.mu.Unlock()
	if release != nil {
		if entered != nil {
			select {
			case entered <- name:
			default:
			}
		}
		<-release
	}
	return f.Fake.Nudge(name, content)
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
	d := newNudgeEventDispatcher(ctx, dir, testWriter(t), "test", testNudgeDispatchStores(dir))
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

	// A sweep HANDS OFF rather than delivering: one enumerating pass, then one
	// fan-out pass per session with pending work, carrying no retry budget.
	// The claim under test is unchanged and is about the budget, not the pass
	// count: the handed-off pass must schedule nothing further, so a session
	// that is busy forever earns no new retry chain from the sweep.
	for i := 0; i < 3; i++ {
		d.kickAll()
		if filter := seen.next(t, "the enumerating sweep"); filter != "" {
			t.Fatalf("sweep %d ran with filter %q, want the enumerating pass", i, filter)
		}
		if filter := seen.next(t, "the sweep's fan-out"); filter != info.SessionName {
			t.Fatalf("sweep %d fanned out to %q, want %q", i, filter, info.SessionName)
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

	d := newNudgeEventDispatcher(ctx, dir, testWriter(t), "test", testNudgeDispatchStores(dir))
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

// TestNudgeEventDispatcherSweepHandsOffInsteadOfDelivering is the sweep half of
// the isolation claim, and it is the half that was wrong.
//
// A targeted kick needs an idle TRANSITION. A session that is already idle when
// its nudge is queued never emits one, so the backstop sweep is its only path.
// While the sweep delivered inline, one pane hanging inside Nudge held the
// sweep's goroutine, and every later kickAll was dropped as a duplicate of the
// pass that was stuck. Those sessions waited on the hung pane with nothing in
// any log to say so.
//
// The sweep now enumerates and hands each session to a pass of its own, so it
// returns while the wedged delivery runs elsewhere and the next sweep is free
// to start.
func TestNudgeEventDispatcherSweepHandsOffInsteadOfDelivering(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info, seen := newNudgeDispatcherFixture(t, fake)

	release := make(chan struct{})
	entered := fake.holdNudge(info.SessionName, release)
	t.Cleanup(func() { close(release) })

	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake.setBusy(info.SessionName, false)
	// Aged well past the fixture's 150ms quiescence window, so the pass gets
	// through the idle gate and actually reaches Nudge.
	fake.setStamp(info.SessionName, time.Now().Add(-time.Minute))

	d.kickAll()
	if filter := seen.next(t, "the enumerating sweep"); filter != "" {
		t.Fatalf("first pass ran with filter %q, want the enumerating sweep", filter)
	}

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery reached Nudge within 10s; the fixture never handed the session off")
	}

	// The delivery is now wedged. A sweep must still complete: nothing about a
	// hung pane belongs to the sweep any more.
	for i := 0; i < 2; i++ {
		d.kickAll()
		if filter := seen.next(t, "a sweep while a delivery is wedged"); filter != "" {
			t.Fatalf("sweep %d ran with filter %q, want the enumerating sweep: a wedged delivery is stalling the sweep, so an already-idle session's only path is blocked behind whichever pane hung first", i, filter)
		}
	}
}

// A second kick for a session whose pass is already running is deferred
// rather than run concurrently, so a burst of idle events for one session
// cannot pile up goroutines; spawnPass re-arms the deferred kick once the
// running pass clears the key (see spawnPass's doc comment). This drives the
// real d.inflight bookkeeping through d.stores (gated to block the first
// pass mid-flight) rather than hand-setting the map: a test that pokes
// d.inflight directly proves only that spawnPass reads the field it itself
// wrote, not that a genuinely running pass holds the key.
func TestNudgeEventDispatcherCoalescesKicksForARunningSession(t *testing.T) {
	fake := newNudgeEventedFake()
	_, d, _, _ := newNudgeDispatcherFixture(t, fake)

	const name = "busy-session"
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	completed := make(chan string, 8)
	d.observePasses(func(sessionFilter string) {
		select {
		case completed <- sessionFilter:
		default:
		}
	})

	// Gate runPass's store resolution so the FIRST call for this session
	// blocks inside a real pass until the test releases it. Later calls
	// pass straight through immediately: sync.Once.Do would serialize a
	// concurrent second caller behind the first (it blocks until the
	// running Do call returns), which would mask exactly the concurrency
	// bug this test exists to catch, so gate with a non-blocking
	// CompareAndSwap instead.
	d.mu.Lock()
	realStores := d.stores
	var gated int32
	d.stores = func(cfg *config.City) (beads.NudgesStore, beads.Store) {
		if atomic.CompareAndSwapInt32(&gated, 0, 1) {
			close(entered)
			<-release
		}
		return realStores(cfg)
	}
	d.mu.Unlock()

	d.spawnPass(name, 0)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first spawnPass never entered runPass")
	}

	// The session is now genuinely in flight (spawnPass set d.inflight
	// under its own lock before starting the goroutine we're blocking).
	// A second kick for the same session must be deferred, not run.
	d.spawnPass(name, 0)
	select {
	case got := <-completed:
		t.Fatalf("spawnPass ran a concurrent pass for a session already in flight (filter %q)", got)
	case <-time.After(250 * time.Millisecond):
	}

	close(release)

	// The blocked first pass completes, then the deferred second kick is
	// rearmed through the same in-flight release path and runs too.
	for i := 0; i < 2; i++ {
		select {
		case got := <-completed:
			if got != name {
				t.Fatalf("completed pass %d was %q, want %q", i, got, name)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("pass %d did not complete after the in-flight session was released", i)
		}
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

	// Suppression needs BOTH halves: an event-capable provider and a
	// controller actually hosting the dispatcher that replaces the sidecar.
	stubNudgePollerDispatcherLive(t, true)

	maybeStartNudgePoller(target, newNudgeEventedFake())
	if spawns != 0 {
		t.Fatalf("spawns = %d, want 0 for an event-capable provider with a live dispatcher", spawns)
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

// resetNudgePollerLiveCache empties the per-process controller-probe cache, so
// one test's answer does not decide another's.
func resetNudgePollerLiveCache(t *testing.T) {
	t.Helper()
	nudgePollerLiveMu.Lock()
	nudgePollerLiveCache = nil
	nudgePollerLiveMu.Unlock()
	t.Cleanup(func() {
		nudgePollerLiveMu.Lock()
		nudgePollerLiveCache = nil
		nudgePollerLiveMu.Unlock()
	})
}

// stubNudgePollerDispatcherLive answers the controller probe without a
// controller. Left unstubbed, the probe pings a socket that is not there and
// every test would take the fail-open branch.
func stubNudgePollerDispatcherLive(t *testing.T, live bool) {
	t.Helper()
	prev := nudgePollerDispatcherIsLive
	nudgePollerDispatcherIsLive = func(string) bool { return live }
	t.Cleanup(func() { nudgePollerDispatcherIsLive = prev })
}

// TestMaybeStartNudgePollerSpawnsWhenNoDispatcherIsHosting is the other half of
// the suppression rule, and it is the one that matters when things go wrong.
//
// Retiring the sidecar is only safe because something else owns delivery. If
// the controller is not answering, nothing does: suppressing here would leave
// every queued nudge undelivered until a controller came back, with no error
// anywhere, because refusing to spawn looks exactly like the healthy case.
func TestMaybeStartNudgePollerSpawnsWhenNoDispatcherIsHosting(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()

	spawns := 0
	prev := startNudgePoller
	startNudgePoller = func(_, _, _ string) error {
		spawns++
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })
	stubNudgePollerDispatcherLive(t, false)

	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{},
		agent:       config.Agent{Name: "worker"},
		sessionName: "gc-worker",
	}

	maybeStartNudgePoller(target, newNudgeEventedFake())
	if spawns != 1 {
		t.Fatalf("spawns = %d, want 1: with no controller answering, nothing hosts the event dispatcher, so the sidecar must not be retired", spawns)
	}
}

// TestNudgeEventDispatcherUnattributedIdleEventSweeps pins the empty-session
// idle event as a reason to reconcile, not a reason to do nothing.
//
// A provider emits one when it saw an agent go idle and could not map the
// event back to a session. Dropping it is silent: the sidecar that used to
// cover the gap has been retired for this provider class, so the queue then
// waits for the next patrol tick instead of reacting to the transition.
func TestNudgeEventDispatcherUnattributedIdleEventSweeps(t *testing.T) {
	fake := newNudgeEventedFake()
	_, _, _, seen := newNudgeDispatcherFixture(t, fake)

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "", Time: time.Now()})

	if filter := seen.next(t, "the sweep an unattributed idle event must trigger"); filter != "" {
		t.Fatalf("unattributed idle event ran a pass with filter %q, want the full sweep", filter)
	}
}

// TestNudgeEventDispatcherNeverResolvesThroughTheOneShotFunnel pins, for this
// file only, the property class_store_emit.go says is held "by construction":
// the controller never reaches the CLI emit injector.
//
// cliStorageRoutes returns routes carrying a bead.* emit target, which exists
// for a one-shot process that has no emitter of its own. This dispatcher runs
// inside the controller, which has the CachingStore's. A call here would put a
// second emitter on every relocated-class write, and nothing at runtime would
// fail: the event log would simply carry two rows per mutation, worst on the
// reconcile path where the cache re-absorbs rows in bulk. So the check is a
// source scan, the same shape as the injection-site guard next to it.
//
// The scope is deliberately this file. Seventeen other non-test files call
// cliStorageRoutes and each is presumed one-shot; establishing that for all of
// them is a wider audit than this guard claims to have done.
func TestNudgeEventDispatcherNeverResolvesThroughTheOneShotFunnel(t *testing.T) {
	const subject = "nudge_event_dispatcher.go"
	file, err := parser.ParseFile(token.NewFileSet(), subject, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", subject, err)
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "cliStorageRoutes" {
			found = append(found, ident.Name)
		}
		return true
	})
	if len(found) != 0 {
		t.Fatalf("%s calls cliStorageRoutes %d time(s); this dispatcher is controller-hosted, and those routes carry a bead.* emit target the controller must not add on top of its own emitter (class_store_emit.go). Take the stores from the CityRuntime instead.", subject, len(found))
	}
}

// TestNudgeEventDispatcherSweepMakesNoProviderCall is the finding the previous
// version of the sweep test missed, and the reason it missed it is the point.
//
// That test parks Nudge. The sweep genuinely stopped reaching Nudge, so it went
// green, and "the sweep cannot be stalled" looked proven. It was not: the
// enumeration still observed each matched session inline, observation is a
// server call on an uncancellable context, and one hung session still stopped
// the walk before it reached the rest. The test pinned the case it wedged.
//
// This one wedges the observation instead. The sweep must complete anyway,
// because it now reads only the queue and the session beads.
func TestNudgeEventDispatcherSweepMakesNoProviderCall(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info, seen := newNudgeDispatcherFixture(t, fake)

	release := make(chan struct{})
	fake.holdObserve(info.SessionName, release)
	t.Cleanup(func() { close(release) })

	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake.setBusy(info.SessionName, false)
	fake.setStamp(info.SessionName, time.Now().Add(-time.Minute))

	for i := 0; i < 3; i++ {
		d.kickAll()
		if filter := seen.next(t, "a sweep while observation is wedged"); filter != "" {
			t.Fatalf("sweep %d ran with filter %q, want the enumerating sweep: the sweep is making a provider call, so one hung session stops it from reaching the others", i, filter)
		}
	}
}

// TestNudgeEventDispatcherFullKickDuringPassIsNotLost pins the window that
// made TestNudgeEventDispatcherSweepMakesNoProviderCall flaky, and the reason
// that one was flaky rather than simply wrong.
//
// pass() fires the observer BEFORE the spawned goroutine clears d.inflight, and
// worker() clears fullPassDue BEFORE it ever calls spawnPass. A full kick that
// lands between those two points finds the slot still marked, so spawnPass
// returns early while the flag recording "a sweep is owed" has already been
// consumed. The sweep is not deferred and not coalesced. It is dropped, and
// nothing re-arms it.
//
// Racing the worker for that window reproduces roughly one run in eight, which
// is a flake, not a test. So this drives worker()'s own two steps by hand from
// inside the observer, where d.inflight is provably still held: consume the
// flag, then offer the pass. That is the identical call sequence with the
// timing removed, so the case is deterministic and carries no sleeps.
func TestNudgeEventDispatcherFullKickDuringPassIsNotLost(t *testing.T) {
	fake := newNudgeEventedFake()
	_, d, _, seen := newNudgeDispatcherFixture(t, fake)

	var once sync.Once
	d.mu.Lock()
	prev := d.passObserver
	d.passObserver = func(filter string) {
		once.Do(func() {
			d.kickAll()
			d.mu.Lock()
			d.fullPassDue = false
			d.mu.Unlock()
			d.spawnPass("", 0)
		})
		if prev != nil {
			prev(filter)
		}
	}
	d.mu.Unlock()

	d.kickAll()
	seen.next(t, "the first sweep")
	seen.next(t, "the sweep that was kicked while the first pass still held the slot")
}

// TestNudgeEventDispatcherBoundsUnattributedSweeps keeps the fix for the
// dropped unattributed event from becoming its own poll loop. One sweep covers
// every session, so a wave of unattributable events and a single one need the
// same work; without a bound, each event bought a full walk of every open
// session.
func TestNudgeEventDispatcherBoundsUnattributedSweeps(t *testing.T) {
	fake := newNudgeEventedFake()
	_, d, _, seen := newNudgeDispatcherFixture(t, fake)

	now := time.Now()
	d.kickAllUnattributed(now)
	if filter := seen.next(t, "the first unattributed event's sweep"); filter != "" {
		t.Fatalf("first unattributed sweep ran with filter %q, want a full sweep", filter)
	}

	for i := 0; i < 5; i++ {
		d.kickAllUnattributed(now.Add(time.Duration(i) * time.Millisecond))
	}
	seen.assertQuiet(t, time.Second, "a burst of unattributed events inside the interval must buy no further sweep")

	// Past the interval, the next one is reconciled again.
	d.kickAllUnattributed(now.Add(nudgeEventUnattributedSweepInterval + time.Millisecond))
	if filter := seen.next(t, "the sweep past the coalescing interval"); filter != "" {
		t.Fatalf("post-interval sweep ran with filter %q, want a full sweep", filter)
	}
}

// TestNudgePollerDispatcherProbeIsAnsweredOncePerProcess bounds what the
// round-1 liveness check costs. controllerAlive carries a 2s read deadline and
// gc prime asks once per resolved agent, so an unbounded probe turns a degraded
// controller into seconds of hook latency per agent.
func TestNudgePollerDispatcherProbeIsAnsweredOncePerProcess(t *testing.T) {
	dir := t.TempDir()
	probes := 0
	prevAlive := controllerAliveForNudgePoller
	controllerAliveForNudgePoller = func(string) int {
		probes++
		return 4242
	}
	t.Cleanup(func() { controllerAliveForNudgePoller = prevAlive })
	resetNudgePollerLiveCache(t)

	for i := 0; i < 5; i++ {
		if !nudgePollerDispatcherIsLive(dir) {
			t.Fatalf("probe %d reported no controller despite a live pid", i)
		}
	}
	if probes != 1 {
		t.Fatalf("probes = %d, want 1: the controller ping is answered once per city per process", probes)
	}
}

// TestAwaitNudgeEventsDownWaitsThenGivesUp covers the wait shutdown now performs
// before it stops sessions or tears down the provider.
//
// The dispatcher bounds its own delivery drain, but that bound meant nothing
// while shutdown ran concurrently with it: a delivery lost its provider
// immediately rather than after the advertised grace. The wait is itself
// bounded for the mirror-image reason, so a dispatcher that will not come down
// cannot hold city shutdown open instead.
func TestAwaitNudgeEventsDownWaitsThenGivesUp(t *testing.T) {
	cr := &CityRuntime{stderr: testWriter(t), logPrefix: "test"}

	// No dispatcher: nothing to wait for.
	cr.awaitNudgeEventsDown()

	cr.nudgeEvents = &nudgeEventDispatcher{workerDone: make(chan struct{})}
	returned := make(chan struct{})
	go func() {
		cr.awaitNudgeEventsDown()
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("awaitNudgeEventsDown returned while the scheduler was still up: shutdown would tear the provider down under a live delivery")
	case <-time.After(250 * time.Millisecond):
	}

	close(cr.nudgeEvents.workerDone)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("awaitNudgeEventsDown did not return after the scheduler came down")
	}
}

func TestProviderRetiresNudgePollers(t *testing.T) {
	target := nudgeTarget{sessionName: "gc-worker"}
	if providerRetiresNudgePollers(target, nil) {
		t.Fatal("nil provider must not retire pollers")
	}
	if providerRetiresNudgePollers(target, runtime.NewFake()) {
		t.Fatal("plain provider must not retire pollers")
	}
	if !providerRetiresNudgePollers(target, newNudgeEventedFake()) {
		t.Fatal("event-capable provider must retire pollers")
	}
}

// routedFake implements runtime.EventCapableRouter to simulate a composite provider
// (e.g. hybrid) whose top-level SessionEventProvider assertion is true
// (because SOME routed backend is event-capable) but whose per-session
// routing decision differs — regression coverage for gc-ey9vgx finding 3:
// a hybrid provider must not suppress the sidecar poller for sessions it
// routes to a non-event-capable backend.
type routedFake struct {
	runtime.Provider
	eventCapableFor map[string]bool
}

var (
	_ runtime.SessionEventProvider = (*routedFake)(nil)
	_ runtime.EventCapableRouter   = (*routedFake)(nil)
)

func (r *routedFake) SubscribeSessionEvents(_ context.Context) (<-chan runtime.SessionEvent, error) {
	ch := make(chan runtime.SessionEvent)
	close(ch)
	return ch, nil
}

func (r *routedFake) EventCapableRoute(name string) bool {
	return r.eventCapableFor[name]
}

func TestProviderRetiresNudgePollersChecksPerTargetRoute(t *testing.T) {
	sp := &routedFake{
		Provider:        runtime.NewFake(),
		eventCapableFor: map[string]bool{"gc-local": true},
	}
	if !providerRetiresNudgePollers(nudgeTarget{sessionName: "gc-local"}, sp) {
		t.Fatal("session routed to the event-capable backend must retire its poller")
	}
	if providerRetiresNudgePollers(nudgeTarget{sessionName: "gc-remote"}, sp) {
		t.Fatal("session routed to a non-event-capable backend must NOT have its poller suppressed")
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
// testNudgeDispatchStores stands in for what the CityRuntime hands the
// dispatcher in production. A test process is one-shot, so resolving through
// the CLI funnel here is the correct construction for it; the production
// wiring deliberately does not, because the controller already has an emitter
// (class_store_emit.go).
func testNudgeDispatchStores(cityPath string) func(*config.City) (beads.NudgesStore, beads.Store) {
	return func(cfg *config.City) (beads.NudgesStore, beads.Store) {
		cityStore, err := openStoreAtForCity(cityPath, cityPath)
		if err != nil || cityStore == nil {
			return beads.NudgesStore{}, nil
		}
		return nudgeDispatchStores(cliStorageRoutes(cityPath), cityStore, cfg, cityPath, nil)
	}
}

func TestNudgeDispatchStoresDeriveBothClassesFromTheCityStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	relocatedNudges := beads.NewMemStore()
	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{
		coordclass.ClassNudges: relocatedNudges,
	}}

	nudges, sessStore := nudgeDispatchStores(routes, cityStore, nil, t.TempDir(), nil)

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

// TestNudgeEventDispatcherBoundsConcurrentPasses covers the cost the previous
// commit uncovered. The sweep used to observe each matched session inline, and
// that serial provider call was throttling the fan-out by accident. With the
// observation gone the sweep hands off every due session at once, and each
// handed-off pass re-reads the session beads, re-runs the queue's maintenance
// sweep and rescans every open session, so a backlog of N sessions would be N
// simultaneous store reads and 2N queue-flock acquisitions.
func TestNudgeEventDispatcherBoundsConcurrentPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := newNudgeEventDispatcher(ctx, t.TempDir(), testWriter(t), "test", nil)
	d.passSlots = make(chan struct{}, 2)
	d.passSlotGrace = time.Hour

	var mu sync.Mutex
	livePasses, peak := 0, 0
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	d.mu.Lock()
	d.passObserver = func(string) {
		mu.Lock()
		livePasses++
		if livePasses > peak {
			peak = livePasses
		}
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		livePasses--
		mu.Unlock()
	}
	d.mu.Unlock()
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	for i := 0; i < 5; i++ {
		d.spawnPass(fmt.Sprintf("s-gc-%d", i), 0)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d passes started; the slot bound must throttle the fan-out, never deadlock it", i)
		}
	}
	select {
	case <-entered:
		t.Fatal("a third pass ran while both slots were held: the fan-out is unbounded, so a backlog of N sessions costs N simultaneous store reads")
	case <-time.After(300 * time.Millisecond):
	}

	releaseAll()
	for i := 2; i < 5; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("pass %d never ran once the slots were freed", i)
		}
	}
	mu.Lock()
	got := peak
	mu.Unlock()
	if got > 2 {
		t.Fatalf("peak concurrent passes = %d, want at most 2", got)
	}
}

// TestNudgeEventDispatcherWedgedPassSurrendersItsSlot is the half that keeps
// the throttle from becoming the defect it replaces. A plain semaphore would
// let a pass wedged inside an uncancellable provider call hold its slot for the
// life of the process, and enough of those would stall every other session
// again, which is the head-of-line blocking the fan-out exists to end.
func TestNudgeEventDispatcherWedgedPassSurrendersItsSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := newNudgeEventDispatcher(ctx, t.TempDir(), testWriter(t), "test", nil)
	d.passSlots = make(chan struct{}, 1)
	d.passSlotGrace = 50 * time.Millisecond

	wedge := make(chan struct{})
	t.Cleanup(func() { close(wedge) })
	entered := make(chan string, 4)
	d.mu.Lock()
	d.passObserver = func(filter string) {
		entered <- filter
		if filter == "s-gc-wedged" {
			<-wedge
		}
	}
	d.mu.Unlock()

	d.spawnPass("s-gc-wedged", 0)
	select {
	case filter := <-entered:
		if filter != "s-gc-wedged" {
			t.Fatalf("first pass ran with filter %q, want the wedged one", filter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wedged pass never started")
	}

	d.spawnPass("s-gc-healthy", 0)
	select {
	case filter := <-entered:
		if filter != "s-gc-healthy" {
			t.Fatalf("second pass ran with filter %q, want the healthy one", filter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the healthy session never got a pass: a wedged pass is holding its slot for the life of the process, which is the head-of-line blocking the fan-out exists to end")
	}
}

// TestProviderRetiresNudgePollersChecksPerTargetRoute_Auto is the production
// counterpart to the routedFake case above: it proves the REAL
// internal/runtime/auto.Provider (not a hand-rolled test double) is asked
// per-session too. Before EventCapableRoute existed on auto.Provider, its
// SubscribeSessionEvents method made the bare runtime.SessionEventProvider
// type assertion in providerRetiresNudgePollers true unconditionally
// (auto.Provider always implements the method, even when neither backend
// does), so an ACP-routed session behind an event-capable default backend
// had its sidecar poller suppressed even though the ACP backend cannot
// deliver its events.
func TestProviderRetiresNudgePollersChecksPerTargetRoute_Auto(t *testing.T) {
	acp := runtime.NewFake()
	sp := sessionauto.New(newNudgeEventedFake(), acp)
	sp.RouteACP("gc-acp")

	if !providerRetiresNudgePollers(nudgeTarget{sessionName: "gc-default"}, sp) {
		t.Fatal("session routed to the event-capable default backend must retire its poller")
	}
	if providerRetiresNudgePollers(nudgeTarget{sessionName: "gc-acp"}, sp) {
		t.Fatal("session routed to the non-event-capable ACP backend must NOT have its poller suppressed")
	}
}

// TestCityRuntimeEnsureNudgeWakeListenerActivatesOnReload proves
// ensureNudgeWakeListener's gate logic: a plain provider under legacy
// dispatcher mode starts no listener, but swapping in an event-capable
// provider (what a config reload does) opens one.
func TestCityRuntimeEnsureNudgeWakeListenerActivatesOnReload(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	cr := &CityRuntime{
		cfg:         &config.City{}, // legacy dispatcher mode (nudgeDispatcherIsSupervisor == false)
		cityPath:    dir,
		stderr:      testWriter(t),
		logPrefix:   "test",
		nudgeWakeCh: make(chan struct{}, 1),
	}
	cr.nudgeEvents = newNudgeEventDispatcher(ctx, dir, cr.stderr, cr.logPrefix, testNudgeDispatchStores(dir))
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
	ctx, cancel := context.WithCancel(context.Background())

	cr := &CityRuntime{
		cfg:         &config.City{},
		cityPath:    dir,
		stderr:      testWriter(t),
		logPrefix:   "test",
		nudgeWakeCh: make(chan struct{}, 1),
	}
	cr.nudgeEvents = newNudgeEventDispatcher(ctx, dir, cr.stderr, cr.logPrefix, testNudgeDispatchStores(dir))
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
	if !testWakeSocketIsHosting(dir) {
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
	if testWakeSocketIsHosting(dir) {
		t.Fatal("socket still answers after the wake listener was torn down; a deferred submit would wrongly suppress its fallback poller")
	}
}

// testWakeSocketIsHosting dials the supervisor nudge wake socket the same
// way pingNudgeWakeSocket does, reporting whether a listener answered.
func testWakeSocketIsHosting(cityPath string) bool {
	conn, err := net.DialTimeout("unix", nudgequeue.WakeSocketPath(cityPath), pingNudgeWakeSocketDialTimeout)
	if err != nil {
		return false
	}
	defer conn.Close() //nolint:errcheck // best-effort test probe
	return true
}

// TestCityRuntimeReloadConfigTracedActivatesNudgeWakeListener drives
// reloadConfigTraced itself, not a hand-rolled stand-in for it. The unit
// tests above assert the gate logic in ensureNudgeWakeListener is correct;
// this one asserts reloadConfigTraced actually calls it. A version of
// reloadConfigTraced that dropped that call would leave the unit tests above
// passing, because they invoke cr.ensureNudgeWakeListener directly rather
// than going through reloadConfigTraced -- exactly the gap this test closes.
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
	cr.nudgeEvents = newNudgeEventDispatcher(ctx, cityPath, cr.stderr, cr.logPrefix, testNudgeDispatchStores(cityPath))
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
	// unlike the unit tests above, nothing here calls
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
