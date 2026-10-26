package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/rollout"
)

var opSweepT0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// opSweepStore is a recording store with the id prefix a production
// sessions store reports, so opjournal.NewStore discovers its namespace.
type opSweepStore struct {
	*beadstest.OpRecordingStore
}

func (opSweepStore) IDPrefix() string { return "gc" }

func newOpSweepStore() opSweepStore {
	return opSweepStore{beadstest.NewOpRecordingStore(&beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true})}
}

// newOpSweepSQLiteStore is a SQLite sessions store under the gc prefix, the
// one store the builtin auto default records over.
func newOpSweepSQLiteStore(t *testing.T) beads.Store {
	t.Helper()
	st, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gc"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = st.(interface{ CloseStore() error }).CloseStore() })
	return st
}

// opSweepLists counts the op-row reads the store saw.
func opSweepLists(rec *beadstest.OpRecordingStore) int {
	n := 0
	for _, op := range rec.Ops() {
		if op.Kind == beadstest.OpList {
			n++
		}
	}
	return n
}

func opSweepWrites(rec *beadstest.OpRecordingStore) int {
	n := 0
	for _, op := range rec.Ops() {
		if op.Kind == beadstest.OpUpdateIfMatch || op.Kind == beadstest.OpUpdate || op.Kind == beadstest.OpClose {
			n++
		}
	}
	return n
}

// opsGatesOff turns all three ops gates off, the opt-out from their auto
// default (D-GATES), so a test starts from a city that records no ops and
// turns on only the gates it names after it.
func opsGatesOff(opts ...rollout.ForTestOption) []rollout.ForTestOption {
	return append([]rollout.ForTestOption{
		rollout.WithOpsContent(rollout.Off),
		rollout.WithOpsControl(rollout.Off),
		rollout.WithOpsLifecycle(rollout.Off),
	}, opts...)
}

// newOpSweepCS is a controller over store with every ops gate off but those
// opts turn on.
func newOpSweepCS(store beads.Store, opts ...rollout.ForTestOption) *controllerState {
	return &controllerState{cityBeadStore: store, rolloutFlags: rollout.ForTest(opsGatesOff(opts...)...), beadsQuiescent: new(atomic.Bool)}
}

// opSweepClock is a fake clock safe to move while the loop goroutine reads it.
type opSweepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *opSweepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *opSweepClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func (c *opSweepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// opSweepHarness is a sweeper over a fake clock with a hand-driven ticker
// and a captured degrade hook.
type opSweepHarness struct {
	s       *opSweeper
	clk     *opSweepClock
	ticks   chan time.Time
	tickers atomic.Int32

	mu        sync.Mutex
	degrades  []legacyop.Degrade
	intervals []time.Duration
	logs      []string
}

func newOpSweepHarness(cs *controllerState) *opSweepHarness {
	h := &opSweepHarness{clk: &opSweepClock{now: opSweepT0}, ticks: make(chan time.Time)}
	h.s = cs.newOpSweeper()
	h.s.clk = h.clk
	h.s.newTicker = func(d time.Duration) (<-chan time.Time, func()) {
		h.tickers.Add(1)
		h.mu.Lock()
		h.intervals = append(h.intervals, d)
		h.mu.Unlock()
		return h.ticks, func() {}
	}
	h.s.degraded = func(d legacyop.Degrade) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.degrades = append(h.degrades, d)
	}
	h.s.logf = func(format string, args ...any) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.logs = append(h.logs, fmt.Sprintf(format, args...))
	}
	return h
}

// logLines returns the sweeper's log lines that contain sub.
func (h *opSweepHarness) logLines(sub string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// tick delivers one tick to the running loop, failing fast rather than
// hanging the package when the loop has stopped listening.
func (h *opSweepHarness) tick(t *testing.T) {
	t.Helper()
	select {
	case h.ticks <- h.clk.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop is not taking ticks")
	}
}

func (h *opSweepHarness) degraded() []legacyop.Degrade {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]legacyop.Degrade(nil), h.degrades...)
}

// seedOp proposes an open op under executor at clk's now, so its finish_by
// is now plus the legacy execution bound.
func seedOp(t *testing.T, store beads.Store, clk clock.Clock, executor opjournal.ExecutorKind, key string) opjournal.Op {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: store}, clk, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("seed NewStore: %v", err)
	}
	now := clk.Now()
	op, created, err := st.Propose(opjournal.Request{
		Conversation: "gc-1", Kind: opjournal.KindMessage, Executor: executor, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Payload:  "hello", StartBy: now.Add(legacyop.ExecutionBound), FinishBy: now.Add(legacyop.ExecutionBound),
	})
	if err != nil || !created {
		t.Fatalf("seed Propose: created=%v err=%v", created, err)
	}
	return op
}

// seedUndecodableOps creates n submitted op rows of a schema this build
// cannot decode, as a newer gc would write them: gc-op-newer00000 and on.
func seedUndecodableOps(t *testing.T, store beads.Store, n int) {
	t.Helper()
	for i := range n {
		if _, err := store.Create(beads.Bead{
			ID: fmt.Sprintf("gc-op-newer%05d", i), Type: opjournal.BeadType,
			CreatedAt: opSweepT0.Add(-time.Hour).Add(time.Duration(i) * time.Millisecond),
			Labels:    []string{opjournal.LabelOp, opjournal.ConversationLabel("gc-1")},
			Metadata:  map[string]string{"gc.op.schema": "99", "gc.op.status": string(opjournal.StatusSubmitted)},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func getOp(t *testing.T, store beads.Store, id string) opjournal.Op {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: store}, &clock.Fake{Time: opSweepT0}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	op, err := st.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return op
}

// TestOpSweepStartedBesideAutocloseSweep pins the sweeper's two entry points:
// runController and startOneCity each start it on the statement right after
// startAutocloseSweep.
func TestOpSweepStartedBesideAutocloseSweep(t *testing.T) {
	for _, tc := range []struct{ file, fn string }{
		{"controller.go", "runController"},
		{"cmd_supervisor.go", "startOneCity"},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), tc.file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.file, err)
		}
		fn := findFuncDecl(file, tc.fn)
		if fn == nil {
			t.Fatalf("%s: no func %s", tc.file, tc.fn)
		}
		found := 0
		ast.Inspect(fn, func(n ast.Node) bool {
			block, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for i, stmt := range block.List {
				if opSweepCallee(stmt) != "startAutocloseSweep" {
					continue
				}
				if i+1 >= len(block.List) || opSweepCallee(block.List[i+1]) != "startOpSweep" {
					t.Errorf("%s.%s: the statement after startAutocloseSweep is not cs.startOpSweep", tc.file, tc.fn)
				}
				found++
			}
			return true
		})
		if found != 1 {
			t.Errorf("%s.%s: found %d startAutocloseSweep calls, want 1", tc.file, tc.fn, found)
		}
	}
}

func opSweepCallee(stmt ast.Stmt) string {
	expr, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return ""
	}
	call, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	return sel.Sel.Name
}

