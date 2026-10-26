package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// laneWaitBudget bounds every wait on a host goroutine in these tests.
const laneWaitBudget = 10 * time.Second

// laneHostEnvFake is the controller state a test host runs over.
type laneHostEnvFake struct {
	f         *laneFixture
	quiescent atomic.Bool
	storeWork atomic.Int64

	mu       sync.Mutex
	store    beads.Store
	enqueued []reconcilekey.Key
}

func (e *laneHostEnvFake) SessionsBeadStore() beads.SessionStore {
	e.mu.Lock()
	defer e.mu.Unlock()
	return beads.SessionStore{Store: e.store}
}

// setStore replaces the sessions store the env answers.
func (e *laneHostEnvFake) setStore(store beads.Store) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store = store
}
func (e *laneHostEnvFake) SessionProvider() runtime.Provider { return e.f.sp }
func (e *laneHostEnvFake) Config() *config.City              { return e.f.cfg }
func (e *laneHostEnvFake) CityPath() string                  { return e.f.dir }
func (e *laneHostEnvFake) Enqueue(keys ...reconcilekey.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enqueued = append(e.enqueued, keys...)
}

func (e *laneHostEnvFake) beginStoreWork() func() {
	e.storeWork.Add(1)
	var once sync.Once
	return func() { once.Do(func() { e.storeWork.Add(-1) }) }
}
func (e *laneHostEnvFake) storesQuiescent() bool { return e.quiescent.Load() }

func (e *laneHostEnvFake) keys() []reconcilekey.Key {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]reconcilekey.Key(nil), e.enqueued...)
}

// laneTimer is one timer or ticker a test host asked for; stopped closes
// when the host stops a timer.
type laneTimer struct {
	d       time.Duration
	c       chan time.Time
	stopped chan struct{}
}

// laneHostHarness is a host over a fixture with hand-driven timers, its
// passes and worker exits reported on channels.
type laneHostHarness struct {
	f        *laneFixture
	env      *laneHostEnvFake
	h        *conversationLaneHost
	ctx      context.Context
	cancel   context.CancelFunc
	passes   chan lanePassRecord
	reprobes chan bool
	exits    chan string
	timers   chan laneTimer

	mu      sync.Mutex
	tickers []laneTimer
	logs    []string
}

type lanePassRecord struct {
	conv string
	out  lanePassOutcome
}

func newLaneHostHarness(t *testing.T, f *laneFixture, kinds ...string) *laneHostHarness {
	t.Helper()
	env := &laneHostEnvFake{f: f, store: f.store}
	hh := &laneHostHarness{
		f: f, env: env,
		passes:   make(chan lanePassRecord, 256),
		reprobes: make(chan bool, 256),
		exits:    make(chan string, 256),
		timers:   make(chan laneTimer, 256),
	}
	h := newConversationLaneHost(env, kinds, session.StopRequestsLive)
	h.clk = f.clk
	h.newTimer = func(d time.Duration) (<-chan time.Time, func()) {
		tm := laneTimer{d: d, c: make(chan time.Time, 1), stopped: make(chan struct{})}
		hh.timers <- tm
		var once sync.Once
		return tm.c, func() { once.Do(func() { close(tm.stopped) }) }
	}
	h.newTicker = func(d time.Duration) (<-chan time.Time, func()) {
		tm := laneTimer{d: d, c: make(chan time.Time, 1)}
		hh.mu.Lock()
		hh.tickers = append(hh.tickers, tm)
		hh.mu.Unlock()
		return tm.c, func() {}
	}
	h.logf = func(format string, args ...any) {
		hh.mu.Lock()
		defer hh.mu.Unlock()
		hh.logs = append(hh.logs, fmt.Sprintf(format, args...))
	}
	h.onPass = func(conv string, out lanePassOutcome) { hh.passes <- lanePassRecord{conv, out} }
	h.onReprobe = func(_ string, still bool) { hh.reprobes <- still }
	h.onWorkerExit = func(conv string) { hh.exits <- conv }
	hh.h = h
	hh.ctx, hh.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		hh.cancel()
		h.wg.Wait()
	})
	return hh
}

func (hh *laneHostHarness) start() { hh.h.start(hh.ctx) }

func (hh *laneHostHarness) nextPass(t *testing.T) lanePassRecord {
	t.Helper()
	select {
	case p := <-hh.passes:
		return p
	case <-time.After(laneWaitBudget):
		t.Fatal("no pass ran")
		return lanePassRecord{}
	}
}

func (hh *laneHostHarness) nextExit(t *testing.T) string {
	t.Helper()
	select {
	case c := <-hh.exits:
		return c
	case <-time.After(laneWaitBudget):
		t.Fatal("no worker exited")
		return ""
	}
}

func (hh *laneHostHarness) nextTimer(t *testing.T) laneTimer {
	t.Helper()
	select {
	case tm := <-hh.timers:
		return tm
	case <-time.After(laneWaitBudget):
		t.Fatal("no timer was set")
		return laneTimer{}
	}
}

func (hh *laneHostHarness) noPass(t *testing.T) {
	t.Helper()
	select {
	case p := <-hh.passes:
		t.Fatalf("unexpected pass over %s: %+v", p.conv, p.out)
	default:
	}
}

func (hh *laneHostHarness) nextReprobe(t *testing.T) bool {
	t.Helper()
	select {
	case still := <-hh.reprobes:
		return still
	case <-time.After(laneWaitBudget):
		t.Fatal("no re-probe ran")
		return false
	}
}

// waitTicker waits for the host's scan ticker and returns it.
func (hh *laneHostHarness) waitTicker(t *testing.T) laneTimer {
	t.Helper()
	deadline := time.After(laneWaitBudget)
	for {
		hh.mu.Lock()
		if len(hh.tickers) > 0 {
			tick := hh.tickers[0]
			hh.mu.Unlock()
			return tick
		}
		hh.mu.Unlock()
		select {
		case <-deadline:
			t.Fatal("no scan ticker started")
		case <-time.After(time.Millisecond):
		}
	}
}

func (hh *laneHostHarness) tickerCount() int {
	hh.mu.Lock()
	defer hh.mu.Unlock()
	return len(hh.tickers)
}

func (hh *laneHostHarness) logLines() []string {
	hh.mu.Lock()
	defer hh.mu.Unlock()
	return append([]string(nil), hh.logs...)
}

// TestLaneHostPokeRunsAPass proves Poke starts a worker whose pass delivers,
// and that the idle worker then exits.
func TestLaneHostPokeRunsAPass(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.h.wg.Wait() // the boot scan found nothing and the loop ended
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	if !hh.h.Poke(f.id) {
		t.Fatal("Poke refused on a running host")
	}
	p := hh.nextPass(t)
	if p.conv != f.id || p.out.err != nil || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want an accepted delivery over %s", p, f.id)
	}
	if p := hh.nextPass(t); p.out.res.Took != opjournal.TakeNone {
		t.Fatalf("re-plan pass = %+v, want nothing taken", p.out.res)
	}
	if hh.nextExit(t) != f.id {
		t.Fatal("the idle worker did not exit")
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v, want succeeded", got.Outcome)
	}
	if st := hh.h.status(f.clk.Now()); st.State != laneHostStateRunning || st.Workers != 0 || st.Passes != 2 || st.LastPassAt == "" {
		t.Fatalf("status = %+v, want running, no workers, one pass", st)
	}
}

// TestLaneHostOverSQLite runs the same delivery over a SQLite sessions
// store.
func TestLaneHostOverSQLite(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	f := newLaneFixtureOver(t, store)
	hh := newLaneHostHarness(t, f)
	hh.start()
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	hh.h.Poke(f.id)
	if p := hh.nextPass(t); p.out.err != nil || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want accepted", p.out)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
		t.Fatalf("op = %+v nudges %d, want one succeeded delivery", got.Outcome, len(f.nudges()))
	}
}

// TestLaneHostBootScanStartsPasses proves the boot scan pokes a
// conversation with an open lane op, and with the list empty keeps the scan
// ticking only until a scan finds no open lane op.
func TestLaneHostBootScanStartsPasses(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.propose(t, st, f.msg("k1", opjournal.RoleNotice, "hello"))
	hh := newLaneHostHarness(t, f)
	hh.start()
	if p := hh.nextPass(t); p.conv != f.id || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the boot scan's delivery", p)
	}
	if p := hh.nextPass(t); p.out.res.Took != opjournal.TakeNone {
		t.Fatalf("re-plan pass = %+v, want nothing taken", p.out.res)
	}
	hh.nextExit(t)
	tick := hh.waitTicker(t)
	tick.c <- f.clk.Now()
	hh.h.wg.Wait() // the tick's scan found nothing open and the loop ended
	if n := hh.tickerCount(); n != 1 {
		t.Fatalf("tickers = %d, want the one the boot scan's find started", n)
	}
	hh.noPass(t)
}

// TestLaneHostDrainsAGatedOpAfterRollback proves, with the list empty, a lane
// op the boot scan finds behind a content gate with no end time is delivered
// on the next scan tick after the gate clears, not left to expire.
func TestLaneHostDrainsAGatedOpAfterRollback(t *testing.T) {
	f := newLaneFixture(t, func(meta map[string]string) { meta["restart_requested"] = "true" })
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	hh := newLaneHostHarness(t, f)
	hh.start()
	p := hh.nextPass(t)
	if !laneWaitsFor(p.out.res, opjournal.WaitContentBlock) {
		t.Fatalf("boot pass = %+v, want a content_block wait", p.out.res)
	}
	if hh.nextExit(t) != f.id {
		t.Fatal("the gated worker kept its slot")
	}
	tick := hh.waitTicker(t)
	if err := f.store.SetMetadata(f.id, "restart_requested", ""); err != nil {
		t.Fatalf("clearing restart_requested: %v", err)
	}
	tick.c <- f.clk.Now()
	if p := hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass after the tick = %+v, want the delivery", p.out.res)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
		t.Fatalf("op = %+v nudges %d, want one succeeded delivery", got.Outcome, len(f.nudges()))
	}
}

