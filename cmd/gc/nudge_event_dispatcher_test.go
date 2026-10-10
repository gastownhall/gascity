package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func testWriter(t *testing.T) testLogWriter { return testLogWriter{t: t} }

type legacyNudgeDispatchProbeStore struct {
	beads.Store
	listCalls atomic.Int32
}

func (s *legacyNudgeDispatchProbeStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls.Add(1)
	return s.Store.List(query)
}

type nudgeEventedFake struct {
	*runtime.Fake

	mu           sync.Mutex
	subs         []chan runtime.SessionEvent
	busySessions map[string]bool
	stamps       map[string]time.Time

	nudgeHeld    map[string]<-chan struct{}
	nudgeEntered chan string
	observeHeld  map[string]<-chan struct{}
}

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

func (f *nudgeEventedFake) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) { //nolint:unparam
	ch := make(chan runtime.SessionEvent, 32)
	ch <- runtime.SessionEvent{Kind: runtime.SessionEventResync, Time: time.Now()}
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, sub := range f.subs {
			if sub == ch {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}()
	return ch, nil
}

func (f *nudgeEventedFake) endStreams() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, sub := range f.subs {
		close(sub)
	}
	f.subs = nil
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

func (f *nudgeEventedFake) setBusy(name string, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busySessions[name] = busy
}

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

type passes struct {
	ch chan string
}

func newPasses() *passes { return &passes{ch: make(chan string, 64)} }

func (p *passes) record(sessionFilter string) {
	select {
	case p.ch <- sessionFilter:
	default:
	}
}

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

func (p *passes) nextN(t *testing.T, n int, what string) {
	t.Helper()
	for i := 0; i < n; i++ {
		p.next(t, fmt.Sprintf("%s (pass %d of %d)", what, i+1, n))
	}
}

func (p *passes) nextSet(t *testing.T, want []string, what string) {
	t.Helper()
	got := make([]string, 0, len(want))
	for i := range want {
		got = append(got, p.next(t, fmt.Sprintf("%s (pass %d of %d)", what, i+1, len(want))))
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Fatalf("%s: pass filters = %q, want %q in any order", what, got, sorted)
	}
}

func (p *passes) assertQuiet(t *testing.T, bound time.Duration, what string) {
	t.Helper()
	select {
	case filter := <-p.ch:
		t.Fatalf("unexpected dispatcher pass (filter %q) after %s: the kick rescheduled instead of dying", filter, what)
	case <-time.After(bound):
	}
}

func attachNudgeEventPump(ctx context.Context, d *nudgeEventDispatcher, sp runtime.Provider, stderr io.Writer) *sessionEventPump {
	pump := newSessionEventPump(ctx, newLegacyWake(make(chan struct{}, 1), nil), stderr, "test")
	pump.observe = d.handleEvent
	d.update(sp, &config.City{})
	d.setEventCapable(pump.restart(sp))
	return pump
}

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
	attachNudgeEventPump(ctx, d, sp, testWriter(t))
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

	fake.setBusy(info.SessionName, true)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})

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

func TestNudgeEventDispatcherFullPassArmsOnlyOneBoundedRetry(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info, seen := newNudgeDispatcherFixture(t, fake)

	fake.setBusy(info.SessionName, true)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})
	seen.nextN(t, 1+nudgeEventRetryBudget, "the idle event's attempt and its retry budget")
	quiet := 4 * (150*time.Millisecond + 30*time.Millisecond)
	seen.assertQuiet(t, quiet, "the idle event's retry budget was exhausted")

	for i := 0; i < 3; i++ {
		d.kickAll()
		seen.nextSet(t, []string{"", info.SessionName, info.SessionName}, fmt.Sprintf("sweep %d, its fan-out and the fan-out's single bounded retry", i))
		seen.assertQuiet(t, quiet, fmt.Sprintf("sweep %d must not restart the retry chain", i))
	}

	if n := countFakeCalls(fake, "Nudge"); n != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for a busy agent", n)
	}
	if state := queueStateSnapshot(t, dir); len(state.Pending) != 1 {
		t.Fatalf("pending = %d, want 1 (the item stays queued for a busy agent); state=%+v", len(state.Pending), state)
	}
}