// TestStartOpSweepRunsTheBootPass drives startOpSweep on a directly built
// controllerState: with every gate off, its boot pass still closes an op
// whose finish_by has passed.
func TestStartOpSweepRunsTheBootPass(t *testing.T) {
	store := newOpSweepStore()
	op := seedOp(t, store, &clock.Fake{Time: time.Now().Add(-time.Hour)}, opjournal.ExecutorLegacyCLI, "boot")
	cs := newOpSweepCS(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs.startOpSweep(ctx)
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	deadline := time.After(10 * time.Second)
	for getOp(t, store, op.ID).Status != opjournal.StatusClosed {
		select {
		case <-poll.C:
		case <-deadline:
			t.Fatalf("the boot pass did not close %s", op.ID)
		}
	}
}

// TestOpSweepClosesOnlyExpiredLegacyOps runs a pass over a MemStore- and a
// SQLite-backed sessions store: expired legacy:api and legacy:cli ops close
// failed{deadline_in_flight} by controller:op-sweep, a lane op and an op
// within finish_by stay open, and a second pass writes nothing.
func TestOpSweepClosesOnlyExpiredLegacyOps(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T) beads.Store
	}{
		{"mem", func(*testing.T) beads.Store { return newOpSweepStore() }},
		{"sqlite", func(t *testing.T) beads.Store {
			opened, err := beads.OpenSQLiteStore(t.TempDir())
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			sqlite := opened.(*beads.SQLiteStore)
			t.Cleanup(func() { _ = sqlite.CloseStore() })
			return sqlite
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			rec := beadstest.NewOpRecordingStore(store)
			seedClk := &clock.Fake{Time: opSweepT0}
			api := seedOp(t, store, seedClk, opjournal.ExecutorLegacyAPI, "api")
			cli := seedOp(t, store, seedClk, opjournal.ExecutorLegacyCLI, "cli")
			lane := seedOp(t, store, seedClk, opjournal.ExecutorLane, "lane")
			seedClk.Advance(legacyop.ExecutionBound)
			fresh := seedOp(t, store, seedClk, opjournal.ExecutorLegacyAPI, "fresh")

			h := newOpSweepHarness(newOpSweepCS(prefixedStore{rec, store}, rollout.WithOpsContent(rollout.Auto)))
			h.clk.Set(api.FinishBy.Add(time.Second))
			if h.s.safePass() {
				t.Fatal("pass panicked")
			}
			for _, op := range []opjournal.Op{api, cli} {
				got := getOp(t, store, op.ID)
				if got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmFailed || got.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
					t.Fatalf("%s (%s): %s %+v, want closed failed{deadline_in_flight}", op.ID, op.Executor, got.Status, got.Outcome)
				}
				if by := got.Transitions[len(got.Transitions)-1].By; by != opSweepActor {
					t.Fatalf("%s closed by %q, want %q", op.ID, by, opSweepActor)
				}
			}
			for _, op := range []opjournal.Op{lane, fresh} {
				if got := getOp(t, store, op.ID); got.Status != opjournal.StatusSubmitted {
					t.Fatalf("%s (%s) was touched: %s", op.ID, op.Executor, got.Status)
				}
			}
			if d := h.degraded(); len(d) != 0 {
				t.Fatalf("degrades = %+v, want none", d)
			}
			rec.Reset()
			h.s.safePass()
			if opSweepLists(rec) == 0 {
				t.Fatal("the second pass read nothing")
			}
			if w := opSweepWrites(rec); w != 0 {
				t.Fatalf("the second pass wrote %d time(s), want none", w)
			}
		})
	}
}

// prefixedStore reports the IDPrefix of the store under a recording wrapper,
// which deliberately does not forward it.
type prefixedStore struct {
	*beadstest.OpRecordingStore
	under beads.Store
}

func (p prefixedStore) IDPrefix() string {
	if s, ok := p.under.(interface{ IDPrefix() string }); ok {
		return s.IDPrefix()
	}
	return ""
}

// TestOpSweepSkipsAQuiescentCity proves a quiescent pass reads no store.
func TestOpSweepSkipsAQuiescentCity(t *testing.T) {
	store := newOpSweepStore()
	cs := newOpSweepCS(store, rollout.WithOpsLifecycle(rollout.Require))
	cs.beadsQuiescent.Store(true)
	h := newOpSweepHarness(cs)
	h.s.safePass()
	if ops := store.Ops(); len(ops) != 0 {
		t.Fatalf("a quiescent pass touched the store: %+v", ops)
	}
}

// runOpSweep starts h's loop and returns a channel closed when it returns.
func runOpSweep(ctx context.Context, h *opSweepHarness) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.s.run(ctx)
	}()
	return done
}

func waitOpSweep(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop did not return")
	}
}

// TestOpSweepBootPassWithEveryGateOff proves the boot pass runs with every
// gate off and the ticker never starts.
func TestOpSweepBootPassWithEveryGateOff(t *testing.T) {
	store := newOpSweepStore()
	h := newOpSweepHarness(newOpSweepCS(store))
	waitOpSweep(t, runOpSweep(context.Background(), h))
	if opSweepLists(store.OpRecordingStore) == 0 {
		t.Fatal("the boot pass did not read the op rows")
	}
	if n := h.tickers.Load(); n != 0 {
		t.Fatalf("started %d ticker(s) with every gate off, want none", n)
	}
}

// TestOpSweepQuiescentBootRetries proves a boot pass a quiescent city skipped
// is retried on the ticker, even with every gate off, until it runs.
func TestOpSweepQuiescentBootRetries(t *testing.T) {
	store := newOpSweepStore()
	cs := newOpSweepCS(store)
	cs.beadsQuiescent.Store(true)
	h := newOpSweepHarness(cs)
	done := runOpSweep(context.Background(), h)
	h.tick(t) // the loop outlived the skipped boot pass
	h.tick(t) // and the skipped first tick
	if n := len(store.Ops()); n != 0 {
		t.Fatalf("a quiescent pass touched the store: %d op(s)", n)
	}
	cs.beadsQuiescent.Store(false)
	// The second tick's pass may already see the city resumed and end the
	// loop; otherwise the next tick's pass does.
	select {
	case h.ticks <- opSweepT0:
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop neither took the tick nor returned")
	}
	waitOpSweep(t, done)
	if opSweepLists(store.OpRecordingStore) == 0 {
		t.Fatal("the retried boot pass did not read the op rows")
	}
	if n := h.tickers.Load(); n != 1 {
		t.Fatalf("started %d ticker(s), want 1", n)
	}
}

// TestOpSweepTicksWhileAGateIsOn proves a gate on keeps a pass running every
// tick until ctx ends.
func TestOpSweepTicksWhileAGateIsOn(t *testing.T) {
	store := newOpSweepStore()
	h := newOpSweepHarness(newOpSweepCS(store, rollout.WithOpsControl(rollout.Auto)))
	ctx, cancel := context.WithCancel(context.Background())
	done := runOpSweep(ctx, h)
	for range 3 {
		h.tick(t)
	}
	h.tick(t) // the third tick's pass has finished
	cancel()
	waitOpSweep(t, done)
	if got := opSweepLists(store.OpRecordingStore); got < 4 {
		t.Fatalf("passes read %d time(s), want at least the boot pass plus 3 ticks", got)
	}
	if n := h.tickers.Load(); n != 1 {
		t.Fatalf("started %d ticker(s), want 1", n)
	}
	h.mu.Lock()
	intervals := append([]time.Duration(nil), h.intervals...)
	h.mu.Unlock()
	if want := clock.Backstop(opSweepInterval); len(intervals) != 1 || intervals[0] != want {
		t.Fatalf("ticker intervals = %v, want [%v] (clock.Backstop(opSweepInterval); the doctor's stale bound rests on it)", intervals, want)
	}
}