// TestLaneHostIdleBootStartsNothing proves an idle city with the list empty
// pays one scan: no ticker, no worker.
func TestLaneHostIdleBootStartsNothing(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.h.wg.Wait()
	if n := hh.tickerCount(); n != 0 {
		t.Fatalf("tickers = %d, want none", n)
	}
	hh.noPass(t)
	if st := hh.h.status(f.clk.Now()); st.State != laneHostStateRunning || st.Workers != 0 {
		t.Fatalf("status = %+v", st)
	}
}

// TestLaneHostAntiEntropyTick proves a listed kind keeps the 60 s scan, and a
// tick pokes a conversation whose op arrived without a doorbell.
func TestLaneHostAntiEntropyTick(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message")
	hh.start()
	tick := hh.waitTicker(t)
	if tick.d != laneHostScanInterval {
		t.Fatalf("scan interval = %s, want %s", tick.d, laneHostScanInterval)
	}
	st := f.laneStore(t)
	f.propose(t, st, f.msg("k1", opjournal.RoleNotice, "hello"))
	tick.c <- f.clk.Now()
	if p := hh.nextPass(t); p.conv != f.id || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the tick's delivery", p)
	}
}

// TestLaneSocketVerbPokes proves lane:<conv> and lane:<conv>:<op> reach the
// host through the controller socket and start a pass, and that the verb
// refuses bad ids and answers no-host for a city without one.
func TestLaneSocketVerbPokes(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.h.wg.Wait()
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	// The fixture's sessions store has no controller cache to install the
	// named op in, so the op waits for the doorbell's retry (refreshRow).
	if got := laneSocketConn(t, f.dir, laneSocketPrefix+f.id+":"+op.ID); got != laneReplyOK+", rows waiting for a retry: 1" {
		t.Fatalf("lane: reply = %q, want the poke with the op waiting", got)
	}
	if p := hh.nextPass(t); p.conv != f.id || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the verb's delivery", p)
	}
	for payload, want := range map[string]string{
		"":             "refused invalid conversation id",
		"bad id":       "refused invalid conversation id",
		f.id + ":":     "refused invalid op id",
		f.id + ":a:b":  "refused invalid op id",
		f.id + ":@bad": "refused invalid op id",
	} {
		if got := laneSocketReply(f.dir, payload); got != want {
			t.Errorf("laneSocketReply(%q) = %q, want %q", payload, got, want)
		}
	}
	if got := laneSocketReply(t.TempDir(), "gc-1"); got != "no-host" {
		t.Fatalf("reply for a city without a host = %q, want no-host", got)
	}
}

// laneSocketConn sends one line through handleControllerConn and reads its
// reply.
func laneSocketConn(t *testing.T, cityPath, line string) string {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleControllerConn(server, cityPath, controllerHostingStandalone, func() {}, nil, nil, nil, nil, nil)
	}()
	if _, err := client.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("writing %q: %v", line, err)
	}
	reply, err := bufio.NewReader(client).ReadString('\n')
	_ = client.Close()
	<-done
	if err != nil {
		t.Fatalf("reading the reply to %q: %v", line, err)
	}
	return strings.TrimSpace(reply)
}

// TestLaneStatusVerb proves lane-status answers the host's typed status, and
// no_host for a city without one.
func TestLaneStatusVerb(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message")
	hh.start()
	if got := laneSocketConn(t, f.dir, laneStatusCommand); !strings.Contains(got, `"state":"running"`) || !strings.Contains(got, `"kinds":["message"]`) {
		t.Fatalf("lane-status = %s, want running with kinds [message]", got)
	}
	if got := conversationLaneStatusFor(t.TempDir(), time.Now()); got.State != laneHostStateNone {
		t.Fatalf("status for a city without a host = %+v, want no_host", got)
	}
}

// TestLaneHostTimerFires proves a worker sleeps until its pass's Next and the
// timer's fire runs the next pass.
func TestLaneHostTimerFires(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "later", func(r *opjournal.Request) {
		r.NotBefore = laneT0.Add(30 * time.Second)
	}))
	hh := newLaneHostHarness(t, f)
	hh.start()
	if p := hh.nextPass(t); p.out.res.Took != opjournal.TakeNone || p.out.res.Next.IsZero() {
		t.Fatalf("first pass = %+v, want a not_before wait with a Next", p.out.res)
	}
	tm := hh.nextTimer(t)
	if tm.d != 30*time.Second {
		t.Fatalf("timer = %s, want 30s", tm.d)
	}
	f.clk.Advance(30 * time.Second)
	tm.c <- f.clk.Now()
	if p := hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass after the timer = %+v, want accepted", p.out.res)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v", got.Outcome)
	}
}

// blockingExec is an executor whose Probe blocks until released, counting
// concurrent probes per conversation.
type blockingExec struct {
	conv string
	b    *laneBlocker
}

type laneBlocker struct {
	clk      *laneTestClock
	mu       sync.Mutex
	inFlight map[string]int
	maxSeen  map[string]int
	probes   map[string]int
	entered  chan string
	release  chan struct{}
}

func newLaneBlocker(clk *laneTestClock) *laneBlocker {
	return &laneBlocker{
		clk: clk, inFlight: map[string]int{}, maxSeen: map[string]int{}, probes: map[string]int{},
		entered: make(chan string, 64), release: make(chan struct{}),
	}
}

func (e blockingExec) Probe(ctx context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	e.b.mu.Lock()
	e.b.inFlight[e.conv]++
	e.b.probes[e.conv]++
	e.b.maxSeen[e.conv] = max(e.b.maxSeen[e.conv], e.b.inFlight[e.conv])
	e.b.mu.Unlock()
	e.b.entered <- e.conv
	select {
	case <-e.b.release:
	case <-ctx.Done():
	}
	e.b.mu.Lock()
	e.b.inFlight[e.conv]--
	e.b.mu.Unlock()
	return opjournal.Probe{
		Available: true, Running: true, Boundary: opjournal.TriYes, Fence: laneFence(conv),
		Caps: opjournal.Caps{Live: true, Input: opjournal.FenceAtomic},
	}, nil
}

func (e blockingExec) Deliver(context.Context, opjournal.Delivery) opjournal.DeliveryResult {
	return opjournal.Delivered{Record: opjournal.DeliveryRecord{At: e.b.clk.Now(), Ack: opjournal.AckDelivered}, Via: "test"}
}

func (blockingExec) Interrupt(context.Context, opjournal.InterruptCall) opjournal.ControlResult {
	return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
}

func (blockingExec) Respond(context.Context, opjournal.RespondCall) opjournal.ControlResult {
	return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
}

func (b *laneBlocker) waitEntered(t *testing.T) string {
	t.Helper()
	select {
	case c := <-b.entered:
		return c
	case <-time.After(laneWaitBudget):
		t.Fatal("no probe started")
		return ""
	}
}

// addSession creates another open session row in f's store.
func addSession(t *testing.T, f *laneFixture, name string) string {
	t.Helper()
	b, err := f.store.Create(beads.Bead{
		Title: "Session: " + name, Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": name, "state": "active", "generation": "1", "continuation_epoch": "1", "instance_token": "t-" + name},
	})
	if err != nil {
		t.Fatalf("creating session %s: %v", name, err)
	}
	return b.ID
}

// TestLaneHostCapsWorkers proves at most maxWorkers conversations are served
// at once: a poke past the cap queues and starts when a worker exits.
func TestLaneHostCapsWorkers(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	convs := []string{f.id, addSession(t, f, "b"), addSession(t, f, "c")}
	for i, conv := range convs {
		r := f.msg(fmt.Sprintf("k%d", i), opjournal.RoleNotice, "hello")
		r.Conversation = conv
		f.propose(t, st, r)
	}
	b := newLaneBlocker(f.clk)
	hh := newLaneHostHarness(t, f)
	hh.h.maxWorkers = 2
	hh.h.newExec = func(_ laneExecDeps, row laneRow) opjournal.Executor { return blockingExec{conv: row.info.ID, b: b} }
	hh.start()
	b.waitEntered(t)
	b.waitEntered(t)
	if st := hh.h.status(f.clk.Now()); st.Workers != 2 || st.Queued != 1 {
		t.Fatalf("status = %+v, want 2 workers and 1 queued", st)
	}
	close(b.release)
	seen := map[string]bool{}
	for len(seen) < len(convs) {
		seen[hh.nextPass(t).conv] = true
	}
	for _, conv := range convs {
		if !seen[conv] {
			t.Fatalf("conversation %s never passed: %v", conv, seen)
		}
	}
}

// TestLaneHostOnePassAtATime proves pokes during a pass never start a second
// pass of the same conversation, and coalesce into one more pass after it.
func TestLaneHostOnePassAtATime(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.propose(t, st, f.msg("k1", opjournal.RoleNotice, "hello"))
	b := newLaneBlocker(f.clk)
	hh := newLaneHostHarness(t, f)
	hh.h.newExec = func(_ laneExecDeps, row laneRow) opjournal.Executor { return blockingExec{conv: row.info.ID, b: b} }
	hh.start()
	b.waitEntered(t)
	for range 10 {
		hh.h.Poke(f.id)
	}
	// A second op arrives while the first pass is blocked, so the pass after
	// it probes again; the ten pokes coalesce instead of running in parallel.
	r := f.msg("k2", opjournal.RoleNotice, "again")
	f.propose(t, st, r)
	close(b.release)
	hh.nextPass(t)
	hh.nextPass(t)
	hh.nextExit(t)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.maxSeen[f.id] != 1 {
		t.Fatalf("max concurrent probes = %d, want 1", b.maxSeen[f.id])
	}
}