func TestNudgeEventDispatcherSweepRetriesAnAgentThatJustWentIdle(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, d, info, seen := newNudgeDispatcherFixture(t, fake)
	d.quiescence = 2 * time.Second

	fake.setStamp(info.SessionName, time.Now())
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	d.kickAll()
	seen.nextSet(t, []string{"", info.SessionName}, "the sweep and its fan-out")
	if n := countFakeCalls(fake, "Nudge"); n != 0 {
		t.Fatalf("Nudge calls = %d before quiescence elapsed, want 0", n)
	}

	if filter := seen.next(t, "the fan-out's retry once quiescence elapses"); filter != info.SessionName {
		t.Fatalf("retry ran with filter %q, want %q", filter, info.SessionName)
	}
	state := queueStateSnapshot(t, dir)
	if len(state.Pending) != 0 || len(state.InFlight) != 0 || countFakeCalls(fake, "Nudge") != 1 {
		t.Fatalf("queued nudge not delivered by the sweep's retry; state=%+v calls=%v", state, fake.SnapshotCalls())
	}
}

func TestNudgeEventDispatcherDeliversWhenStampLagsEvent(t *testing.T) {
	fake := newNudgeEventedFake()
	dir, _, info, seen := newNudgeDispatcherFixture(t, fake)

	fake.setBusy(info.SessionName, true)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: info.SessionName, Time: time.Now()})
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

	d.kickAll()

	if !waitForDeliveredNudge(t, dir, fake, seen) {
		t.Fatalf("queued nudge not delivered on kickAll; state=%+v", queueStateSnapshot(t, dir))
	}
}

func TestNudgeEventDispatcherEmptyQueueSkipsObservation(t *testing.T) {
	fake := newNudgeEventedFake()
	_, _, info, seen := newNudgeDispatcherFixture(t, fake)

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
	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "no-such-session", Time: time.Now()})
	if filter := seen.next(t, "the barrier pass"); filter != "no-such-session" {
		t.Fatalf("barrier pass ran with filter %q, want %q: a non-idle event scheduled a pass", filter, "no-such-session")
	}

	if n := countFakeCalls(fake, "Nudge"); n != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for non-idle statuses", n)
	}
}

func TestNudgeEventDispatcherActivationFollowsSessionEventPump(t *testing.T) {
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

	pump := attachNudgeEventPump(ctx, d, runtime.NewFake(), testWriter(t))
	if d.active() {
		t.Fatal("active() = true for a provider without an event stream")
	}

	evented := newNudgeEventedFake()
	d.update(evented, &config.City{})
	d.setEventCapable(pump.restart(evented))
	if !d.active() {
		t.Fatal("active() = false after the pump subscribed to an event-capable provider")
	}

	d.update(evented, &config.City{})
	if !d.active() {
		t.Fatal("cfg-only update deactivated the dispatcher")
	}

	plain := runtime.NewFake()
	d.update(plain, &config.City{})
	d.setEventCapable(pump.restart(plain))
	if d.active() {
		t.Fatal("active() = true after swapping back to a plain provider")
	}
}

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

	for i := 0; i < 2; i++ {
		d.kickAll()
		if filter := seen.next(t, "a sweep while a delivery is wedged"); filter != "" {
			t.Fatalf("sweep %d ran with filter %q, want the enumerating sweep: a wedged delivery is stalling the sweep, so an already-idle session's only path is blocked behind whichever pane hung first", i, filter)
		}
	}
}