// incapableStore hides the conditional-write family, as an old bd does.
type incapableStore struct{ beads.Store }

// TestOpSweepOpenFailure proves an open failure with a gate on is one
// classified degrade per pass at the strongest gate's mode, retried next
// pass. Its gates-off half proves an empty store is never opened: the
// submitted-op probe finds nothing, so nothing is reported.
func TestOpSweepOpenFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		store  beads.Store
		reason string
	}{
		{"no conditional writes", incapableStore{beads.NewMemStore()}, legacyop.DegradeConditionalWritesUnsupported},
		{"no namespace", &beads.MemStore{HonorExplicitIDs: true}, legacyop.DegradeNoNamespace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off := newOpSweepHarness(newOpSweepCS(tc.store))
			off.s.safePass()
			if d := off.degraded(); len(d) != 0 {
				t.Fatalf("gates off: degrades = %+v, want none", d)
			}
			if l := off.logLines("op journal degraded"); len(l) != 0 {
				t.Fatalf("gates off: an empty store was opened: %q", l)
			}

			cs := newOpSweepCS(tc.store, rollout.WithOpsContent(rollout.Auto), rollout.WithOpsLifecycle(rollout.Require))
			h := newOpSweepHarness(cs)
			h.s.safePass()
			d := h.degraded()
			if len(d) != 1 {
				t.Fatalf("degrades = %+v, want exactly one", d)
			}
			if d[0].Mode != rollout.Require || d[0].Surface != legacyop.SurfaceController || d[0].Stage != legacyop.StageOpen || d[0].Reason != tc.reason || d[0].Err == nil {
				t.Fatalf("degrade = %+v, want require/controller/open/%s with the error", d[0], tc.reason)
			}

			// The store becomes capable: the next pass opens it and sweeps.
			store := newOpSweepStore()
			op := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "retry")
			cs.mu.Lock()
			cs.cityBeadStore = store
			cs.mu.Unlock()
			h.clk.Set(op.FinishBy.Add(time.Second))
			h.s.safePass()
			if got := getOp(t, store, op.ID); got.Status != opjournal.StatusClosed {
				t.Fatalf("the retried open did not sweep: %s", got.Status)
			}
			if n := len(h.degraded()); n != 1 {
				t.Fatalf("degrades after the retry = %d, want still 1", n)
			}
		})
	}
	if got := legacyop.DegradeReason(errors.New("transport down")); got != legacyop.DegradeStoreError {
		t.Fatalf("an unclassified open error = %q, want %q", got, legacyop.DegradeStoreError)
	}
}

// panicOnceStore panics on its first List.
type panicOnceStore struct {
	opSweepStore
	panicked atomic.Bool
}

func (p *panicOnceStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if p.panicked.CompareAndSwap(false, true) {
		panic("list exploded")
	}
	return p.opSweepStore.List(q)
}

// TestOpSweepRecoversAPanickingPass proves a panicking pass is recovered, the
// store work it counted is ended, and the next pass runs.
func TestOpSweepRecoversAPanickingPass(t *testing.T) {
	store := &panicOnceStore{opSweepStore: newOpSweepStore()}
	op := seedOp(t, store.opSweepStore, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyAPI, "panic")
	cs := newOpSweepCS(store, rollout.WithOpsContent(rollout.Auto))
	h := newOpSweepHarness(cs)
	h.clk.Set(op.FinishBy.Add(time.Second))
	if !h.s.safePass() {
		t.Fatal("the first pass did not panic")
	}
	if cs.storeWorkInFlight() {
		t.Fatal("the panicking pass left its store work counted")
	}
	if l := h.logLines("op-sweep: pass panicked"); len(l) != 1 || !strings.Contains(l[0], "list exploded") || strings.Contains(l[0], "stops") {
		t.Fatalf("panic lines = %q, want one naming the panic, not saying the sweeper stops", l)
	}
	if h.s.safePass() {
		t.Fatal("the second pass panicked")
	}
	if got := getOp(t, store.opSweepStore, op.ID); got.Status != opjournal.StatusClosed {
		t.Fatalf("the pass after the panic did not sweep: %s", got.Status)
	}
}

// TestOpSweepGatesOffPanicStops proves a panicking gates-off pass leaves no
// work, so a panic that recurs every pass cannot keep the loop ticking, and
// that its log line says the sweeper stops.
func TestOpSweepGatesOffPanicStops(t *testing.T) {
	store := &panicOnceStore{opSweepStore: newOpSweepStore()}
	op := seedOp(t, store.opSweepStore, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyAPI, "panic")
	cs := newOpSweepCS(store)
	h := newOpSweepHarness(cs)
	h.clk.Set(op.FinishBy.Add(time.Second))
	waitOpSweep(t, runOpSweep(context.Background(), h))
	if !store.panicked.Load() {
		t.Fatal("the boot pass did not panic")
	}
	if n := h.tickers.Load(); n != 0 {
		t.Fatalf("started %d ticker(s) after a panicking pass with every gate off, want none", n)
	}
	if cs.storeWorkInFlight() {
		t.Fatal("the panicking pass left its store work counted")
	}
	if l := h.logLines("op-sweep: pass panicked"); len(l) != 1 || !strings.Contains(l[0], "every ops gate is off, so the sweeper stops") {
		t.Fatalf("panic lines = %q, want one saying the sweeper stops", l)
	}
	if got := getOp(t, store.opSweepStore, op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("the expired op = %s after the panicking pass, want it left open", got.Status)
	}
}

// TestOpSweepGatesOffFinishesPendingOps is the rollback path (DES-O0API §12):
// with every gate off, an op still within its finish_by at boot keeps the
// ticker running until a pass closes it, and then the loop returns.
func TestOpSweepGatesOffFinishesPendingOps(t *testing.T) {
	store := newOpSweepStore()
	op := seedOp(t, store, &clock.Fake{Time: opSweepT0.Add(-10 * time.Second)}, opjournal.ExecutorLegacyAPI, "inflight")
	h := newOpSweepHarness(newOpSweepCS(store))
	done := runOpSweep(context.Background(), h)
	h.tick(t) // still within finish_by: the loop keeps going
	if got := getOp(t, store, op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("an op within its finish_by was touched: %s", got.Status)
	}
	// The pass that tick started may already see the moved clock, close the
	// op and end the loop; otherwise the next tick's pass does.
	h.clk.Set(op.FinishBy.Add(time.Second))
	select {
	case h.ticks <- h.clk.Now():
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop neither took the tick nor returned")
	}
	waitOpSweep(t, done)
	if got := getOp(t, store, op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
		t.Fatalf("the pending op = %s %+v, want closed failed{deadline_in_flight}", got.Status, got.Outcome)
	}
	if n := h.tickers.Load(); n != 1 {
		t.Fatalf("started %d ticker(s), want 1", n)
	}
	if d := h.degraded(); len(d) != 0 {
		t.Fatalf("degrades = %+v, want none", d)
	}
}

// opSweepFlakyStore fails its first n Lists.
type opSweepFlakyStore struct {
	opSweepStore
	fails atomic.Int32
}

func (f *opSweepFlakyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if f.fails.Add(-1) >= 0 {
		return nil, errors.New("dolt down")
	}
	return f.opSweepStore.List(q)
}