// TestLaneHostRecoversPanic proves a panicking pass is recovered, backs
// off, and the next pass runs.
func TestLaneHostRecoversPanic(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	var calls atomic.Int32
	hh := newLaneHostHarness(t, f)
	hh.h.newExec = func(deps laneExecDeps, row laneRow) opjournal.Executor {
		if calls.Add(1) == 1 {
			panic("executor bug")
		}
		return newConversationLaneExec(deps, row)
	}
	hh.start()
	if p := hh.nextPass(t); p.out.err == nil || !strings.Contains(p.out.err.Error(), "panicked") {
		t.Fatalf("first pass = %+v, want a recovered panic", p.out)
	}
	tm := hh.nextTimer(t)
	if tm.d != laneErrBackoffStart {
		t.Fatalf("backoff = %s, want %s", tm.d, laneErrBackoffStart)
	}
	tm.c <- f.clk.Now()
	if p := hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass after the panic = %+v, want accepted", p.out)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v", got.Outcome)
	}
}

// laneCountingStore counts reads.
type laneCountingStore struct {
	laneTestStore
	reads *atomic.Int32
}

func (s laneCountingStore) Get(id string) (beads.Bead, error) {
	s.reads.Add(1)
	return s.laneTestStore.Get(id)
}

func (s laneCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.reads.Add(1)
	return s.laneTestStore.List(q)
}

// TestLaneHostQuiescentReadsNothing proves a quiescent city's scan and
// passes read no store, and the worker retries later.
func TestLaneHostQuiescentReadsNothing(t *testing.T) {
	reads := new(atomic.Int32)
	base := newLaneTestStore()
	f := newLaneFixtureOver(t, laneCountingStore{laneTestStore: base, reads: reads})
	hh := newLaneHostHarness(t, f)
	hh.env.quiescent.Store(true)
	reads.Store(0)
	hh.start()
	hh.h.Poke(f.id)
	if p := hh.nextPass(t); !p.out.skipped {
		t.Fatalf("pass = %+v, want skipped", p.out)
	}
	if tm := hh.nextTimer(t); tm.d != laneQuiescentRetry {
		t.Fatalf("retry = %s, want %s", tm.d, laneQuiescentRetry)
	}
	// The quiescent boot scan owes another step, so the scan ticker starts
	// only once the scan, and its store work, have ended.
	hh.waitTicker(t)
	if n := reads.Load(); n != 0 {
		t.Fatalf("store reads while quiescent = %d, want 0", n)
	}
	if n := hh.env.storeWork.Load(); n != 0 {
		t.Fatalf("store work left in flight = %d", n)
	}
}

// TestLaneHostNeverTakesLegacyOps proves I2: an executor=legacy op is never
// taken by the lane host.
func TestLaneHostNeverTakesLegacyOps(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	r := f.msg("k1", opjournal.RolePrompt, "legacy")
	r.Executor = opjournal.ExecutorLegacy
	op := f.propose(t, st, r)
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.h.Poke(f.id)
	if p := hh.nextPass(t); p.out.res.Took != opjournal.TakeNone {
		t.Fatalf("pass = %+v, want nothing taken", p.out.res)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusSubmitted || len(f.nudges()) != 0 {
		t.Fatalf("legacy op = %s, nudges %d; want untouched", got.Status, len(f.nudges()))
	}
}

// TestLaneHostRefusesIncapableStore proves a BdStore sessions store keeps
// the host off: silently with the list empty, and with a kind listed with the
// typed refusal logged and reported.
func TestLaneHostRefusesIncapableStore(t *testing.T) {
	runner := func(string, string, ...string) ([]byte, error) {
		return nil, errors.New("the bd CLI must not run")
	}
	for _, tc := range []struct {
		name  string
		kinds []string
		state string
		logs  int
	}{
		{"list empty", nil, laneHostStateOff, 0},
		{"message listed", []string{"message"}, laneHostStateRefused, 1},
		{"message listed over the controller's wrapping", []string{"message"}, laneHostStateRefused, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			hh := newLaneHostHarness(t, f, tc.kinds...)
			var store beads.Store = beads.NewBdStore(t.TempDir(), runner)
			if strings.Contains(tc.name, "wrapping") {
				// The controller's sessions store: bead policies over its
				// cache over the bd CLI.
				store = wrapStoreWithBeadPolicies(beads.NewCachingStore(store, nil), f.cfg)
			}
			hh.env.setStore(store)
			hh.start()
			hh.h.wg.Wait()
			st := hh.h.status(time.Now())
			if st.State != tc.state {
				t.Fatalf("state = %q, want %q", st.State, tc.state)
			}
			logs := hh.logLines()
			if len(logs) != tc.logs {
				t.Fatalf("logs = %q, want %d line(s)", logs, tc.logs)
			}
			if tc.logs > 0 && !strings.Contains(logs[0], "conversation lanes need guarded batches; this city's sessions class is bound to the bd CLI (BdStore). Remove the kind from [ops] authoritative, or bind the sessions class to a native store") {
				t.Fatalf("refusal = %q", logs[0])
			}
			if hh.h.Poke(f.id) {
				t.Fatal("Poke accepted by a host that is not running")
			}
			if got := laneSocketReply(f.dir, f.id); !strings.HasPrefix(got, "refused conversation lanes need guarded batches") {
				t.Fatalf("lane: reply = %q, want the refusal", got)
			}
			if !errors.Is(laneStoreCapability(store), errLaneStoreIncapable) {
				t.Fatal("the refusal is not errLaneStoreIncapable")
			}
		})
	}
}

// TestLaneHostRequestsWake proves a due if_needed op for a dormant
// conversation records a wake and asks the controller to reconcile the
// session, rechecks quickly while the woken session starts, and delivers once
// it has; a held row records none.
func TestLaneHostRequestsWake(t *testing.T) {
	t.Run("recorded", func(t *testing.T) {
		f := newLaneFixture(t, func(meta map[string]string) { meta["state"] = string(session.StateAsleep) })
		st := f.laneStore(t)
		op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "wake up", func(r *opjournal.Request) {
			r.Policy.Message.Wake = opjournal.WakeIfNeeded
		}))
		hh := newLaneHostHarness(t, f)
		hh.start()
		p := hh.nextPass(t)
		if !p.out.res.Wake || p.out.wake != session.WakeRecorded {
			t.Fatalf("pass = %+v, want a recorded wake", p.out)
		}
		keys := hh.env.keys()
		if len(keys) != 2 || keys[0] != reconcilekey.Allocator() || keys[1] != reconcilekey.Session(f.id) {
			t.Fatalf("enqueued = %+v, want the allocator and the session", keys)
		}
		row, err := f.store.Get(f.id)
		if err != nil || row.Metadata["wake_request"] == "" {
			t.Fatalf("row = %v, %v; want a wake request", row.Metadata, err)
		}
		tm := hh.nextTimer(t)
		if tm.d != laneWakeRecheck {
			t.Fatalf("recheck = %s, want %s", tm.d, laneWakeRecheck)
		}
		// The reconciler is starting the session: the recheck finds the
		// row creating, which asks for no wake, and looks again as soon,
		// not at the op's start_by.
		if err := f.store.SetMetadata(f.id, "state", string(session.StateCreating)); err != nil {
			t.Fatalf("marking the row creating: %v", err)
		}
		tm.c <- f.clk.Now()
		p = hh.nextPass(t)
		if p.out.res.Wake || len(p.out.res.Waits) != 1 || p.out.res.Waits[0].Reason != opjournal.WaitContentBlock ||
			p.out.res.Waits[0].Detail != session.ContentGateStarting {
			t.Fatalf("pass while starting = %+v, want one content_block starting wait and no wake", p.out.res)
		}
		tm = hh.nextTimer(t)
		if tm.d != laneWakeRecheck {
			t.Fatalf("recheck while starting = %s, want %s", tm.d, laneWakeRecheck)
		}
		// The start commits: the next recheck delivers.
		if err := f.store.SetMetadata(f.id, "state", string(session.StateAwake)); err != nil {
			t.Fatalf("marking the row awake: %v", err)
		}
		tm.c <- f.clk.Now()
		if p = hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
			t.Fatalf("pass after the start = %+v, want the delivery", p.out.res)
		}
		if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
			t.Fatalf("op = %+v nudges %d, want one succeeded delivery", got.Outcome, len(f.nudges()))
		}
	})
	t.Run("held", func(t *testing.T) {
		f := newLaneFixture(t, func(meta map[string]string) {
			meta["state"] = string(session.StateAsleep)
			meta["sleep_intent"] = string(session.SleepReasonUserHold)
			meta["held_until"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		})
		st := f.laneStore(t)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "wake up", func(r *opjournal.Request) {
			r.Policy.Message.Wake = opjournal.WakeIfNeeded
		}))
		hh := newLaneHostHarness(t, f)
		hh.start()
		if p := hh.nextPass(t); p.out.wake != session.WakeHeld {
			t.Fatalf("pass = %+v, want a held wake", p.out)
		}
		if keys := hh.env.keys(); len(keys) != 0 {
			t.Fatalf("enqueued = %+v, want none for a held row", keys)
		}
		if tm := hh.nextTimer(t); tm.d != laneWakeHeldRecheck {
			t.Fatalf("recheck = %s, want %s", tm.d, laneWakeHeldRecheck)
		}
	})
	t.Run("past a stale cache", func(t *testing.T) {
		// The controller's cache still holds the row active after it went
		// dormant: the wake is decided on the live row the view was read
		// from, not on the cached one.
		f := newLaneFixture(t)
		cache := beads.NewCachingStoreForTestWithPrefix(f.store, "gc", nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if err := f.store.SetMetadata(f.id, "state", string(session.StateAsleep)); err != nil {
			t.Fatalf("marking the row asleep: %v", err)
		}
		if row, err := cache.Get(f.id); err != nil || row.Metadata["state"] != string(session.StateActive) {
			t.Fatalf("cached row = %v, %v; want the stale active row", row.Metadata, err)
		}
		st := f.laneStore(t)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "wake up", func(r *opjournal.Request) {
			r.Policy.Message.Wake = opjournal.WakeIfNeeded
		}))
		hh := newLaneHostHarness(t, f)
		hh.env.setStore(cache)
		hh.start()
		if p := hh.nextPass(t); !p.out.res.Wake || p.out.wake != session.WakeRecorded {
			t.Fatalf("pass = %+v, want a recorded wake", p.out)
		}
		keys := hh.env.keys()
		if len(keys) != 2 || keys[0] != reconcilekey.Allocator() || keys[1] != reconcilekey.Session(f.id) {
			t.Fatalf("enqueued = %+v, want the allocator and the session", keys)
		}
		if row, err := f.store.Get(f.id); err != nil || row.Metadata["wake_request"] == "" {
			t.Fatalf("row = %v, %v; want a wake request", row.Metadata, err)
		}
		// The wake was written through the cache, which now holds it.
		if row, err := cache.Get(f.id); err != nil || row.Metadata["wake_request"] == "" {
			t.Fatalf("cached row = %v, %v; want the wake request", row.Metadata, err)
		}
	})
}

