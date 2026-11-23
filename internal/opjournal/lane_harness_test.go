package opjournal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
)

// laneRecStore is an OpRecordingStore that also forwards guarded batches,
// through a batcher the test can fault.
type laneRecStore struct {
	*beadstest.OpRecordingStore
	batcher *faultBatcher
}

// GuardedBatcherHandle opts the recorder in to guarded batches.
func (s laneRecStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) { return s.batcher, true }

// faultBatcher records every batch and lets a test fail one: fault returns
// whether to commit the batch first and the error to report.
type faultBatcher struct {
	inner beads.GuardedBatcher
	mu    sync.Mutex
	log   []beads.GuardedBatch
	fault func(n int, b beads.GuardedBatch) (commit bool, err error)
}

func (f *faultBatcher) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	f.mu.Lock()
	n := len(f.log)
	f.log = append(f.log, b)
	fault := f.fault
	f.mu.Unlock()
	if fault != nil {
		if commit, err := fault(n, b); err != nil {
			if commit {
				if _, cerr := f.inner.ApplyGuarded(ctx, b); cerr != nil {
					return beads.GuardedResult{}, cerr
				}
			}
			return beads.GuardedResult{}, err
		}
	}
	return f.inner.ApplyGuarded(ctx, b)
}

func (f *faultBatcher) batches() []beads.GuardedBatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]beads.GuardedBatch(nil), f.log...)
}

func (f *faultBatcher) setFault(fn func(n int, b beads.GuardedBatch) (bool, error)) {
	f.mu.Lock()
	f.fault = fn
	f.mu.Unlock()
}

var errLinkDropped = errors.New("connection reset mid-batch")

// errFlakyRead is a store read that failed transiently.
var errFlakyRead = errors.New("transient read error")

// flakyGetStore fails Get while fail says so for the id, as a store hiccup
// would. Its guarded batches go through a batcher the test can fault.
type flakyGetStore struct {
	*beads.MemStore
	batcher *faultBatcher
	mu      sync.Mutex
	fail    func(id string) bool
}

// GuardedBatcherHandle forwards guarded batches to the fault batcher.
func (s *flakyGetStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) { return s.batcher, true }

func (s *flakyGetStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	fail := s.fail
	s.mu.Unlock()
	if fail != nil && fail(id) {
		return beads.Bead{}, errFlakyRead
	}
	return s.MemStore.Get(id)
}

func (s *flakyGetStore) setFail(fn func(id string) bool) {
	s.mu.Lock()
	s.fail = fn
	s.mu.Unlock()
}

// newLaneHarnessFlaky opens the lane store over a MemStore whose Get the test
// can fail, with no recorder.
func newLaneHarnessFlaky(t *testing.T) (*laneHarness, *flakyGetStore) {
	t.Helper()
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	fb := &faultBatcher{inner: mem}
	fs := &flakyGetStore{MemStore: mem, batcher: fb}
	clk := &clock.Fake{Time: t0}
	s, err := NewLaneStore(beads.SessionStore{Store: fs}, clk, WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("NewLaneStore: %v", err)
	}
	return &laneHarness{mem: mem, batcher: fb, clk: clk, store: s}, fs
}

type laneHarness struct {
	mem     *beads.MemStore
	rec     *beadstest.OpRecordingStore
	batcher *faultBatcher
	clk     *clock.Fake
	store   *Store
}

func newLaneHarness(t *testing.T) *laneHarness {
	t.Helper()
	mem := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	return newLaneHarnessOver(t, mem, &clock.Fake{Time: t0})
}

func newLaneHarnessOver(t *testing.T, mem *beads.MemStore, clk *clock.Fake) *laneHarness {
	t.Helper()
	rec := beadstest.NewOpRecordingStore(mem)
	fb := &faultBatcher{inner: mem}
	s, err := NewLaneStore(beads.SessionStore{Store: laneRecStore{OpRecordingStore: rec, batcher: fb}}, clk, WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("NewLaneStore: %v", err)
	}
	t.Cleanup(func() { assertOnlyKeyedLists(t, rec) })
	return &laneHarness{mem: mem, rec: rec, batcher: fb, clk: clk, store: s}
}