// TestOpSweepGatesOffRetriesAFailedRead proves a failed boot read with every
// gate off is retried on the ticker, logged rather than handed to the degrade
// hook, and the loop returns once a pass finishes the work.
func TestOpSweepGatesOffRetriesAFailedRead(t *testing.T) {
	inner := newOpSweepStore()
	op := seedOp(t, inner, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "retry")
	store := &opSweepFlakyStore{opSweepStore: inner}
	store.fails.Store(1)
	h := newOpSweepHarness(newOpSweepCS(store))
	h.clk.Set(op.FinishBy.Add(time.Second))
	done := runOpSweep(context.Background(), h)
	select {
	case h.ticks <- h.clk.Now():
	case <-done:
		t.Fatal("the loop returned after a failed boot read with every gate off")
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop is not taking ticks")
	}
	waitOpSweep(t, done)
	if got := getOp(t, inner, op.ID); got.Status != opjournal.StatusClosed {
		t.Fatalf("the retried pass did not sweep: %s", got.Status)
	}
	if d := h.degraded(); len(d) != 0 {
		t.Fatalf("gates off: degrade hook heard %+v, want nothing (it never receives mode off)", d)
	}
	if l := h.logLines("reason=store_error"); len(l) != 1 || !strings.Contains(l[0], "mode=off") {
		t.Fatalf("degrade log lines = %q, want one mode=off store_error line", l)
	}
}

// TestOpSweepGatesOffNoOpsSkipsTheProbe proves a boot pass with every gate
// off over a store with no submitted op rows reads once and never opens the
// op store (NewStore's capability probe), and the loop returns.
func TestOpSweepGatesOffNoOpsSkipsTheProbe(t *testing.T) {
	store := newOpSweepStore()
	h := newOpSweepHarness(newOpSweepCS(store))
	waitOpSweep(t, runOpSweep(context.Background(), h))
	if n := opSweepLists(store.OpRecordingStore); n != 1 {
		t.Fatalf("the boot pass read %d time(s), want the one submitted-op probe", n)
	}
	h.s.mu.Lock()
	opened := h.s.st != nil
	h.s.mu.Unlock()
	if opened {
		t.Fatal("the boot pass opened the op store with nothing to sweep")
	}
}

// TestOpSweepGatesOffIncapableStoreStops proves an incapable store with op
// rows, every gate off, is not left work: the loop returns rather than
// ticking forever, and nothing reaches the degrade hook.
func TestOpSweepGatesOffIncapableStoreStops(t *testing.T) {
	inner := newOpSweepStore()
	seedOp(t, inner, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "stuck")
	h := newOpSweepHarness(newOpSweepCS(incapableStore{inner}))
	waitOpSweep(t, runOpSweep(context.Background(), h))
	if n := h.tickers.Load(); n != 0 {
		t.Fatalf("started %d ticker(s) over an incapable store with every gate off, want none", n)
	}
	if d := h.degraded(); len(d) != 0 {
		t.Fatalf("degrade hook heard %+v, want nothing", d)
	}
	if l := h.logLines("reason=conditional_writes_unsupported"); len(l) != 1 {
		t.Fatalf("degrade log lines = %q, want one", l)
	}
}

// opSweepIncapableCases are sessions stores that cannot record ops, each
// over a recording store that sees their reads.
var opSweepIncapableCases = []struct {
	name   string
	store  func(*beadstest.OpRecordingStore) beads.Store
	reason string
}{
	{"no conditional writes", func(rec *beadstest.OpRecordingStore) beads.Store {
		return incapableStore{opSweepStore{rec}}
	}, legacyop.DegradeConditionalWritesUnsupported},
	{"no namespace", func(rec *beadstest.OpRecordingStore) beads.Store { return rec }, legacyop.DegradeNoNamespace},
}

// TestOpSweepDefaultAutoIncapableStoreIdles proves the builtin default
// (every ops gate auto, none set) over a store that cannot record ops runs
// as if every gate were off: no degrade event and one log line saying why.
// The loop keeps its ticker, as the gates are on, but a tick over that same
// store reads nothing. An explicit auto over the same store degrades and
// keeps ticking.
func TestOpSweepDefaultAutoIncapableStoreIdles(t *testing.T) {
	for _, tc := range opSweepIncapableCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := beadstest.NewOpRecordingStore(&beads.MemStore{HonorExplicitIDs: true})
			cs := &controllerState{cityBeadStore: tc.store(rec), rolloutFlags: rollout.ForTest(), beadsQuiescent: new(atomic.Bool)}
			h := newOpSweepHarness(cs)
			ctx, cancel := context.WithCancel(context.Background())
			done := runOpSweep(ctx, h)
			h.tick(t)
			h.tick(t) // the first tick's pass has finished
			cancel()
			waitOpSweep(t, done)
			if n := h.tickers.Load(); n != 1 {
				t.Fatalf("started %d ticker(s) under the builtin default over an incapable store, want 1", n)
			}
			if n := opSweepLists(rec); n != 1 {
				t.Fatalf("passes read the store %d time(s), want only the boot pass's submitted-op probe", n)
			}
			if d := h.degraded(); len(d) != 0 {
				t.Fatalf("degrade hook heard %+v, want nothing under the builtin default", d)
			}
			// The builtin default records only over SQLite, so it leaves this
			// store unopened before any capability check.
			if l := h.logLines("auto by default"); len(l) != 1 || !strings.Contains(l[0], legacyop.DegradeNotRecordedByDefault) {
				t.Fatalf("log lines = %q, want one naming %s", l, legacyop.DegradeNotRecordedByDefault)
			}

			explicitStore := tc.store(beadstest.NewOpRecordingStore(&beads.MemStore{HonorExplicitIDs: true}))
			explicit := newOpSweepHarness(newOpSweepCS(explicitStore, rollout.WithOpsContent(rollout.Auto)))
			explicit.s.safePass()
			if d := explicit.degraded(); len(d) != 1 || d[0].Reason != tc.reason {
				t.Fatalf("explicit auto: degrades = %+v, want one %s", d, tc.reason)
			}
			if !explicit.s.wantsTicker() {
				t.Fatal("explicit auto: the sweeper stopped ticking")
			}
		})
	}
}

// TestOpSweepDefaultAutoSweepsAStoreThatCanRecordOnceSwappedIn proves the
// builtin default's incapable latch holds only for the store it was found
// on: after an incapable boot pass, once a reload swaps in a SQLite sessions
// store, the next tick opens that store and sweeps it, as the API and the CLI
// record over it from then on.
func TestOpSweepDefaultAutoSweepsAStoreThatCanRecordOnceSwappedIn(t *testing.T) {
	for _, tc := range opSweepIncapableCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := beadstest.NewOpRecordingStore(&beads.MemStore{HonorExplicitIDs: true})
			cs := &controllerState{cityBeadStore: tc.store(rec), rolloutFlags: rollout.ForTest(), beadsQuiescent: new(atomic.Bool)}
			h := newOpSweepHarness(cs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runOpSweep(ctx, h)
			h.tick(t)
			h.tick(t) // the first tick's pass, over the incapable store, has finished

			capable := newOpSweepSQLiteStore(t)
			op := seedOp(t, capable, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyAPI, "after-reload")
			h.clk.Set(op.FinishBy.Add(time.Second))
			cs.mu.Lock()
			cs.cityBeadStore = capable
			cs.mu.Unlock()
			h.tick(t)
			h.tick(t) // a pass over the swapped-in store has finished
			cancel()
			waitOpSweep(t, done)
			if got := getOp(t, capable, op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
				t.Fatalf("the expired op on the swapped-in store = %+v, want closed failed{deadline_in_flight}", got)
			}
			if d := h.degraded(); len(d) != 0 {
				t.Fatalf("degrade hook heard %+v, want nothing", d)
			}
		})
	}
}