func TestNudgeEventDispatcherSweepStopsSpawningAfterParentCancel(t *testing.T) {
	fake := newNudgeEventedFake()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	createSession := func(template string) session.Info {
		info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: template, Title: template, Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
		if err != nil {
			t.Fatalf("CreateSession(%s): %v", template, err)
		}
		if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
			t.Fatalf("Start(%s): %v", template, err)
		}
		return info
	}
	wedgedInfo := createSession("wedged")
	laterInfo := createSession("later")

	ctx, cancel := context.WithCancel(context.Background())
	d := newNudgeEventDispatcher(ctx, dir, testWriter(t), "test", testNudgeDispatchStores(dir))
	d.quiescence = 150 * time.Millisecond
	d.retryEpsilon = 30 * time.Millisecond
	seen := newPasses()
	d.observePasses(seen.record)
	attachNudgeEventPump(ctx, d, fake, testWriter(t))
	workerDone := d.workerDone
	t.Cleanup(func() {
		cancel()
		select {
		case <-workerDone:
		case <-time.After(3 * time.Second):
			t.Log("dispatcher worker did not stop within 3s")
		}
	})
	seen.next(t, "the subscription's leading resync pass")

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWedged := func() { releaseOnce.Do(func() { close(release) }) }
	entered := fake.holdNudge(wedgedInfo.SessionName, release)
	t.Cleanup(releaseWedged)

	if err := enqueueQueuedNudge(dir, newQueuedNudge("wedged", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge(wedged): %v", err)
	}
	fake.setBusy(wedgedInfo.SessionName, false)
	fake.setStamp(wedgedInfo.SessionName, time.Now().Add(-time.Minute))

	d.kickAll()
	if filter := seen.next(t, "the enumerating sweep"); filter != "" {
		t.Fatalf("sweep ran with filter %q, want the enumerating sweep", filter)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no delivery reached Nudge within 10s; wedged session was never handed off")
	}

	cancel()

	if err := enqueueQueuedNudge(dir, newQueuedNudge("later", "wait satisfied: proceed", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge(later): %v", err)
	}
	fake.setBusy(laterInfo.SessionName, false)
	fake.setStamp(laterInfo.SessionName, time.Now().Add(-time.Minute))

	d.runPass("", 0)

	select {
	case got := <-seen.ch:
		t.Fatalf("a sweep ran after parent cancellation (filter %q); the post-cancel session must never be spawned", got)
	case <-time.After(300 * time.Millisecond):
	}

	releaseWedged()
	if filter := seen.next(t, "the wedged session's own delivery pass"); filter != wedgedInfo.SessionName {
		t.Fatalf("completed pass had filter %q, want %q", filter, wedgedInfo.SessionName)
	}

	if n := countFakeCalls(fake, "Nudge"); n != 1 {
		t.Fatalf("Nudge calls = %d, want 1 (only the pre-cancel wedged session); a post-cancel sweep spawned new delivery work", n)
	}
	state := queueStateSnapshot(t, dir)
	if len(state.Pending) == 0 {
		t.Fatal("the post-cancel nudge for 'later' was consumed by a sweep that should have refused to run after cancellation")
	}
}

func TestNudgeEventDispatcherWorkerStopsAtEntryWhenParentAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := &nudgeEventDispatcher{
		parent:     ctx,
		cityPath:   t.TempDir(),
		stderr:     testWriter(t),
		logPrefix:  "test",
		pending:    make(map[string]nudgeEventKick),
		kicked:     make(chan struct{}, 1),
		workerDone: make(chan struct{}),

		fullPassDue: true,
	}
	observed := make(chan string, 1)
	d.observePasses(func(sessionFilter string) { observed <- sessionFilter })

	d.worker(ctx)

	select {
	case <-d.workerDone:
	default:
		t.Fatal("worker() returned without closing workerDone")
	}
	select {
	case filter := <-observed:
		t.Fatalf("a pass ran (filter %q) even though the parent was already canceled before worker() started", filter)
	case <-time.After(200 * time.Millisecond):
	}
}

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

	d.spawnPass(name, 0)
	select {
	case got := <-completed:
		t.Fatalf("spawnPass ran a concurrent pass for a session already in flight (filter %q)", got)
	case <-time.After(250 * time.Millisecond):
	}

	close(release)

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

	stubNudgePollerDispatcherLive(t, true)

	maybeStartNudgePoller(target, newNudgeEventedFake())
	if spawns != 0 {
		t.Fatalf("spawns = %d, want 0 for an event-capable provider with a live dispatcher", spawns)
	}

	maybeStartNudgePoller(target, runtime.NewFake())
	if spawns != 1 {
		t.Fatalf("spawns = %d, want 1 for a plain provider", spawns)
	}

	maybeStartNudgePoller(target, nil)
	if spawns != 2 {
		t.Fatalf("spawns = %d, want 2 for a nil provider", spawns)
	}
}

func stubNudgePollerDispatcherLive(t *testing.T, live bool) {
	t.Helper()
	prev := nudgePollerDispatcherIsLive
	nudgePollerDispatcherIsLive = func(string) bool { return live }
	t.Cleanup(func() { nudgePollerDispatcherIsLive = prev })
}

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

func TestNudgePollerDispatcherRejectsLiveControllerWithoutWakeListener(t *testing.T) {
	dir := shortSocketTempDir(t, "nudge-dispatcher-")
	startFakeControllerSocket(t, dir, "4242\n")

	if controllerAlive(dir) == 0 {
		t.Fatal("precondition: controller socket is not answering")
	}
	if nudgePollerDispatcherIsLive(dir) {
		t.Fatal("controller liveness was mistaken for dispatcher liveness without a wake listener")
	}
}