// TestLaneHostReportsQuarantine proves the status counts op rows a pass
// could not decode.
func TestLaneHostReportsQuarantine(t *testing.T) {
	f := newLaneFixture(t)
	if _, err := f.store.Create(beads.Bead{
		Title: "op", Type: "op", Status: "open", Labels: []string{opjournal.LabelOp},
		Metadata: map[string]string{"gc.op.open": f.id, "gc.op.schema": "99"},
	}); err != nil {
		t.Fatalf("creating a row from the future: %v", err)
	}
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.h.Poke(f.id)
	hh.nextPass(t)
	if st := hh.h.status(f.clk.Now()); st.Quarantined != 1 || len(st.QuarantinedIDs) != 1 {
		t.Fatalf("status = %+v, want one quarantined row", st)
	}
}

// TestLaneHostCancelStopsWorkers proves ending the host's ctx stops every
// worker, including one waiting on its timer.
func TestLaneHostCancelStopsWorkers(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "later", func(r *opjournal.Request) {
		r.NotBefore = laneT0.Add(time.Hour)
	}))
	hh := newLaneHostHarness(t, f, "message")
	hh.start()
	hh.nextPass(t)
	hh.nextTimer(t)
	hh.cancel()
	done := make(chan struct{})
	go func() {
		hh.h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(laneWaitBudget):
		t.Fatal("workers did not stop on cancel")
	}
	if hh.h.Poke(f.id) {
		t.Fatal("Poke accepted after cancel")
	}
}

// TestLaneWorkerNextDelay pins the worker's wait after each kind of pass.
func TestLaneWorkerNextDelay(t *testing.T) {
	clk := &laneTestClock{now: laneT0}
	w := &conversationLaneWorker{clk: clk}
	for _, tc := range []struct {
		name string
		out  lanePassOutcome
		want time.Duration
		idle bool
	}{
		{"idle", lanePassOutcome{}, 0, true},
		{"more", lanePassOutcome{res: opjournal.PassResult{More: true}}, 0, false},
		{"took, so re-plan", lanePassOutcome{res: opjournal.PassResult{Took: opjournal.TakeContent, Next: laneT0.Add(time.Hour)}}, 0, false},
		{"next", lanePassOutcome{res: opjournal.PassResult{Next: laneT0.Add(7 * time.Second)}}, 7 * time.Second, false},
		{"busy", lanePassOutcome{res: opjournal.PassResult{Busy: true}}, laneBusyFirst, false},
		{"busy beats a later next", lanePassOutcome{res: opjournal.PassResult{Busy: true, Next: laneT0.Add(time.Minute)}}, laneBusyFirst, false},
		{"quiescent", lanePassOutcome{skipped: true}, laneQuiescentRetry, false},
		{"wake recorded", lanePassOutcome{res: opjournal.PassResult{Wake: true}, wake: session.WakeRecorded}, laneWakeRecheck, false},
		{"wake held", lanePassOutcome{res: opjournal.PassResult{Wake: true}, wake: session.WakeHeld}, laneWakeHeldRecheck, false},
		{"wake refused, not dormant", lanePassOutcome{res: opjournal.PassResult{Wake: true}, wake: session.WakeNotDormant}, laneWakeHeldRecheck, false},
		{"error", lanePassOutcome{err: errors.New("x")}, laneErrBackoffStart, false},
		{"error again", lanePassOutcome{err: errors.New("x")}, 2 * laneErrBackoffStart, false},
		{"wake request failed", lanePassOutcome{res: opjournal.PassResult{Wake: true}, wakeErr: errors.New("conflict")}, 4 * laneErrBackoffStart, false},
	} {
		got, idle := w.nextDelay(tc.out)
		if got != tc.want || idle != tc.idle {
			t.Errorf("%s: delay %s idle %v, want %s idle %v", tc.name, got, idle, tc.want, tc.idle)
		}
	}
	busy := lanePassOutcome{res: opjournal.PassResult{Busy: true}}
	w.state = laneWorkerState{}
	w.nextDelay(busy)
	clk.Advance(laneBusyFirstFor)
	if d, _ := w.nextDelay(busy); d != laneBusyNext {
		t.Errorf("busy after %s = %s, want %s", laneBusyFirstFor, d, laneBusyNext)
	}
	clk.Advance(laneBusyNextFor)
	if d, _ := w.nextDelay(busy); d != laneBusyMax {
		t.Errorf("busy after a minute = %s, want %s", d, laneBusyMax)
	}
	// A delivery ends the busy stretch: the new turn's first busy pass
	// starts the backoff over.
	w.nextDelay(lanePassOutcome{res: opjournal.PassResult{Took: opjournal.TakeContent}})
	if d, _ := w.nextDelay(busy); d != laneBusyFirst {
		t.Errorf("busy after a delivery = %s, want %s", d, laneBusyFirst)
	}
}

// TestLaneWorkerRechecksAStartingWake pins the quick recheck through a woken
// session's start: a pass that finds the session starting rechecks at
// laneWakeRecheck up to startFor after the recorded wake, and otherwise waits
// for the op's own Next. A take, or any other wait, ends it.
func TestLaneWorkerRechecksAStartingWake(t *testing.T) {
	next := laneT0.Add(24 * time.Hour)
	blocked := func(detail string) lanePassOutcome {
		return lanePassOutcome{res: opjournal.PassResult{Next: next, Waits: []opjournal.OpWait{
			{ID: "op-1", Reason: opjournal.WaitContentBlock, Detail: detail},
		}}}
	}
	starting, held := blocked(session.ContentGateStarting), blocked(session.ContentGateHeld)
	woke := lanePassOutcome{res: opjournal.PassResult{Wake: true}, wake: session.WakeRecorded}
	took := lanePassOutcome{res: opjournal.PassResult{Took: opjournal.TakeContent}}
	for _, tc := range []struct {
		name    string
		before  []lanePassOutcome
		advance time.Duration
		quick   bool
	}{
		{name: "no wake recorded"},
		{name: "after a recorded wake", before: []lanePassOutcome{woke}, quick: true},
		{name: "still starting inside startFor", before: []lanePassOutcome{woke, starting}, advance: time.Minute - time.Second, quick: true},
		{name: "startFor passed", before: []lanePassOutcome{woke, starting}, advance: time.Minute},
		{name: "another wait ended it", before: []lanePassOutcome{woke, held}},
		{name: "a take ended it", before: []lanePassOutcome{woke, took}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &laneTestClock{now: laneT0}
			w := &conversationLaneWorker{clk: clk, startFor: time.Minute}
			for _, out := range tc.before {
				w.nextDelay(out)
			}
			clk.Advance(tc.advance)
			want := next.Sub(clk.Now())
			if tc.quick {
				want = laneWakeRecheck
			}
			if d, idle := w.nextDelay(starting); d != want || idle {
				t.Errorf("delay %s idle %v, want %s", d, idle, want)
			}
		})
	}
}

// TestConversationLaneHostStartedAfterOpSweep pins runController's start
// line: the statement right after cs.startOpSweep(ctx), with the
// controller's ctx and the reconciler mode it latched (V2-18).
func TestConversationLaneHostStartedAfterOpSweep(t *testing.T) {
	fn := parseFuncDecl(t, "controller.go", "runController")
	if next := calleesAfterOpSweep(fn); len(next) != 1 || next[0] != "startConversationLaneHost(ctx, wiring.mode)" {
		t.Fatalf("runController: the statements after its startOpSweep calls call %q, want one cs.startConversationLaneHost(ctx, wiring.mode)", next)
	}
}

// TestSupervisorStartsLaneHostAfterOpSweep pins startOneCity's start line:
// the statement right after cs.startOpSweep(cityCtx), as in runController,
// with the city's context, so the host stops with the city, and the
// reconciler mode the city latched (V2-18).
func TestSupervisorStartsLaneHostAfterOpSweep(t *testing.T) {
	fn := parseFuncDecl(t, "cmd_supervisor.go", "startOneCity")
	if next := calleesAfterOpSweep(fn); len(next) != 1 || next[0] != "startConversationLaneHost(cityCtx, wiring.mode)" {
		t.Fatalf("startOneCity: the statements after its startOpSweep calls call %q, want one cs.startConversationLaneHost(cityCtx, wiring.mode)", next)
	}
}

// parseFuncDecl parses file, a source file of this package, and returns its
// func name.
func parseFuncDecl(t *testing.T, file, name string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	fn := findFuncDecl(f, name)
	if fn == nil {
		t.Fatalf("%s: no func %s", file, name)
	}
	return fn
}