// TestOpSweepDefaultAutoCapableStoreTicks proves the builtin default over a
// SQLite store opens the op store and keeps the sweeper's ticker, as an
// explicit auto does.
func TestOpSweepDefaultAutoCapableStoreTicks(t *testing.T) {
	cs := &controllerState{cityBeadStore: newOpSweepSQLiteStore(t), rolloutFlags: rollout.ForTest(), beadsQuiescent: new(atomic.Bool)}
	h := newOpSweepHarness(cs)
	if h.s.safePass() {
		t.Fatal("the pass panicked")
	}
	if !h.s.hasStore() || h.s.incapableNow() {
		t.Fatal("the builtin default did not open the op store over SQLite")
	}
	if !h.s.wantsTicker() {
		t.Fatal("the builtin default over a capable store stopped ticking")
	}
	if d := h.degraded(); len(d) != 0 {
		t.Fatalf("degrades = %+v, want none", d)
	}
}

// TestOpSweepGatesOffNilStoreRetries is the boot whose best-effort city-store
// open failed (cityBeadStore nil): with every gate off the boot pass neither
// panics nor ends the loop, and once a reload installs the store the next
// pass closes the expired op and the loop returns.
func TestOpSweepGatesOffNilStoreRetries(t *testing.T) {
	cs := newOpSweepCS(nil)
	h := newOpSweepHarness(cs)
	if h.s.safePass() {
		t.Fatal("a gates-off pass over a nil sessions store panicked")
	}
	if !h.s.wantsTicker() {
		t.Fatal("a nil-store pass left no work with every gate off")
	}
	if l := h.logLines("reason=store_error"); len(l) != 1 || !strings.Contains(l[0], "mode=off") {
		t.Fatalf("degrade log lines = %q, want one mode=off store_error line", l)
	}
	store := newOpSweepStore()
	op := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "late-store")
	h.clk.Set(op.FinishBy.Add(time.Second))
	done := runOpSweep(context.Background(), h)
	h.tick(t) // the store is still nil: the loop keeps going
	cs.mu.Lock()
	cs.cityBeadStore = store
	cs.mu.Unlock()
	select {
	case h.ticks <- h.clk.Now():
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop neither took the tick nor returned")
	}
	waitOpSweep(t, done)
	if got := getOp(t, store, op.ID); got.Status != opjournal.StatusClosed {
		t.Fatalf("the op = %s, want closed once the store is installed", got.Status)
	}
	if d := h.degraded(); len(d) != 0 {
		t.Fatalf("gates off: degrade hook heard %+v, want nothing", d)
	}
}

// opSweepLegacyReadFailStore fails its first sorted List (ListExpiredLegacy's
// read), not the unsorted HasSubmittedOps probe before it.
type opSweepLegacyReadFailStore struct {
	opSweepStore
	failed atomic.Bool
}

func (f *opSweepLegacyReadFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Sort == beads.SortCreatedAsc && f.failed.CompareAndSwap(false, true) {
		return nil, errors.New("dolt down")
	}
	return f.opSweepStore.List(q)
}

// opSweepCloseFailStore fails its first conditional write with a store error.
type opSweepCloseFailStore struct {
	opSweepStore
	failed atomic.Bool
}

func (f *opSweepCloseFailStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if f.failed.CompareAndSwap(false, true) {
		return errors.New("dolt down")
	}
	return f.opSweepStore.UpdateIfMatch(id, rev, opts)
}

// TestOpSweepGatesOffRetriesAFailedSweep proves a retryable failure inside
// legacyop.Sweep after the probe found ops (a failed legacy read, a failed
// close), with every gate off, keeps the ticker until a pass closes the op.
func TestOpSweepGatesOffRetriesAFailedSweep(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(opSweepStore) beads.Store
	}{
		{"legacy read", func(s opSweepStore) beads.Store { return &opSweepLegacyReadFailStore{opSweepStore: s} }},
		{"close", func(s opSweepStore) beads.Store { return &opSweepCloseFailStore{opSweepStore: s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := newOpSweepStore()
			op := seedOp(t, inner, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "retry")
			h := newOpSweepHarness(newOpSweepCS(tc.wrap(inner)))
			h.clk.Set(op.FinishBy.Add(time.Second))
			done := runOpSweep(context.Background(), h)
			select {
			case h.ticks <- h.clk.Now():
			case <-done:
				t.Fatal("the loop returned after a retryable sweep failure with every gate off")
			case <-time.After(10 * time.Second):
				t.Fatal("the sweeper loop is not taking ticks")
			}
			waitOpSweep(t, done)
			if got := getOp(t, inner, op.ID); got.Status != opjournal.StatusClosed {
				t.Fatalf("the retried pass did not close %s: %s", op.ID, got.Status)
			}
		})
	}
}

// TestOpSweepGatesOffStuckTruncatedReadStops proves a gates-off pass whose
// read stopped at its page cap and closed nothing leaves no work: a head of
// unsweepable rows longer than the cap, which no later pass gets past, cannot
// keep the loop ticking. The pass says the sweeper stops, and gc doctor's
// ops-recording check, with every gate off too, reports the expired op the
// head hides.
func TestOpSweepGatesOffStuckTruncatedReadStops(t *testing.T) {
	store := newOpSweepStore()
	seedUndecodableOps(t, store, 8*opSweepLimit) // more rows than legacyScanPages pages of opSweepLimit
	op := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "behind")
	h := newOpSweepHarness(newOpSweepCS(store))
	h.clk.Set(op.FinishBy.Add(time.Second))
	waitOpSweep(t, runOpSweep(context.Background(), h))
	if n := h.tickers.Load(); n != 0 {
		t.Fatalf("started %d ticker(s) after a truncated pass that closed nothing with every gate off, want none", n)
	}
	if lines := h.logLines("op-sweep: listed="); len(lines) != 1 || !strings.Contains(lines[0], "closed=0 ") || !strings.Contains(lines[0], "truncated=true") {
		t.Fatalf("summary = %q, want one truncated pass that closed nothing", lines)
	}
	if lines := h.logLines("the sweeper stops"); len(lines) != 1 || !strings.Contains(lines[0], "page cap") || !strings.Contains(lines[0], "gc doctor") {
		t.Fatalf("stop lines = %q, want one saying the read stopped at its page cap and pointing at gc doctor", lines)
	}
	if got := getOp(t, store, op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("the op behind the head = %s, want it left open", got.Status)
	}

	c, _ := newTestOpsRecordingCheck(store, op.FinishBy.Add(opsRecordingStaleAfter+time.Second))
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusWarning || !strings.HasPrefix(r.Message, "ops recording is off, but") || !strings.Contains(r.Message, "page cap") {
		t.Fatalf("doctor = %v %q, want a gates-off Warning that its scan stopped at the page cap", r.Status, r.Message)
	}
	if !strings.Contains(r.FixHint, "restart the controller once the head has shrunk") {
		t.Fatalf("doctor remedy %q, want the gates-off restart once the head shrinks", r.FixHint)
	}
}