func TestNudgeEventDispatcherUnattributedIdleEventSweeps(t *testing.T) {
	fake := newNudgeEventedFake()
	_, _, _, seen := newNudgeDispatcherFixture(t, fake)

	fake.emit(runtime.SessionEvent{Kind: runtime.SessionEventAgentIdle, Session: "", Time: time.Now()})

	if filter := seen.next(t, "the sweep an unattributed idle event must trigger"); filter != "" {
		t.Fatalf("unattributed idle event ran a pass with filter %q, want the full sweep", filter)
	}
}

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

	d.kickAllUnattributed(now.Add(nudgeEventUnattributedSweepInterval + time.Millisecond))
	if filter := seen.next(t, "the sweep past the coalescing interval"); filter != "" {
		t.Fatalf("post-interval sweep ran with filter %q, want a full sweep", filter)
	}
}

func TestAwaitNudgeEventsDownWaitsThenGivesUp(t *testing.T) {
	cr := &CityRuntime{stderr: testWriter(t), logPrefix: "test"}

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

func TestAwaitNudgeEventsDownStopsALiveDispatcher(t *testing.T) {
	cr := &CityRuntime{stderr: testWriter(t), logPrefix: "test"}
	cr.nudgeEvents = newNudgeEventDispatcher(context.Background(), t.TempDir(), testWriter(t), "test", nil)

	returned := make(chan struct{})
	go func() {
		cr.awaitNudgeEventsDown()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(nudgeEventDeliveryDrainGrace):
		t.Fatal("awaitNudgeEventsDown waited out its grace on a dispatcher whose parent context is still live: shutdown must stop it, not only wait")
	}
	select {
	case <-cr.nudgeEvents.workerDone:
	default:
		t.Fatal("dispatcher worker still running after awaitNudgeEventsDown returned")
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

type routedFake struct {
	runtime.Provider
	routes map[string]runtime.Route
}

var (
	_ runtime.SessionEventProvider = (*routedFake)(nil)
	_ runtime.Router               = (*routedFake)(nil)
)

func (r *routedFake) SubscribeSessionEvents(_ context.Context) (<-chan runtime.SessionEvent, error) {
	ch := make(chan runtime.SessionEvent)
	close(ch)
	return ch, nil
}

func (r *routedFake) RouteFor(name string) runtime.Route {
	route, ok := r.routes[name]
	if !ok {
		return runtime.Route{Backend: runtime.Backend{Provider: runtime.NewFake()}}
	}
	return route
}

func TestProviderRetiresNudgePollersChecksPerTargetRoute(t *testing.T) {
	sp := &routedFake{
		Provider: runtime.NewFake(),
		routes: map[string]runtime.Route{
			"gc-local": {
				Backend: runtime.Backend{Provider: newNudgeEventedFake()},
				Known:   true,
			},
			"gc-remote": {
				Backend: runtime.Backend{Provider: runtime.NewFake()},
				Known:   true,
			},
		},
	}
	if !providerRetiresNudgePollers(nudgeTarget{sessionName: "gc-local"}, sp) {
		t.Fatal("session routed to the event-capable backend must retire its poller")
	}
	if providerRetiresNudgePollers(nudgeTarget{sessionName: "gc-remote"}, sp) {
		t.Fatal("session routed to a non-event-capable backend must NOT have its poller suppressed")
	}
}

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

func TestNudgeEventDispatcherBoundsConcurrentPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := newNudgeEventDispatcher(ctx, t.TempDir(), testWriter(t), "test", nil)
	d.passSlots = make(chan struct{}, 2)

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

func TestNudgeEventDispatcherWedgedPassRetainsItsSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := newNudgeEventDispatcher(ctx, t.TempDir(), testWriter(t), "test", nil)
	d.passSlots = make(chan struct{}, 1)

	wedge := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(wedge) }) }
	t.Cleanup(release)
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
		t.Fatalf("pass %q ran while the only slot was held", filter)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case filter := <-entered:
		if filter != "s-gc-healthy" {
			t.Fatalf("second pass ran with filter %q, want the healthy one", filter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the healthy session never ran after the slot was released")
	}
}