// calleesAfterOpSweep returns, for each statement of fn that calls
// startOpSweep, what the statement right after it calls, as
// name(identifier arguments) ("" when that is not a call, or there is
// none).
func calleesAfterOpSweep(fn *ast.FuncDecl) []string {
	var next []string
	ast.Inspect(fn, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for i, stmt := range block.List {
			if opSweepCallee(stmt) != "startOpSweep" {
				continue
			}
			name := ""
			if i+1 < len(block.List) {
				if expr, ok := block.List[i+1].(*ast.ExprStmt); ok {
					if call, ok := expr.X.(*ast.CallExpr); ok {
						name = laneCallName(call) + "(" + laneCallArgs(call) + ")"
					}
				}
			}
			next = append(next, name)
		}
		return true
	})
	return next
}

// laneCallArgs is call's arguments that are identifiers, or selectors of
// an identifier (X.Sel), comma-separated; any other argument reads "?".
func laneCallArgs(call *ast.CallExpr) string {
	args := make([]string, 0, len(call.Args))
	for _, a := range call.Args {
		name := "?"
		switch arg := a.(type) {
		case *ast.Ident:
			name = arg.Name
		case *ast.SelectorExpr:
			if x, ok := arg.X.(*ast.Ident); ok {
				name = x.Name + "." + arg.Sel.Name
			}
		}
		args = append(args, name)
	}
	return strings.Join(args, ", ")
}

// laneCallName is the name call calls: a method's or a function's.
func laneCallName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	}
	return ""
}