// TestOpSweepGatesOffTruncatedReadKeepsTickingAfterProgress proves a gates-off
// pass whose read stopped at its page cap after closing ops leaves work, as
// the closes shrink the head the next read starts from, and the loop returns
// once a pass reads past the head.
func TestOpSweepGatesOffTruncatedReadKeepsTickingAfterProgress(t *testing.T) {
	store := newOpSweepStore()
	clk := &clock.Fake{Time: opSweepT0}
	ops := []opjournal.Op{
		seedOp(t, store, clk, opjournal.ExecutorLegacyCLI, "head0"),
		seedOp(t, store, clk, opjournal.ExecutorLegacyCLI, "head1"),
	}
	seedUndecodableOps(t, store, 8*opSweepLimit-len(ops)) // with the head ops, legacyScanPages full pages
	ops = append(ops, seedOp(t, store, clk, opjournal.ExecutorLegacyCLI, "behind"))
	h := newOpSweepHarness(newOpSweepCS(store))
	h.clk.Set(ops[0].FinishBy.Add(time.Second))
	done := runOpSweep(context.Background(), h)
	select {
	case h.ticks <- h.clk.Now():
	case <-done:
		t.Fatal("the loop returned after a truncated pass that closed ops with every gate off")
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop is not taking ticks")
	}
	waitOpSweep(t, done)
	for _, op := range ops {
		if got := getOp(t, store, op.ID); got.Status != opjournal.StatusClosed {
			t.Fatalf("op %s = %s after two passes, want closed", op.ID, got.Status)
		}
	}
	want := []string{
		"listed=2 closed=2 pending=0 raced=0 failed=0 full=false truncated=true ",
		"listed=1 closed=1 pending=0 raced=0 failed=0 full=false truncated=false ",
	}
	lines := h.logLines("op-sweep: listed=")
	if len(lines) != len(want) {
		t.Fatalf("summaries = %q, want %d", lines, len(want))
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Fatalf("summary %d = %q, want %q", i, lines[i], w)
		}
	}
	if n := h.tickers.Load(); n != 1 {
		t.Fatalf("started %d ticker(s), want 1", n)
	}
}

// TestOpSweepPagesPastInFlightOps proves ops still within their finish_by do
// not use up a pass's limit: one pass closes an expired op behind
// opSweepLimit older ops an adopter gave a later finish_by, and leaves those
// pending.
func TestOpSweepPagesPastInFlightOps(t *testing.T) {
	store := newOpSweepStore()
	clk := &clock.Fake{Time: opSweepT0}
	inflight := make([]opjournal.Op, 0, opSweepLimit)
	for i := range opSweepLimit {
		inflight = append(inflight, seedOp(t, store, clk, opjournal.ExecutorLegacyAPI, fmt.Sprintf("inflight%03d", i)))
	}
	expired := seedOp(t, store, clk, opjournal.ExecutorLegacyCLI, "expired")
	adopter, err := opjournal.NewStore(beads.SessionStore{Store: store}, &clock.Fake{Time: expired.FinishBy}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for _, op := range inflight {
		if _, err := adopter.Adopt(op.ID, op.Attempt, string(legacyop.ExecutorAPI), expired.FinishBy.Add(legacyop.ExecutionBound)); err != nil {
			t.Fatalf("Adopt %s: %v", op.ID, err)
		}
	}
	h := newOpSweepHarness(newOpSweepCS(store))
	h.clk.Set(expired.FinishBy.Add(time.Second))
	h.s.safePass()
	if got := getOp(t, store, expired.ID); got.Status != opjournal.StatusClosed || got.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
		t.Fatalf("the expired op behind %d in-flight ops = %s %+v, want closed failed{deadline_in_flight}", opSweepLimit, got.Status, got.Outcome)
	}
	for _, op := range inflight {
		if got := getOp(t, store, op.ID); got.Status != opjournal.StatusSubmitted {
			t.Fatalf("in-flight op %s = %s, want submitted", op.ID, got.Status)
		}
	}
	want := fmt.Sprintf("listed=%d closed=1 pending=%d ", opSweepLimit+1, opSweepLimit)
	if l := h.logLines("op-sweep: listed="); len(l) != 1 || !strings.Contains(l[0], want) {
		t.Fatalf("summaries = %q, want one containing %q", l, want)
	}
	if !h.s.wantsTicker() {
		t.Fatal("a gates-off pass that left in-flight ops pending left no work")
	}
}

// TestOpSweepGatesOffFullBatchKeepsTicking proves a gates-off pass that
// closed a full batch of opSweepLimit ops leaves work: the read stopped at
// the limit, so the ops behind it, an op still within its finish_by among
// them, are reached only by later passes, and the loop returns once every op
// is closed.
func TestOpSweepGatesOffFullBatchKeepsTicking(t *testing.T) {
	store := newOpSweepStore()
	expired := make([]opjournal.Op, 0, opSweepLimit+1)
	for i := range opSweepLimit + 1 {
		expired = append(expired, seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, fmt.Sprintf("expired%03d", i)))
	}
	// Seeded last, so the oldest-first read reaches it only behind the backlog.
	inflight := seedOp(t, store, &clock.Fake{Time: opSweepT0.Add(legacyop.ExecutionBound)}, opjournal.ExecutorLegacyAPI, "inflight")
	h := newOpSweepHarness(newOpSweepCS(store))
	h.clk.Set(expired[0].FinishBy.Add(time.Second))
	done := runOpSweep(context.Background(), h)
	select {
	case h.ticks <- h.clk.Now():
	case <-done:
		t.Fatal("the loop returned after a full batch with every gate off")
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop is not taking ticks")
	}
	h.tick(t) // the second pass left the in-flight op pending
	for _, op := range expired {
		if got := getOp(t, store, op.ID); got.Status != opjournal.StatusClosed {
			t.Fatalf("expired op %s = %s after two passes, want closed", op.ID, got.Status)
		}
	}
	if got := getOp(t, store, inflight.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("an op within its finish_by was touched: %s", got.Status)
	}
	// The pass that tick started may already see the moved clock, close the
	// op and end the loop; otherwise the next tick's pass does.
	h.clk.Set(inflight.FinishBy.Add(time.Second))
	select {
	case h.ticks <- h.clk.Now():
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweeper loop neither took the tick nor returned")
	}
	waitOpSweep(t, done)
	if got := getOp(t, store, inflight.ID); got.Status != opjournal.StatusClosed || got.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
		t.Fatalf("the in-flight op = %s %+v, want closed failed{deadline_in_flight}", got.Status, got.Outcome)
	}
	// The boot pass read a full batch of expired ops, not the in-flight op.
	want := []string{
		fmt.Sprintf("listed=%d closed=%d pending=0 ", opSweepLimit, opSweepLimit),
		"listed=2 closed=1 pending=1 ",
		"listed=1 closed=1 pending=0 ",
	}
	lines := h.logLines("op-sweep: listed=")
	if len(lines) != len(want) {
		t.Fatalf("summaries = %q, want %d", lines, len(want))
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Fatalf("summary %d = %q, want %q", i, lines[i], w)
		}
	}
	if n := h.tickers.Load(); n != 1 {
		t.Fatalf("started %d ticker(s), want 1", n)
	}
	if d := h.degraded(); len(d) != 0 {
		t.Fatalf("degrades = %+v, want none", d)
	}
}

