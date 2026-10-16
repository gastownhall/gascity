package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
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

func newOpSweepCS(store beads.Store, opts ...rollout.ForTestOption) *controllerState {
	return &controllerState{cityBeadStore: store, rolloutFlags: rollout.ForTest(opts...), beadsQuiescent: new(atomic.Bool)}
}

// opSweepHarness is a sweeper over a fake clock with a hand-driven ticker
// and a captured degrade hook.
type opSweepHarness struct {
	s       *opSweeper
	clk     *clock.Fake
	ticks   chan time.Time
	tickers atomic.Int32

	mu       sync.Mutex
	degrades []legacyop.Degrade
}

func newOpSweepHarness(cs *controllerState) *opSweepHarness {
	h := &opSweepHarness{clk: &clock.Fake{Time: opSweepT0}, ticks: make(chan time.Time)}
	h.s = cs.newOpSweeper()
	h.s.clk = h.clk
	h.s.newTicker = func(time.Duration) (<-chan time.Time, func()) {
		h.tickers.Add(1)
		return h.ticks, func() {}
	}
	h.s.degraded = func(d legacyop.Degrade) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.degrades = append(h.degrades, d)
	}
	return h
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
			h.clk.Time = api.FinishBy.Add(time.Second)
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
	h.ticks <- opSweepT0 // the loop outlived the skipped boot pass
	h.ticks <- opSweepT0 // and the skipped first tick
	if n := len(store.Ops()); n != 0 {
		t.Fatalf("a quiescent pass touched the store: %d op(s)", n)
	}
	cs.beadsQuiescent.Store(false)
	// The second tick's pass may already see the city resumed and end the
	// loop; otherwise the next tick's pass does.
	select {
	case h.ticks <- opSweepT0:
	case <-done:
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
		h.ticks <- opSweepT0
	}
	h.ticks <- opSweepT0 // the third tick's pass has finished
	cancel()
	waitOpSweep(t, done)
	if got := opSweepLists(store.OpRecordingStore); got < 4 {
		t.Fatalf("passes read %d time(s), want at least the boot pass plus 3 ticks", got)
	}
	if n := h.tickers.Load(); n != 1 {
		t.Fatalf("started %d ticker(s), want 1", n)
	}
}

// incapableStore hides the conditional-write family, as an old bd does.
type incapableStore struct{ beads.Store }

// TestOpSweepOpenFailure proves a NewStore failure is silent with every gate
// off, and with a gate on is one classified degrade per pass at the strongest
// gate's mode, retried next pass.
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
			h.clk.Time = op.FinishBy.Add(time.Second)
			h.s.safePass()
			if got := getOp(t, store, op.ID); got.Status != opjournal.StatusClosed {
				t.Fatalf("the retried open did not sweep: %s", got.Status)
			}
			if n := len(h.degraded()); n != 1 {
				t.Fatalf("degrades after the retry = %d, want still 1", n)
			}
		})
	}
	if got := opStoreOpenReason(errors.New("transport down")); got != legacyop.DegradeStoreError {
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
	h.clk.Time = op.FinishBy.Add(time.Second)
	if !h.s.safePass() {
		t.Fatal("the first pass did not panic")
	}
	if cs.storeWorkInFlight() {
		t.Fatal("the panicking pass left its store work counted")
	}
	if h.s.safePass() {
		t.Fatal("the second pass panicked")
	}
	if got := getOp(t, store.opSweepStore, op.ID); got.Status != opjournal.StatusClosed {
		t.Fatalf("the pass after the panic did not sweep: %s", got.Status)
	}
}