// TestStartConversationLaneHostDelivers drives startConversationLaneHost on
// a directly built controllerState: the boot scan finds the open lane op and
// the live executor delivers it to the fake runtime.
func TestStartConversationLaneHostDelivers(t *testing.T) {
	f := newLaneFixture(t)
	// The started host reads the wall clock, so the op is proposed on it
	// too; a fixed fixture time would expire before the host starts it.
	f.clk.mu.Lock()
	f.clk.now = time.Now().UTC()
	f.clk.mu.Unlock()
	f.cfg.Ops.Authoritative = []string{"message"}
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	cs := &controllerState{cityBeadStore: f.store, cfg: f.cfg, sp: f.sp, cityPath: f.dir, beadsQuiescent: new(atomic.Bool)}
	ctx, cancel := context.WithCancel(context.Background())
	h := cs.startConversationLaneHost(ctx, reconcilerLegacy)
	t.Cleanup(func() {
		cancel()
		h.wg.Wait()
	})
	deadline := time.After(laneWaitBudget)
	for {
		if got := f.get(t, st, op.ID); got.Status == opjournal.StatusClosed {
			if got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
				t.Fatalf("op = %+v nudges %d, want one succeeded delivery", got.Outcome, len(f.nudges()))
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the started host never delivered the op")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if s := conversationLaneStatusFor(f.dir, time.Now()); s.State != laneHostStateRunning || len(s.Kinds) != 1 {
		t.Fatalf("status = %+v, want running with the latched list", s)
	}
}

// TestLaneStopReading (V2-18) pins how a lane host reads stop requests by
// its controller's latched mode: as residue under legacy, which ignores the
// keys (the R1 rollback), and live under v2.
func TestLaneStopReading(t *testing.T) {
	if got := laneStopReading(reconcilerLegacy); got != session.StopRequestsIgnored {
		t.Errorf("legacy reads %d, want StopRequestsIgnored", got)
	}
	if got := laneStopReading(reconcilerV2); got != session.StopRequestsLive {
		t.Errorf("v2 reads %d, want StopRequestsLive", got)
	}
}

// TestStartConversationLaneHostReadsStopRequestsByItsLatchedMode (V2-18)
// drives startConversationLaneHost over a row carrying v2's stop request at
// its generation. A controller that latched legacy, as after an R1
// rollback, delivers: legacy will never carry the request out. One that
// latched v2 holds the prompt as content_block stop_requested, and
// releases it once the request clears: each pass reads the request afresh,
// so a cleared request never keeps holding content.
func TestStartConversationLaneHostReadsStopRequestsByItsLatchedMode(t *testing.T) {
	start := func(t *testing.T, mode reconcilerMode) (*laneFixture, *opjournal.Store, opjournal.Op, *conversationLaneHost) {
		t.Helper()
		f := newLaneFixture(t, laneStopRequestedAt(1))
		// The started host reads the wall clock, so the op is proposed on
		// it too.
		f.clk.mu.Lock()
		f.clk.now = time.Now().UTC()
		f.clk.mu.Unlock()
		f.cfg.Ops.Authoritative = []string{"message"}
		st := f.laneStore(t)
		op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
		cs := &controllerState{cityBeadStore: f.store, cfg: f.cfg, sp: f.sp, cityPath: f.dir, beadsQuiescent: new(atomic.Bool)}
		ctx, cancel := context.WithCancel(context.Background())
		h := cs.startConversationLaneHost(ctx, mode)
		t.Cleanup(func() {
			cancel()
			h.wg.Wait()
		})
		return f, st, op, h
	}
	waitStopRequested := func(t *testing.T, f *laneFixture, st *opjournal.Store, op opjournal.Op) {
		t.Helper()
		deadline := time.After(laneWaitBudget)
		for !laneStatusWaitsFor(conversationLaneStatusFor(f.dir, time.Now()), op.ID, opjournal.WaitContentBlock, session.ContentGateStopRequested) {
			select {
			case <-deadline:
				t.Fatalf("the v2 host never reported the op waiting on stop_requested: %+v", conversationLaneStatusFor(f.dir, time.Now()).Waiting)
			case <-time.After(5 * time.Millisecond):
			}
		}
		if got := f.get(t, st, op.ID); got.Status != opjournal.StatusSubmitted || len(f.nudges()) != 0 {
			t.Fatalf("op = %s, nudges %d; want submitted and nothing typed", got.Status, len(f.nudges()))
		}
	}
	t.Run("legacy: the R1 rollback delivers", func(t *testing.T) {
		f, st, op, _ := start(t, reconcilerLegacy)
		deadline := time.After(laneWaitBudget)
		for {
			if got := f.get(t, st, op.ID); got.Status == opjournal.StatusClosed {
				if got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
					t.Fatalf("op = %+v nudges %d, want one succeeded delivery", got.Outcome, len(f.nudges()))
				}
				return
			}
			select {
			case <-deadline:
				t.Fatal("the legacy host never delivered past a request legacy ignores")
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	t.Run("v2 holds the prompt", func(t *testing.T) {
		f, st, op, _ := start(t, reconcilerV2)
		waitStopRequested(t, f, st, op)
	})
	t.Run("v2 releases the prompt once the request clears", func(t *testing.T) {
		f, st, op, h := start(t, reconcilerV2)
		waitStopRequested(t, f, st, op)
		if err := f.store.SetMetadataBatch(f.id, stopCancelPatch()); err != nil {
			t.Fatal(err)
		}
		h.Poke(f.id)
		deadline := time.After(laneWaitBudget)
		for {
			if got := f.get(t, st, op.ID); got.Status == opjournal.StatusClosed {
				if got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
					t.Fatalf("op = %+v nudges %d, want one succeeded delivery", got.Outcome, len(f.nudges()))
				}
				return
			}
			select {
			case <-deadline:
				t.Fatal("the v2 host kept holding the prompt after the stop request cleared")
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
}

// laneStatusWaitsFor reports whether st shows op waiting for reason with
// detail.
func laneStatusWaitsFor(st conversationLaneStatus, op string, reason opjournal.WaitReason, detail string) bool {
	for _, conv := range st.Waiting {
		for _, w := range conv.Waits {
			if w.Op == op && w.Reason == string(reason) && w.Detail == detail {
				return true
			}
		}
	}
	return false
}

// laneJournalingStore is a journaling sessions store whose guarded batches
// journal every row they write, as a journaling bd's do, under the id prefix
// the op store discovers its namespace from.
type laneJournalingStore struct {
	*journalingStore
	mem *beads.MemStore
}

func newLaneJournalingStore() laneJournalingStore {
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	return laneJournalingStore{journalingStore: &journalingStore{Store: mem, j: newScriptedJournal()}, mem: mem}
}

func (laneJournalingStore) IDPrefix() string { return "gc" }

func (s laneJournalingStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	res, err := s.mem.ApplyGuarded(ctx, b)
	if err != nil {
		return res, err
	}
	for _, it := range b.Items {
		switch {
		case it.Create != nil:
			s.j.add(journalOpCreate, it.Create.ID, "gascity", s.image(it.Create.ID))
		case it.Update != nil:
			s.j.add("update", it.Update.ID, "gascity", s.image(it.Update.ID))
		}
	}
	return res, nil
}

// journal records a write to id that succeeded.
func (s laneJournalingStore) journal(op, id string, err error) error {
	if err == nil {
		s.j.add(op, id, "gascity", s.image(id))
	}
	return err
}

func (s laneJournalingStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	return s.journal("update", id, s.mem.UpdateIfMatch(id, rev, opts))
}

func (s laneJournalingStore) CloseIfMatch(id string, rev int64) error {
	return s.journal(journalOpClose, id, s.mem.CloseIfMatch(id, rev))
}

func (s laneJournalingStore) DeleteIfMatch(id string, rev int64) error {
	err := s.mem.DeleteIfMatch(id, rev)
	if err == nil {
		s.j.add("delete", id, "gascity", "")
	}
	return err
}

func (s laneJournalingStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	ok, err := s.mem.CompareAndSetMetadataKey(id, key, expected, next)
	if ok {
		err = s.journal("update", id, err)
	}
	return ok, err
}

// TestStartConversationLaneHostRingsFromTheLedgerFeed drives the shipped
// doorbell: a host startConversationLaneHost started over a controller
// whose ledger feed sees an op row written out of band runs that
// conversation's pass, with [ops] authoritative empty, so no scan finds it.
func TestStartConversationLaneHostRingsFromTheLedgerFeed(t *testing.T) {
	store := newLaneJournalingStore()
	f := newLaneFixtureOver(t, store)
	f.clk.mu.Lock()
	f.clk.now = time.Now().UTC()
	f.clk.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feeds := newLedgerFeedSet(t.TempDir(), ledgerFeedHooks{})
	feeds.setWake(newLegacyWake(make(chan struct{}, 1), make(chan struct{}, 1)))
	att := attachLedgerFeed(ctx, &ledgerFeedRequest{set: feeds, scope: ledgerFeedCityScope, mode: config.BeadsLedgerFeedAuto}, store, true)
	if att == nil || att.entry.feed == nil {
		t.Fatalf("no feed attached: %+v", att)
	}
	cache := beads.NewCachingStore(store, att.observe(cacheChangeRecorder(events.NewFake())))
	att.bind(ctx, feeds, cache)
	if err := cache.Prime(ctx); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cs := &controllerState{cityBeadStore: cache, cfg: f.cfg, sp: f.sp, cityPath: f.dir, beadsQuiescent: new(atomic.Bool), ledgerFeeds: feeds}
	hctx, hcancel := context.WithCancel(ctx)
	h := cs.startConversationLaneHost(hctx, reconcilerLegacy)
	t.Cleanup(func() {
		hcancel()
		h.wg.Wait()
	})
	h.wg.Wait() // the boot scan found nothing and, with the list empty, the loop ended
	if s := h.status(time.Now()); s.State != laneHostStateRunning || s.Errors != 0 {
		t.Fatalf("status = %+v, want running, no errors", s)
	}

	st := f.laneStore(t) // over the backing store: out of band for the cache
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	for range 10 {
		more, err := att.entry.feed.tailer.Step(ctx)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if !more {
			break
		}
	}
	deadline := time.After(laneWaitBudget)
	for {
		if got := f.get(t, st, op.ID); got.Status == opjournal.StatusClosed {
			if got.Outcome.Arm != opjournal.ArmSucceeded || len(f.nudges()) != 1 {
				t.Fatalf("op = %+v nudges %d, want one succeeded delivery", got.Outcome, len(f.nudges()))
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("the ledger feed's out-of-band op row never rang the started host")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestLaneHostParksLongWaits proves a worker whose next pass is more than
// laneParkAfter away gives up its slot, and its park timer pokes it back.
func TestLaneHostParksLongWaits(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "much later", func(r *opjournal.Request) {
		r.NotBefore = laneT0.Add(time.Hour)
	}))
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.nextPass(t)
	if hh.nextExit(t) != f.id {
		t.Fatal("the worker kept its slot")
	}
	tm := hh.nextTimer(t)
	if tm.d != time.Hour {
		t.Fatalf("park timer = %s, want 1h", tm.d)
	}
	if s := hh.h.status(f.clk.Now()); s.Workers != 0 || s.Parked != 1 {
		t.Fatalf("status = %+v, want no worker and one parked", s)
	}
	f.clk.Advance(time.Hour)
	tm.c <- f.clk.Now()
	if p := hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass after the park = %+v, want accepted", p.out.res)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v", got.Outcome)
	}
}

// TestLaneHostWaitsForAMissingStore proves a controller with no sessions
// store at start is not refused: the host reports unavailable, re-checks on
// the scan cadence, and runs once the store appears.
func TestLaneHostWaitsForAMissingStore(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	hh := newLaneHostHarness(t, f)
	hh.env.setStore(nil)
	hh.start()
	tick := hh.waitTicker(t)
	if s := hh.h.status(f.clk.Now()); s.State != laneHostStateUnavailable || !strings.Contains(s.Reason, "no sessions store yet") {
		t.Fatalf("status = %+v, want unavailable for the missing store", s)
	}
	if logs := hh.logLines(); len(logs) != 0 {
		t.Fatalf("logs = %q, want no refusal for a missing store", logs)
	}
	if got := laneSocketReply(f.dir, f.id); !strings.HasPrefix(got, "refused the controller has no sessions store yet") {
		t.Fatalf("lane: reply = %q, want the unavailable reason", got)
	}
	hh.env.setStore(f.store)
	tick.c <- f.clk.Now()
	if p := hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass once the store appeared = %+v, want the delivery", p.out.res)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v", got.Outcome)
	}
	if s := hh.h.status(f.clk.Now()); s.State != laneHostStateRunning {
		t.Fatalf("status = %+v, want running", s)
	}
}

// laneBoundaryExec probes a running live runtime at a fixed boundary, and
// delivers whatever it is given.
type laneBoundaryExec struct {
	clk      *laneTestClock
	boundary opjournal.Tri
}

func (e laneBoundaryExec) Probe(_ context.Context, conv opjournal.ConversationView) (opjournal.Probe, error) {
	return opjournal.Probe{
		Available: true, Running: true, Boundary: e.boundary, Fence: laneFence(conv),
		Caps: opjournal.Caps{Live: true, Input: opjournal.FenceAtomic},
	}, nil
}

func (e laneBoundaryExec) Deliver(context.Context, opjournal.Delivery) opjournal.DeliveryResult {
	return opjournal.Delivered{Record: opjournal.DeliveryRecord{At: e.clk.Now(), Ack: opjournal.AckDelivered}, Via: "test"}
}

func (laneBoundaryExec) Interrupt(context.Context, opjournal.InterruptCall) opjournal.ControlResult {
	return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
}

func (laneBoundaryExec) Respond(context.Context, opjournal.RespondCall) opjournal.ControlResult {
	return opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
}

// TestLaneHostBusyWorkersYieldToTheQueue proves busy conversations do not
// keep their slots while another waits: with every slot held by a busy
// conversation, the queued idle one delivers without any timer firing.
func TestLaneHostBusyWorkersYieldToTheQueue(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	busy := map[string]bool{f.id: true}
	b := addSession(t, f, "b")
	busy[b] = true
	idle := addSession(t, f, "c")
	for i, conv := range []string{f.id, b, idle} {
		r := f.msg(fmt.Sprintf("k%d", i), opjournal.RolePrompt, "hello")
		r.Conversation = conv
		f.propose(t, st, r)
	}
	hh := newLaneHostHarness(t, f)
	hh.h.maxWorkers = 2
	hh.h.newExec = func(_ laneExecDeps, row laneRow) opjournal.Executor {
		if busy[row.info.ID] {
			return laneBoundaryExec{clk: f.clk, boundary: opjournal.TriNo}
		}
		return laneBoundaryExec{clk: f.clk, boundary: opjournal.TriYes}
	}
	hh.start()
	for {
		p := hh.nextPass(t)
		if busy[p.conv] {
			if !p.out.res.Busy {
				t.Fatalf("busy conversation's pass = %+v, want busy", p.out.res)
			}
			continue
		}
		if p.conv != idle || p.out.res.Done != opjournal.DoneAccepted {
			t.Fatalf("pass = %+v, want the queued idle conversation's delivery", p)
		}
		break
	}
	// A yielding worker parks in the step that hands its slot over, so its
	// park is in place before the queued conversation's first pass.
	if s := hh.h.status(f.clk.Now()); s.Parked == 0 {
		t.Fatalf("status = %+v, want a busy conversation parked on its backoff", s)
	}
}

// TestLaneHostReprobesABusyDrainNoticeWithoutAPass proves a busy drain
// notice in a draining row backs off on re-probes like any busy content: the
// row's draining block, which the probe admits the notice past, is not a
// change a full pass must plan.
func TestLaneHostReprobesABusyDrainNoticeWithoutAPass(t *testing.T) {
	f := newLaneFixture(t, func(meta map[string]string) {
		meta["state"] = string(session.StateDraining)
		meta["state_reason"] = session.DrainAckStopPendingReason
	})
	st := f.laneStore(t)
	f.propose(t, st, f.msg("drain", opjournal.RoleNotice, "ack your drain", func(r *opjournal.Request) {
		r.Producer = opjournal.Producer{Principal: laneDrainNoticeClass.Principal, Actor: opjournal.ActorController}
		r.Policy.Message = opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly}
	}))
	hh := newLaneHostHarness(t, f)
	hh.h.newExec = func(deps laneExecDeps, row laneRow) opjournal.Executor {
		e := newConversationLaneExec(deps, row)
		e.idle = func(nudgeTarget, worker.LiveObservation) bool { return false }
		return e
	}
	hh.start()
	if p := hh.nextPass(t); !p.out.res.Busy {
		t.Fatalf("first pass = %+v, want busy", p.out.res)
	}
	tm := hh.nextTimer(t)
	f.clk.Advance(tm.d)
	tm.c <- f.clk.Now()
	if !hh.nextReprobe(t) {
		t.Fatal("the re-probe of a busy drain notice reported a change")
	}
	hh.nextTimer(t)
	hh.noPass(t)
}

// TestLaneHostReprobesBusyRuntimeWithoutRows proves a busy conversation's
// backoff re-probes the runtime without a full pass or any op-row read, and
// the first re-probe that finds a boundary runs the pass that delivers. The
// executor resolves the runtime through the controller's sessions store, not
// the live-read wrapper.
func TestLaneHostReprobesBusyRuntimeWithoutRows(t *testing.T) {
	var mu sync.Mutex
	var gets []string
	var lists []beads.ListQuery
	f := newLaneFixtureOver(t, laneReadLog{laneTestStore: newLaneTestStore(), mu: &mu, gets: &gets, lists: &lists})
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	var idle atomic.Bool
	hh := newLaneHostHarness(t, f)
	hh.h.newExec = func(deps laneExecDeps, row laneRow) opjournal.Executor {
		if _, live := deps.sessions.(laneLiveSessions); live {
			t.Error("the executor reads the session through the live-read wrapper")
		}
		e := newConversationLaneExec(deps, row)
		e.idle = func(nudgeTarget, worker.LiveObservation) bool { return idle.Load() }
		return e
	}
	hh.start()
	if p := hh.nextPass(t); !p.out.res.Busy {
		t.Fatalf("first pass = %+v, want busy", p.out.res)
	}
	tm := hh.nextTimer(t)
	if tm.d != laneBusyFirst {
		t.Fatalf("backoff = %s, want %s", tm.d, laneBusyFirst)
	}
	mu.Lock()
	gets, lists = nil, nil
	mu.Unlock()
	f.clk.Advance(tm.d)
	tm.c <- f.clk.Now()
	if !hh.nextReprobe(t) {
		t.Fatal("the re-probe of a busy runtime reported a change")
	}
	tm = hh.nextTimer(t)
	hh.noPass(t)
	mu.Lock()
	runtimeRow, _ := opjournal.RuntimeRowID(st.Prefix(), f.id)
	for _, id := range gets {
		if id == runtimeRow {
			t.Errorf("the re-probe read the runtime row")
		}
	}
	for _, q := range lists {
		if q.Label == opjournal.LabelOp {
			t.Errorf("the re-probe listed op rows: %+v", q)
		}
	}
	mu.Unlock()
	idle.Store(true)
	f.clk.Advance(tm.d)
	tm.c <- f.clk.Now()
	if hh.nextReprobe(t) {
		t.Fatal("the re-probe that found a boundary reported still busy")
	}
	if p := hh.nextPass(t); p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass after the boundary = %+v, want the delivery", p.out.res)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = %+v", got.Outcome)
	}
	if s := hh.h.status(f.clk.Now()); s.Reprobes != 2 {
		t.Fatalf("status = %+v, want two re-probes", s)
	}
}

// TestLaneHostWakeNotDormantBacksOff proves a wake the row refuses because
// it is not dormant (a crashed runtime the reconciler has not healed) is not
// re-asked every few seconds, and the status shows the op's wait and the
// answer.
func TestLaneHostWakeNotDormantBacksOff(t *testing.T) {
	f := newLaneFixture(t)
	if err := f.sp.Stop(f.name); err != nil {
		t.Fatal(err)
	}
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "wake up", func(r *opjournal.Request) {
		r.Policy.Message.Wake = opjournal.WakeIfNeeded
	}))
	hh := newLaneHostHarness(t, f)
	hh.start()
	p := hh.nextPass(t)
	if !p.out.res.Wake || p.out.wake != session.WakeNotDormant {
		t.Fatalf("pass = %+v, want a wake refused as not dormant", p.out)
	}
	if tm := hh.nextTimer(t); tm.d != laneWakeHeldRecheck {
		t.Fatalf("recheck = %s, want %s", tm.d, laneWakeHeldRecheck)
	}
	s := hh.h.status(f.clk.Now())
	if len(s.Waiting) != 1 || s.Waiting[0].Conversation != f.id || s.Waiting[0].Wake != "not_dormant" ||
		len(s.Waiting[0].Waits) != 1 || s.Waiting[0].Waits[0].Op != op.ID || s.Waiting[0].Waits[0].Reason != string(opjournal.WaitNotRunning) {
		t.Fatalf("waiting = %+v, want the op's not_running wait and the not_dormant answer", s.Waiting)
	}
}