// opSweepCloseRefusedStore refuses every conditional write as unsupported, a
// close failure no later pass can clear.
type opSweepCloseRefusedStore struct{ opSweepStore }

func (opSweepCloseRefusedStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return fmt.Errorf("closing: %w", beads.ErrConditionalWriteUnsupported)
}

// TestOpSweepGatesOffRefusedFullBatchStops proves a gates-off full batch in
// which no op closed, its closes refused for good, leaves no work: a store
// refusing every close cannot keep the loop ticking.
func TestOpSweepGatesOffRefusedFullBatchStops(t *testing.T) {
	inner := newOpSweepStore()
	var last opjournal.Op
	for i := range opSweepLimit + 1 {
		last = seedOp(t, inner, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, fmt.Sprintf("refused%03d", i))
	}
	h := newOpSweepHarness(newOpSweepCS(opSweepCloseRefusedStore{inner}))
	h.clk.Set(last.FinishBy.Add(time.Second))
	waitOpSweep(t, runOpSweep(context.Background(), h))
	if n := h.tickers.Load(); n != 0 {
		t.Fatalf("started %d ticker(s) over a store refusing every close with every gate off, want none", n)
	}
	want := fmt.Sprintf("listed=%d closed=0 pending=0 raced=0 failed=%d ", opSweepLimit, opSweepLimit)
	if l := h.logLines("op-sweep: listed="); len(l) != 1 || !strings.Contains(l[0], want) {
		t.Fatalf("summaries = %q, want one containing %q", l, want)
	}
	if l := h.logLines("reason=conditional_writes_unsupported"); len(l) != 1 || !strings.Contains(l[0], "mode=off") {
		t.Fatalf("degrade log lines = %q, want one mode=off conditional_writes_unsupported line", l)
	}
	if l := h.logLines("the sweeper stops"); len(l) != 1 || !strings.Contains(l[0], fmt.Sprintf("its limit of %d expired ops", opSweepLimit)) {
		t.Fatalf("stop lines = %q, want one saying the read stopped at its limit", l)
	}
}

// TestOpSweepReopensAReplacedStore proves the cached op store is reopened
// when the sessions store is replaced after a successful pass.
func TestOpSweepReopensAReplacedStore(t *testing.T) {
	first := newOpSweepStore()
	cs := newOpSweepCS(first, rollout.WithOpsContent(rollout.Auto))
	h := newOpSweepHarness(cs)
	h.s.safePass()
	if opSweepLists(first.OpRecordingStore) == 0 {
		t.Fatal("the first pass did not read the first store")
	}
	second := newOpSweepStore()
	op := seedOp(t, second, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyAPI, "swapped")
	cs.mu.Lock()
	cs.cityBeadStore = second
	cs.mu.Unlock()
	first.Reset()
	h.clk.Set(op.FinishBy.Add(time.Second))
	h.s.safePass()
	if got := getOp(t, second, op.ID); got.Status != opjournal.StatusClosed {
		t.Fatalf("the pass after the swap did not sweep the new store: %s", got.Status)
	}
	if n := len(first.Ops()); n != 0 {
		t.Fatalf("the pass after the swap still touched the old store: %d op(s)", n)
	}
}

// TestOpSweepLogDegradeRateLimit proves the default hook logs one line per
// (stage, reason) per legacyop.DegradeLogInterval.
func TestOpSweepLogDegradeRateLimit(t *testing.T) {
	h := newOpSweepHarness(newOpSweepCS(newOpSweepStore()))
	d := legacyop.Degrade{
		Mode: rollout.Auto, Surface: legacyop.SurfaceController, Stage: legacyop.StageSweep,
		Reason: legacyop.DegradeStoreError, Err: errors.New("dolt down"),
	}
	h.s.logDegrade(d)
	h.clk.Advance(legacyop.DegradeLogInterval - time.Second)
	h.s.logDegrade(d)
	other := d
	other.Stage = legacyop.StageOpen
	h.s.logDegrade(other)
	if l := h.logLines("degraded"); len(l) != 2 {
		t.Fatalf("log lines inside the interval = %q, want one per (stage, reason)", l)
	}
	h.clk.Advance(time.Second)
	h.s.logDegrade(d)
	if l := h.logLines("stage=sweep"); len(l) != 2 {
		t.Fatalf("sweep-stage lines after the interval = %q, want 2", l)
	}
}