func TestNudgeEventDispatcherWaitingPassStopsAfterParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := newNudgeEventDispatcher(ctx, t.TempDir(), testWriter(t), "test", nil)
	d.passSlots = make(chan struct{}, 1)

	firstEntered := make(chan struct{})
	firstRelease := make(chan struct{})
	secondRan := make(chan struct{})
	d.observePasses(func(filter string) {
		if filter == "first" {
			close(firstEntered)
			<-firstRelease
			return
		}
		close(secondRan)
	})

	d.spawnPass("first", 0)
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first pass never acquired the only slot")
	}

	d.spawnPass("second", 0)
	cancel()
	close(firstRelease)

	select {
	case <-d.workerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not finish after cancellation")
	}
	select {
	case <-secondRan:
		t.Fatal("pass waiting for a slot ran after parent cancellation")
	default:
	}
}

func TestProviderRetiresNudgePollersChecksPerTargetRoute_Auto(t *testing.T) {
	acp := runtime.NewFake()
	sp := sessionauto.New(newNudgeEventedFake(), acp)
	sp.RouteACP("gc-acp")
	sp.SeedRoutes([]string{"gc-acp"})

	if !providerRetiresNudgePollers(nudgeTarget{sessionName: "gc-default"}, sp) {
		t.Fatal("session routed to the event-capable default backend must retire its poller")
	}
	if providerRetiresNudgePollers(nudgeTarget{sessionName: "gc-acp"}, sp) {
		t.Fatal("session routed to the non-event-capable ACP backend must NOT have its poller suppressed")
	}
}

func TestCityRuntimeEnsureNudgeWakeListenerActivatesOnReload(t *testing.T) {
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

	cr.nudgeEvents.update(runtime.NewFake(), cr.cfg)
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener != nil {
		t.Fatal("wake listener started for a non-event provider under legacy dispatcher mode, want none")
	}

	cr.nudgeEvents.update(newNudgeEventedFake(), cr.cfg)
	cr.nudgeEvents.setEventCapable(true)
	if !cr.nudgeEvents.active() {
		t.Fatal("precondition: dispatcher must report active() after swapping to an event-capable provider")
	}
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener == nil {
		t.Fatal("wake listener was not started after a reload made the provider event-capable")
	}
}

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

	cr.nudgeEvents.update(newNudgeEventedFake(), cr.cfg)
	cr.nudgeEvents.setEventCapable(true)
	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener == nil {
		t.Fatal("precondition: wake listener did not start for an event-capable provider")
	}
	if !testWakeSocketIsHosting(dir) {
		t.Fatal("precondition: socket must answer while the listener is up")
	}

	cr.nudgeEvents.update(runtime.NewFake(), cr.cfg)
	cr.nudgeEvents.setEventCapable(false)
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

func testWakeSocketIsHosting(cityPath string) bool {
	conn, err := net.DialTimeout("unix", nudgequeue.WakeSocketPath(cityPath), pingNudgeWakeSocketDialTimeout)
	if err != nil {
		return false
	}
	defer conn.Close() //nolint:errcheck
	return true
}

func TestCityRuntimeReloadConfigTracedActivatesNudgeWakeListener(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := newNudgeEventedFake()
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
	cr.nudgeEvents.update(cr.sp, cr.cfg)
	cr.sessionEvents = newSessionEventPump(ctx, newLegacyWake(make(chan struct{}, 1), nil), cr.stderr, cr.logPrefix)
	cr.sessionEvents.observe = cr.nudgeEvents.handleEvent

	cr.ensureNudgeWakeListener(ctx)
	if cr.nudgeWakeListener != nil {
		t.Fatal("wake listener started before the dispatcher saw an event stream under legacy dispatcher mode, want none")
	}

	if !cr.sessionEvents.restart(cr.sp) {
		t.Fatal("precondition: the session-event pump must subscribe to the event-capable provider")
	}
	lastProviderName := "fake"
	reply := cr.reloadConfigTraced(ctx, &lastProviderName, cityPath, nil, reloadSourceManual)
	if reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reloadConfigTraced failed: %s", reply.Error)
	}
	if !cr.nudgeEvents.active() {
		t.Fatal("dispatcher must report active() after reloadConfigTraced observes the streaming session-event pump")
	}
	if cr.nudgeWakeListener == nil {
		t.Fatal("reloadConfigTraced did not start the wake listener after the provider became event-capable")
	}
}