// laneFailingExec fails every probe.
type laneFailingExec struct{ laneBoundaryExec }

func (laneFailingExec) Probe(context.Context, opjournal.ConversationView) (opjournal.Probe, error) {
	return opjournal.Probe{}, errors.New("runtime exploded")
}

// TestLaneHostLogsFailures proves a failing pass is logged once per interval
// and reported as the last pass failing, and a pass that succeeds clears it.
func TestLaneHostLogsFailures(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	var fail atomic.Bool
	fail.Store(true)
	hh := newLaneHostHarness(t, f)
	hh.h.newExec = func(_ laneExecDeps, _ laneRow) opjournal.Executor {
		if fail.Load() {
			return laneFailingExec{laneBoundaryExec{clk: f.clk}}
		}
		return laneBoundaryExec{clk: f.clk, boundary: opjournal.TriYes}
	}
	hh.start()
	for i := range 2 {
		if p := hh.nextPass(t); p.out.err == nil {
			t.Fatalf("pass = %+v, want the probe failure", p.out)
		}
		tm := hh.nextTimer(t)
		if s := hh.h.status(f.clk.Now()); !s.LastPassFailed || s.ErrorsSinceSuccess != int64(i+1) {
			t.Fatalf("status = %+v, want the last pass failed", s)
		}
		if i == 1 {
			fail.Store(false)
		}
		f.clk.Advance(tm.d)
		tm.c <- f.clk.Now()
	}
	var failed []string
	for _, l := range hh.logLines() {
		if strings.Contains(l, "conversation-lanes: pass failed") && strings.Contains(l, "runtime exploded") {
			failed = append(failed, l)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("pass failure log lines = %q, want one per interval", failed)
	}
	if p := hh.nextPass(t); p.out.err != nil || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want the delivery once the probe works", p.out)
	}
	if s := hh.h.status(f.clk.Now()); s.LastPassFailed || s.ErrorsSinceSuccess != 0 || s.LastSuccessAt == "" || s.Errors != 2 {
		t.Fatalf("status = %+v, want a success clearing the failure, two errors counted", s)
	}
}

// laneParkedBusy starts a host with one slot over a busy conversation and an
// idle one queued behind it, and returns once the busy one has yielded its
// slot and parked on its backoff, with the park's timer.
func laneParkedBusy(t *testing.T) (*laneHostHarness, string, laneTimer) {
	t.Helper()
	f := newLaneFixture(t)
	st := f.laneStore(t)
	idle := addSession(t, f, "c")
	for i, conv := range []string{f.id, idle} {
		r := f.msg(fmt.Sprintf("k%d", i), opjournal.RolePrompt, "hello")
		r.Conversation = conv
		f.propose(t, st, r)
	}
	hh := newLaneHostHarness(t, f)
	hh.h.maxWorkers = 1
	hh.h.newExec = func(_ laneExecDeps, row laneRow) opjournal.Executor {
		if row.info.ID == f.id {
			return laneBoundaryExec{clk: f.clk, boundary: opjournal.TriNo}
		}
		return laneBoundaryExec{clk: f.clk, boundary: opjournal.TriYes}
	}
	hh.start()
	if p := hh.nextPass(t); p.conv != f.id || !p.out.res.Busy {
		t.Fatalf("first pass = %+v, want the busy conversation's", p)
	}
	park := hh.nextTimer(t)
	if park.d != laneBusyFirst {
		t.Fatalf("park timer = %s, want the busy backoff %s", park.d, laneBusyFirst)
	}
	// The yielding worker exits after its park, so the two exits race.
	if exits := map[string]bool{hh.nextExit(t): true, hh.nextExit(t): true}; !exits[f.id] || !exits[idle] {
		t.Fatalf("exits = %v, want the busy worker to yield and the idle one to exit", exits)
	}
	delivered := false
	for len(hh.passes) > 0 {
		if p := <-hh.passes; p.conv == idle && p.out.res.Done == opjournal.DoneAccepted {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("the queued idle conversation did not deliver in the yielded slot")
	}
	if s := hh.h.status(f.clk.Now()); s.Workers != 0 || s.Parked != 1 {
		t.Fatalf("status = %+v, want the busy conversation parked", s)
	}
	return hh, f.id, park
}

// TestLaneHostParkedConversationKeepsItsBackoff proves a parked busy
// conversation's next worker resumes the backoff where the parked one left
// it, rather than restarting at the first step.
func TestLaneHostParkedConversationKeepsItsBackoff(t *testing.T) {
	hh, busy, park := laneParkedBusy(t)
	hh.f.clk.Advance(laneBusyFirstFor + time.Second)
	park.c <- hh.f.clk.Now()
	if p := hh.nextPass(t); p.conv != busy || !p.out.res.Busy {
		t.Fatalf("pass after the park = %+v, want the busy conversation", p)
	}
	if tm := hh.nextTimer(t); tm.d != laneBusyNext {
		t.Fatalf("backoff after the park = %s, want %s: busy since before it parked", tm.d, laneBusyNext)
	}
}

// TestLaneHostScanStartedWorkerStopsThePark proves a worker the anti-entropy
// scan starts for a parked conversation stops the park's timer, so the park
// cannot poke it again.
func TestLaneHostScanStartedWorkerStopsThePark(t *testing.T) {
	hh, busy, park := laneParkedBusy(t)
	hh.waitTicker(t).c <- hh.f.clk.Now()
	if p := hh.nextPass(t); p.conv != busy {
		t.Fatalf("pass after the scan = %+v, want the parked conversation", p)
	}
	select {
	case <-park.stopped:
	case <-time.After(laneWaitBudget):
		t.Fatal("the park's timer was not stopped when the scan started a worker")
	}
	if s := hh.h.status(hh.f.clk.Now()); s.Parked != 0 {
		t.Fatalf("status = %+v, want nothing parked", s)
	}
}

// sameLaneWorkerState reports whether a and b hold the same backoff and wake.
func sameLaneWorkerState(a, b laneWorkerState) bool {
	return a.busySince.Equal(b.busySince) && a.errors == b.errors && a.wokeAt.Equal(b.wokeAt)
}

// TestLaneHostReleaseParksInOneStep proves a yielding worker's release parks
// its conversation in the step that frees the slot, so no doorbell finds the
// conversation neither served nor parked: a poke right after the release
// starts the next worker from the parked state and stops the park's timer,
// and no park stays beside that worker. A release a poke refuses parks
// nothing.
func TestLaneHostReleaseParksInOneStep(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f)
	hh.h.newExec = func(laneExecDeps, laneRow) opjournal.Executor {
		return laneBoundaryExec{clk: f.clk, boundary: opjournal.TriNo}
	}
	hh.start()
	hh.h.wg.Wait() // the boot scan found nothing and the loop ended
	f.propose(t, f.laneStore(t), f.msg("k1", opjournal.RolePrompt, "hello"))
	w := &laneWorker{poke: make(chan struct{}, 1)}
	hh.h.mu.Lock()
	hh.h.workers[f.id] = w
	hh.h.mu.Unlock()
	now := f.clk.Now()
	want := laneWorkerState{busySince: now.Add(-(laneBusyFirstFor + time.Second)), errors: 3, wokeAt: now.Add(-time.Second)}
	park := &lanePark{after: laneBusyFirst, state: want}

	w.poke <- struct{}{}
	if hh.h.retire(f.id, w, false, park) {
		t.Fatal("the release gave the slot up with a poke since the pass")
	}
	if s := hh.h.status(now); s.Workers != 1 || s.Parked != 0 || len(hh.timers) != 0 {
		t.Fatalf("status = %+v, %d timers, want the slot kept and nothing parked", s, len(hh.timers))
	}

	if !hh.h.retire(f.id, w, false, park) {
		t.Fatal("the release kept the slot")
	}
	timer := hh.nextTimer(t)
	if timer.d != laneBusyFirst {
		t.Fatalf("park timer = %s, want %s", timer.d, laneBusyFirst)
	}
	hh.h.mu.Lock()
	parked, served := hh.h.parked[f.id], hh.h.workers[f.id] != nil
	hh.h.mu.Unlock()
	if parked == nil || !sameLaneWorkerState(parked.state, want) || served {
		t.Fatalf("after the release: parked %+v, served %v; want parked from %+v, not served", parked, served, want)
	}
	if !hh.h.Poke(f.id) {
		t.Fatal("Poke refused on a running host")
	}
	select {
	case <-timer.stopped:
	case <-time.After(laneWaitBudget):
		t.Fatal("the worker the poke started did not stop the park's timer")
	}
	if p := hh.nextPass(t); p.conv != f.id || !p.out.res.Busy {
		t.Fatalf("pass after the poke = %+v, want the busy conversation", p)
	}
	if tm := hh.nextTimer(t); tm.d != laneBusyNext {
		t.Fatalf("backoff after the poke = %s, want %s: busy since before the park", tm.d, laneBusyNext)
	}
	if s := hh.h.status(f.clk.Now()); s.Workers != 1 || s.Parked != 0 {
		t.Fatalf("status = %+v, want one worker and nothing parked", s)
	}
}