// undecodableStore holds an open op row this build cannot decode.
func undecodableStore(t *testing.T, n int) opSweepStore {
	t.Helper()
	store := newOpSweepStore()
	for i := range n {
		if _, err := store.Create(beads.Bead{
			ID: fmt.Sprintf("gc-op-newer%d", i), Type: opjournal.BeadType,
			Labels:   []string{opjournal.LabelOp, opjournal.ConversationLabel("gc-1")},
			Metadata: map[string]string{"gc.op.schema": "99", "gc.op.status": string(opjournal.StatusSubmitted)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

// TestOpSweepStuckSummaryIsRateLimited proves a pass that only saw rows it
// cannot decode logs its summary once per legacyop.DegradeLogInterval, with
// the count and the first few ids, not every pass and not every id.
func TestOpSweepStuckSummaryIsRateLimited(t *testing.T) {
	store := undecodableStore(t, opSweepLogIDs+3)
	h := newOpSweepHarness(newOpSweepCS(store, rollout.WithOpsLifecycle(rollout.Auto)))
	h.s.safePass()
	h.clk.Advance(opSweepInterval)
	h.s.safePass()
	lines := h.logLines("op-sweep: listed=")
	if len(lines) != 1 {
		t.Fatalf("summary lines = %q, want one inside the interval", lines)
	}
	if !strings.Contains(lines[0], fmt.Sprintf("undecodable=%d", opSweepLogIDs+3)) || !strings.Contains(lines[0], "(+3 more)") ||
		strings.Contains(lines[0], fmt.Sprintf("gc-op-newer%d", opSweepLogIDs)) {
		t.Fatalf("summary = %q, want the count and the first %d ids", lines[0], opSweepLogIDs)
	}
	h.clk.Advance(legacyop.DegradeLogInterval)
	h.s.safePass()
	if lines := h.logLines("op-sweep: listed="); len(lines) != 2 {
		t.Fatalf("summary lines after the interval = %d, want 2", len(lines))
	}
}

// unsupportedAtCallStore passes the capability probe but rejects every
// conditional write, as a bd that advertises --if-revision and then refuses
// it does.
type unsupportedAtCallStore struct{ opSweepStore }

func (unsupportedAtCallStore) UpdateIfMatch(string, int64, beads.UpdateOpts) error {
	return fmt.Errorf("bd update --if-revision: %w", beads.ErrConditionalWriteUnsupported)
}

// TestOpSweepClassifiesSweepFailures proves a failed close degrades with its
// classified reason, not a blanket store_error.
func TestOpSweepClassifiesSweepFailures(t *testing.T) {
	inner := newOpSweepStore()
	op := seedOp(t, inner, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyCLI, "unsupported")
	h := newOpSweepHarness(newOpSweepCS(unsupportedAtCallStore{inner}, rollout.WithOpsControl(rollout.Require)))
	h.clk.Set(op.FinishBy.Add(time.Second))
	h.s.safePass()
	d := h.degraded()
	if len(d) != 1 || d[0].Stage != legacyop.StageSweep || d[0].Reason != legacyop.DegradeConditionalWritesUnsupported || d[0].Mode != rollout.Require {
		t.Fatalf("degrades = %+v, want one require/sweep/conditional_writes_unsupported", d)
	}
}

// TestOpSweepDefaultAutoNonSQLiteStoreSweepsLeftoversThenIdles proves the
// builtin default over a sessions store that can record ops but is not
// SQLite-backed (a Dolt-served one in production) sweeps like a gates-off
// pass: the op an earlier explicit gate left expired is closed, nothing
// degrades, and once nothing is left a tick over that same store reads
// nothing. An explicit auto over the same store sweeps under auto.
func TestOpSweepDefaultAutoNonSQLiteStoreSweepsLeftoversThenIdles(t *testing.T) {
	store := newOpSweepStore()
	op := seedOp(t, store, &clock.Fake{Time: opSweepT0}, opjournal.ExecutorLegacyAPI, "left-by-explicit")
	cs := &controllerState{cityBeadStore: store, rolloutFlags: rollout.ForTest(), beadsQuiescent: new(atomic.Bool)}
	h := newOpSweepHarness(cs)
	h.clk.Set(op.FinishBy.Add(time.Second))
	if h.s.safePass() {
		t.Fatal("the pass panicked")
	}
	if got := getOp(t, store, op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
		t.Fatalf("the expired op = %+v, want closed failed{deadline_in_flight}", got)
	}
	if d := h.degraded(); len(d) != 0 {
		t.Fatalf("degrade hook heard %+v, want nothing under the builtin default", d)
	}
	if !h.s.incapableNow() {
		t.Fatal("the gates-off pass cleared the latch for a store the default does not record on")
	}
	h.s.safePass() // the leftover sweep's pass left no work
	reads := opSweepLists(store.OpRecordingStore)
	h.s.safePass()
	if n := opSweepLists(store.OpRecordingStore); n != reads {
		t.Fatalf("an idle pass read the store %d more time(s), want none", n-reads)
	}

	explicitStore := newOpSweepStore()
	explicit := newOpSweepHarness(newOpSweepCS(explicitStore, rollout.WithOpsContent(rollout.Auto)))
	if explicit.s.safePass() {
		t.Fatal("explicit auto: the pass panicked")
	}
	if !explicit.s.hasStore() || explicit.s.incapableNow() {
		t.Fatal("explicit auto: the op store did not open over a capable non-SQLite store")
	}
	if d := explicit.degraded(); len(d) != 0 {
		t.Fatalf("explicit auto: degrades = %+v, want none", d)
	}
}

// The builtin auto default must see through every production wrapper around a
// split city's SQLite sessions store: the controller's cache, the CLI's
// emitting wrapper, and the stores the sweeper and doctor open. A wrapper that
// stops pointing at what it wraps would quietly turn recording off by default.

// opSweepSplitRoutes opens the production split routes (sqlite-beads binding
// for every infra class) the way storage boot does.
func opSweepSplitRoutes(t *testing.T, root string, cfg *config.City) *storageRoutes {
	t.Helper()
	plan, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatalf("resolving the storage plan: %v", err)
	}
	routes, err := openStorageRoutes(plan, mustResolveInfraTarget(t, root, cfg), cfg, root, nil)
	if err != nil {
		t.Fatalf("openStorageRoutes: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	return routes
}

func TestOpsBuiltinDefaultRecordsOnARealSplitCity(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	work := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)

	// Controller composition: CachingStore over the stamped SQLite engine.
	ctlRoutes := opSweepSplitRoutes(t, root, cfg).withControllerCache(context.Background(), nil)
	ctlSess := beads.SessionStore{Store: resolveSessionStore(ctlRoutes, work, cfg, root, nil)}
	if b := beads.ResolveConditionalWritesBackend(ctlSess); !b.SQLite {
		t.Fatalf("controller split sessions store resolves to %+v (%T), want SQLite", b, ctlSess.Store)
	}
	if err := opjournal.CheckRecordsByDefault(ctlSess); err != nil {
		t.Fatalf("controller split sessions store: CheckRecordsByDefault = %v", err)
	}
	if _, err := opjournal.NewStore(ctlSess, clock.Real{}); err != nil {
		t.Fatalf("controller split sessions store: NewStore = %v", err)
	}

	// Sweeper over a controllerState that routes sessions to that cache.
	cs := &controllerState{cityBeadStore: work, storageRoutes: ctlRoutes, cfg: cfg, cityPath: root, rolloutFlags: rollout.ForTest(), beadsQuiescent: new(atomic.Bool)}
	if got := cs.SessionsBeadStore().Store; got != ctlSess.Store {
		t.Fatalf("cs.SessionsBeadStore = %T, want the routed cache", got)
	}
	h := newOpSweepHarness(cs)
	if h.s.safePass() {
		t.Fatal("pass panicked")
	}
	if !h.s.hasStore() || h.s.incapableNow() {
		t.Fatalf("sweeper under the builtin default did not open the op store over the split SQLite sessions store; logs=%q", h.logLines(""))
	}
	if d := h.degraded(); len(d) != 0 {
		t.Fatalf("degrades = %+v", d)
	}

	// CLI composition: the funnel's emitting wrapper over the engine, then
	// withMainStoreEmission (cliSessionStore's shape).
	cliRoutes := opSweepSplitRoutes(t, root, cfg).withCLIEmission(root)
	cliSess := withMainStoreEmission(resolveSessionStore(cliRoutes, work, cfg, root, nil), root)
	if b := beads.ResolveConditionalWritesBackend(beads.SessionStore{Store: cliSess}); !b.SQLite {
		t.Fatalf("CLI split sessions store resolves to %+v (%T), want SQLite", b, cliSess)
	}
	h2 := newCLIOpHarness(t, nil)
	h2.opStore = nil
	cliOpenOpStore = func(s beads.Store) (*opjournal.Store, error) {
		h2.opens++
		return opjournal.NewStore(beads.SessionStore{Store: s}, cliOpClock)
	}
	op := newCLIOp(&config.City{}, cliSess, cliOpRequest{verb: "gc session wake", kind: opjournal.KindWake, stderr: new(strings.Builder)})
	if op.rec.Mode != rollout.Auto || !op.rec.QuietCannotRecord {
		t.Fatalf("recorder mode=%s quiet=%t, want builtin auto", op.rec.Mode, op.rec.QuietCannotRecord)
	}
	st, err := op.rec.Open()
	if err != nil || st == nil {
		t.Fatalf("CLI builtin default over the split SQLite sessions store: Open = %v, %v", st, err)
	}

	// Doctor over the CLI composition.
	c := newOpsRecordingDoctorCheck(root, cfg, rollout.ForTest(), nil)
	c.open = func() (beads.Store, error) { return work, nil }
	c.sessions = func(beads.Store) beads.Store { return cliSess }
	c.clk = &clock.Fake{Time: opSweepT0}
	r := c.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "can record ops") {
		t.Fatalf("doctor = %v %q, want OK recording", r.Status, r.Message)
	}
	t.Logf("doctor: %s", r.Message)
}