func TestCityRuntimeReloadHandsQueuedNudgesToPollersWhenEventsStop(t *testing.T) {
	due := func() queuedNudge {
		return newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))
	}
	retrying := func() queuedNudge {
		item := newQueuedNudge("worker", "wait satisfied: proceed", time.Now().Add(-time.Minute))
		item.DeliverAfter = time.Now().Add(time.Hour).UTC()
		item.Attempts = 1
		return item
	}
	for name, item := range map[string]func() queuedNudge{"due": due, "deferred retry": retrying} {
		t.Run(name, func(t *testing.T) {
			assertReloadHandsQueuedNudgeToPoller(t, item)
		})
	}
}

func assertReloadHandsQueuedNudgeToPoller(t *testing.T, newItem func() queuedNudge) {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	t.Setenv("GC_BEADS", "file")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := newNudgeEventedFake()
	mgr := newSessionManagerWithConfig(cityPath, openNudgeBeadStore(cityPath), sp, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: cityPath, Provider: "codex", Hints: runtime.Config{WorkDir: cityPath}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: cityPath}); err != nil {
		t.Fatalf("Start: %v", err)
	}

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
	cr.nudgeEvents.update(cr.sp, cr.cfg)
	cr.sessionEvents = newSessionEventPump(ctx, newLegacyWake(make(chan struct{}, 1), nil), cr.stderr, cr.logPrefix)
	cr.sessionEvents.observe = cr.nudgeEvents.handleEvent
	if !cr.sessionEvents.restart(cr.sp) {
		t.Fatal("precondition: the session-event pump must subscribe to the event-capable provider")
	}
	lastProviderName := "fake"
	if reply := cr.reloadConfigTraced(ctx, &lastProviderName, cityPath, nil, reloadSourceManual); reply.Outcome == reloadOutcomeFailed {
		t.Fatalf("reloadConfigTraced failed: %s", reply.Error)
	}
	if !cr.nudgeEvents.active() || cr.nudgeWakeListener == nil {
		t.Fatal("precondition: the dispatcher and its wake listener must be live while the stream runs")
	}

	var mu sync.Mutex
	var started []string
	prev := startNudgePoller
	startNudgePoller = func(_, _, sessionName string) error {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, sessionName)
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	if err := enqueueQueuedNudge(cityPath, newItem()); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	sp.endStreams()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(5 * time.Second)
	for cr.sessionEvents.streaming() {
		select {
		case <-timeout:
			t.Fatal("the session-event pump did not notice the ended stream")
		case <-tick.C:
		}
	}

	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[session]\nprovider = \"fake\"\n\n[daemon]\nshutdown_timeout = \"1s\"\n"), 0o644); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	if reply := cr.reloadConfigTraced(ctx, &lastProviderName, cityPath, nil, reloadSourceManual); reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reloadConfigTraced outcome = %q (%s), want applied", reply.Outcome, reply.Error)
	}
	if cr.nudgeEvents.active() || cr.nudgeWakeListener != nil {
		t.Fatal("precondition: the reload must retire the dispatcher and its wake listener once the stream ended")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(started, []string{info.SessionName}) {
		t.Fatalf("pollers started = %q, want [%q]: the queued nudge is stranded with no poller and no dispatcher", started, info.SessionName)
	}
}

func TestCityRuntimeRunActivatesNudgeWakeListenerAtStartup(t *testing.T) {
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")

	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sp := newNudgeEventedFake()
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
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		cr.run(ctx)
		close(done)
	}()

	awaitCond(t, func() bool {
		return testWakeSocketIsHosting(cityPath)
	}, "CityRuntime.run startup opening the nudge wake listener")
	cancel()
	awaitClose(t, done, "CityRuntime.run returning after nudge wake listener startup")
}

func TestCityRuntimeNudgeDispatchTickKicksActiveEventDispatcher(t *testing.T) {
	store := &legacyNudgeDispatchProbeStore{Store: beads.NewMemStore()}
	dispatcher := &nudgeEventDispatcher{kicked: make(chan struct{}, 1)}
	dispatcher.setEventCapable(true)
	cr := &CityRuntime{
		cfg:                 supervisorCfg(),
		nudgeEvents:         dispatcher,
		standaloneCityStore: store,
	}

	cr.nudgeDispatchTick(context.Background())

	select {
	case <-dispatcher.kicked:
	default:
		t.Fatal("nudge dispatch tick did not kick the active event dispatcher")
	}
	if got := store.listCalls.Load(); got != 0 {
		t.Fatalf("legacy nudge dispatch issued %d store List calls while the event dispatcher was active, want 0", got)
	}
}