// lane returns a lane named host with a counting nonce and a no-op sleep.
func (h *laneHarness) lane(t *testing.T, host string) *ConversationLane {
	t.Helper()
	l, err := NewLane(h.store, host, h.clk)
	if err != nil {
		t.Fatalf("NewLane: %v", err)
	}
	var mu sync.Mutex
	n := 0
	l.nonce = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return "0000000000" + string(rune('a'+n%26)) + string(rune('a'+(n/26)%26)), nil
	}
	l.sleep = func(context.Context, time.Duration) error { return nil }
	return l
}

func (h *laneHarness) propose(t *testing.T, req Request) Op {
	t.Helper()
	op, created, err := h.store.Propose(req)
	if err != nil || !created {
		t.Fatalf("Propose(%+v) = created %v, %v", req, created, err)
	}
	return op
}

func (h *laneHarness) get(t *testing.T, id string) Op {
	t.Helper()
	op, err := h.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return op
}

func (h *laneHarness) runtime(t *testing.T) RuntimeRow {
	t.Helper()
	r, _, found, err := h.store.Runtime("gc-1")
	if err != nil || !found {
		t.Fatalf("Runtime(gc-1) = found %v, %v", found, err)
	}
	return r
}

// laneMsg is a lane message on conversation gc-1. mods adjust the request.
func laneMsg(key string, role Role, payload string, mods ...func(*Request)) Request {
	r := Request{
		Conversation: "gc-1", Kind: KindMessage, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: "gc:mail", Actor: ActorBackground}, Payload: payload,
		Policy: Policy{Message: MessagePolicy{Role: role}},
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func withWhen(w When) func(*Request) {
	return func(r *Request) { r.Policy.Message.When = w }
}

func atMostOnce(r *Request) { r.Policy.Guarantee = GuaranteeAtMostOnce }

func liveOnly(r *Request) { r.Policy.Message.Wake = WakeLiveOnly }

func view() ConversationView {
	return ConversationView{ID: "gc-1", Generation: 1, Epoch: 1, Token: "tok-1"}
}

// fakeExec is a scriptable executor. Each call records what it saw.
type fakeExec struct {
	mu         sync.Mutex
	clk        *clock.Fake
	probe      Probe
	probeErr   error
	probes     int
	deliver    func(d Delivery) DeliveryResult
	interrupt  func(c InterruptCall) ControlResult
	respond    func(c RespondCall) ControlResult
	deliveries []Delivery
	interrupts []InterruptCall
	responds   []RespondCall
	// onEffect runs inside every effect, before the result is returned.
	onEffect func()
}

func newFakeExec(clk *clock.Fake) *fakeExec {
	return &fakeExec{clk: clk, probe: idleProbe()}
}

func idleProbe() Probe {
	return Probe{
		Available: true, Running: true, Boundary: TriYes, TurnLive: TriNo,
		Caps: Caps{Live: true, Input: FenceAtomic},
	}
}

func (f *fakeExec) Probe(context.Context, ConversationView) (Probe, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	return f.probe, f.probeErr
}

func (f *fakeExec) Deliver(_ context.Context, d Delivery) DeliveryResult {
	f.mu.Lock()
	f.deliveries = append(f.deliveries, d)
	fn, hook := f.deliver, f.onEffect
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fn != nil {
		return fn(d)
	}
	return Delivered{Record: DeliveryRecord{At: f.clk.Now(), Ack: AckDelivered}, Via: "fake"}
}

func (f *fakeExec) Interrupt(_ context.Context, c InterruptCall) ControlResult {
	f.mu.Lock()
	f.interrupts = append(f.interrupts, c)
	fn, hook := f.interrupt, f.onEffect
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fn != nil {
		return fn(c)
	}
	return Settled{Turn: c.Turn, Via: "fake"}
}

func (f *fakeExec) Respond(_ context.Context, c RespondCall) ControlResult {
	f.mu.Lock()
	f.responds = append(f.responds, c)
	fn, hook := f.respond, f.onEffect
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fn != nil {
		return fn(c)
	}
	return Settled{Via: "fake"}
}

func (f *fakeExec) delivered() []Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Delivery(nil), f.deliveries...)
}

// staleProbeExec runs during once, after its first probe returns: the probe
// is then stale when the pass acts on it.
type staleProbeExec struct {
	*fakeExec
	once   sync.Once
	during func()
}

func (s *staleProbeExec) Probe(ctx context.Context, conv ConversationView) (Probe, error) {
	p, err := s.fakeExec.Probe(ctx, conv)
	s.once.Do(s.during)
	return p, err
}