// TestLaneHostFiredParkQueuesWithItsState proves a park that fires while
// every slot is held queues its conversation and keeps the parked state for
// it: the worker the freed slot starts resumes the backoff, and nothing stays
// parked.
func TestLaneHostFiredParkQueuesWithItsState(t *testing.T) {
	hh, busy, park := laneParkedBusy(t)
	holder := addSession(t, hh.f, "holder")
	w := &laneWorker{poke: make(chan struct{}, 1)}
	hh.h.mu.Lock()
	hh.h.workers[holder] = w // the one slot
	hh.h.mu.Unlock()
	hh.f.clk.Advance(laneBusyFirstFor + time.Second)
	park.c <- hh.f.clk.Now()
	// A fired park stops its timer once its poke is done.
	select {
	case <-park.stopped:
	case <-time.After(laneWaitBudget):
		t.Fatal("the fired park did not finish")
	}
	if s := hh.h.status(hh.f.clk.Now()); s.Workers != 1 || s.Queued != 1 || s.Parked != 1 {
		t.Fatalf("status = %+v, want the conversation queued behind the held slot, its state kept", s)
	}
	hh.noPass(t)
	if !hh.h.retire(holder, w, true, nil) {
		t.Fatal("the forced release kept the slot")
	}
	if p := hh.nextPass(t); p.conv != busy || !p.out.res.Busy {
		t.Fatalf("pass in the freed slot = %+v, want the busy conversation", p)
	}
	if tm := hh.nextTimer(t); tm.d != laneBusyNext {
		t.Fatalf("backoff after the queue = %s, want %s: busy since before it parked", tm.d, laneBusyNext)
	}
	if s := hh.h.status(hh.f.clk.Now()); s.Workers != 1 || s.Queued != 0 || s.Parked != 0 {
		t.Fatalf("status = %+v, want the busy conversation served and nothing parked", s)
	}
}

// laneFailingListStore fails every List.
type laneFailingListStore struct{ laneTestStore }

func (laneFailingListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list exploded")
}

// TestLaneHostScanFailureCounts proves a failed scan, not only a failed
// pass, counts in errors_since_success and is logged.
func TestLaneHostScanFailureCounts(t *testing.T) {
	f := newLaneFixtureOver(t, laneFailingListStore{newLaneTestStore()})
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.waitTicker(t)
	if s := hh.h.status(f.clk.Now()); s.Errors != 1 || s.ErrorsSinceSuccess != 1 || s.LastPassFailed || !strings.Contains(s.LastError, "list exploded") {
		t.Fatalf("status = %+v, want the scan failure counted", s)
	}
	if logs := hh.logLines(); len(logs) != 1 || !strings.Contains(logs[0], "conversation-lanes: scan failed") {
		t.Fatalf("logs = %q, want the scan failure", logs)
	}
}

// TestLaneHostLogsAbsentConversation proves a pass over a conversation with
// no session row logs the absence its open ops close for.
func TestLaneHostLogsAbsentConversation(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	ghost := "gc-ghost"
	r := f.msg("k1", opjournal.RolePrompt, "anyone?")
	r.Conversation = ghost
	op := f.propose(t, st, r)
	hh := newLaneHostHarness(t, f)
	hh.start()
	for {
		if p := hh.nextPass(t); p.conv == ghost {
			break
		}
	}
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmSkipped {
		t.Fatalf("op = %+v, want it closed skipped", got.Outcome)
	}
	want := "conversation-lanes: conversation " + ghost + " has no session row on two reads"
	for _, l := range hh.logLines() {
		if strings.Contains(l, want) {
			return
		}
	}
	t.Fatalf("logs = %q, want %q", hh.logLines(), want)
}

// TestLaneConversationsOfRows pins which rows ring the ledger-feed doorbell:
// a lane op row, a closed op row (a lane op's after can name it), a runtime
// row and an op row that does not decode name their conversation, once
// each, in first-seen order; an open legacy op row and any other row name
// none.
func TestLaneConversationsOfRows(t *testing.T) {
	row := func(f *laneFixture, st *opjournal.Store, key string, executor opjournal.ExecutorKind) beads.Bead {
		t.Helper()
		op := f.propose(t, st, f.msg(key, opjournal.RolePrompt, "hi", func(r *opjournal.Request) { r.Executor = executor }))
		b, err := f.store.Get(op.ID)
		if err != nil {
			t.Fatalf("reading op row %s: %v", op.ID, err)
		}
		return b
	}
	f := newLaneFixture(t)
	st := f.laneStore(t)
	lane := row(f, st, "k1", opjournal.ExecutorLane)
	legacy := row(f, st, "k2", opjournal.ExecutorLegacy)
	closedLegacy := legacy
	closedLegacy.Status = "closed"
	rows := []beads.Bead{
		{ID: "gc-s1", Labels: []string{"gc:session"}},
		legacy,
		{ID: "gc-rt-1", Labels: []string{opjournal.LabelRuntime, opjournal.RuntimeConversationLabel("gc-b")}},
		lane,
		{ID: "gc-op-x", Labels: []string{opjournal.LabelOp, opjournal.ConversationLabel("gc-c")}},
		{ID: "gc-fact-1", Labels: []string{opjournal.LabelFact, opjournal.FactConversationLabel("gc-d")}},
	}
	if got, want := laneConversationsOfRows(rows), []string{"gc-b", f.id, "gc-c"}; !slices.Equal(got, want) {
		t.Fatalf("conversations = %q, want %q", got, want)
	}
	if got := laneConversationsOfRows([]beads.Bead{legacy}); len(got) != 0 {
		t.Fatalf("an open legacy op row rang %q, want nothing", got)
	}
	if got := laneConversationsOfRows([]beads.Bead{closedLegacy}); !slices.Equal(got, []string{f.id}) {
		t.Fatalf("a closed legacy op row rang %q, want %q", got, f.id)
	}
}

// TestLaneHostLedgerFeedDoorbellRunsAPass proves a changed op row the
// ledger feed hands the host pokes its conversation: its pass delivers.
func TestLaneHostLedgerFeedDoorbellRunsAPass(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f)
	hh.start()
	hh.h.wg.Wait() // the boot scan found nothing and the loop ended
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hello"))
	row, err := f.store.Get(op.ID)
	if err != nil {
		t.Fatalf("reading the op row: %v", err)
	}
	hh.h.pokeChangedRows([]beads.Bead{{ID: "gc-other", Labels: []string{"gc:session"}}, row})
	if p := hh.nextPass(t); p.conv != f.id || p.out.res.Done != opjournal.DoneAccepted {
		t.Fatalf("pass = %+v, want an accepted delivery over %s", p, f.id)
	}
}

// TestLedgerFeedRingsTheLaneHost proves the feed hands every refresh pass's
// changed rows to the lane host's doorbell, under the legacy reconciler and
// under v2 alike, and rings nothing for the echo of a write the cache made.
func TestLedgerFeedRingsTheLaneHost(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%v", v2), func(t *testing.T) {
			h := newFeedHarness(t, newJournalingStore())
			if v2 {
				h.set.setWake(&controllerWake{planner: newTestPlanner()})
			}
			var mu sync.Mutex
			var rung []string
			h.set.setRowsChanged(func(rows []beads.Bead) {
				mu.Lock()
				defer mu.Unlock()
				rung = append(rung, laneConversationsOfRows(rows)...)
			})
			labels := []string{opjournal.LabelOp, opjournal.ConversationLabel("gc-conv")}
			if _, err := h.cache.Create(beads.Bead{Title: "own op", Labels: labels}); err != nil {
				t.Fatal(err)
			}
			h.step()
			if _, err := h.store.Create(beads.Bead{Title: "out-of-band op", Labels: labels}); err != nil {
				t.Fatal(err)
			}
			h.step()
			mu.Lock()
			defer mu.Unlock()
			if want := []string{"gc-conv"}; !slices.Equal(rung, want) {
				t.Fatalf("doorbell rang for %q, want %q: the out-of-band row once, the echo never", rung, want)
			}
		})
	}
}
